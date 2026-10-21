package mcp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultTimeout bounds one whole discovery attempt (connect + initialize + tools/list).
const DefaultTimeout = 30 * time.Second

// Failure classes reported by discovery.
const (
	ClassCommandNotFound = "command_not_found"
	ClassExecFailed      = "exec_failed"
	ClassTimeout         = "timeout"
	ClassProtocol        = "protocol"
	ClassHTTPStatus      = "http_status"
	ClassAuthRequired    = "auth_required"
	ClassConnection      = "connection"
	ClassEnvMissing      = "env_missing"
	ClassProcessExit     = "process_exit"
)

const (
	maxMessage = 400
	stderrTail = 300
	// maxStdoutLine caps one newline-delimited stdout message from a stdio
	// server. MCP stdio messages are one JSON document per line, so a longer
	// line without a newline is a broken (or hostile) server.
	maxStdoutLine = 4 << 20
)

// RefreshOptions tunes one discovery attempt.
type RefreshOptions struct {
	// Timeout overrides the server's timeout_ms and DefaultTimeout when > 0.
	Timeout time.Duration
}

// RefreshResult reports one discovery attempt. A failed attempt is not a Go
// error: the previous cache is kept and only the last error is recorded.
type RefreshResult struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	ErrorClass string `json:"error_class,omitempty"`
	Error      string `json:"error,omitempty"`
	Hint       string `json:"hint,omitempty"`
	// Detail is the sanitized raw diagnostic (SDK/OS wording); the CLI shows it only with --verbose.
	Detail          string `json:"detail,omitempty"`
	ToolCount       int    `json:"tool_count"`
	Added           int    `json:"added"`
	Changed         int    `json:"changed"`
	Removed         int    `json:"removed"` // newly stale
	ServerName      string `json:"server_name,omitempty"`
	ServerVersion   string `json:"server_version,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
}

type failure struct {
	class  string
	msg    string
	detail string
}

type discovered struct {
	tools   []*sdk.Tool
	proto   string
	srvName string
	srvVer  string
}

// Refresh connects to the server (initialize -> tools/list), then stores the
// result in the discovery cache. On connection failure the cached tools are
// left untouched and only the attempt/error are recorded.
func (r *Registry) Refresh(ctx context.Context, name string, opt RefreshOptions) (*RefreshResult, error) {
	var (
		id int64
		in *Info
	)
	id, in, err := scanInfo(r.DB.QueryRowContext(ctx,
		`SELECT `+infoCols+` FROM mcp_servers WHERE scope='global' AND project_id IS NULL AND name=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	if err := r.fill(ctx, id, in, true); err != nil {
		return nil, err
	}
	timeout := DefaultTimeout
	if in.TimeoutMS > 0 {
		timeout = time.Duration(in.TimeoutMS) * time.Millisecond
	}
	if opt.Timeout > 0 {
		timeout = opt.Timeout
	}

	start := time.Now()
	res := &RefreshResult{Name: name}
	secrets := map[string]bool{}
	resolved, fail := resolveValues(in.Values, secrets)
	var d *discovered
	if fail == nil {
		d, fail = discover(ctx, in, resolved, timeout, secrets)
	}
	res.DurationMS = time.Since(start).Milliseconds()

	now := r.now()
	if fail != nil {
		res.ErrorClass = fail.class
		res.Error = fail.msg
		res.Detail = fail.detail
		res.Hint = Hint(fail.class, fail.msg)
		if err := r.storeFailure(ctx, id, now, fail); err != nil {
			return res, err
		}
		return res, nil
	}
	res.OK = true
	res.ServerName, res.ServerVersion, res.ProtocolVersion = d.srvName, d.srvVer, d.proto
	if err := r.storeSuccess(ctx, id, now, d, res); err != nil {
		return nil, err
	}
	return res, nil
}

func (r *Registry) storeFailure(ctx context.Context, id, now int64, f *failure) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Keep protocol/server info and tools from the last success; record the error only.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mcp_discovery(server_id, last_attempt_at, ok, error) VALUES (?, ?, 0, ?)
		ON CONFLICT(server_id) DO UPDATE SET last_attempt_at=excluded.last_attempt_at, ok=0, error=excluded.error`,
		id, now, "["+f.class+"] "+f.msg); err != nil {
		return fmt.Errorf("발견 실패 기록 중 오류 (서버가 삭제되었을 수 있습니다): %w", err)
	}
	return tx.Commit()
}

func (r *Registry) storeSuccess(ctx context.Context, id, now int64, d *discovered, res *RefreshResult) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mcp_discovery(server_id, last_attempt_at, ok, error, protocol_version, server_name, server_version) VALUES (?, ?, 1, NULL, ?, ?, ?)
		ON CONFLICT(server_id) DO UPDATE SET last_attempt_at=excluded.last_attempt_at, ok=1, error=NULL,
			protocol_version=excluded.protocol_version, server_name=excluded.server_name, server_version=excluded.server_version`,
		id, now, nullStr(d.proto), nullStr(d.srvName), nullStr(d.srvVer)); err != nil {
		return fmt.Errorf("발견 결과 저장 실패 (서버가 삭제되었을 수 있습니다): %w", err)
	}
	old := map[string]struct {
		hash  string
		stale bool
	}{}
	rows, err := tx.QueryContext(ctx, `SELECT name, schema_hash, stale FROM mcp_tools WHERE server_id=?`, id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n, h string
		var st int
		if err := rows.Scan(&n, &h, &st); err != nil {
			_ = rows.Close()
			return err
		}
		old[n] = struct {
			hash  string
			stale bool
		}{h, st == 1}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	seen := map[string]bool{}
	for _, t := range d.tools {
		if seen[t.Name] {
			continue // duplicate names from a misbehaving server
		}
		seen[t.Name] = true
		schema, err := json.Marshal(t.InputSchema)
		if err != nil || string(schema) == "null" {
			schema = []byte("{}")
		}
		hash := toolHash(t.Name, t.Description, schema)
		if o, ok := old[t.Name]; !ok {
			res.Added++
		} else if o.hash != hash {
			res.Changed++
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mcp_tools(server_id, name, description, input_schema, schema_hash, stale, discovered_at) VALUES (?,?,?,?,?,0,?)
			ON CONFLICT(server_id, name) DO UPDATE SET description=excluded.description, input_schema=excluded.input_schema,
				schema_hash=excluded.schema_hash, stale=0, discovered_at=excluded.discovered_at`,
			id, t.Name, nullStr(t.Description), string(schema), hash, now); err != nil {
			return err
		}
	}
	for n, o := range old {
		if !seen[n] && !o.stale {
			res.Removed++
			if _, err := tx.ExecContext(ctx, `UPDATE mcp_tools SET stale=1 WHERE server_id=? AND name=?`, id, n); err != nil {
				return err
			}
		}
	}
	res.ToolCount = len(seen)
	return tx.Commit()
}

func toolHash(name, desc string, schema []byte) string {
	h := sha256.New()
	for _, p := range [][]byte{[]byte(name), []byte(desc), schema} {
		h.Write(p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// resolveValues turns references into concrete values (only held in memory and
// handed to the child process / HTTP request). Resolved secrets are collected
// into secrets so that diagnostics can redact them.
func resolveValues(vals []Value, secrets map[string]bool) (map[string]map[string]string, *failure) {
	out := map[string]map[string]string{SlotEnv: {}, SlotHeader: {}}
	for _, v := range vals {
		var s string
		switch v.RefType {
		case RefLiteral:
			s = v.Literal
		case RefEnv:
			var ok bool
			s, ok = os.LookupEnv(v.Ref)
			if !ok || s == "" {
				return nil, &failure{class: ClassEnvMissing, msg: fmt.Sprintf("%s %s 가 참조하는 환경변수 %s 가 설정되어 있지 않습니다", slotLabel(v.Slot), v.Key, v.Ref)}
			}
			secrets[s] = true
		default:
			// TODO(keychain): resolve via macOS `security`.
			return nil, &failure{class: ClassExecFailed, msg: fmt.Sprintf("%s %s: keychain 참조는 아직 지원하지 않습니다", slotLabel(v.Slot), v.Key)}
		}
		out[v.Slot][v.Key] = s
	}
	return out, nil
}

// diagTransport injects headers and remembers the last HTTP error status.
type diagTransport struct {
	base    http.RoundTripper
	headers map[string]string
	mu      sync.Mutex
	status  int
}

func (t *diagTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp.StatusCode >= 400 {
		t.record(resp.StatusCode)
	}
	return resp, err
}

// record keeps the first error status of an attempt; auth failures (401/403)
// take priority so that a later unrelated error (e.g. DELETE 501 on close)
// cannot turn auth_required into http_status.
func (t *diagTransport) record(code int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	isAuth := func(c int) bool { return c == 401 || c == 403 }
	if t.status == 0 || (isAuth(code) && !isAuth(t.status)) {
		t.status = code
	}
}

func (t *diagTransport) lastStatus() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// tailBuf keeps the last bytes written (child stderr).
type tailBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > 4096 {
		b.buf = b.buf[len(b.buf)-4096:]
	}
	return len(p), nil
}

func (b *tailBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func discover(parent context.Context, in *Info, vals map[string]map[string]string, timeout time.Duration, secrets map[string]bool) (*discovered, *failure) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	client := sdk.NewClient(&sdk.Implementation{Name: "forge", Version: "0"}, nil)
	var (
		transport sdk.Transport
		cmd       *exec.Cmd
		errBuf    = &tailBuf{}
		dt        *diagTransport
		lim       *lineLimit
	)
	switch in.Transport {
	case TransportStdio:
		cmd = exec.Command(in.Command, in.Args...)
		if cmd.Err != nil {
			return nil, &failure{class: ClassCommandNotFound, msg: fmt.Sprintf("실행 파일을 찾을 수 없습니다: %s", in.Command)}
		}
		cmd.Dir = in.Cwd
		cmd.Env = os.Environ()
		for k, v := range vals[SlotEnv] {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stderr = errBuf
		cmd.WaitDelay = 2 * time.Second
		setProcGroup(cmd)
		lim = &lineLimit{max: maxStdoutLine}
		transport = &limitedCommandTransport{cmd: cmd, lim: lim}
	case TransportHTTP, TransportSSE:
		dt = &diagTransport{base: http.DefaultTransport, headers: vals[SlotHeader]}
		hc := &http.Client{Transport: dt}
		if in.Transport == TransportHTTP {
			transport = &sdk.StreamableClientTransport{Endpoint: in.URL, HTTPClient: hc, MaxRetries: -1, DisableStandaloneSSE: true}
		} else {
			transport = &sdk.SSEClientTransport{Endpoint: in.URL, HTTPClient: hc}
		}
	default:
		return nil, &failure{class: ClassProtocol, msg: "알 수 없는 transport: " + in.Transport}
	}

	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		if cmd != nil {
			killGroup(cmd)
		}
		return nil, classifyLimited(lim, ctx, err, in, dt, errBuf.String(), secrets, timeout)
	}
	d := &discovered{}
	if ir := cs.InitializeResult(); ir != nil {
		d.proto = ir.ProtocolVersion
		if ir.ServerInfo != nil {
			d.srvName, d.srvVer = ir.ServerInfo.Name, ir.ServerInfo.Version
		}
	}
	var lerr error
	for t, e := range cs.Tools(ctx, nil) {
		if e != nil {
			lerr = e
			break
		}
		d.tools = append(d.tools, t)
	}
	_ = cs.Close()
	if cmd != nil {
		killGroup(cmd)
	}
	if lerr != nil {
		return nil, classifyLimited(lim, ctx, lerr, in, dt, errBuf.String(), secrets, timeout)
	}
	return d, nil
}

func classifyLimited(lim *lineLimit, ctx context.Context, err error, in *Info, dt *diagTransport, stderr string, secrets map[string]bool, timeout time.Duration) *failure {
	if lim != nil && lim.exceeded() {
		return &failure{ClassProtocol,
			sanitize(fmt.Sprintf("서버가 줄바꿈 없이 %d MiB 를 초과하는 출력을 보내 중단했습니다 (프로토콜 위반)", maxStdoutLine>>20), secrets),
			sanitize(err.Error(), secrets)}
	}
	return classify(ctx, err, in, dt, stderr, secrets, timeout)
}

// classify maps a connection error to a class and a user-facing Korean message.
// The raw SDK/OS wording goes to failure.detail only.
func classify(ctx context.Context, err error, in *Info, dt *diagTransport, stderr string, secrets map[string]bool, timeout time.Duration) *failure {
	raw := err.Error()
	mk := func(class, msg string) *failure {
		return &failure{class, sanitize(msg, secrets), sanitize(raw, secrets)}
	}
	withStderr := func(msg string) string {
		if t := strings.TrimSpace(stderr); t != "" {
			return msg + " | stderr: " + lastRunes(oneLine(t), stderrTail)
		}
		return msg
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return mk(ClassTimeout, withStderr(fmt.Sprintf("%s 안에 응답이 없습니다", timeout)))
	}
	if errors.Is(err, context.Canceled) {
		return mk(ClassTimeout, "작업이 취소되었습니다")
	}
	if in.Transport == TransportStdio {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) ||
			strings.Contains(raw, "executable file not found") || strings.Contains(raw, "no such file or directory") {
			return mk(ClassCommandNotFound, fmt.Sprintf("실행 파일을 찾을 수 없습니다: %s", in.Command))
		}
		if errors.Is(err, fs.ErrPermission) || strings.Contains(raw, "permission denied") || strings.Contains(raw, "exec format error") {
			return mk(ClassExecFailed, fmt.Sprintf("명령을 실행할 수 없습니다: %s (권한 또는 실행 파일 형식 문제)", in.Command))
		}
		if strings.Contains(raw, "EOF") || strings.Contains(raw, "closed") || strings.Contains(raw, "broken pipe") || strings.Contains(raw, "exit status") {
			return mk(ClassProcessExit, withStderr("서버 프로세스가 응답 전에 종료되었습니다"))
		}
		return mk(ClassProtocol, withStderr("MCP 응답을 해석할 수 없습니다 (서버가 JSON-RPC 가 아닌 출력을 보냈을 수 있습니다)"))
	}
	if dt != nil {
		switch s := dt.lastStatus(); {
		case s == 401 || s == 403:
			return mk(ClassAuthRequired, fmt.Sprintf("HTTP %d: 인증이 필요하거나 거부되었습니다", s))
		case s >= 400:
			return mk(ClassHTTPStatus, fmt.Sprintf("HTTP %d 응답", s))
		}
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return mk(ClassConnection, "서버에 연결할 수 없습니다: "+oe.Error())
	}
	var ne net.Error
	if errors.As(err, &ne) || strings.Contains(raw, "connection refused") || strings.Contains(raw, "no such host") {
		return mk(ClassConnection, "서버에 연결할 수 없습니다 (주소/포트/네트워크 확인)")
	}
	return mk(ClassProtocol, "MCP 응답을 해석할 수 없습니다 (transport 종류나 URL 이 서버와 맞는지 확인하세요)")
}

// sanitize redacts resolved secret values and bounds the message length.
func sanitize(s string, secrets map[string]bool) string {
	for sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	s = oneLine(s)
	if utf8.RuneCountInString(s) > maxMessage {
		s = string([]rune(s)[:maxMessage-1]) + "…"
	}
	return s
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

// ErrorClassOf extracts the class from a stored "[class] message" error.
func ErrorClassOf(stored string) string {
	if strings.HasPrefix(stored, "[") {
		if i := strings.Index(stored, "] "); i > 1 {
			return stored[1:i]
		}
	}
	return ""
}

// Hint returns a Korean remediation hint for a failure class.
func Hint(class, msg string) string {
	switch class {
	case ClassCommandNotFound:
		return "명령이 설치되어 PATH 에 있는지 확인하거나 절대 경로로 다시 등록하세요 (예: npx/uvx/node 설치 여부)."
	case ClassExecFailed:
		return "실행 권한과 파일 형식(아키텍처)을 확인하세요. keychain 참조는 아직 지원되지 않습니다."
	case ClassTimeout:
		return "서버가 처음 실행되며 패키지를 내려받는 중일 수 있습니다. 서버 단독 실행으로 확인하거나 --timeout 으로 시간을 늘려 다시 시도하세요."
	case ClassProtocol:
		return "MCP 프로토콜 응답이 아닙니다. stdout 에 로그를 출력하지 않는지, transport(stdio/http/sse)가 서버와 맞는지 확인하세요."
	case ClassHTTPStatus:
		return "URL 경로와 transport 종류(http=Streamable HTTP, sse=레거시 SSE)가 맞는지 확인하세요."
	case ClassAuthRequired:
		return "인증이 필요합니다. --header 'Authorization=${ENV_NAME}' 로 토큰 환경변수를 지정하고, 현재 셸에 해당 환경변수가 설정되어 있는지 확인하세요."
	case ClassConnection:
		return "주소/포트/네트워크/프록시를 확인하세요. 서버가 실행 중인지도 확인하세요."
	case ClassEnvMissing:
		return "참조된 환경변수를 현재 셸에서 export 한 뒤 다시 시도하세요 (값은 Forge 가 저장하지 않습니다)."
	case ClassProcessExit:
		return "서버가 시작 직후 종료되었습니다. 위 stderr 내용(필수 환경변수/인자 누락 등)을 확인하세요."
	}
	return ""
}
