package mcp

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/as950118/forge/internal/textsafe"
)

// Registration preview (TASK-0013): what `forge mcp add` is about to register,
// shown before anything is stored. Registering does not run the server, but the
// next `forge mcp refresh` (or a client using the server) does, so the exact
// command line is the thing a person must read. Every dynamic string has gone
// through textsafe once at construction; env and header VALUES are never part of
// a Preview (references only; literals are shown as "literal, value hidden").
//
// The findings are heuristics over the command line: they point at common risks
// (unpinned package runners, plaintext http, private addresses) and are not a
// verdict on the server's behaviour.

// Finding is one preview remark. Level is "warn" or "note".
type Finding struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// PreviewValue is one env/header entry shown by reference only.
type PreviewValue struct {
	Slot string `json:"slot"`
	Key  string `json:"key"`
	// Ref is "${NAME}" / "keychain:..." or "literal (값 비표시)".
	Ref string `json:"ref"`
}

// Preview is the stable JSON schema (schema 1) of the registration preview.
type Preview struct {
	Schema    int    `json:"schema"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
	// CommandLine is the exact command that would run, POSIX shell quoted
	// (stdio only). Control characters are shown as visible escapes.
	CommandLine string `json:"command_line,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
	Runner      string `json:"runner,omitempty"`  // npx, uvx, docker run, ...
	Package     string `json:"package,omitempty"` // package/image spec the runner will fetch
	// Pin: pinned | range | tag | unpinned | local (package runners only).
	Pin       string         `json:"pin,omitempty"`
	URL       string         `json:"url,omitempty"` // without query values
	Scheme    string         `json:"scheme,omitempty"`
	Host      string         `json:"host,omitempty"`
	Values    []PreviewValue `json:"values"`
	ChildEnv  string         `json:"child_env,omitempty"`
	Findings  []Finding      `json:"findings"`
	TimeoutMS int            `json:"timeout_ms,omitempty"`
}

// Warnings returns the texts of the "warn" findings.
func (p *Preview) Warnings() []string {
	var out []string
	for _, f := range p.Findings {
		if f.Level == "warn" {
			out = append(out, f.Text)
		}
	}
	return out
}

func safeText(s string, n int) string { return textsafe.Display(s, n) }

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// ShellQuote quotes one word for a POSIX shell; words with control characters
// are shown with visible escapes (for display, not to be pasted).
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if textsafe.NeedsEscape(r) {
			return "'" + strings.ReplaceAll(textsafe.Display(s, 300), "'", `'\''`) + "'"
		}
	}
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func quoteLine(cmd string, args []string) string {
	parts := []string{ShellQuote(cmd)}
	for _, a := range args {
		parts = append(parts, ShellQuote(a))
	}
	return strings.Join(parts, " ")
}

// BuildPreview computes the preview of a validated Spec.
func BuildPreview(s Spec) *Preview {
	p := &Preview{Schema: 1, Name: safeText(s.Name, 80), Transport: s.Transport, Values: []PreviewValue{}, Findings: []Finding{}, TimeoutMS: s.TimeoutMS}
	warn := func(f string, a ...any) {
		p.Findings = append(p.Findings, Finding{"warn", safeText(fmt.Sprintf(f, a...), 400)})
	}
	note := func(f string, a ...any) {
		p.Findings = append(p.Findings, Finding{"note", safeText(fmt.Sprintf(f, a...), 400)})
	}

	for _, v := range s.Values {
		ref := v.Display
		if v.RefType == RefLiteral {
			ref = "literal (값 비표시)"
		} else if v.RefType == RefKeychain {
			ref = "keychain:" + v.Ref
		} else {
			ref = "${" + v.Ref + "}"
		}
		p.Values = append(p.Values, PreviewValue{Slot: v.Slot, Key: safeText(v.Key, 80), Ref: safeText(ref, 120)})
	}

	if s.Transport == TransportStdio {
		p.CommandLine = quoteLine(s.Command, s.Args)
		if s.Cwd != "" {
			p.Cwd = safeText(s.Cwd, 200)
		}
		analyzeStdio(p, s, warn, note)
		var keys []string
		for _, v := range s.Values {
			if v.Slot == SlotEnv {
				keys = append(keys, v.Key)
			}
		}
		sort.Strings(keys)
		extra := "선언한 환경변수 없음"
		if len(keys) > 0 {
			extra = "선언한 환경변수: " + strings.Join(keys, ", ")
		}
		p.ChildEnv = safeText("자식 프로세스 환경 = 최소 환경(PATH·HOME·LANG·프록시·CA 경로 등) + 선언한 --env 변수만. 부모 셸의 다른 환경변수(API 키 등)는 전달되지 않음. "+extra, 600)
		note("등록만으로는 서버가 실행되지 않습니다. `forge mcp refresh` 를 실행하면 위 명령이 실제로 실행되어 도구 목록을 가져옵니다.")
	} else {
		analyzeRemote(p, s, warn, note)
	}
	return p
}

var (
	semverExact = regexp.MustCompile(`^v?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.+-]+)?$`)
	movingTags  = map[string]bool{"latest": true, "next": true, "beta": true, "alpha": true, "canary": true, "nightly": true, "dev": true, "stable": true, "main": true, "master": true, "edge": true, "rc": true, "*": true, "x": true, "": true}
)

func baseName(cmd string) string {
	b := strings.ToLower(filepath.Base(cmd))
	for _, suf := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		b = strings.TrimSuffix(b, suf)
	}
	return b
}

// firstOperand returns the index of the first argument that is not a flag (or a
// flag's value) and whether one was found.
func firstOperand(args []string, valueFlags map[string]bool) (int, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return i + 1, i+1 < len(args)
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			if valueFlags[a] && !strings.Contains(a, "=") {
				i++
			}
			continue
		}
		return i, true
	}
	return 0, false
}

// isShellScript reports whether the command is a shell given a command string
// (sh -c '...'): the shell, not Forge or the client, interprets the argument.
func isShellScript(cmd string, args []string) bool {
	switch baseName(cmd) {
	case "sh", "bash", "zsh", "dash", "ksh", "fish", "cmd", "powershell", "pwsh":
		return hasFlag(args, "-c", "/c", "/C", "-Command", "-command", "-lc") || len(args) > 0 && args[0] == "-c"
	}
	return false
}

func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
	}
	return false
}

func flagValues(args []string, flags ...string) []string {
	var out []string
	for i, a := range args {
		for _, f := range flags {
			if a == f && i+1 < len(args) {
				out = append(out, args[i+1])
			}
			if strings.HasPrefix(a, f+"=") {
				out = append(out, strings.TrimPrefix(a, f+"="))
			}
		}
	}
	return out
}

// npmPin classifies an npm package spec.
func npmPin(spec string) string {
	s := strings.TrimPrefix(spec, "npm:")
	switch {
	case strings.HasPrefix(s, ".") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "file:"):
		return "local"
	case strings.Contains(s, "://") || strings.HasPrefix(s, "git+") || strings.HasPrefix(s, "github:"):
		return "unpinned" // URL/git specs: pinned only by a commit, which we cannot judge here
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 { // no version ("name" or "@scope/name")
		return "unpinned"
	}
	ver := s[at+1:]
	switch {
	case semverExact.MatchString(ver):
		return "pinned"
	case movingTags[strings.ToLower(ver)]:
		return "unpinned"
	case strings.ContainsAny(ver, "^~<>|*xX "):
		return "range"
	default:
		return "tag"
	}
}

// pyPin classifies a pip/uv requirement spec ("pkg==1.2.3", "pkg@1.2.3").
func pyPin(spec string) string {
	switch {
	case strings.Contains(spec, "=="):
		if v := spec[strings.Index(spec, "==")+2:]; semverExact.MatchString(strings.TrimSpace(v)) || regexp.MustCompile(`^\d[\w.]*$`).MatchString(strings.TrimSpace(v)) {
			return "pinned"
		}
		return "range"
	case strings.Contains(spec, "@") && !strings.Contains(spec, "://"):
		v := spec[strings.LastIndexByte(spec, '@')+1:]
		if semverExact.MatchString(v) || regexp.MustCompile(`^\d[\w.]*$`).MatchString(v) {
			return "pinned"
		}
		return "unpinned"
	case strings.ContainsAny(spec, "<>~!"):
		return "range"
	case strings.Contains(spec, "://"):
		return "unpinned"
	}
	return "unpinned"
}

func dockerPin(image string) string {
	if strings.Contains(image, "@sha256:") {
		return "pinned"
	}
	rest := image[strings.LastIndexByte(image, '/')+1:]
	i := strings.IndexByte(rest, ':')
	if i < 0 {
		return "unpinned"
	}
	if movingTags[strings.ToLower(rest[i+1:])] {
		return "unpinned"
	}
	return "tag"
}

// pinTarget strips a trailing version/tag/range from a package or image spec so
// that the advice can show "<name>@<버전>".
func pinTarget(runner, pkg string) string {
	if strings.HasPrefix(runner, "docker") || strings.HasPrefix(runner, "podman") {
		if i := strings.Index(pkg, "@sha256:"); i >= 0 {
			pkg = pkg[:i]
		}
		if i := strings.LastIndexByte(pkg, ':'); i > strings.LastIndexByte(pkg, '/') {
			pkg = pkg[:i]
		}
		return pkg
	}
	if pkg == "" || strings.Contains(pkg, "://") {
		return "<패키지>"
	}
	for _, scheme := range []string{"npm:", "jsr:"} { // deno specifiers: keep the scheme
		if strings.HasPrefix(pkg, scheme) {
			return scheme + pinTarget(runner, strings.TrimPrefix(pkg, scheme))
		}
	}
	for _, sep := range []string{"==", "@", ">", "<", "~", "!"} {
		if i := strings.Index(pkg[1:], sep); i >= 0 { // [1:]: skip the "@" of a scope
			return pkg[:i+1]
		}
	}
	return pkg
}

// pinAdvice is the per-runner how-to of the unpinned-package warning. Syntax
// that rests on the runner's documentation rather than a check against an
// installed binary is marked "미확인" where it is version dependent.
func pinAdvice(runner, pkg string) string {
	n := safeText(pinTarget(runner, pkg), 100)
	switch {
	case strings.HasPrefix(runner, "docker"), strings.HasPrefix(runner, "podman"):
		return fmt.Sprintf("이미지를 정확한 태그(%s:<버전>) 또는 다이제스트(%s@sha256:<64자 hex>)로 쓰세요. 태그는 다시 가리켜질 수 있어 다이제스트가 더 확실합니다. 다이제스트는 이미지를 받은 뒤 `%s inspect --format '{{index .RepoDigests 0}}' <이미지>` 로 확인합니다.", n, n, strings.Fields(runner)[0])
	case runner == "uvx", runner == "uv tool run":
		return fmt.Sprintf("`%s==<버전>` 으로 고정하세요 (예: forge mcp add NAME -- uvx %s==1.2.3). `%s@<버전>` 형태는 uv 버전에 따라 다를 수 있어 미확인입니다. 사용 가능한 버전은 PyPI 에서 확인합니다.", n, n, n)
	case runner == "pipx run":
		return fmt.Sprintf("`pipx run --spec '%s==<버전>' <실행 파일>` 또는 `pipx run '%s==<버전>'` 로 고정하세요 (후자는 pipx 버전에 따라 미확인). 사용 가능한 버전은 PyPI 에서 확인합니다.", n, n)
	case runner == "deno run":
		switch {
		case strings.HasPrefix(n, "npm:"), strings.HasPrefix(n, "jsr:"):
			return fmt.Sprintf("`%s@<버전>` 처럼 specifier 에 정확한 버전을 넣으세요.", n)
		}
		return "URL 경로에 정확한 버전을 포함하세요 (예: .../pkg@1.2.3/mod.ts)."
	default: // npx, bunx, pnpm dlx, yarn dlx, bun x, npm exec
		return fmt.Sprintf("`%s@<정확한 버전>` 으로 고정하세요 (예: %s %s@1.2.3). 최신 버전은 `npm view %s version` 으로 확인합니다. @latest·^범위·태그는 고정이 아닙니다.", n, runner, n, n)
	}
}

func analyzeStdio(p *Preview, s Spec, warn, note func(string, ...any)) {
	cmd, args := s.Command, s.Args
	b := baseName(cmd)
	switch {
	case filepath.IsAbs(cmd):
		note("절대 경로 실행 파일: %s", safeText(cmd, 160))
	case !strings.ContainsAny(cmd, `/\`):
		note("실행 파일 %q 은 PATH 에서 찾습니다 (PATH 가 바뀌면 다른 프로그램이 실행될 수 있음)", safeText(cmd, 80))
	}
	judge := func(runner, pkg, pin string) {
		p.Runner, p.Package, p.Pin = runner, safeText(pkg, 200), pin
		switch pin {
		case "unpinned":
			warn("%s 패키지 %q 의 버전이 고정되지 않았습니다 (@latest·태그 없음·URL): 실행할 때마다 다른 코드를 내려받을 수 있습니다. 정확한 버전(예: name@1.2.3)으로 고정하세요.", runner, safeText(pkg, 120))
		case "range":
			warn("%s 패키지 %q 는 버전 범위입니다 (고정 아님): 정확한 버전으로 고정하세요.", runner, safeText(pkg, 120))
		case "tag":
			warn("%s 패키지 %q 는 이동 가능한 태그입니다 (고정 아님): 정확한 버전/다이제스트로 고정하세요.", runner, safeText(pkg, 120))
		case "local":
			note("%s 가 로컬 경로 %q 를 실행합니다 (파일 내용은 검사하지 않음)", runner, safeText(pkg, 120))
		case "pinned":
			note("%s 패키지 %q 는 버전이 고정되어 있습니다 (패키지 내용 자체는 검사하지 않음)", runner, safeText(pkg, 120))
		}
		if pin == "unpinned" || pin == "range" || pin == "tag" {
			note("고정 방법: %s", pinAdvice(runner, pkg))
		}
	}
	npmLike := func(runner string, rest []string, valueFlags map[string]bool) {
		pkgs := flagValues(rest, "-p", "--package")
		if len(pkgs) == 0 {
			if i, ok := firstOperand(rest, valueFlags); ok {
				pkgs = []string{rest[i]}
			}
		}
		if hasFlag(rest, "-y", "--yes") {
			warn("%s -y/--yes: 패키지 설치 확인 없이 내려받아 실행합니다 (설치 전 확인 단계를 건너뜀).", runner)
		}
		if hasFlag(rest, "-c", "--call") {
			warn("%s -c: 셸 명령 문자열을 실행합니다.", runner)
		}
		if len(pkgs) == 0 {
			note("%s: 패키지 이름을 찾지 못해 버전 고정 여부를 판단하지 못했습니다", runner)
			p.Runner = runner
			return
		}
		rank := map[string]int{"pinned": 0, "local": 0, "tag": 1, "range": 2, "unpinned": 3}
		worst := pkgs[0]
		for _, pk := range pkgs {
			if rank[npmPin(pk)] > rank[npmPin(worst)] {
				worst = pk
			}
		}
		judge(runner, worst, npmPin(worst))
	}
	npmFlags := map[string]bool{"-p": true, "--package": true, "--registry": true, "--cache": true, "--prefix": true, "-w": true, "--workspace": true, "--userconfig": true, "--call": false}
	switch b {
	case "npx", "bunx":
		npmLike(b, args, npmFlags)
	case "pnpm", "yarn", "npm", "bun":
		if len(args) > 0 && (args[0] == "dlx" || args[0] == "exec" || args[0] == "x") {
			npmLike(b+" "+args[0], args[1:], npmFlags)
		}
	case "uvx", "uv":
		rest := args
		runner := "uvx"
		if b == "uv" {
			if len(args) >= 2 && args[0] == "tool" && args[1] == "run" {
				rest, runner = args[2:], "uv tool run"
			} else {
				break
			}
		}
		spec := ""
		if v := flagValues(rest, "--from"); len(v) > 0 {
			spec = v[0]
		} else if i, ok := firstOperand(rest, map[string]bool{"--from": true, "--with": true, "--python": true, "-p": true, "--index-url": true, "--default-index": true, "--index": true}); ok {
			spec = rest[i]
		}
		if spec == "" {
			note("%s: 패키지 이름을 찾지 못했습니다", runner)
			p.Runner = runner
			break
		}
		judge(runner, spec, pyPin(spec))
	case "pipx":
		if len(args) > 0 && args[0] == "run" {
			spec := ""
			if v := flagValues(args[1:], "--spec"); len(v) > 0 {
				spec = v[0]
			} else if i, ok := firstOperand(args[1:], map[string]bool{"--spec": true, "--python": true, "--index-url": true}); ok {
				spec = args[1:][i]
			}
			if spec != "" {
				judge("pipx run", spec, pyPin(spec))
			}
		}
	case "deno":
		if len(args) > 0 && args[0] == "run" {
			rest := args[1:]
			if hasFlag(rest, "-A", "--allow-all") {
				warn("deno run -A/--allow-all: 모든 권한(파일·네트워크·실행)을 줍니다.")
			}
			if hasFlag(rest, "--allow-run") {
				warn("deno run --allow-run: 하위 프로세스 실행 권한을 줍니다.")
			}
			if i, ok := firstOperand(rest, map[string]bool{"--config": true, "-c": true, "--import-map": true}); ok {
				spec := rest[i]
				switch {
				case strings.HasPrefix(spec, "npm:"):
					judge("deno run", spec, npmPin(strings.TrimPrefix(spec, "npm:")))
				case strings.HasPrefix(spec, "jsr:") || strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://"):
					pin := "unpinned"
					if regexp.MustCompile(`@v?\d+\.\d+\.\d+`).MatchString(spec) {
						pin = "pinned"
					}
					judge("deno run", spec, pin)
				default:
					judge("deno run", spec, "local")
				}
			}
		}
	case "docker", "podman":
		if len(args) > 0 && args[0] == "run" {
			rest := args[1:]
			vf := map[string]bool{"-e": true, "--env": true, "-v": true, "--volume": true, "-p": true, "--publish": true, "--name": true, "-w": true, "--workdir": true,
				"--entrypoint": true, "--network": true, "--net": true, "-u": true, "--user": true, "--mount": true, "--label": true, "-l": true, "--env-file": true,
				"--platform": true, "-m": true, "--memory": true, "--cpus": true, "--add-host": true, "--hostname": true, "-h": true, "--restart": true,
				"--cap-add": true, "--cap-drop": true, "--device": true, "--pull": true, "--runtime": true, "--security-opt": true, "--pid": true}
			if i, ok := firstOperand(rest, vf); ok {
				judge(b+" run", rest[i], dockerPin(rest[i]))
			}
			if hasFlag(rest, "--privileged") {
				warn("%s run --privileged: 컨테이너에 호스트 거의 전체 권한을 줍니다.", b)
			}
			if hasFlag(rest, "--network", "--net") {
				for _, v := range flagValues(rest, "--network", "--net") {
					if v == "host" {
						warn("%s run --network host: 컨테이너가 호스트 네트워크를 그대로 씁니다.", b)
					}
				}
			}
			if hasFlag(rest, "--pid") {
				warn("%s run --pid: 호스트 프로세스 네임스페이스를 공유할 수 있습니다.", b)
			}
			for _, v := range flagValues(rest, "-v", "--volume") {
				src := strings.SplitN(v, ":", 2)[0]
				if src == "/" || src == "~" || strings.HasPrefix(src, "/var/run/docker.sock") || strings.HasPrefix(src, "/etc") {
					warn("%s run -v %s: 호스트의 민감한 경로를 컨테이너에 마운트합니다.", b, safeText(v, 80))
				} else if strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") || strings.HasPrefix(src, "~") {
					note("%s run -v %s: 호스트 경로를 마운트합니다.", b, safeText(v, 80))
				}
			}
		}
	case "sh", "bash", "zsh", "dash", "ksh", "fish", "cmd", "powershell", "pwsh":
		if isShellScript(cmd, args) {
			warn("셸(%s)이 명령 문자열을 실행합니다: 파이프·치환 등 임의의 셸 구문이 가능하므로 문자열 전체를 읽어 보세요.", b)
		}
	}
	if i := ArgRefIndex(cmd, args); i >= 0 {
		warn("인자 #%d 에 ${...} 참조가 있습니다: 게이트웨이·refresh 는 확장하지 않아 서버가 글자 그대로 받고, Claude Code 직접 노출만 확장하며 OpenCode 직접 노출은 sync 에서 거부됩니다. --env KEY='${ENV_NAME}' 또는 래퍼 스크립트를 쓰세요.", i+1)
	}
	for _, a := range args {
		if regexp.MustCompile(`\|\s*(?:ba|z)?sh\b|curl\s|wget\s`).MatchString(a) {
			warn("인자에 curl/wget 또는 셸 파이프가 포함되어 있습니다: %q", safeText(a, 120))
			break
		}
	}
}

func analyzeRemote(p *Preview, s Spec, warn, note func(string, ...any)) {
	u, err := url.Parse(s.URL)
	if err != nil {
		return
	}
	p.Scheme = safeText(u.Scheme, 10)
	p.Host = safeText(u.Host, 200)
	clean := *u
	if clean.RawQuery != "" {
		clean.RawQuery = "***"
	}
	p.URL = safeText(clean.String(), 300)
	if q := len(u.Query()); q > 0 {
		note("URL 에 쿼리 매개변수 %d개가 있습니다 (값은 표시하지 않음)", q)
	}
	hasAuth := false
	for _, v := range s.Values {
		if v.Slot == SlotHeader && (LooksSecret(v.Key) || v.IsSecret) {
			hasAuth = true
		}
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	loopback := host == "localhost" || strings.HasSuffix(host, ".localhost") || (ip != nil && ip.IsLoopback())
	if u.Scheme == "http" {
		switch {
		case loopback:
			note("평문 http 이지만 로컬 호스트(%s) 입니다", safeText(host, 80))
		case hasAuth:
			warn("평문 http 입니다: 인증 헤더와 도구 응답이 암호화되지 않고 네트워크에 노출됩니다 (https 를 사용하세요).")
		default:
			warn("평문 http 입니다: 도구 응답이 암호화·검증되지 않아 중간자가 바꿀 수 있습니다 (https 를 사용하세요).")
		}
	}
	switch {
	case ip != nil && !loopback && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()):
		warn("호스트 %s 는 사설/링크 로컬 주소입니다 (클라우드 메타데이터 주소 169.254.169.254 포함 가능): 내부망 서비스에 연결됩니다.", safeText(host, 80))
	case ip == nil && !loopback && (strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") || strings.HasSuffix(host, ".home.arpa") || !strings.Contains(host, ".")):
		note("호스트 %s 는 내부망 이름으로 보입니다 (DNS 해석은 하지 않으므로 실제 주소는 확인하지 못함)", safeText(host, 80))
	case ip == nil && !loopback:
		note("호스트 %s 의 DNS 해석 결과(사설 주소 여부)는 등록 시점에 확인하지 않습니다", safeText(host, 80))
	}
	if s.Transport == TransportSSE {
		note("sse 전송은 서버가 보낸 이벤트 스트림을 그대로 받습니다")
	}
}
