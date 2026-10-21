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

	"github.com/heonjinjeong/forge/internal/ledger"
	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/tool"
)

// Error classes recorded in the ledger (error_class) for gateway-level failures.
const (
	classNotFound    = "not_found"
	classDisabled    = "disabled"
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
}

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
	if g.o.Recorder == nil {
		return &call{g: g, done: true}
	}
	c := &call{g: g, in: args}
	c.rec = &ledger.GatewayRecorder{
		Rec: g.o.Recorder, Client: client, ClientVersion: version,
		Project: ledger.NormalizeProject(g.o.Project), SessionID: g.o.SessionID,
	}
	c.gc = &ledger.GatewayCall{Kind: kind, Name: string(name), MCPServer: server, Invocation: invocation}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
		g.recordUnknown(ctx, client, version, p.Name)
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("알 수 없는 도구입니다 (노출되지 않았거나 비활성): \"%s\". forge_list 로 사용 가능한 도구를 확인하세요", cleanName(p.Name))}
	}
	c := g.beginFor(client, version, e, p.Arguments)
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

// beginFor opens the ledger row for a catalog entry. Meta tools record their own rows.
func (g *Gateway) beginFor(client, version string, e *entry, raw json.RawMessage) *call {
	switch e.Kind {
	case kindCustom:
		return g.begin(client, version, ledger.KindTool, cleanName(e.Tool), "", "", raw)
	case kindProxy:
		return g.begin(client, version, ledger.KindMCP, cleanName(e.Upstream), e.Server, "", raw)
	}
	return &call{g: g, done: true}
}

// maxUnknownRows bounds the ledger rows a gateway session spends on names that
// match nothing registered (a client or model that invents tool names must not
// grow the ledger without limit). Further names are only debug-logged.
const maxUnknownRows = 100

// recordUnknown logs a call to a name the gateway does not expose. A name that
// matches something registered (forge_<registered tool>, <normalized registered
// server>__<tool>) is recorded as a detailed miss for that tool/server. Any other
// forge_*/<x>__* name (including <x>__<tool> whose <x> is not a registered
// server) is recorded as kind=tool with no server attribution, once per session
// (deduplicated by sanitized name, at most maxUnknownRows rows per session; only
// rows that were really written use up a slot); a name of neither shape is not
// recorded. The name is sanitized exactly once, by cleanName.
func (g *Gateway) recordUnknown(ctx context.Context, client, version, name string) {
	if tl, ok := g.registeredTool(ctx, name); ok {
		g.begin(client, version, ledger.KindTool, cleanName(tl), "", "", nil).endClass(classNotFound, ledger.Bool(false))
		return
	}
	if srv, tl, ok := g.registeredServerTool(ctx, name); ok {
		g.begin(client, version, ledger.KindMCP, tl, srv, "", nil).endClass(classNotFound, ledger.Bool(false))
		return
	}
	switch {
	case strings.HasPrefix(name, CustomPrefix) && len(name) > len(CustomPrefix):
	case strings.Contains(name, ProxySep) && !strings.HasPrefix(name, ProxySep) && !strings.HasSuffix(name, ProxySep):
	default:
		return
	}
	clean := cleanName(name)
	// The lock is held across the write so the count only ever includes rows that
	// were written and concurrent calls cannot overshoot the limit.
	g.unkMu.Lock()
	defer g.unkMu.Unlock()
	if _, dup := g.unknown[clean]; dup {
		g.log.Debug("unknown tool name not recorded (duplicate)", "name", clean)
		return
	}
	if len(g.unknown) >= maxUnknownRows {
		g.log.Debug("unknown tool name not recorded (session limit)", "name", clean)
		return
	}
	c := g.begin(client, version, ledger.KindTool, clean, "", "", nil)
	if c.endClass(classNotFound, ledger.Bool(false)) {
		if g.unknown == nil {
			g.unknown = map[safeName]struct{}{}
		}
		g.unknown[clean] = struct{}{}
	}
}

// registeredTool matches forge_<name> against registered custom tools (enabled or not).
func (g *Gateway) registeredTool(ctx context.Context, name string) (string, bool) {
	if !strings.HasPrefix(name, CustomPrefix) || len(name) == len(CustomPrefix) {
		return "", false
	}
	defs, err := g.o.Tools.List(ctx)
	if err != nil {
		return "", false
	}
	for _, d := range defs {
		if name == CustomPrefix+d.Name || name == customBase(d.Name) {
			return d.Name, true
		}
	}
	return "", false
}

// registeredServerTool matches <server>__<tool> against the normalized names of
// registered servers (longest prefix wins) and returns the registered name.
func (g *Gateway) registeredServerTool(ctx context.Context, name string) (server string, tl safeName, ok bool) {
	servers, err := g.o.Servers.List(ctx)
	if err != nil {
		return "", "", false
	}
	best := -1
	for _, s := range servers {
		p := NormalizeServer(s.Name) + ProxySep
		if strings.HasPrefix(name, p) && len(name) > len(p) && len(p) > best {
			best, server, tl = len(p), s.Name, cleanName(name[len(p):])
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
