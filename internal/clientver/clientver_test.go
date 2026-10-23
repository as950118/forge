package clientver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode"
)

// These tests never run a real claude or opencode: every binary here is a small
// shell script written into a temporary directory.

// TestMain gives every test a generous per-run timeout: the production 5s limit
// is a policy, not something a test about versions should depend on when the
// machine is loaded (many parallel -race packages). Tests of the timeout itself
// set their own value and wait for the script to be running (see
// TestTimeoutKillsProcessGroup).
func TestMain(m *testing.M) {
	Timeout = 60 * time.Second
	os.Exit(m.Run())
}

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
}

// fake writes an executable script and returns its directory (a PATH entry).
func fake(t *testing.T, name, body string) string {
	t.Helper()
	skipWindows(t)
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func claudeSpec() Spec { s, _ := SpecFor("claude-code"); return s }

func check(t *testing.T, dir string, projects ...string) Result {
	t.Helper()
	c := &Checker{PathEnv: dir, ProjectDirs: projects}
	return c.Check(context.Background(), claudeSpec())
}

func TestVersionClassification(t *testing.T) {
	cases := []struct {
		out    string
		status Status
		code   string
		ver    string
	}{
		{"2.1.305 (Claude Code)", StatusOK, CodeVerified, "2.1.305"},
		{"2.1.305", StatusOK, CodeVerified, "2.1.305"},
		{"v2.1.305", StatusOK, CodeVerified, "2.1.305"},
		{"claude 2.1.305", StatusOK, CodeVerified, "2.1.305"},
		{"Claude Code 2.1.305 (build x)", StatusOK, CodeVerified, "2.1.305"},
		{"banner line\n2.1.305 (Claude Code)", StatusOK, CodeVerified, "2.1.305"},
		{"2.1.999", StatusInfo, CodeUnverifiedPat, "2.1.999"},
		{"2.2.0", StatusWarn, CodeUnverifiedVer, "2.2.0"},
		{"3.1.305", StatusWarn, CodeUnverifiedVer, "3.1.305"},
		{"1.0.0", StatusWarn, CodeUnverifiedVer, "1.0.0"},
		{"hello world", StatusWarn, CodeParseFailed, ""},
		{"2.1", StatusWarn, CodeParseFailed, ""},
		{"2.1.305-beta.1", StatusWarn, CodeParseFailed, ""},
		{"version: 2.1.305; rm -rf /", StatusWarn, CodeParseFailed, ""},
		{"", StatusWarn, CodeParseFailed, ""},
	}
	for _, c := range cases {
		dir := fake(t, "claude", "printf '%s\\n' \""+strings.ReplaceAll(c.out, "\n", "\"; printf '%s\\n' \"")+"\"")
		r := check(t, dir)
		if r.Status != c.status || r.Code != c.code || r.Version != c.ver {
			t.Errorf("%q: got %s/%s/%q want %s/%s/%q (%s)", c.out, r.Status, r.Code, r.Version, c.status, c.code, c.ver, r.Message)
		}
	}
}

func TestOpenCodeVerifiedVersionForms(t *testing.T) {
	spec, _ := SpecFor("opencode")
	for _, out := range []string{"2.0.24", "v2.0.24", "opencode 2.0.24"} {
		dir := fake(t, "opencode", "echo '"+out+"'")
		r := (&Checker{PathEnv: dir}).Check(context.Background(), spec)
		if r.Status != StatusOK || r.Version != "2.0.24" {
			t.Errorf("%q: %+v", out, r)
		}
	}
}

func TestWarnMessageWording(t *testing.T) {
	_, _, msg := Classify("9.9.9", "2.1.305")
	if !strings.Contains(msg, "미검증 버전: 생성한 설정이 올바르게 로드되지 않을 수 있습니다") {
		t.Fatal(msg)
	}
	if _, _, m := Classify("2.1.9", "2.1.305"); !strings.Contains(m, "미검증 패치") {
		t.Fatal(m)
	}
}

func TestNotInstalledIsInfo(t *testing.T) {
	r := check(t, t.TempDir())
	if r.Status != StatusInfo || r.Code != CodeNotInstalled {
		t.Fatalf("%+v", r)
	}
	// A file without the exec bit and a directory called claude are not binaries.
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho 2.1.305\n"), 0o644)
	if r := check(t, dir); r.Code != CodeNotInstalled {
		t.Fatalf("non-exec file: %+v", r)
	}
	dir2 := t.TempDir()
	_ = os.Mkdir(filepath.Join(dir2, "claude"), 0o755)
	if r := check(t, dir2); r.Code != CodeNotInstalled {
		t.Fatalf("directory: %+v", r)
	}
}

// The script records its argv: it must be exactly one argument, "--version".
func TestArgvIsExactlyVersion(t *testing.T) {
	skipWindows(t)
	logf := filepath.Join(t.TempDir(), "argv.log")
	dir := fake(t, "claude", `printf 'n=%s\n' "$#" >> `+logf+`; for a in "$@"; do printf 'arg=[%s]\n' "$a" >> `+logf+`; done; echo 2.1.305`)
	for i := 0; i < 3; i++ { // never cached: runs every time
		if r := check(t, dir); r.Status != StatusOK {
			t.Fatalf("%+v", r)
		}
	}
	b, _ := os.ReadFile(logf)
	want := strings.Repeat("n=1\narg=[--version]\n", 3)
	if string(b) != want {
		t.Fatalf("argv log:\n%s\nwant:\n%s", b, want)
	}
}

func TestRunEnvironmentIsIsolated(t *testing.T) {
	skipWindows(t)
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(realHome, "cfg"))
	t.Setenv("GITHUB_TOKEN", "ghp_SECRETSECRETSECRET")
	t.Setenv("OPENAI_API_KEY", "sk-SECRETSECRETSECRET")
	t.Setenv("HTTPS_PROXY", "http://user:pw@proxy.example:3128")
	t.Setenv("FORGE_HOME", realHome)
	logf := filepath.Join(t.TempDir(), "env.log")
	dir := fake(t, "claude", `{ pwd; echo "---"; env; echo "---"; cat | wc -c; } >> `+logf+`; touch "$HOME/created-by-client"; echo 2.1.305`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if r := check(t, dir); r.Status != StatusOK {
		t.Fatalf("%+v", r)
	}
	b, _ := os.ReadFile(logf)
	s := string(b)
	for _, bad := range []string{"SECRETSECRET", "user:pw", realHome, "FORGE_HOME", "GITHUB_TOKEN", "OPENAI_API_KEY"} {
		if strings.Contains(s, bad) {
			t.Errorf("child environment leaks %q:\n%s", bad, s)
		}
	}
	lines := strings.Split(s, "\n")
	cwd := lines[0]
	if !strings.Contains(cwd, "forge-ver-") {
		t.Errorf("cwd is not the temp directory: %q", cwd)
	}
	if !strings.Contains(s, "HOME="+cwd) && !strings.Contains(s, "HOME=/private"+cwd) {
		// macOS reports /private/var..., the env holds /var/...: compare the tail
		if !strings.Contains(s, "forge-ver-") {
			t.Errorf("HOME is not the temp directory:\n%s", s)
		}
	}
	if !strings.Contains(s, "PATH=") {
		t.Errorf("PATH must stay:\n%s", s)
	}
	if !strings.Contains(s, "---\n0\n") && !strings.HasSuffix(strings.TrimSpace(s), "0") {
		t.Errorf("stdin must be empty (/dev/null):\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(realHome, "created-by-client")); err == nil {
		t.Error("the client wrote into the user's home")
	}
	if _, err := os.Stat(cwd); err == nil {
		t.Error("the temporary directory was not removed")
	}
}

func TestInfiniteOutputStopsEarly(t *testing.T) {
	skipWindows(t)
	dir := fake(t, "claude", `yes 2.1.305`)
	start := time.Now()
	r := check(t, dir)
	if r.Status != StatusWarn || r.Code != CodeRunFailed || !strings.Contains(r.Message, "넘어 중단") {
		t.Fatalf("%+v", r)
	}
	if time.Since(start) > 30*time.Second { // far below Timeout (60s): it stopped on the output cap
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	skipWindows(t)
	old := Timeout
	// Long enough that even a heavily loaded machine has started the shell and
	// written the pid file before the timeout fires; the test still waits for the
	// timeout and then checks that the whole process group is gone.
	Timeout = 4 * time.Second
	defer func() { Timeout = old }()
	pidf := filepath.Join(t.TempDir(), "pid")
	dir := fake(t, "claude", `sleep 30 & echo $! > `+pidf+`; sleep 30`)
	start := time.Now()
	r := check(t, dir)
	if r.Status != StatusWarn || r.Code != CodeRunFailed || !strings.Contains(r.Message, "끝나지 않아") {
		t.Fatalf("%+v", r)
	}
	if time.Since(start) > 40*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	assertDead(t, pidf)
}

func assertDead(t *testing.T, pidf string) {
	t.Helper()
	b, err := os.ReadFile(pidf)
	if err != nil {
		t.Fatalf("pid file: %v", err)
	}
	var pid int
	fmt.Sscan(strings.TrimSpace(string(b)), &pid)
	for i := 0; i < 40; i++ {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("a descendant process (%d) survived", pid)
}

func TestSuccessfulRunStopsLeftoverChildren(t *testing.T) {
	skipWindows(t)
	pidf := filepath.Join(t.TempDir(), "pid")
	dir := fake(t, "claude", `sleep 30 >/dev/null 2>&1 & echo $! > `+pidf+`; echo 2.1.305`)
	if r := check(t, dir); r.Status != StatusOK {
		t.Fatalf("%+v", r)
	}
	assertDead(t, pidf)
}

func TestNonZeroExitAndSanitizedStderr(t *testing.T) {
	skipWindows(t)
	dir := fake(t, "claude", `printf 'boom \033[31mred\033]0;title\007 \342\200\256rtl\n' >&2; echo 2.1.305; exit 3`)
	r := check(t, dir)
	if r.Status != StatusWarn || r.Code != CodeRunFailed || !strings.Contains(r.Message, "종료 코드 3") {
		t.Fatalf("%+v", r)
	}
	if r.Version != "" {
		t.Errorf("a failed run must not report a version: %+v", r)
	}
	noControl(t, r.Message)
}

func noControl(t *testing.T, ss ...string) {
	t.Helper()
	for _, s := range ss {
		for _, c := range s {
			if c < 0x20 || c == 0x7f || unicode.Is(unicode.Cf, c) || unicode.Is(unicode.Zl, c) {
				t.Errorf("control or format character %U in %q", c, s)
			}
		}
	}
}

func TestControlCharactersInOutputNeverReachTheResult(t *testing.T) {
	skipWindows(t)
	dir := fake(t, "claude", `printf '\033]0;evil\007\033[2J\n2.1.305 (Claude\033[31m Code)\n'`)
	r := check(t, dir)
	noControl(t, r.Message, r.Path, r.Resolved, r.Version)
	// only the parsed version is kept; the rest of the line (with its escapes) is dropped
	if r.Status != StatusOK || r.Version != "2.1.305" {
		t.Fatalf("%+v", r)
	}
	dir = fake(t, "claude", `printf '2.1.305\033[31m\n'`)
	if r := check(t, dir); r.Code != CodeParseFailed {
		t.Fatalf("an escape right after the version must not parse: %+v", r)
	}
	dir = fake(t, "claude", `printf '\033[2J\033]0;evil\007\n2.1.305\n'`)
	r = check(t, dir)
	if r.Status != StatusOK || r.Version != "2.1.305" {
		t.Fatalf("%+v", r)
	}
	noControl(t, r.Message, r.Version)
}

func TestRelativePathEntriesAreIgnored(t *testing.T) {
	skipWindows(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.WriteFile(filepath.Join(cwd, "claude"), []byte("#!/bin/sh\necho 2.1.305\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(cwd, "rel"), 0o755)
	_ = os.WriteFile(filepath.Join(cwd, "rel", "claude"), []byte("#!/bin/sh\necho 2.1.305\n"), 0o755)
	for _, p := range []string{".", "", ":", ".:", "rel", "./rel", "::rel:.:..", ".." + string(os.PathListSeparator) + "rel"} {
		// PathEnv is used as given (OSPath is false): never the real PATH
		r := (&Checker{PathEnv: p}).Check(context.Background(), claudeSpec())
		if r.Code != CodeNotInstalled {
			t.Errorf("PATH=%q: %+v", p, r)
		}
	}
	// an absolute entry later in the list is still used
	abs := fake(t, "claude", "echo 2.1.305")
	r := (&Checker{PathEnv: ".:rel::" + abs}).Check(context.Background(), claudeSpec())
	if r.Status != StatusOK || filepath.Dir(r.Path) != abs {
		t.Fatalf("%+v", r)
	}
}

func TestBinaryInsideProjectIsNeverRun(t *testing.T) {
	skipWindows(t)
	proj := t.TempDir()
	proj, _ = filepath.EvalSymlinks(proj)
	marker := filepath.Join(t.TempDir(), "ran")
	bin := filepath.Join(proj, "node_modules", ".bin")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\ntouch "+marker+"\necho 2.1.305\n"), 0o755)
	r := check(t, bin, proj)
	if r.Code != CodeBlocked || r.Status != StatusWarn {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the project's binary was executed")
	}
	// a trusted binary later in PATH is used instead
	good := fake(t, "claude", "echo 2.1.305")
	r = check(t, bin+string(os.PathListSeparator)+good, proj)
	if r.Status != StatusOK || filepath.Dir(r.Path) != good {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the project's binary was executed")
	}
}

func TestSymlinkIsResolvedAndShown(t *testing.T) {
	skipWindows(t)
	target := fake(t, "claude-real", "echo 2.1.305")
	linkDir := t.TempDir()
	linkDir, _ = filepath.EvalSymlinks(linkDir)
	if err := os.Symlink(filepath.Join(target, "claude-real"), filepath.Join(linkDir, "claude")); err != nil {
		t.Fatal(err)
	}
	r := check(t, linkDir)
	if r.Status != StatusOK {
		t.Fatalf("%+v", r)
	}
	if r.Path != filepath.Join(linkDir, "claude") || r.Resolved != filepath.Join(target, "claude-real") {
		t.Fatalf("path %q resolved %q", r.Path, r.Resolved)
	}
	// a link that points INTO the project is blocked even though the link is outside
	proj, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.WriteFile(filepath.Join(proj, "claude"), []byte("#!/bin/sh\necho 2.1.305\n"), 0o755)
	l2, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.Symlink(filepath.Join(proj, "claude"), filepath.Join(l2, "claude"))
	if r := check(t, l2, proj); r.Code != CodeBlocked {
		t.Fatalf("link into project: %+v", r)
	}
	// a dangling link is not installed
	l3, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.Symlink(filepath.Join(l3, "nowhere"), filepath.Join(l3, "claude"))
	if r := check(t, l3); r.Code != CodeNotInstalled {
		t.Fatalf("dangling: %+v", r)
	}
}

func TestRunnerReceivesResolvedAbsolutePath(t *testing.T) {
	skipWindows(t)
	dir := fake(t, "claude", "echo 2.1.305")
	fr := &fakeRunner{out: RunResult{Stdout: []byte("2.1.305\n")}}
	c := &Checker{PathEnv: dir, Runner: fr}
	if r := c.Check(context.Background(), claudeSpec()); r.Status != StatusOK {
		t.Fatalf("%+v", r)
	}
	if len(fr.paths) != 1 || !filepath.IsAbs(fr.paths[0]) {
		t.Fatalf("%v", fr.paths)
	}
	if _, err := (Exec{}).RunVersion(context.Background(), "claude"); err == nil {
		t.Fatal("a relative path must be refused")
	}
}

type fakeRunner struct {
	out   RunResult
	err   error
	paths []string
}

func (f *fakeRunner) RunVersion(_ context.Context, p string) (RunResult, error) {
	f.paths = append(f.paths, p)
	return f.out, f.err
}

func TestRunnerErrorIsAWarning(t *testing.T) {
	dir := fake(t, "claude", "echo 2.1.305")
	c := &Checker{PathEnv: dir, Runner: &fakeRunner{err: fmt.Errorf("exec format error \x1b[31m")}}
	r := c.Check(context.Background(), claudeSpec())
	if r.Status != StatusWarn || r.Code != CodeRunFailed {
		t.Fatalf("%+v", r)
	}
	noControl(t, r.Message)
}

func TestSpecsMatchDocs(t *testing.T) {
	for _, tc := range []struct{ client, doc, want string }{
		{"claude-code", "../../docs/clients/claude-code.md", "Claude Code 2.1.305"},
		{"opencode", "../../docs/clients/opencode.md", "OpenCode v2.0.24"},
	} {
		b, err := os.ReadFile(tc.doc)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "**"+tc.want+"**") {
			t.Errorf("%s: docs no longer say %s", tc.doc, tc.want)
		}
		s, _ := SpecFor(tc.client)
		if !strings.HasSuffix(tc.want, s.Verified) {
			t.Errorf("%s: spec %s vs docs %s", tc.client, s.Verified, tc.want)
		}
	}
}
