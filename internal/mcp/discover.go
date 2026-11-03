package mcp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/as950118/forge/internal/netsafe"
	"github.com/as950118/forge/internal/textsafe"
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
	// ClassBlockedAddress: the connection was refused because the host resolved to a
	// loopback/private/link-local/metadata address that is not allowed (netsafe).
	ClassBlockedAddress = "blocked_address"
	ClassEnvMissing     = "env_missing"
	ClassProcessExit    = "process_exit"
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
	Detail    string `json:"detail,omitempty"`
	ToolCount int    `json:"tool_count"`
	Added     int    `json:"added"`
	Changed   int    `json:"changed"`
	Removed   int    `json:"removed"` // newly stale
	// Changes lists what differs from the approved definitions (or, for tools
	// that were never approved, from the previous cache): new tools are
	// "unapproved (new)", changed definitions come with a sanitized diff.
	Changes []ToolChange `json:"changes,omitempty"`
	// Unapproved counts the offered tools without an approval; ChangedFromApproved
	// the approved tools whose definition differs now.
	Unapproved          int    `json:"unapproved"`
	ChangedFromApproved int    `json:"changed_from_approved"`
	Enforce             bool   `json:"enforce_pinned"`
	ServerName          string `json:"server_name,omitempty"`
	ServerVersion       string `json:"server_version,omitempty"`
	ProtocolVersion     string `json:"protocol_version,omitempty"`
	DurationMS          int64  `json:"duration_ms"`
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
	// Everything the server returned is masked with this server's resolved secrets
	// before it reaches the cache (and the change-detection hash): a server that
	// echoes a declared env value in a tool description must not leak it into
	// forge.db or `forge mcp show`.
	maskDiscovered(d, secrets)
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
	type oldTool struct {
		hash, desc, schema string
		stale              bool
	}
	old := map[string]oldTool{}
	rows, err := tx.QueryContext(ctx, `SELECT name, schema_hash, COALESCE(description,''), input_schema, stale FROM mcp_tools WHERE server_id=?`, id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		var o oldTool
		var st int
		if err := rows.Scan(&n, &o.hash, &o.desc, &o.schema, &st); err != nil {
			_ = rows.Close()
			return err
		}
		o.stale = st == 1
		old[n] = o
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	approvals, err := loadApprovals(ctx, tx, id)
	if err != nil {
		return err
	}
	var enforceVal string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=(SELECT 'mcp.enforce.' || name FROM mcp_servers WHERE id=?)`, id).Scan(&enforceVal); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	res.Enforce = enforceVal == "1"
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
		o, inOld := old[t.Name]
		if !inOld {
			res.Added++
		} else if o.hash != hash {
			res.Changed++
		}
		desc := t.Description
		ap, approved := approvals[t.Name]
		switch {
		case approved && ap.hash == hash:
			// approved and unchanged: nothing to report
		case approved:
			res.ChangedFromApproved++
			res.Changes = append(res.Changes, toolChange(t.Name, "changed", "approved", true, ap.desc, desc, ap.schema, string(schema), res.Enforce))
		case !inOld || o.stale:
			res.Unapproved++
			res.Changes = append(res.Changes, ToolChange{Name: t.Name, Kind: "new", Hidden: res.Enforce})
		case o.hash != hash:
			res.Unapproved++
			res.Changes = append(res.Changes, toolChange(t.Name, "changed", "previous", false, o.desc, desc, o.schema, string(schema), res.Enforce))
		default:
			res.Unapproved++
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
			_, wasApproved := approvals[n]
			res.Changes = append(res.Changes, ToolChange{Name: n, Kind: "removed", Approved: wasApproved})
			if _, err := tx.ExecContext(ctx, `UPDATE mcp_tools SET stale=1 WHERE server_id=? AND name=?`, id, n); err != nil {
				return err
			}
		}
	}
	res.ToolCount = len(seen)
	for i := range res.Changes {
		res.Changes[i].Name = textsafe.Display(res.Changes[i].Name, 120) // display only
	}
	sort.SliceStable(res.Changes, func(i, j int) bool { return res.Changes[i].Name < res.Changes[j].Name })
	return tx.Commit()
}

// toolChange builds the diff of a changed definition against a baseline
// (approved definition or previous cache). Names are sanitized for display.
func toolChange(name, kind, baseline string, approved bool, oldDesc, newDesc, oldSchema, newSchema string, hidden bool) ToolChange {
	c := ToolChange{Name: name, Kind: kind, Baseline: baseline, Approved: approved, Hidden: hidden}
	if oldDesc != newDesc {
		c.DescriptionChanged = true
		c.OldSpan, c.NewSpan = changedSpan(oldDesc, newDesc)
	}
	if sd := schemaDiff(oldSchema, newSchema); len(sd) > 0 {
		c.SchemaChanged, c.SchemaDiff = true, sd
	}
	return c
}

// minEncodedSecret: shorter secrets are masked only verbatim (their base64 or
// URL-escaped forms are short enough to collide with ordinary text).
const minEncodedSecret = 8

// SecretForms returns a resolved secret together with the forms in which a
// server commonly echoes it back: standard and URL-safe base64 (padded and raw)
// and URL escaping (query and path). It is a bounded set (at most 7 strings per
// secret). NOT covered: a secret embedded inside a larger base64/URL-encoded text
// (the encoding of the whole differs by alignment), hex, double encoding.
func SecretForms(v string) []string {
	out := []string{v}
	if len(v) < minEncodedSecret {
		return out
	}
	b := []byte(v)
	for _, f := range []string{
		base64.StdEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b),
		url.QueryEscape(v), url.PathEscape(v),
	} {
		if f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// addSecret records a resolved secret and its echo forms for masking.
func addSecret(secrets map[string]bool, v string) {
	for _, f := range SecretForms(v) {
		secrets[f] = true
	}
}

// maskText replaces every resolved secret value in s with "***" (longest first,
// so a secret that contains another one is replaced as a whole).
func maskText(s string, secrets map[string]bool) string {
	if len(secrets) == 0 {
		return s
	}
	list := make([]string, 0, len(secrets))
	for sec := range secrets {
		if sec != "" {
			list = append(list, sec)
		}
	}
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, sec := range list {
		s = strings.ReplaceAll(s, sec, "***")
	}
	return s
}

// maskAny masks every string (keys and values) of a decoded JSON value.
func maskAny(v any, secrets map[string]bool) any {
	switch x := v.(type) {
	case string:
		return maskText(x, secrets)
	case []any:
		for i := range x {
			x[i] = maskAny(x[i], secrets)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[maskText(k, secrets)] = maskAny(e, secrets)
		}
		return out
	}
	return v
}

// maskDiscovered masks the server-supplied strings (tool names, descriptions,
// input schemas, server info) with the resolved secrets. The tool hash is
// computed from the masked values, so changing a secret is not reported as a
// tool change.
func maskDiscovered(d *discovered, secrets map[string]bool) {
	if len(secrets) == 0 {
		return
	}
	d.proto, d.srvName, d.srvVer = maskText(d.proto, secrets), maskText(d.srvName, secrets), maskText(d.srvVer, secrets)
	for i, t := range d.tools {
		c := *t
		c.Name, c.Description = maskText(t.Name, secrets), maskText(t.Description, secrets)
		if t.InputSchema != nil {
			if raw, err := json.Marshal(t.InputSchema); err == nil {
				var v any
				if json.Unmarshal(raw, &v) == nil {
					c.InputSchema = maskAny(v, secrets)
				}
			}
		}
		d.tools[i] = &c
	}
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
			addSecret(secrets, s)
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
	// host is the normalized origin (scheme://host:port, see originOf) the headers belong
	// to. Requests to any other origin (a redirect to another host, port or scheme, e.g.
	// an https->http downgrade) get none of them: they carry secrets (Authorization)
	// that must not follow a redirect. "" = every request.
	host   string
	mu     sync.Mutex
	status int
}

func (t *diagTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if t.host == "" || originOf(req.URL) == t.host {
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
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
	tb, fail := newTransport(in, vals, MinimalEnv())
	if fail != nil {
		return nil, fail
	}
	transport, cmd, errBuf, dt, lim := tb.transport, tb.cmd, tb.errBuf, tb.dt, tb.lim

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
			sanitize(fmt.Sprintf("MCP 서버 %q 가 stdout 한 줄(JSON-RPC 메시지 하나, 예: tools/list 응답)에 상한 %d MiB 를 넘는 데이터를 보내 연결을 끊었습니다. 도구 설명·스키마를 줄이거나 서버를 나눠 등록하세요 (줄바꿈 없이 출력이 계속되는 서버도 같은 오류입니다)", textsafe.Display(in.Name, 60), maxStdoutLine>>20), secrets),
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
	var be *netsafe.BlockedError
	if errors.As(err, &be) {
		return mk(ClassBlockedAddress, fmt.Sprintf("연결이 차단되었습니다: 호스트 %s 가 허용되지 않는 주소(%s)로 해석되었습니다", textsafe.Display(be.Host, 80), be.IP))
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
	s = oneLine(maskText(s, secrets))
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
		return "주소/포트/네트워크/프록시를 확인하세요. 서버가 실행 중인지도 확인하세요. (stdio 서버에는 ://user:pw@ 자격 증명이 든 HTTPS_PROXY 같은 변수가 전달되지 않습니다: --env HTTPS_PROXY=${HTTPS_PROXY} 로 선언)"
	case ClassBlockedAddress:
		return "링크 로컬·클라우드 메타데이터 주소(169.254.x.x 등)는 항상 차단됩니다. 사내망(사설 주소)에 있는 서버라면 신뢰하는 경우에만 forge mcp allow-private <서버> 로 허용하세요. 로컬 개발 서버는 URL 에 localhost 또는 127.0.0.1 을 직접 쓰면 허용됩니다."
	case ClassEnvMissing:
		return "참조된 환경변수를 현재 셸에서 export 한 뒤 다시 시도하세요 (값은 Forge 가 저장하지 않습니다)."
	case ClassProcessExit:
		return "서버가 시작 직후 종료되었습니다. 위 stderr 내용(필수 환경변수/인자 누락 등)을 확인하세요. 서버에 필요한 환경변수는 --env KEY=${KEY} 로 선언하세요 (PATH/HOME/LANG/프록시 등 최소 변수 외의 부모 환경변수는 서버에 전달되지 않습니다). 값에 ://user:pw@ 자격 증명이 든 프록시/레지스트리 변수(HTTPS_PROXY 등)도 전달되지 않으므로 필요하면 --env HTTPS_PROXY=${HTTPS_PROXY} 로 선언하세요."
	}
	return ""
}
