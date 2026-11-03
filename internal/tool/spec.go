// Package tool implements the custom tool registry (command / script / http
// kinds) and their sandboxed executors (TASK-0006, ADR-F0001).
//
// Security model in short: command tools are argv templates run without a shell,
// script tools run from a 0600 temp copy, http tools go through an SSRF-safe
// client; every kind runs with a minimal environment, a timeout, output caps and
// process-group cleanup. Secrets are only ever stored as ${ENV} references.
package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/as950118/forge/internal/mcp"
)

// ScopeGlobal is the only scope implemented so far.
// TODO(project-scope): `--project` (tools.scope='project', projects row) is
// deferred together with skill/MCP project scope (ADR-F0001 D2, O-2).
const ScopeGlobal = "global"

// Kinds (schema tools.kind).
const (
	KindCommand = "command"
	KindScript  = "script"
	KindHTTP    = "http"
)

// Confirm modes (schema tools.confirm). `deny` makes Run refuse; `ask` is
// enforced by callers that talk to a human/agent (gateway, TASK-0007).
const (
	ConfirmNone = "none"
	ConfirmAsk  = "ask"
	ConfirmDeny = "deny"
)

// Policy limits.
const (
	DefaultTimeout      = 30 * time.Second
	MaxTimeout          = 10 * time.Minute
	DefaultMaxOutput    = 64 << 10
	MaxOutputCap        = 1 << 20
	DefaultMaxRedirects = 3
	MaxRedirectsCap     = 5
	MaxScriptBytes      = 1 << 20
	maxArgsBytes        = 256 << 10
)

// Sentinel errors.
var (
	ErrNotFound = errors.New("등록된 도구가 없습니다")
	ErrExists   = errors.New("이미 등록된 도구입니다")
)

var (
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`) // == schema CHECK on tools.name
	varRe    = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	identRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	shellSet = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true}
	// interpreters allowed for kind=script (names only; resolved through PATH).
	interpreterSet = map[string]bool{"python3": true, "python": true, "node": true, "bash": true, "sh": true, "ruby": true, "perl": true}
)

// Policy is the execution policy stored with a tool definition. Defaults are on
// the safe side (shell off, https only, no private hosts, no leading dashes).
type Policy struct {
	TimeoutMS int    `json:"timeout_ms"`
	Confirm   string `json:"confirm"`
	// WorkingDir is the cwd root (absolute). Empty = the current project
	// directory at run time. A run can only use this directory or one below it.
	WorkingDir string `json:"working_dir,omitempty"`
	// Allow: command = executable names / absolute paths; script = extra
	// interpreter names; http = host patterns (example.com, *.example.com).
	Allow []string `json:"allow,omitempty"`
	// AllowLeadingDash lets a substituted value start with '-' (option injection risk).
	AllowLeadingDash bool `json:"allow_leading_dash,omitempty"`
	// AllowVarSubcommand lets a {{variable}} appear in argv[1] (the program /
	// subcommand slot, e.g. `git {{sub}}`). Default off: variables belong in
	// data positions (argv[2:]); `cat -- {{file}}` is the portable spelling.
	AllowVarSubcommand bool `json:"allow_var_subcommand,omitempty"`
	// AllowPrivate permits loopback/private/CGNAT targets (http). Link-local and
	// cloud metadata addresses are always blocked.
	AllowPrivate bool `json:"allow_private,omitempty"`
	// AllowHTTP permits plain http:// (default https only).
	AllowHTTP bool `json:"allow_http,omitempty"`
	// AckCodeExec records that the registrant explicitly accepted running a
	// code-exec tool (Definition.Risk) with confirm=none, i.e. without the
	// gateway's forced confirm=ask (TASK-0018). Stored in the settings ext JSON.
	AckCodeExec    bool `json:"ack_code_exec,omitempty"`
	MaxOutputBytes int  `json:"max_output_bytes"`
	MaxRedirects   int  `json:"max_redirects"`
}

// Definition is a custom tool. Values carry references only, never secrets.
//
// Not representable in the v1 schema columns (shell flag, body template,
// allowlist and the extra policy flags) are kept as JSON in settings
// ("tool.ext.<name>"). TODO(schema-0002): promote to columns/tables.
type Definition struct {
	Name        string          `json:"name"`
	Scope       string          `json:"scope"`
	Kind        string          `json:"kind"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`

	// command: Command is argv[0], Args the remaining argv templates.
	// With Shell, Command=="sh" and Args==["-c", <script using $FORGE_ARG_x>].
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Shell   bool     `json:"shell,omitempty"`

	// script: ScriptBody is only set on input / by Registry.Resolve.
	Interpreter string `json:"interpreter,omitempty"`
	ScriptPath  string `json:"script_path,omitempty"`
	ScriptHash  string `json:"script_hash,omitempty"`
	ScriptBody  string `json:"-"`

	// http
	Method       string `json:"method,omitempty"`
	URL          string `json:"url,omitempty"`
	BodyTemplate string `json:"body_template,omitempty"`

	Policy Policy      `json:"policy"`
	Values []mcp.Value `json:"values,omitempty"`

	// Risk is derived, never stored: "code-exec" for a command tool whose
	// program interprets data as code (risk.go), otherwise empty.
	Risk string `json:"risk,omitempty"`

	Enabled   bool  `json:"enabled"`
	Revision  int   `json:"revision"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// Timeout returns the effective default timeout.
func (d *Definition) Timeout() time.Duration {
	if d.Policy.TimeoutMS > 0 {
		return time.Duration(d.Policy.TimeoutMS) * time.Millisecond
	}
	return DefaultTimeout
}

// schemaInfo is the part of the input schema the template checks need.
type schemaInfo struct {
	Type       any `json:"type"`
	Properties map[string]struct {
		Type any `json:"type"`
	} `json:"properties"`
}

func scalarType(t any) bool {
	ok := func(s string) bool { return s == "string" || s == "integer" || s == "number" || s == "boolean" }
	switch v := t.(type) {
	case string:
		return ok(v)
	case []any:
		if len(v) == 0 {
			return false
		}
		for _, x := range v {
			s, isStr := x.(string)
			if !isStr || !ok(s) {
				return false
			}
		}
		return true
	}
	return false
}

// Validate normalizes (defaults, schema) and checks a definition. It is
// idempotent; Registry.Add calls it so callers cannot skip validation.
func (d *Definition) Validate() error {
	if !nameRe.MatchString(d.Name) {
		return fmt.Errorf("도구 이름이 올바르지 않습니다: %q (영문/숫자로 시작, 영문/숫자/_/- 만, 최대 64자)", d.Name)
	}
	d.Scope = ScopeGlobal
	d.Description = strings.TrimSpace(d.Description)
	if d.Description == "" {
		return errors.New("도구 설명(--description)이 필요합니다")
	}
	if len(d.Description) > 2000 {
		return errors.New("도구 설명이 너무 깁니다 (최대 2000바이트)")
	}
	norm, info, err := normalizeSchema(d.InputSchema)
	if err != nil {
		return err
	}
	d.InputSchema = norm
	if err := d.validatePolicy(); err != nil {
		return err
	}
	switch d.Kind {
	case KindCommand:
		err = d.validateCommand(info)
	case KindScript:
		err = d.validateScript(info)
	case KindHTTP:
		err = d.validateHTTP(info)
	default:
		return fmt.Errorf("kind 는 command|script|http 중 하나여야 합니다: %q", d.Kind)
	}
	if err != nil {
		return err
	}
	d.refreshRisk()
	return d.validateValues()
}

// ValidateNew is Validate plus the registration-only checks (never run on
// stored definitions): a ${ENV}-style reference in a command tool's argv is not
// expanded by Forge (no shell), so the program would receive the literal text.
func (d *Definition) ValidateNew() error {
	if err := d.Validate(); err != nil {
		return err
	}
	if err := d.validateCodeExecAck(); err != nil {
		return err
	}
	if d.Kind == KindCommand && !d.Shell {
		if i := mcp.ArgRefIndex(d.Command, d.Args); i >= 0 {
			return fmt.Errorf("argv #%d 에 ${ENV} 형태의 참조가 있습니다. 도구는 셸 없이 실행되어 Forge 가 ${...} 를 확장하지 않으므로 프로그램이 글자 그대로 받습니다. %s", i+2, mcp.ArgRefAdvice)
		}
	}
	return nil
}

// ValidateExec re-checks the execution-relevant policy of command/script
// definitions (shell ban, deny list, allowlist consistency, interpreter rules)
// on a copy-safe subset: Run calls it on every run so a tampered DB row or a
// hand-built Definition cannot skip what Add enforces. http tools keep their
// own run-time enforcement (scheme, host allowlist, SSRF dialer).
func (d *Definition) ValidateExec() error {
	if d.Kind == KindHTTP {
		return nil
	}
	_, info, err := normalizeSchema(d.InputSchema)
	if err != nil {
		return err
	}
	if err := d.validatePolicy(); err != nil {
		return err
	}
	switch d.Kind {
	case KindCommand:
		return d.validateCommand(info)
	case KindScript:
		return d.validateScript(info)
	}
	return fmt.Errorf("kind 는 command|script|http 중 하나여야 합니다: %q", d.Kind)
}

func (d *Definition) validatePolicy() error {
	p := &d.Policy
	if p.TimeoutMS == 0 {
		p.TimeoutMS = int(DefaultTimeout / time.Millisecond)
	}
	if p.TimeoutMS < 0 || time.Duration(p.TimeoutMS)*time.Millisecond > MaxTimeout {
		return fmt.Errorf("타임아웃은 1ms~%s 범위여야 합니다", MaxTimeout)
	}
	switch p.Confirm {
	case "":
		p.Confirm = ConfirmAsk
	case ConfirmNone, ConfirmAsk, ConfirmDeny:
	default:
		return fmt.Errorf("confirm 은 none|ask|deny 중 하나여야 합니다: %q", p.Confirm)
	}
	if p.MaxOutputBytes == 0 {
		p.MaxOutputBytes = DefaultMaxOutput
	}
	if p.MaxOutputBytes < 0 || p.MaxOutputBytes > MaxOutputCap {
		return fmt.Errorf("출력 상한은 1~%d 바이트여야 합니다", MaxOutputCap)
	}
	if p.MaxRedirects == 0 {
		p.MaxRedirects = DefaultMaxRedirects
	}
	if p.MaxRedirects < 0 || p.MaxRedirects > MaxRedirectsCap {
		return fmt.Errorf("리다이렉트 상한은 0~%d 여야 합니다", MaxRedirectsCap)
	}
	if p.WorkingDir != "" {
		if !filepath.IsAbs(p.WorkingDir) {
			return errors.New("working_dir 는 절대 경로여야 합니다")
		}
		p.WorkingDir = filepath.Clean(p.WorkingDir)
	}
	for i, a := range p.Allow {
		if strings.TrimSpace(a) == "" || strings.ContainsAny(a, " \t\r\n\x00") {
			return fmt.Errorf("allow 항목 #%d 가 올바르지 않습니다", i+1)
		}
	}
	return nil
}

// templateVars returns the variables referenced in s and rejects stray braces.
func templateVars(s string) ([]string, error) {
	var out []string
	for _, m := range varRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	if rest := varRe.ReplaceAllString(s, ""); strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		return nil, errors.New("템플릿 표기가 올바르지 않습니다 ({{name}} 형식만 허용, 중괄호 두 개가 남아 있음)")
	}
	return out, nil
}

// checkVars requires every variable to be a declared scalar schema property.
func checkVars(vars []string, info *schemaInfo, where string) error {
	for _, v := range vars {
		p, ok := info.Properties[v]
		if !ok {
			return fmt.Errorf("%s 의 템플릿 변수 {{%s}} 가 입력 스키마(properties)에 선언되어 있지 않습니다", where, v)
		}
		if !scalarType(p.Type) {
			return fmt.Errorf("%s 의 템플릿 변수 {{%s}} 는 스키마에서 string/integer/number/boolean 타입이어야 합니다", where, v)
		}
	}
	return nil
}

func (d *Definition) validateCommand(info *schemaInfo) error {
	if d.Interpreter != "" || d.ScriptBody != "" || d.Method != "" || d.URL != "" || d.BodyTemplate != "" {
		return errors.New("command 도구에는 script/http 필드를 지정할 수 없습니다")
	}
	cmd := strings.TrimSpace(d.Command)
	if cmd == "" || cmd != d.Command || strings.ContainsAny(cmd, "\x00\r\n") {
		return errors.New("실행 파일(command)이 필요합니다. 예: --argv '[\"git\",\"log\",\"-n\",\"{{count}}\"]'")
	}
	if strings.Contains(cmd, "{{") {
		return errors.New("실행 파일(argv[0])에는 템플릿 변수를 쓸 수 없습니다")
	}
	if strings.Contains(cmd, "/") && !filepath.IsAbs(cmd) {
		return errors.New("실행 파일은 이름(PATH 검색) 또는 절대 경로여야 합니다")
	}
	base := filepath.Base(cmd)
	if d.Shell {
		if base != "sh" || len(d.Args) != 2 || d.Args[0] != "-c" {
			return errors.New("shell 도구는 command=sh, args=[\"-c\", <스크립트>] 형태여야 합니다")
		}
		if strings.Contains(d.Args[1], "{{") {
			return errors.New("shell 도구의 스크립트에는 {{변수}} 치환을 쓸 수 없습니다. 인자는 환경변수 $FORGE_ARG_<이름> 으로 전달됩니다")
		}
		for k := range info.Properties {
			if !identRe.MatchString(k) {
				return fmt.Errorf("shell 도구의 속성 이름 %q 는 환경변수 이름으로 쓸 수 없습니다", k)
			}
		}
		if err := mcp.CheckArgs(strings.Fields(d.Args[1])); err != nil {
			return err
		}
	} else {
		if shellSet[base] || interpreterSet[base] {
			return fmt.Errorf("%s 은(는) 셸/인터프리터라 command 도구로 등록할 수 없습니다 (인자가 코드로 해석될 수 있음). 셸이 필요하면 --shell, 코드 실행은 script 도구를 사용하세요", base)
		}
		// deny list: wrappers / evaluators that run other programs (allowlist bypass), by
		// name and by real binary behind symlinks (see denylist.go).
		if err := checkExecutable(cmd, lookupForCheck(cmd)); err != nil {
			return err
		}
		if len(d.Args) > 0 && strings.Contains(d.Args[0], "{{") && !d.Policy.AllowVarSubcommand {
			return errors.New("argv[1] (프로그램/서브커맨드 자리)에는 {{변수}} 를 쓸 수 없습니다. 변수는 argv[2:] 데이터 위치에만 두세요 (예: cat -- {{file}}). 정말 필요하면 --allow-var-subcommand")
		}
		if base == "git" {
			if err := checkGitArgs(d.Args); err != nil {
				return err
			}
		}
		if len(d.Args) > 128 {
			return errors.New("argv 가 너무 깁니다 (최대 128개)")
		}
		for i, a := range d.Args {
			if strings.ContainsRune(a, 0) {
				return fmt.Errorf("argv #%d 에 널 문자가 있습니다", i+2)
			}
			vars, err := templateVars(a)
			if err != nil {
				return err
			}
			if err := checkVars(vars, info, fmt.Sprintf("argv #%d", i+2)); err != nil {
				return err
			}
		}
		if err := mcp.CheckArgs(stripVars(d.Args)); err != nil {
			return err
		}
	}
	if len(d.Policy.Allow) == 0 {
		d.Policy.Allow = []string{cmd}
	}
	if p := lookupForCheck(cmd); !execAllowed(cmd, cmd, d.Policy.Allow) && !(p != "" && execAllowed(cmd, p, d.Policy.Allow)) {
		return fmt.Errorf("실행 파일 %q 가 allowlist 에 없습니다 (allow: %s). command 는 allowlist 에 포함되어야 합니다", cmd, strings.Join(d.Policy.Allow, ", "))
	}
	d.Interpreter = ""
	return nil
}

// stripVars replaces {{var}} by a neutral word so that secret heuristics only see literal text.
func stripVars(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = varRe.ReplaceAllString(a, "x")
	}
	return out
}

func (d *Definition) validateScript(info *schemaInfo) error {
	if d.Command != "" || len(d.Args) > 0 || d.Shell || d.Method != "" || d.URL != "" || d.BodyTemplate != "" {
		return errors.New("script 도구에는 command/http 필드를 지정할 수 없습니다")
	}
	if !interpreterSet[d.Interpreter] && !contains(d.Policy.Allow, d.Interpreter) {
		return fmt.Errorf("인터프리터 %q 는 허용 목록에 없습니다 (허용: python3, python, node, bash, sh, ruby, perl; 추가는 --allow)", d.Interpreter)
	}
	if d.Interpreter == "" || strings.ContainsAny(d.Interpreter, "/\\ \x00") {
		return errors.New("인터프리터는 PATH 에서 찾을 이름이어야 합니다 (예: python3)")
	}
	if d.ScriptBody == "" && d.ScriptHash == "" {
		return errors.New("스크립트 본문(--script-body) 또는 파일(--script-file)이 필요합니다")
	}
	if len(d.ScriptBody) > MaxScriptBytes {
		return fmt.Errorf("스크립트가 너무 큽니다 (최대 %d바이트)", MaxScriptBytes)
	}
	if strings.ContainsRune(d.ScriptBody, 0) {
		return errors.New("스크립트에 널 문자가 있습니다")
	}
	// Only credential-shaped values are refused; code is otherwise free.
	for _, l := range strings.Split(d.ScriptBody, "\n") {
		if mcp.LooksSecretValue(l) {
			return errors.New("스크립트 본문에 비밀 값(토큰/키)처럼 보이는 문자열이 있어 저장할 수 없습니다 (평문 저장 금지). 환경변수(--env KEY=${ENV})로 전달하세요")
		}
	}
	_ = info
	return nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

var httpMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

func (d *Definition) validateHTTP(info *schemaInfo) error {
	if d.Command != "" || len(d.Args) > 0 || d.Shell || d.Interpreter != "" || d.ScriptBody != "" {
		return errors.New("http 도구에는 command/script 필드를 지정할 수 없습니다")
	}
	d.Method = strings.ToUpper(d.Method)
	if d.Method == "" {
		d.Method = "GET"
	}
	if !httpMethods[d.Method] {
		return fmt.Errorf("method 는 GET|POST|PUT|PATCH|DELETE 중 하나여야 합니다: %q", d.Method)
	}
	if d.BodyTemplate != "" && d.Method == "GET" {
		return errors.New("GET 요청에는 본문(--body-template)을 지정할 수 없습니다")
	}
	if len(d.BodyTemplate) > 256<<10 {
		return errors.New("본문 템플릿이 너무 큽니다")
	}
	vars, err := templateVars(d.URL)
	if err != nil {
		return err
	}
	if err := checkVars(vars, info, "URL"); err != nil {
		return err
	}
	host, err := d.checkURLTemplate()
	if err != nil {
		return err
	}
	if d.BodyTemplate != "" {
		bv, err := templateVars(d.BodyTemplate)
		if err != nil {
			return err
		}
		if err := checkVars(bv, info, "본문"); err != nil {
			return err
		}
		if !json.Valid([]byte(varRe.ReplaceAllString(d.BodyTemplate, "x"))) {
			return errors.New("본문 템플릿은 JSON 이어야 합니다 ({{변수}} 는 JSON 문자열 안에 두세요. 예: {\"q\":\"{{query}}\"})")
		}
		if err := mcp.CheckArgs([]string{varRe.ReplaceAllString(d.BodyTemplate, "x")}); err != nil {
			return fmt.Errorf("본문 템플릿에 비밀 값이 있습니다 (평문 저장 금지): %w", err)
		}
	}
	if len(d.Policy.Allow) == 0 {
		d.Policy.Allow = []string{host}
	}
	for _, h := range d.Policy.Allow {
		if strings.Contains(h, "/") || strings.Contains(h, ":") {
			return fmt.Errorf("allow 항목 %q: 호스트 이름만 지정하세요 (example.com 또는 *.example.com)", h)
		}
	}
	if !hostAllowed(host, d.Policy.Allow) {
		return fmt.Errorf("URL 호스트 %q 가 allow 목록에 없습니다", host)
	}
	return nil
}

// checkURLTemplate validates the URL template and returns its (lowercase) host.
func (d *Definition) checkURLTemplate() (string, error) {
	raw := d.URL
	i := strings.Index(raw, "://")
	if i < 0 {
		return "", errors.New("URL 에 스킴이 필요합니다 (https://...)")
	}
	rest := raw[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		return "", errors.New("URL 의 스킴/호스트/포트에는 템플릿 변수를 쓸 수 없습니다 (SSRF 방지). 경로/쿼리에만 사용하세요")
	}
	u, err := url.Parse(varRe.ReplaceAllString(raw, "x"))
	if err != nil {
		return "", errors.New("URL 형식이 올바르지 않습니다") // never echo: may embed secrets
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && d.Policy.AllowHTTP:
	default:
		return "", fmt.Errorf("URL 스킴은 https 여야 합니다 (평문 http 는 --allow-http 로 명시 허용): %q", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("URL 에 호스트가 없습니다")
	}
	if u.User != nil {
		return "", errors.New("URL 에 사용자 정보(user:pass@)를 넣을 수 없습니다. --header 'Authorization=${ENV_NAME}' 를 사용하세요")
	}
	for k, vs := range u.Query() {
		for _, v := range vs {
			if mcp.LooksSecret(k) && v != "" && v != "x" {
				return "", fmt.Errorf("URL 쿼리스트링에 비밀 값(%s)을 넣을 수 없습니다. --header 와 ${ENV_NAME} 참조를 사용하세요", k)
			}
		}
	}
	if err := mcp.CheckURLSecrets(u); err != nil {
		return "", err
	}
	host := strings.ToLower(u.Hostname())
	if !d.Policy.AllowPrivate {
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
			return "", fmt.Errorf("호스트 %q 는 로컬/내부 주소라 허용되지 않습니다 (--allow-private 로 명시 허용)", host)
		}
	}
	if ip := net.ParseIP(host); ip != nil && ipBlocked(ip, d.Policy.AllowPrivate) {
		return "", fmt.Errorf("IP %s 는 사설/루프백/링크로컬/메타데이터 주소라 허용되지 않습니다 (SSRF 방어; 사설망은 --allow-private 로 명시 허용)", host)
	}
	return host, nil
}

func hostAllowed(host string, allow []string) bool {
	host = strings.ToLower(host)
	for _, a := range allow {
		a = strings.ToLower(a)
		if a == host {
			return true
		}
		if strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:]) && len(host) > len(a)-1 {
			return true
		}
	}
	return false
}

var forbiddenHeaders = map[string]bool{"host": true, "content-length": true, "transfer-encoding": true, "connection": true}

func (d *Definition) validateValues() error {
	want := mcp.SlotEnv
	if d.Kind == KindHTTP {
		want = mcp.SlotHeader
	}
	seen := map[string]bool{}
	for _, v := range d.Values {
		if v.Slot != want {
			if want == mcp.SlotEnv {
				return errors.New("command/script 도구에는 --env 만 지정할 수 있습니다 (--header 불가)")
			}
			return errors.New("http 도구에는 --header 만 지정할 수 있습니다 (--env 불가)")
		}
		k := v.Key
		if d.Kind == KindHTTP {
			k = strings.ToLower(k)
		}
		if seen[k] {
			return fmt.Errorf("%s 가 중복 지정되었습니다", v.Key)
		}
		seen[k] = true
		if d.Kind == KindHTTP {
			if forbiddenHeaders[k] {
				return fmt.Errorf("헤더 %s 는 지정할 수 없습니다", v.Key)
			}
		} else {
			if strings.HasPrefix(strings.ToUpper(v.Key), "FORGE_") {
				return fmt.Errorf("환경변수 %s: FORGE_ 접두어는 예약되어 있습니다", v.Key)
			}
		}
		switch v.RefType {
		case mcp.RefLiteral:
			if v.IsSecret || mcp.LooksSecret(v.Key) {
				return fmt.Errorf("%s: 비밀 값은 참조(${ENV})로만 저장할 수 있습니다", v.Key)
			}
			if mcp.LooksSecretKV(v.Key, v.Literal) {
				return fmt.Errorf("%s: 값이 비밀처럼 보여 literal 로 저장할 수 없습니다 (평문 저장 금지). %s=${ENV_NAME} 참조를 사용하세요", v.Key, v.Key)
			}
		case mcp.RefEnv:
		default:
			return fmt.Errorf("%s: 지원하지 않는 값 참조 형식입니다 (%s)", v.Key, v.RefType)
		}
	}
	return nil
}

// lookupForCheck resolves cmd through the current PATH for the symlink/deny
// check at registration/load time; "" when it cannot be found (checked at run).
func lookupForCheck(cmd string) string {
	p, err := lookPath(cmd, RunOptions{Getenv: os.Getenv})
	if err != nil {
		return ""
	}
	return p
}
