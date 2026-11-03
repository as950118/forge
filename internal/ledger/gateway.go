package ledger

import (
	"context"
	"time"

	"github.com/as950118/forge/internal/tool"
)

// GatewayRecorder is what the gateway (TASK-0007) calls around each invocation.
// Wiring it into the gateway is TASK-0007 AC-4; this type only maps gateway
// events and tool.Result to Events with source=gateway.
//
// Rows are keyed by ToolUseID only when the caller has one (e.g. an id the client
// passed along). Without one no id is invented: Start records nothing and End
// writes a single row.
type GatewayRecorder struct {
	Rec           Recorder
	Client        string
	ClientVersion string
	Project       string
	SessionID     string
	Now           func() time.Time // test hook; default time.Now
}

// GatewayCall identifies one gateway invocation.
type GatewayCall struct {
	Kind       string // skill|tool|mcp
	Name       string
	MCPServer  string
	ToolUseID  string
	Invocation string // skill only
	AgentID    string

	started time.Time
}

func (g *GatewayRecorder) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *GatewayRecorder) base(c *GatewayCall) Event {
	return Event{
		Kind: c.Kind, Name: c.Name, MCPServer: c.MCPServer, Client: g.Client, ClientVersion: g.ClientVersion,
		Project: g.Project, SessionID: g.SessionID, AgentID: c.AgentID, ToolUseID: c.ToolUseID,
		Source: SourceGateway, Invocation: c.Invocation, TS: c.started,
	}
}

// Start marks the beginning of a call. With a ToolUseID it records a pending row
// (ok=NULL) that End completes; otherwise it only remembers the start time.
func (g *GatewayRecorder) Start(ctx context.Context, c *GatewayCall) error {
	c.started = g.now()
	if c.ToolUseID == "" {
		return nil
	}
	_, err := g.Rec.Record(ctx, g.base(c))
	return err
}

// End records the outcome. latency is measured from Start unless the result
// carries its own duration.
func (g *GatewayRecorder) End(ctx context.Context, c *GatewayCall, res *tool.Result) error {
	if c.started.IsZero() {
		c.started = g.now()
	}
	ev := g.base(c)
	if res != nil {
		ApplyToolResult(&ev, res)
	}
	if ev.LatencyMS == nil {
		ev.LatencyMS = Int64(g.now().Sub(c.started).Milliseconds())
	}
	_, err := g.Rec.Record(ctx, ev)
	return err
}

// EndError records a failed call that produced no tool.Result (e.g. unknown name).
func (g *GatewayRecorder) EndError(ctx context.Context, c *GatewayCall, errorClass string, found *bool) error {
	ev := g.base(c)
	ev.OK = Bool(false)
	ev.ErrorClass = sanitizeClass(errorClass)
	ev.Found = found
	ev.LatencyMS = Int64(g.now().Sub(g.startOr(c)).Milliseconds())
	_, err := g.Rec.Record(ctx, ev)
	return err
}

func (g *GatewayRecorder) startOr(c *GatewayCall) time.Time {
	if c.started.IsZero() {
		return g.now()
	}
	return c.started
}

// ApplyToolResult copies the outcome of a tool.Result into ev: ok, latency and a
// classification (never the stdout/stderr/error text). Stdout/Stderr are not
// forwarded as Output; payload capture for the gateway is the caller's choice.
func ApplyToolResult(ev *Event, res *tool.Result) {
	ev.OK = Bool(res.OK)
	if res.DurationMS > 0 || res.OK {
		ev.LatencyMS = Int64(max(res.DurationMS, 0))
	}
	if !res.OK {
		ev.ErrorClass = ClassifyToolResult(res)
	}
}

// ClassifyToolResult derives a short error class: the tool's own ErrorClass
// when it is a valid code, else timeout / http_error / exec / error.
func ClassifyToolResult(res *tool.Result) string {
	if res.OK {
		return ""
	}
	if errorClassRe.MatchString(res.ErrorClass) {
		return res.ErrorClass
	}
	switch {
	case res.TimedOut:
		return "timeout"
	case res.HTTPStatus >= 400:
		return "http_error"
	case res.ExitCode != 0:
		return "exec"
	}
	return "error"
}

func sanitizeClass(s string) string {
	if errorClassRe.MatchString(s) {
		return s
	}
	return "other"
}

// PruneGatewayMisses is the cross-session retention of the gateway's unattributed
// miss rows (TASK-0020 AC-3): a gateway that restarts in a loop starts a new
// session (and a new 100-row budget) every time, so the rows would otherwise grow
// without limit. It deletes all but the newest keep rows of
//
//	source='gateway', found=0, error_class='not_found', session 'gw-...' and
//	(kind='tool' with no registered tool, or kind='mcp')
//
// i.e. names that matched nothing registered and misses of registered servers.
// Rows of registered custom tools (tool_id set), hidden_by_profile rows (found=1),
// hook rows and every row with a real session are never touched. It returns the
// number of rows deleted.
func (r *SQLite) PruneGatewayMisses(ctx context.Context, keep int) (int64, error) {
	if keep < 0 {
		keep = 0
	}
	res, err := r.DB.ExecContext(ctx, `
DELETE FROM ledger WHERE id IN (
  SELECT id FROM ledger
  WHERE source='gateway' AND found=0 AND error_class='not_found' AND substr(session_id,1,3)=?
    AND ((kind='tool' AND tool_id IS NULL) OR kind='mcp')
  ORDER BY id DESC LIMIT -1 OFFSET ?)`, GatewaySessionPrefix, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
