package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/as950118/forge/internal/ledger"
	"github.com/as950118/forge/internal/mcp"
	"github.com/as950118/forge/internal/tool"
)

// Error classes recorded in the ledger (error_class) for gateway-level failures.
const (
	classNotFound = "not_found"
	classDisabled = "disabled"
	// classHidden marks a call to a name that IS registered but is not exposed to this
	// client (profile allow list, per-client exposure setting). It is not a miss.
	classHidden      = "hidden_by_profile"
	classDenied      = "denied"
	classConfirm     = "confirm_refused"
	classNoElicit    = "confirm_unsupported"
	classInvalidArgs = "invalid_args"
	classUpstream    = "upstream_error"
	classConnection  = "connection"
	classTimeout     = "timeout"
	classInternal    = "internal"
)

// ---- result helpers ------------------------------------------------------

func textResult(s string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
}

func errResult(s string) *sdk.CallToolResult {
	r := textResult(s)
	r.IsError = true
	return r
}

// ---- ledger --------------------------------------------------------------

// call is the ledger bookkeeping of one invocation. Every method is best-effort:
// a recorder failure (or panic) is logged in debug mode and never changes the
// tool result.
type call struct {
	g    *Gateway
	rec  *ledger.GatewayRecorder
	gc   *ledger.GatewayCall
	in   json.RawMessage
	done bool
	// wait bounds each ledger write of this call (0 = defaultLedgerWait). Miss rows
	// use missWriteTimeout so a busy database never stalls the client's call.
	wait time.Duration
}

// defaultLedgerWait bounds a ledger write of a normal call; missWriteTimeout bounds
// the rows of a call that failed to resolve (AC-9: a miss row is never worth a
// multi-second wait on a locked database).
const (
	defaultLedgerWait = 5 * time.Second
	missWriteTimeout  = 250 * time.Millisecond
)

// callRecorder adds the payload (stored only when settings.record_payload=1,
// and then always masked by the ledger) to the event the GatewayRecorder builds.
type callRecorder struct {
	inner   ledger.Recorder
	in, out json.RawMessage
}

func (c *callRecorder) Record(ctx context.Context, ev ledger.Event) (int64, error) {
	ev.Input, ev.Output = c.in, c.out
	return c.inner.Record(ctx, ev)
}

func (g *Gateway) begin(client, version, kind string, name safeName, server, invocation string, args json.RawMessage) *call {
	return g.beginWithID(client, version, kind, name, server, invocation, "", args)
}

// beginWithID is begin with the client's own tool_use_id (already vetted by
// toolUseIDFrom; "" = none).
func (g *Gateway) beginWithID(client, version, kind string, name safeName, server, invocation, toolUseID string, args json.RawMessage) *call {
	if g.o.Recorder == nil {
		return &call{g: g, done: true}
	}
	c := &call{g: g, in: args}
	c.rec = &ledger.GatewayRecorder{
		Rec: g.o.Recorder, Client: client, ClientVersion: version,
		Project: ledger.NormalizeProject(g.o.Project), SessionID: g.o.SessionID,
	}
	c.gc = &ledger.GatewayCall{Kind: kind, Name: string(name), MCPServer: server, Invocation: invocation, ToolUseID: toolUseID}
	c.safe(func(ctx context.Context) error { return c.rec.Start(ctx, c.gc) })
	return c
}

func (c *call) safe(f func(ctx context.Context) error) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
			c.g.log.Debug("ledger panic", "recover", r)
		}
	}()
	wait := c.wait
	if wait <= 0 {
		wait = defaultLedgerWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if err := f(ctx); err != nil {
		c.g.log.Debug("ledger write failed", "error", err)
		return false
	}
	return true
}

// withPayload installs the payload decorator when the recorder reports payload capture on.
func (c *call) withPayload(output string) {
	pe, ok := c.g.o.Recorder.(interface{ PayloadEnabled(context.Context) bool })
	if !ok {
		return
	}
	var enabled bool
	c.safe(func(ctx context.Context) error { enabled = pe.PayloadEnabled(ctx); return nil })
	if !enabled {
		return
	}
	cr := &callRecorder{inner: c.g.o.Recorder}
	if len(c.in) > 0 && json.Valid(c.in) {
		cr.in = c.in
	}
	if output != "" {
		cr.out, _ = json.Marshal(map[string]string{"text": output})
	}
	c.rec.Rec = cr
}

// end records the outcome of a call that produced a tool.Result.
func (c *call) end(res *tool.Result, output string) {
	if c.done {
		return
	}
	c.done = true
	c.withPayload(output)
	c.safe(func(ctx context.Context) error { return c.rec.End(ctx, c.gc, res) })
}

// endClass records a failed call by class (no tool.Result), optionally with found.
// It reports whether a row was written.
func (c *call) endClass(class string, found *bool) bool {
	if c.done {
		return false
	}
	c.done = true
	c.withPayload("")
	return c.safe(func(ctx context.Context) error { return c.rec.EndError(ctx, c.gc, class, found) })
}

// ---- tools/call ----------------------------------------------------------

func (g *Gateway) callTool(ctx context.Context, ss *sdk.ServerSession, p *sdk.CallToolParamsRaw) (sdk.Result, error) {
	client, version := g.clientOf(ss)
	cat, err := g.build(ctx, client)
	if err != nil {
		return errResult("게이트웨이가 도구 목록을 만들지 못했습니다: " + err.Error()), nil
	}
	e := cat.byName[p.Name]
	if e == nil {
		if class := g.recordUnknown(ctx, client, version, p.Name); class != "" {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("등록된 도구이지만 이 클라이언트에는 노출되지 않습니다 (%s): \"%s\". 프로필·노출 설정(forge profile, forge mcp expose)을 확인하거나 forge_list 로 사용 가능한 도구를 확인하세요", hiddenReason(class), cleanName(p.Name))}
		}
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("알 수 없는 도구입니다 (노출되지 않았거나 비활성): \"%s\". forge_list 로 사용 가능한 도구를 확인하세요", cleanName(p.Name))}
	}
	c := g.beginFor(client, version, e, p.Arguments, toolUseIDFrom(client, p.Meta))
	var args map[string]any
	if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			if e.Kind == kindMeta && e.Tool == MetaSkill {
				c = g.begin(client, version, ledger.KindSkill, cleanName(MetaSkill), "", ledger.InvocationModel, nil)
			}
			c.endClass(classInvalidArgs, nil)
			return errResult("도구 인자는 JSON 객체여야 합니다"), nil
		}
	}
	g.log.Debug("tools/call", "tool", e.Name, "client", client)
	var res *sdk.CallToolResult
	switch e.Kind {
	case kindMeta:
		res = g.callMeta(ctx, e, args, client, version)
	case kindCustom:
		res = g.callCustom(ctx, ss, e, args, c)
	case kindProxy:
		res = g.callProxy(ctx, e, args, c)
	}
	c.endClass(classInternal, nil) // no-op when the call already recorded its outcome
	return res, nil
}

// metaToolUseID is the Claude Code private key under tools/call params._meta that
// carries the tool_use_id of the call (observed in a live Claude Code 2.1.311
// session, TASK-0029; single observation, see docs/clients/claude-code.md).
const metaToolUseID = "claudecode/toolUseId"

// maxToolUseIDLen bounds an accepted id. Claude Code ids look like "toolu_01..."
// (about 30 bytes); the ledger itself allows 256.
const maxToolUseIDLen = 128

// toolUseIDFrom returns the client's tool_use_id from params._meta, or "" when it
// must be ignored. Only for client == claude-code: the key is Claude Code's own name,
// so another client (or an unidentified one) sending it would give the id a meaning
// it does not have, and such rows have no Claude Code hook row to merge with anyway.
// A missing key, a non-string value, an empty or over-long value, or any character
// outside [A-Za-z0-9_-] (control, bidi, zero-width and every non-ASCII character
// included) is ignored without an error: the call is recorded without an id and the
// time-window linking of internal/ledger stays the fallback.
func toolUseIDFrom(client string, meta sdk.Meta) string {
	if client != ClientClaudeCode || meta == nil {
		return ""
	}
	v, ok := meta[metaToolUseID].(string)
	if !ok || v == "" || len(v) > maxToolUseIDLen {
		return ""
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return ""
		}
	}
	return v
}

// beginFor opens the ledger row for a catalog entry. Meta tools record their own rows.
func (g *Gateway) beginFor(client, version string, e *entry, raw json.RawMessage, toolUseID string) *call {
	switch e.Kind {
	case kindCustom:
		return g.beginWithID(client, version, ledger.KindTool, cleanName(e.Tool), "", "", toolUseID, raw)
	case kindProxy:
		return g.beginWithID(client, version, ledger.KindMCP, cleanName(e.Upstream), e.Server, "", toolUseID, raw)
	}
	return &call{g: g, done: true}
}

// Row-count policy of the gateway's miss rows (TASK-0020 AC-3, AC-9).
//
//   - Names that match nothing registered, and names with a registered server's
//     prefix whose tool that server does not have ("invented" names), are
//     deduplicated per session and bounded by maxUnknownRows per session.
//   - A call to a name that IS registered (custom tool, or a tool a registered
//     server is known to have) keeps one row per attempt, because ADR-F0002's
//     miss counts are attempt counts; a session is bounded by
//     maxRegisteredMissRows as a safety net against a looping client.
//   - Across sessions (restart loops start a new session every time) the first
//     invented-name row of a gateway process prunes old unattributed miss rows to
//     the newest globalMissKeep (ledger.SQLite.PruneGatewayMisses). Rows of
//     registered custom tools and hidden_by_profile rows are never pruned.
//
// Every miss write is bounded by missWriteTimeout and never runs under a lock.
const (
	maxUnknownRows        = 100
	maxRegisteredMissRows = 1000
	globalMissKeep        = 2000
	maxPruneAttempts      = 3
)

// pruner is implemented by the SQLite recorder (ledger.SQLite).
type pruner interface {
	PruneGatewayMisses(ctx context.Context, keep int) (int64, error)
}

// reserveInvented claims one of the per-session slots for an invented name. A
// duplicate or a full session gets false. The slot is claimed before the write and
// released again (releaseInvented) when the write fails, so only rows that were
// really written use up a slot while the lock is never held across the write.
func (g *Gateway) reserveInvented(key safeName) bool {
	g.unkMu.Lock()
	defer g.unkMu.Unlock()
	if _, dup := g.unknown[key]; dup {
		g.log.Debug("unknown tool name not recorded (duplicate)", "name", string(key))
		return false
	}
	if len(g.unknown) >= maxUnknownRows {
		g.log.Debug("unknown tool name not recorded (session limit)", "name", string(key))
		return false
	}
	if g.unknown == nil {
		g.unknown = map[safeName]struct{}{}
	}
	g.unknown[key] = struct{}{}
	return true
}

func (g *Gateway) releaseInvented(key safeName) {
	g.unkMu.Lock()
	delete(g.unknown, key)
	g.unkMu.Unlock()
}

func (g *Gateway) reserveRegistered() bool {
	g.unkMu.Lock()
	defer g.unkMu.Unlock()
	if g.regMisses >= maxRegisteredMissRows {
		g.log.Debug("registered miss not recorded (session limit)")
		return false
	}
	g.regMisses++
	return true
}

func (g *Gateway) releaseRegistered() {
	g.unkMu.Lock()
	g.regMisses--
	g.unkMu.Unlock()
}

// missCall opens the (bounded-wait) ledger bookkeeping of a miss row.
func (g *Gateway) missCall(client, version, kind string, name safeName, server string) *call {
	c := g.begin(client, version, kind, name, server, "", nil)
	c.wait = missWriteTimeout
	return c
}

// recordRegistered writes one per-attempt row of a registered name's miss.
func (g *Gateway) recordRegistered(client, version, kind string, name safeName, server, class string, found *bool) {
	if !g.reserveRegistered() {
		return
	}
	if !g.missCall(client, version, kind, name, server).endClass(class, found) {
		g.releaseRegistered()
	}
}

// recordInvented writes the once-per-session row of an invented name and, after the
// first successful write of this process, prunes old rows across sessions.
func (g *Gateway) recordInvented(client, version, kind string, name safeName, server string) {
	key := name
	if server != "" {
		key = safeName(server + "\x00" + string(name))
	}
	if !g.reserveInvented(key) {
		return
	}
	if !g.missCall(client, version, kind, name, server).endClass(classNotFound, ledger.Bool(false)) {
		g.releaseInvented(key)
		return
	}
	g.pruneOld()
}

// pruneOld applies the global retention once per process (at most
// maxPruneAttempts tries when the database is busy).
func (g *Gateway) pruneOld() {
	p, ok := g.o.Recorder.(pruner)
	if !ok {
		return
	}
	g.unkMu.Lock()
	if g.pruned || g.pruneTries >= maxPruneAttempts {
		g.unkMu.Unlock()
		return
	}
	g.pruneTries++
	g.unkMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), missWriteTimeout)
	defer cancel()
	n, err := p.PruneGatewayMisses(ctx, globalMissKeep)
	if err != nil {
		g.log.Debug("ledger prune failed", "error", err)
		return
	}
	g.unkMu.Lock()
	g.pruned = true
	g.unkMu.Unlock()
	if n > 0 {
		g.log.Debug("old unattributed miss rows pruned", "rows", n)
	}
}

// recordUnknown logs a call to a name the gateway does not expose. A name that
// matches something registered (forge_<registered tool>, <normalized registered
// server>__<tool>) is recorded as a detailed miss for that tool/server. Any other
// forge_*/<x>__* name (including <x>__<tool> whose <x> is not a registered
// server) is recorded as kind=tool with no server attribution, once per session
// (deduplicated by sanitized name, at most maxUnknownRows rows per session; only
// rows that were really written use up a slot); a name of neither shape is not
// recorded. The name is sanitized exactly once, by cleanName. See the policy
// comment above for the row-count rules.
func (g *Gateway) recordUnknown(ctx context.Context, client, version, name string) (hiddenClass string) {
	if d, ok := g.registeredTool(ctx, name); ok {
		// Registered, enabled and allowed by policy but not visible to this client under
		// its profile: the name exists, so it is not a miss (found=1, hidden_by_profile).
		// Disabled and confirm=deny tools keep the earlier not_found/found=0 record.
		class, found := classNotFound, ledger.Bool(false)
		if d.Enabled && d.Policy.Confirm != tool.ConfirmDeny && !g.exposure(ctx).ToolVisible(d, client) {
			class, found = classHidden, ledger.Bool(true)
		}
		g.recordRegistered(client, version, ledger.KindTool, cleanName(d.Name), "", class, found)
		if *found {
			return class
		}
		return ""
	}
	if s, tl, ok := g.registeredServerTool(ctx, name); ok {
		class, found := classNotFound, ledger.Bool(false)
		known := g.serverHasTool(ctx, s.Name, string(tl))
		if s.Enabled && g.exposure(ctx).ServerMode(s, client) != mcp.ExposureGateway {
			// The (enabled) server is registered but not proxied for this client. Only a tool
			// the server is known to have counts as found (a typo on a hidden server stays a miss).
			if known {
				class, found = classHidden, ledger.Bool(true)
			}
		}
		if !known {
			// A name the server does not have: an invented name, deduplicated and bounded
			// per session (the registered server prefix would otherwise let one client
			// write unlimited distinct rows).
			g.recordInvented(client, version, ledger.KindMCP, tl, s.Name)
		} else {
			g.recordRegistered(client, version, ledger.KindMCP, tl, s.Name, class, found)
		}
		if *found {
			return class
		}
		return ""
	}
	switch {
	case strings.HasPrefix(name, CustomPrefix) && len(name) > len(CustomPrefix):
	case strings.Contains(name, ProxySep) && !strings.HasPrefix(name, ProxySep) && !strings.HasSuffix(name, ProxySep):
	default:
		return ""
	}
	g.recordInvented(client, version, ledger.KindTool, cleanName(name), "")
	return ""
}

// hiddenReason is the wording for a registered-but-not-exposed call.
func hiddenReason(string) string { return "프로필/노출 설정에서 숨김" }

// serverHasTool reports whether the registry knows a (non-stale) tool of the server
// whose exposed (sanitized) name is name.
func (g *Gateway) serverHasTool(ctx context.Context, server, name string) bool {
	full, err := g.o.Servers.Get(ctx, server)
	if err != nil {
		return false
	}
	for _, t := range full.Tools {
		if !t.Stale && (t.Name == name || sanitizeTool(t.Name) == name) {
			return true
		}
	}
	return false
}

// registeredTool matches forge_<name> against registered custom tools (enabled or not).
func (g *Gateway) registeredTool(ctx context.Context, name string) (*tool.Definition, bool) {
	if !strings.HasPrefix(name, CustomPrefix) || len(name) == len(CustomPrefix) {
		return nil, false
	}
	defs, err := g.o.Tools.List(ctx)
	if err != nil {
		return nil, false
	}
	for i := range defs {
		if name == CustomPrefix+defs[i].Name || name == customBase(defs[i].Name) {
			return &defs[i], true
		}
	}
	return nil, false
}

// registeredServerTool matches <server>__<tool> against the normalized names of
// registered servers (longest prefix wins) and returns the registered name.
func (g *Gateway) registeredServerTool(ctx context.Context, name string) (server *mcp.Info, tl safeName, ok bool) {
	servers, err := g.o.Servers.List(ctx)
	if err != nil {
		return nil, "", false
	}
	best := -1
	for i := range servers {
		p := NormalizeServer(servers[i].Name) + ProxySep
		if strings.HasPrefix(name, p) && len(name) > len(p) && len(p) > best {
			best, server, tl = len(p), &servers[i], cleanName(name[len(p):])
		}
	}
	return server, tl, best >= 0
}

// ---- custom tools --------------------------------------------------------

func (g *Gateway) callCustom(ctx context.Context, ss *sdk.ServerSession, e *entry, args map[string]any, c *call) *sdk.CallToolResult {
	def, err := g.o.Tools.Resolve(ctx, e.Tool)
	if err != nil {
		c.endClass(classInternal, nil)
		if errors.Is(err, tool.ErrNotFound) {
			return errResult("도구가 더 이상 등록되어 있지 않습니다: " + e.Tool)
		}
		return errResult("도구 정의를 불러올 수 없습니다: " + err.Error())
	}
	if !def.Enabled || def.Policy.Confirm == tool.ConfirmDeny {
		c.endClass(classDenied, nil)
		return errResult("이 도구는 정책상 실행할 수 없습니다 (비활성 또는 confirm=deny)")
	}
	if effectiveConfirm(def) == tool.ConfirmAsk {
		if refusal, class := g.confirm(ctx, ss, def, args); refusal != "" {
			c.endClass(class, nil)
			return errResult(refusal)
		}
	}
	res, _ := tool.Run(ctx, def, args, tool.RunOptions{ProjectDir: g.o.Project})
	out := formatToolResult(res)
	c.end(res, out)
	if res.OK {
		return textResult(out)
	}
	return errResult(out)
}

func formatToolResult(r *tool.Result) string {
	var b strings.Builder
	if r.OK {
		b.WriteString(r.Stdout)
		if r.StdoutTruncated {
			b.WriteString("\n[출력이 정책 상한으로 잘렸습니다]")
		}
		if s := strings.TrimSpace(r.Stderr); s != "" {
			b.WriteString("\n[stderr]\n" + s)
		}
		if b.Len() == 0 {
			return "(출력 없음)"
		}
		return b.String()
	}
	b.WriteString(r.Error)
	if r.Error == "" {
		b.WriteString("도구 실행에 실패했습니다")
	}
	if r.ExitCode > 0 {
		fmt.Fprintf(&b, " (종료 코드 %d)", r.ExitCode)
	}
	if s := strings.TrimSpace(r.Stderr); s != "" {
		b.WriteString("\n[stderr]\n" + tailBytes(s, 2048))
	}
	if s := strings.TrimSpace(r.Stdout); s != "" {
		b.WriteString("\n[stdout]\n" + tailBytes(s, 2048))
	}
	return b.String()
}

func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for !utf8.ValidString(s) {
		s = s[1:]
	}
	return "…" + s
}

// ---- proxy ---------------------------------------------------------------

func (g *Gateway) callProxy(ctx context.Context, e *entry, args map[string]any, c *call) *sdk.CallToolResult {
	// Minimal argument check against the cached schema; the upstream server
	// remains the authority. A schema we cannot compile is not enforced here.
	if len(e.Raw) > 0 {
		if _, err := tool.ValidateArgs(&tool.Definition{InputSchema: e.Raw}, args); err != nil {
			var ae *tool.ArgsError
			if errors.As(err, &ae) {
				c.endClass(classInvalidArgs, nil)
				return errResult(ae.Summary)
			}
		}
	}
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	case <-ctx.Done():
		c.endClass(classTimeout, nil)
		return errResult("동시 호출이 많아 대기 중 취소되었습니다")
	}
	start := time.Now()
	conn, derr := g.pool.get(ctx, e.Server)
	if derr != nil {
		c.endClass(orConnection(derr.Class), nil)
		msg := fmt.Sprintf("MCP 서버 %q 에 연결할 수 없습니다: %s", e.Server, derr.Message)
		if derr.Hint != "" {
			msg += " (" + derr.Hint + ")"
		}
		return errResult(msg)
	}
	callCtx, cancel := context.WithTimeout(ctx, g.timeoutFor(conn))
	defer cancel()
	res, err := conn.Session.CallTool(callCtx, &sdk.CallToolParams{Name: e.Upstream, Arguments: args})
	if err != nil {
		class, msg := g.classifyCallError(callCtx, conn, e, err)
		if class != classUpstream { // timed out / dead: do not reuse a possibly wedged session
			g.pool.drop(e.Server, conn)
		}
		c.endClass(class, nil)
		return errResult(msg)
	}
	out := g.convertResult(conn, res)
	tr := &tool.Result{OK: !out.IsError, DurationMS: time.Since(start).Milliseconds()}
	if out.IsError {
		tr.ErrorClass = classUpstream
	}
	c.end(tr, resultText(out))
	return out
}

func orConnection(s string) string {
	if s == "" {
		return classConnection
	}
	return s
}

func (g *Gateway) timeoutFor(conn *mcp.Conn) time.Duration {
	if conn.Info != nil && conn.Info.TimeoutMS > 0 {
		return time.Duration(conn.Info.TimeoutMS) * time.Millisecond
	}
	return g.o.CallTimeout
}

func (g *Gateway) classifyCallError(callCtx context.Context, conn *mcp.Conn, e *entry, err error) (string, string) {
	switch {
	case errors.Is(callCtx.Err(), context.DeadlineExceeded):
		return classTimeout, fmt.Sprintf("MCP 서버 %q 의 도구 %q 가 %s 안에 응답하지 않아 연결을 끊었습니다", e.Server, e.Upstream, g.timeoutFor(conn))
	case errors.Is(callCtx.Err(), context.Canceled):
		return classTimeout, "호출이 취소되었습니다"
	case errors.Is(err, sdk.ErrConnectionClosed) || !aliveSoon(conn):
		msg := fmt.Sprintf("MCP 서버 %q 와의 연결이 끊어졌습니다 (서버가 종료되었을 수 있습니다). 다음 호출 때 다시 연결합니다", e.Server)
		if s := conn.Stderr(); s != "" {
			msg += " | stderr: " + s
		}
		return classConnection, msg
	}
	return classUpstream, fmt.Sprintf("MCP 서버 %q 가 오류를 반환했습니다: %s", e.Server, conn.Sanitize(err.Error()))
}

// convertResult masks the server's resolved secrets and applies the response
// size cap (text in order, images/audio by encoded size, structured content only
// when small, other content types are not forwarded).
func (g *Gateway) convertResult(conn *mcp.Conn, res *sdk.CallToolResult) *sdk.CallToolResult {
	out := &sdk.CallToolResult{IsError: res.IsError, Content: []sdk.Content{}}
	budget := g.o.MaxResponse
	truncated := false
	for _, c := range res.Content {
		if budget <= 0 {
			truncated = true
			break
		}
		switch v := c.(type) {
		case *sdk.TextContent:
			t := conn.Mask(v.Text)
			if len(t) > budget {
				t = cutBytes(t, budget)
				truncated = true
			}
			budget -= len(t)
			out.Content = append(out.Content, &sdk.TextContent{Text: t})
		case *sdk.ImageContent:
			if len(v.Data) > budget {
				truncated = true
				continue
			}
			budget -= len(v.Data)
			out.Content = append(out.Content, v)
		case *sdk.AudioContent:
			if len(v.Data) > budget {
				truncated = true
				continue
			}
			budget -= len(v.Data)
			out.Content = append(out.Content, v)
		default:
			truncated = true
		}
	}
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		var v any
		if err == nil && len(b) <= budget && json.Unmarshal([]byte(conn.Mask(string(b))), &v) == nil {
			out.StructuredContent = v
		} else {
			truncated = true
		}
	}
	if truncated {
		out.Content = append(out.Content, &sdk.TextContent{Text: fmt.Sprintf("\n[응답이 상한(%d바이트)을 넘거나 지원하지 않는 형식이라 일부만 전달했습니다]", g.o.MaxResponse)})
	}
	return out
}

func cutBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func resultText(r *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// aliveSoon reports whether the connection is still open, giving a dying
// process a moment to be noticed (the call error often arrives first).
func aliveSoon(conn *mcp.Conn) bool {
	select {
	case <-conn.Done():
		return false
	case <-time.After(300 * time.Millisecond):
		return true
	}
}
