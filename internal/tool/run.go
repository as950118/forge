package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/heonjinjeong/forge/internal/mcp"
)

// Error classes (Result.ErrorClass). Callers (ledger, TASK-0008) can map them to error_class.
const (
	ClassInvalidArgs = "invalid_args"
	ClassDenied      = "denied"
	ClassPolicy      = "policy"
	ClassSSRF        = "ssrf"
	ClassTimeout     = "timeout"
	ClassCanceled    = "canceled"
	ClassExec        = "exec"
	ClassExit        = "exit"
	ClassNetwork     = "network"
	ClassHTTPStatus  = "http_status"
)

// RunOptions are per-call options; the zero value is valid.
type RunOptions struct {
	// Cwd is the requested working directory (relative to the policy root or
	// absolute); it must resolve (symlinks included) to the root or below.
	Cwd string
	// ProjectDir is the default cwd root when the policy sets none (default: process cwd).
	ProjectDir string
	// Timeout overrides the policy timeout (must be <= MaxTimeout).
	Timeout time.Duration
	// DryRun renders the command/request with secrets masked and executes nothing.
	DryRun bool
	// Getenv resolves ${ENV} references (default os.Getenv). Tests inject it.
	Getenv func(string) string
	// Lookup resolves http hosts (default net.DefaultResolver); tests inject it.
	Lookup func(ctx context.Context, host string) ([]net.IP, error)
}

// Result is the outcome of one run. It is designed to feed the ledger later:
// OK, DurationMS, ExitCode, truncation flags and ErrorClass map onto its columns.
type Result struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Revision   int    `json:"revision"`
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exit_code"` // command/script; -1 = not started or killed by signal
	HTTPStatus int    `json:"http_status,omitempty"`
	Stdout     string `json:"stdout,omitempty"` // http: response body
	Stderr     string `json:"stderr,omitempty"`
	// StdoutTruncated / StderrTruncated: output exceeded the policy cap and was cut.
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
	ErrorClass      string `json:"error_class,omitempty"`
	Error           string `json:"error,omitempty"`
	// ErrorDetail carries the raw (English) validator text for schema violations;
	// the CLI shows it only with --verbose. Secrets are masked.
	ErrorDetail string `json:"error_detail,omitempty"`
	DryRun      *Plan  `json:"dry_run,omitempty"`
}

// Plan is the masked rendering shown by --dry-run.
type Plan struct {
	Argv        []string          `json:"argv,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Interpreter string            `json:"interpreter,omitempty"`
	ScriptHash  string            `json:"script_hash,omitempty"`
	Method      string            `json:"method,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        string            `json:"body,omitempty"`
}

// concurrency limit across all runs in this process.
var runSem = make(chan struct{}, 8)

// Run validates args against the schema, applies the execution policy and runs
// the tool. A non-nil error means the call was rejected or could not start; a
// started run that fails (non-zero exit, timeout, HTTP error) is reported in
// the Result with OK=false and a nil error. The Result is never nil.
func Run(ctx context.Context, def *Definition, args map[string]any, opts ...RunOptions) (*Result, error) {
	var o RunOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	res := &Result{Name: def.Name, Kind: def.Kind, Revision: def.Revision, ExitCode: -1}
	start := time.Now()
	m := &masker{}
	finish := func() { res.DurationMS = time.Since(start).Milliseconds() }
	fail := func(class string, err error) (*Result, error) {
		res.ErrorClass = class
		res.Error = m.mask(err.Error())
		var ae *ArgsError
		if errors.As(err, &ae) {
			res.ErrorDetail = m.mask(ae.Detail)
		}
		res.OK = false
		finish()
		return res, errors.New(res.Error)
	}

	if def.Policy.Confirm == ConfirmDeny {
		return fail(ClassDenied, errors.New("이 도구는 정책(confirm=deny)상 실행이 금지되어 있습니다"))
	}
	// Policy is re-checked on every run: the definition may come from a tampered
	// DB row or be hand-built. Validate works on a copy (it normalizes fields).
	dc := *def
	if err := dc.ValidateExec(); err != nil {
		return fail(ClassPolicy, fmt.Errorf("도구 정의가 정책을 위반합니다: %w", err))
	}
	clean, err := ValidateArgs(def, args)
	if err != nil {
		return fail(ClassInvalidArgs, err)
	}
	timeout := def.Timeout()
	if o.Timeout != 0 {
		if o.Timeout < 0 || o.Timeout > MaxTimeout {
			return fail(ClassPolicy, fmt.Errorf("타임아웃은 0 초과 %s 이하여야 합니다", MaxTimeout))
		}
		timeout = o.Timeout
	}
	maxOut := def.Policy.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = DefaultMaxOutput
	}

	if !o.DryRun {
		select {
		case runSem <- struct{}{}:
			defer func() { <-runSem }()
		case <-ctx.Done():
			return fail(ClassCanceled, ctx.Err())
		}
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var runErr error
	switch def.Kind {
	case KindCommand, KindScript:
		runErr = runLocal(tctx, def, clean, o, m, res, maxOut)
	case KindHTTP:
		runErr = runHTTP(tctx, def, clean, o, m, res, maxOut)
	default:
		runErr = &runError{ClassPolicy, fmt.Errorf("알 수 없는 kind: %s", def.Kind)}
	}
	if res.Stdout != "" {
		res.Stdout = m.mask(res.Stdout)
	}
	if res.Stderr != "" {
		res.Stderr = m.mask(res.Stderr)
	}
	if runErr != nil {
		class := ClassExec
		var re *runError
		if errors.As(runErr, &re) {
			class = re.class
		}
		if res.TimedOut {
			class = ClassTimeout
		}
		return fail(class, runErr)
	}
	finish()
	return res, nil
}

type runError struct {
	class string
	err   error
}

func (e *runError) Error() string { return e.err.Error() }
func (e *runError) Unwrap() error { return e.err }

func policyErr(format string, a ...any) error {
	return &runError{ClassPolicy, fmt.Errorf(format, a...)}
}

// masker replaces resolved secret values in anything that may be shown.
type masker struct {
	mu   sync.Mutex
	vals []string
}

func (m *masker) add(v string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range mcp.SecretForms(v) {
		if len(f) >= 4 {
			m.vals = append(m.vals, f)
		}
	}
}

func (m *masker) mask(s string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	vals := append([]string(nil), m.vals...)
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}

// capBuf keeps at most max bytes and discards (but counts) the rest.
type capBuf struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) <= room {
			c.buf.Write(p)
		} else {
			c.buf.Write(p[:room])
			c.truncated = true
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

func (c *capBuf) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.ToValidUTF8(c.buf.String(), "�")
}

// scalarString renders a validated scalar argument for template substitution.
func scalarString(v any) (string, error) {
	switch x := v.(type) {
	case string:
		if strings.ContainsRune(x, 0) {
			return "", errors.New("널 문자(\\x00)가 포함된 값은 허용되지 않습니다")
		}
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return "", errors.New("숫자 값이 올바르지 않습니다")
		}
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case json.Number:
		return x.String(), nil
	}
	return "", fmt.Errorf("스칼라(string/number/boolean) 값만 템플릿에 쓸 수 있습니다: %T", v)
}

// render substitutes {{var}} inside ONE string; esc post-processes each value.
func render(tpl string, args map[string]any, esc func(val string, pos int) string) (string, error) {
	var sb strings.Builder
	last := 0
	for _, loc := range varRe.FindAllStringSubmatchIndex(tpl, -1) {
		sb.WriteString(tpl[last:loc[0]])
		name := tpl[loc[2]:loc[3]]
		v, ok := args[name]
		if !ok {
			return "", &runError{ClassInvalidArgs, fmt.Errorf("템플릿 변수 %s 의 값이 지정되지 않았습니다", name)}
		}
		s, err := scalarString(v)
		if err != nil {
			return "", &runError{ClassInvalidArgs, fmt.Errorf("%s: %w", name, err)}
		}
		if esc != nil {
			s = esc(s, loc[0])
		}
		sb.WriteString(s)
		last = loc[1]
	}
	sb.WriteString(tpl[last:])
	return sb.String(), nil
}

// renderArgv substitutes inside each argv element only: a value can never be
// split into several arguments and, unless the policy allows it, can never turn
// an argument into an option.
func renderArgv(def *Definition, args map[string]any) ([]string, error) {
	out := make([]string, 0, len(def.Args))
	for i, t := range def.Args {
		s, err := render(t, args, nil)
		if err != nil {
			return nil, err
		}
		if !def.Policy.AllowLeadingDash && strings.HasPrefix(s, "-") && !strings.HasPrefix(t, "-") {
			return nil, &runError{ClassInvalidArgs, fmt.Errorf("argv #%d: 값이 '-' 로 시작해 옵션으로 해석될 수 있어 거부합니다 (정책 allow_leading_dash 로 허용 가능)", i+2)}
		}
		out = append(out, s)
	}
	return out, nil
}

var baseEnvKeys = []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR"}

// buildEnv returns the child environment (minimal base + declared ${ENV}
// references/literals + FORGE_TOOL_*) and a masked display map. The parent
// environment is never inherited wholesale.
func buildEnv(def *Definition, args map[string]any, o RunOptions, m *masker, dry bool) ([]string, map[string]string, error) {
	env := map[string]string{}
	disp := map[string]string{}
	for _, k := range baseEnvKeys {
		if v := o.Getenv(k); v != "" {
			env[k] = v
			disp[k] = v
			if k == "PATH" { // never echo the parent's full PATH (home paths, tool layout)
				disp[k] = fmt.Sprintf("(부모 PATH 사용, %d개 디렉터리; 값은 표시하지 않음)", len(filepath.SplitList(v)))
			}
		}
	}
	for _, v := range def.Values {
		if v.Slot != mcp.SlotEnv {
			continue
		}
		switch v.RefType {
		case mcp.RefLiteral:
			env[v.Key], disp[v.Key] = v.Literal, v.Literal
		case mcp.RefEnv:
			disp[v.Key] = "*** (${" + v.Ref + "})"
			if dry {
				continue
			}
			val, ok := lookupEnvRef(o, v.Ref)
			if !ok {
				return nil, nil, &runError{ClassPolicy, fmt.Errorf("환경변수 %s 가 설정되어 있지 않습니다 (도구가 ${%s} 를 참조합니다)", v.Ref, v.Ref)}
			}
			env[v.Key] = val
			m.add(val)
		}
	}
	env["FORGE_TOOL_NAME"], env["FORGE_TOOL_KIND"], env["FORGE_TOOL_REVISION"] = def.Name, def.Kind, strconv.Itoa(def.Revision)
	for _, k := range []string{"FORGE_TOOL_NAME", "FORGE_TOOL_KIND", "FORGE_TOOL_REVISION"} {
		disp[k] = env[k]
	}
	if def.Shell {
		// arguments reach the shell only as data in the environment, never as code.
		for name, v := range args {
			if !identRe.MatchString(name) {
				continue
			}
			s, err := scalarString(v)
			if err != nil {
				b, _ := json.Marshal(v)
				s = string(b)
			}
			env["FORGE_ARG_"+name] = s
			disp["FORGE_ARG_"+name] = maskIfSecret(s)
		}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out, disp, nil
}

func lookupEnvRef(o RunOptions, name string) (string, bool) {
	v := o.Getenv(name)
	return v, v != ""
}

func maskIfSecret(s string) string {
	if mcp.LooksSecretValue(s) {
		return "***"
	}
	return s
}

// resolveCwd enforces the cwd policy: the effective directory must be the root
// or below it after symlink resolution (so `..` and symlink escapes fail).
func resolveCwd(def *Definition, o RunOptions) (string, error) {
	root := def.Policy.WorkingDir
	if root == "" {
		root = o.ProjectDir
	}
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", policyErr("현재 디렉터리를 알 수 없습니다: %v", err)
		}
		root = wd
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", policyErr("작업 디렉터리 루트를 확인할 수 없습니다: %v", err)
	}
	rootReal, _ = filepath.Abs(rootReal)
	target := rootReal
	if o.Cwd != "" {
		target = o.Cwd
		if !filepath.IsAbs(target) {
			target = filepath.Join(rootReal, target)
		}
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", policyErr("작업 디렉터리를 확인할 수 없습니다: %v", err)
	}
	real, _ = filepath.Abs(real)
	if real != rootReal && !strings.HasPrefix(real, rootReal+string(filepath.Separator)) {
		return "", policyErr("작업 디렉터리가 허용 범위(%s)를 벗어납니다 (.. / 심볼릭 링크 탈출 금지)", rootReal)
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", policyErr("작업 디렉터리가 디렉터리가 아닙니다")
	}
	return real, nil
}

// execAllowed checks the resolved executable against the allowlist (name or absolute path).
func execAllowed(cmd, resolved string, allow []string) bool {
	for _, a := range allow {
		if a == cmd || a == resolved {
			return true
		}
		if !strings.Contains(a, "/") && !strings.Contains(cmd, "/") && a == filepath.Base(resolved) {
			return true
		}
	}
	return false
}

func lookPath(name string, o RunOptions) (string, error) {
	if strings.Contains(name, "/") {
		if fi, err := os.Stat(name); err != nil || fi.IsDir() {
			return "", &runError{ClassExec, fmt.Errorf("실행 파일을 찾을 수 없습니다: %s", name)}
		}
		return name, nil
	}
	for _, dir := range filepath.SplitList(o.Getenv("PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue // never run from "." via PATH
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", &runError{ClassExec, fmt.Errorf("PATH 에서 실행 파일을 찾을 수 없습니다: %s", name)}
}

func runLocal(ctx context.Context, def *Definition, args map[string]any, o RunOptions, m *masker, res *Result, maxOut int) error {
	var (
		exe    string
		argv   []string
		stdin  []byte
		tmpDir string
	)
	cwd, err := resolveCwd(def, o)
	if err != nil {
		return err
	}
	switch def.Kind {
	case KindCommand:
		exe = def.Command
		if !def.Shell {
			if argv, err = renderArgv(def, args); err != nil {
				return err
			}
		} else {
			argv = append([]string(nil), def.Args...)
		}
	case KindScript:
		exe = def.Interpreter
		if !interpreterSet[exe] && !contains(def.Policy.Allow, exe) {
			return policyErr("인터프리터 %q 는 허용 목록에 없습니다", exe)
		}
		if def.ScriptBody == "" {
			return policyErr("스크립트 본문이 로드되지 않았습니다 (Registry.Resolve 로 불러오세요)")
		}
		if h := hashHex([]byte(def.ScriptBody)); def.ScriptHash != "" && h != def.ScriptHash {
			return policyErr("스크립트가 저장 시점과 다릅니다 (해시 불일치). 다시 등록하세요")
		}
		stdin, _ = json.Marshal(args)
	}
	resolved, err := lookPath(exe, o)
	if err != nil {
		if o.DryRun {
			resolved = exe // dry-run shows the plan even if the binary is absent
		} else {
			return err
		}
	}
	if def.Kind == KindCommand && !execAllowed(def.Command, resolved, def.Policy.Allow) {
		return policyErr("실행 파일 %q 가 allowlist 에 없습니다 (허용: %s)", def.Command, strings.Join(def.Policy.Allow, ", "))
	}
	if def.Kind == KindCommand && !def.Shell {
		if err := checkExecutable(def.Command, resolved); err != nil {
			return &runError{ClassPolicy, err}
		}
	}
	env, disp, err := buildEnv(def, args, o, m, o.DryRun)
	if err != nil {
		return err
	}
	if o.DryRun {
		p := &Plan{Cwd: cwd, Env: disp}
		if def.Kind == KindScript {
			p.Interpreter, p.ScriptHash = resolved, def.ScriptHash
			p.Argv = []string{resolved, "<임시 스크립트 파일>"}
			p.Body = "stdin(JSON 인자): " + maskIfSecret(string(stdin))
		} else {
			for i, a := range append([]string{def.Command}, argv...) {
				if i > 0 {
					a = maskIfSecret(a)
				}
				p.Argv = append(p.Argv, a)
			}
		}
		res.DryRun, res.OK = p, true
		return nil
	}
	if def.Kind == KindScript {
		tmpDir, err = os.MkdirTemp("", "forge-tool-*")
		if err != nil {
			return &runError{ClassExec, fmt.Errorf("임시 디렉터리 생성 실패: %w", err)}
		}
		defer os.RemoveAll(tmpDir)
		_ = os.Chmod(tmpDir, 0o700)
		script := filepath.Join(tmpDir, "script")
		if err := os.WriteFile(script, []byte(def.ScriptBody), 0o600); err != nil {
			return &runError{ClassExec, fmt.Errorf("임시 스크립트 쓰기 실패: %w", err)}
		}
		argv = []string{script}
	}

	// Re-check right before exec: PATH lookup result, allowlist and deny list
	// must still agree (narrows the window for a link swapped after the first check).
	if def.Kind == KindCommand {
		again, err := lookPath(exe, o)
		if err != nil || again != resolved {
			return policyErr("실행 파일 경로가 검사 이후 바뀌었습니다: %s", exe)
		}
		if !execAllowed(def.Command, again, def.Policy.Allow) {
			return policyErr("실행 파일 %q 가 allowlist 에 없습니다", def.Command)
		}
		if !def.Shell {
			if err := checkExecutable(def.Command, again); err != nil {
				return &runError{ClassPolicy, err}
			}
		}
	}
	cmd := exec.CommandContext(ctx, resolved, argv...)
	cmd.Env = env
	cmd.Dir = cwd
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	so, se := &capBuf{max: maxOut}, &capBuf{max: maxOut}
	cmd.Stdout, cmd.Stderr = so, se
	setProcGroup(cmd)
	cmd.Cancel = func() error { killGroup(cmd); return nil }
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	killGroup(cmd) // leftover grandchildren (also after a normal exit)
	res.Stdout, res.Stderr = so.String(), se.String()
	res.StdoutTruncated, res.StderrTruncated = so.truncated, se.truncated
	if ps := cmd.ProcessState; ps != nil {
		res.ExitCode = ps.ExitCode()
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.TimedOut = true
		return &runError{ClassTimeout, errors.New("타임아웃으로 프로세스 그룹을 종료했습니다")}
	case ctx.Err() != nil:
		return &runError{ClassCanceled, errors.New("취소되어 프로세스 그룹을 종료했습니다")}
	case err == nil || errors.Is(err, exec.ErrWaitDelay):
		if res.ExitCode == 0 {
			res.OK = true
			return nil
		}
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) || res.ExitCode > 0 {
		res.ErrorClass = ClassExit
		res.Error = fmt.Sprintf("종료 코드 %d", res.ExitCode)
		return nil // started and finished: reported through the Result
	}
	return &runError{ClassExec, fmt.Errorf("실행 실패: %w", err)}
}
