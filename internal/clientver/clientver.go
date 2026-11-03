// Package clientver finds a client binary (claude, opencode) and reads its
// version for `forge doctor` and the `forge sync --apply` version gate.
//
// The ONLY thing this package ever runs is "<binary> --version": exactly one
// argument, no sub-command, no other flag (a sub-command could call a model or
// change the client's configuration). The decision is user-approved and
// enforced in one place: see Exec (exec.go). Details: docs/doctor.md.
package clientver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/as950118/forge/internal/textsafe"
)

// Status is the severity of a check result (same words as `forge doctor`).
type Status string

// Statuses.
const (
	StatusOK   Status = "ok"
	StatusInfo Status = "info"
	StatusWarn Status = "warn"
)

// Result codes (stable, used by --json and tests).
const (
	CodeVerified      = "verified"
	CodeUnverifiedPat = "unverified-patch"
	CodeUnverifiedVer = "unverified-version"
	CodeNotInstalled  = "not-installed"
	CodeBlocked       = "blocked-in-project"
	CodeUnsafeLoc     = "unsafe-location" // writable by other users (location.go)
	CodeRunFailed     = "run-failed"
	CodeParseFailed   = "parse-failed"
)

// Spec describes one supported client: the binary name and the version the
// adapter formats were verified against (docs/clients/<client>.md).
type Spec struct {
	Client   string // forge client id (claude-code, opencode)
	Binary   string // command name looked up in PATH
	Verified string // exact verified version, no leading "v"
}

// Specs returns the supported clients. Keep in sync with the "확인 버전" line of
// docs/clients/claude-code.md (2.1.305) and docs/clients/opencode.md (v2.0.24).
func Specs() []Spec {
	return []Spec{
		{Client: "claude-code", Binary: "claude", Verified: "2.1.305"},
		{Client: "opencode", Binary: "opencode", Verified: "2.0.24"},
	}
}

// SpecFor returns the spec of a client id.
func SpecFor(client string) (Spec, bool) {
	for _, s := range Specs() {
		if s.Client == client {
			return s, true
		}
	}
	return Spec{}, false
}

// Result is the outcome of checking one client. Only the parsed version is kept
// from the process output.
type Result struct {
	Client   string `json:"client"`
	Binary   string `json:"binary"`
	Status   Status `json:"status"`
	Code     string `json:"code"`
	Path     string `json:"path,omitempty"`     // absolute path found in PATH
	Resolved string `json:"resolved,omitempty"` // symlink-resolved path (what actually runs)
	Version  string `json:"version,omitempty"`
	Verified string `json:"verified"`
	Message  string `json:"message"`
	// UnsafePath is the file or directory that made the location unsafe
	// (Code == CodeUnsafeLoc); the binary was NOT run.
	UnsafePath string `json:"unsafe_path,omitempty"`
	// Inexact reports that Path/UnsafePath/Message show a path with visible
	// escapes (control or invisible characters) or cut: pasting them into a
	// shell names a different path (TASK-0024 AC-3).
	Inexact bool `json:"inexact,omitempty"`
}

// RunResult is what a Runner returns: the capped output of "<bin> --version".
type RunResult struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	TimedOut  bool
	Truncated bool // output exceeded the cap (and the process was stopped)
}

// Runner runs "<path> --version". Implementations must pass exactly that one
// argument. Tests inject a fake; production uses Exec.
type Runner interface {
	RunVersion(ctx context.Context, path string) (RunResult, error)
}

// Checker locates and checks client binaries.
type Checker struct {
	Runner Runner
	// OSPath makes the search use the process's $PATH. Without it PathEnv is the
	// whole search path (the zero Checker finds nothing, so a test that forgets to
	// set it can never run a real client).
	OSPath  bool
	PathEnv string
	// ProjectDirs: binaries inside these directories are never executed.
	ProjectDirs []string
}

// Locate searches PATH for binary. Only absolute PATH entries are used (an empty
// entry, "." or any relative entry is ignored: it would resolve against the
// current directory). Binaries inside a project directory are skipped; the
// returned blocked path is the first such candidate.
func (c *Checker) Locate(binary string) (path, resolved, blocked string) {
	l := c.locate(binary)
	return l.path, l.resolved, l.blocked
}

// location is the outcome of the PATH search.
type location struct {
	path, resolved, blocked string
	// unsafe is the first candidate that other users can modify (not run); the
	// search stops there because that is the program a shell would start.
	unsafe, unsafeOffender, unsafeReason string
}

func (c *Checker) locate(binary string) (loc location) {
	pathEnv := c.PathEnv
	if c.OSPath {
		pathEnv = os.Getenv("PATH")
	}
	var projects []string
	for _, p := range c.ProjectDirs {
		if p == "" {
			continue
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		if a, err := filepath.Abs(p); err == nil {
			projects = append(projects, a)
		}
	}
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" || !filepath.IsAbs(dir) || seen[dir] {
			continue
		}
		seen[dir] = true
		cand := dir + string(filepath.Separator) + binary // not Join: ".." in a PATH entry must be walked, not cleaned away
		if _, err := exec.LookPath(cand); err != nil {    // absolute path: only the executable check
			continue
		}
		res, err := filepath.EvalSymlinks(cand)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(res); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if inAny(cand, projects) || inAny(res, projects) {
			if loc.blocked == "" {
				loc.blocked = cand
			}
			continue
		}
		if off, why := unsafeLocation(cand, res); off != "" {
			loc.unsafe, loc.unsafeOffender, loc.unsafeReason = cand, off, why
			return loc
		}
		loc.path, loc.resolved = cand, res
		return loc
	}
	return loc
}

func inAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Check locates the client's binary, runs "<bin> --version" and classifies the
// version. Nothing is cached: every call runs the binary again.
func (c *Checker) Check(ctx context.Context, spec Spec) Result {
	r := Result{Client: spec.Client, Binary: spec.Binary, Verified: spec.Verified}
	loc := c.locate(spec.Binary)
	path, resolved, blocked := loc.path, loc.resolved, loc.blocked
	if loc.unsafe != "" {
		r.Status, r.Code = StatusWarn, CodeUnsafeLoc
		r.Path = textsafe.Display(loc.unsafe, 300)
		r.UnsafePath = textsafe.Display(loc.unsafeOffender, 300)
		r.Inexact = r.Path != loc.unsafe || r.UnsafePath != loc.unsafeOffender
		r.Message = unsafeMessage(spec.Binary, loc.unsafe, loc.unsafeOffender, loc.unsafeReason)
		if r.Inexact {
			r.Message += " [경로의 제어·비가시 문자는 보이는 이스케이프로 표시했으므로 그대로 붙여 넣으면 실제 경로와 다릅니다]"
		}
		return r
	}
	if path == "" {
		if blocked != "" {
			r.Status, r.Code = StatusWarn, CodeBlocked
			r.Message = fmt.Sprintf("PATH 에서 프로젝트 디렉터리 안의 %s (%s) 만 찾아 실행하지 않았습니다. 신뢰할 수 있는 위치에 설치하세요", spec.Binary, textsafe.Display(blocked, 200))
			return r
		}
		r.Status, r.Code = StatusInfo, CodeNotInstalled
		r.Message = fmt.Sprintf("%s 가 PATH(절대 경로 항목)에 없습니다. 설치하지 않은 클라이언트면 정상입니다", spec.Binary)
		return r
	}
	r.Path, r.Resolved = textsafe.Display(path, 300), textsafe.Display(resolved, 300)
	run := c.Runner
	if run == nil {
		run = Exec{}
	}
	// The resolved path is executed: what was checked (project dir, regular file)
	// is what runs.
	out, err := run.RunVersion(ctx, resolved)
	if err != nil {
		r.Status, r.Code = StatusWarn, CodeRunFailed
		r.Message = fmt.Sprintf("%s --version 을 실행하지 못했습니다: %s%s", spec.Binary, textsafe.Display(err.Error(), 160), shimHint(spec.Binary, path, resolved))
		return r
	}
	if out.TimedOut || out.Truncated || out.ExitCode != 0 {
		r.Status, r.Code = StatusWarn, CodeRunFailed
		switch {
		case out.TimedOut:
			r.Message = fmt.Sprintf("%s --version 이 %s 안에 끝나지 않아 중단했습니다", spec.Binary, Timeout)
		case out.Truncated:
			r.Message = fmt.Sprintf("%s --version 출력이 %d바이트를 넘어 중단했습니다", spec.Binary, MaxOutput)
		default:
			r.Message = fmt.Sprintf("%s --version 이 종료 코드 %d 로 끝났습니다%s%s", spec.Binary, out.ExitCode, stderrHint(out.Stderr), shimHint(spec.Binary, path, resolved))
		}
		return r
	}
	v, ok := ParseVersion(out.Stdout)
	if !ok {
		r.Status, r.Code = StatusWarn, CodeParseFailed
		r.Message = fmt.Sprintf("%s --version 출력에서 버전(x.y.z)을 인식하지 못했습니다. 생성한 설정이 올바르게 로드되지 않을 수 있습니다", spec.Binary)
		return r
	}
	r.Version = v
	r.Status, r.Code, r.Message = Classify(v, spec.Verified)
	return r
}

// versionManagerDirs are directory names of runtime/version managers whose shims
// look up their runtime through the real HOME (TASK-0024 AC-7).
var versionManagerDirs = []string{"/.nvm/", "/.asdf/", "/.volta/", "/.fnm/", "/.pyenv/", "/.rbenv/", "/.local/share/mise/", "/.local/share/fnm/", "/.proto/"}

// shimHint explains a failed "--version" run: the binary runs with HOME and XDG_*
// pointing to an empty temporary directory (isolation, docs/doctor.md), which
// breaks shims that find their runtime through the real HOME. The hint says what
// to do instead; it is specific when the path looks like a version manager.
func shimHint(binary, path, resolved string) string {
	for _, d := range versionManagerDirs {
		if strings.Contains(path, d) || strings.Contains(resolved, d) {
			return fmt.Sprintf(" [이 경로는 버전 관리자(nvm/asdf/volta 등)의 shim 으로 보입니다. Forge 는 HOME 을 임시 디렉터리로 바꿔 실행하므로 shim 이 런타임을 찾지 못해 실패했을 수 있습니다. 터미널에서 `%s --version` 으로 버전을 직접 확인하고, 문제가 없으면 sync 에 --allow-untested-version 을 쓰세요]", binary)
		}
	}
	return fmt.Sprintf(" [Forge 는 HOME 을 임시 디렉터리로 바꿔 실행합니다. 실제 HOME 이 필요한 shim(nvm/asdf 류)이면 이 때문에 실패할 수 있으니 터미널에서 `%s --version` 으로 직접 확인하세요]", binary)
}

func stderrHint(b []byte) string {
	s := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	if s == "" {
		return ""
	}
	return " (stderr: " + textsafe.Display(s, 120) + ")"
}

// versionRe is deliberately strict: an optional short word prefix ("opencode ",
// "Claude Code "), an optional "v", then x.y.z followed by space, "(" or the
// end of the line. A pre-release suffix ("2.1.305-beta") is not recognized.
var versionRe = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9._-]{0,30}(?: [A-Za-z][A-Za-z0-9._-]{0,30}){0,2} )?v?(\d{1,4})\.(\d{1,4})\.(\d{1,4})(?:[ \t(]|$)`)

// ParseVersion extracts "x.y.z" from the first lines of a --version output.
// Everything else in the output is discarded.
func ParseVersion(b []byte) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n")
	for i, ln := range lines {
		if i >= 5 {
			break
		}
		ln = strings.TrimSpace(ln)
		if ln == "" || len(ln) > 200 {
			continue
		}
		if m := versionRe.FindStringSubmatch(ln); m != nil {
			return normNum(m[1]) + "." + normNum(m[2]) + "." + normNum(m[3]), true
		}
	}
	return "", false
}

// normNum drops leading zeros of one version component ("0305" -> "305", "00"
// -> "0") so "02.1.0305" is the same version as "2.1.305". The regexp allows at
// most four digits, so no overflow is possible.
func normNum(s string) string {
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0"
	}
	return s
}

// Classify compares a parsed version with the verified one: the exact version
// is ok, another patch of the same major.minor is info, anything else is warn.
func Classify(version, verified string) (Status, string, string) {
	if version == verified {
		return StatusOK, CodeVerified, "검증된 버전입니다"
	}
	if majorMinor(version) == majorMinor(verified) {
		return StatusInfo, CodeUnverifiedPat, fmt.Sprintf("미검증 패치 (%s; 검증된 버전 %s)", version, verified)
	}
	return StatusWarn, CodeUnverifiedVer, fmt.Sprintf("미검증 버전: 생성한 설정이 올바르게 로드되지 않을 수 있습니다 (%s; 검증된 버전 %s)", version, verified)
}

func majorMinor(v string) string {
	p := strings.Split(v, ".")
	if len(p) < 2 {
		return v
	}
	return p[0] + "." + p[1]
}

// quoteForHint shows a path in a message/command: POSIX single quotes when it has
// anything but plain characters, control characters made visible.
func quoteForHint(p string) string {
	plain := p != ""
	for _, r := range p {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+=:@%", r)) {
			plain = false
			break
		}
	}
	if plain {
		return p
	}
	return "'" + strings.ReplaceAll(textsafe.Display(p, 300), "'", `'\''`) + "'"
}
