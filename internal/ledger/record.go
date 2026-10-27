package ledger

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

// SQLite is the Recorder backed by the Forge registry database. The *sql.DB
// should come from store.Open/OpenRaw (foreign_keys, WAL, busy_timeout and
// BEGIN IMMEDIATE transactions are configured there).
type SQLite struct {
	DB *sql.DB
}

var _ Recorder = (*SQLite)(nil)

// NormalizeProject returns the cleaned absolute (symlink-resolved when possible)
// path used in ledger.project; "" stays "".
func NormalizeProject(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Clean(p)
}

// PayloadEnabled reports settings.record_payload=1.
func (r *SQLite) PayloadEnabled(ctx context.Context) bool {
	return payloadEnabled(ctx, r.DB)
}

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func payloadEnabled(ctx context.Context, q queryer) bool {
	var v string
	if err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='record_payload'`).Scan(&v); err != nil {
		return false
	}
	return strings.TrimSpace(v) == "1"
}

func nstr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nbool(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}

func nint(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

func nid(id sql.NullInt64) any {
	if id.Valid {
		return id.Int64
	}
	return nil
}

// Record implements Recorder. One BEGIN IMMEDIATE transaction per call.
//
// Client handling: ledger.client is a FK to clients(id). Only registered
// clients are stored; an unregistered (but well-formed) client is recorded as
// NULL instead of failing or auto-registering (ADR-F0001: new clients are added
// by INSERT, not by whatever a hook claims). NULL-client rows are not deduplicated
// (the unique index only covers rows with a client).
//
// Dedup limits (intended, fixed by tests): the unique index needs client AND
// session_id AND tool_use_id. If any of them is absent (unregistered client stored
// as NULL, no session_id, no tool_use_id) a re-sent post is a new row, so
// duplicates are possible; clients are expected to be registered by INSERT first
// (ADR-F0001) and to send ids.
func (r *SQLite) Record(ctx context.Context, ev Event) (int64, error) {
	if err := ev.Validate(); err != nil {
		return 0, err
	}
	if ev.Kind == KindSkill {
		ev.Name = NormalizeSkillName(ev.Name)
		if ev.Name == "" {
			return 0, errors.New("name 이 비어 있습니다")
		}
	}
	ts := ev.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var clientKnown int
	if ev.Client != "" {
		_ = tx.QueryRowContext(ctx, `SELECT count(*) FROM clients WHERE id=?`, ev.Client).Scan(&clientKnown)
	}
	var client any
	if clientKnown > 0 {
		client = ev.Client
	}

	// A gateway row that carries the client's tool_use_id may already have been joined
	// with its hook row (session_id replaced by the hook's, see linkCall) between the
	// call's Start and End records. Keep addressing that row instead of inserting a
	// second one under the gateway's own "gw-" session.
	if clientKnown > 0 && ev.Source == SourceGateway && ev.ToolUseID != "" && strings.HasPrefix(ev.SessionID, GatewaySessionPrefix) {
		var joined string
		err := tx.QueryRowContext(ctx, `
SELECT session_id FROM ledger WHERE source='gateway' AND client=? AND tool_use_id=? AND session_id IS NOT NULL AND substr(session_id,1,3) != ?
ORDER BY id DESC LIMIT 1`, ev.Client, ev.ToolUseID, GatewaySessionPrefix).Scan(&joined)
		if err == nil {
			ev.SessionID = joined
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}

	var skillID, toolID, mcpID sql.NullInt64
	skillVersion := ev.SkillVersion
	var registered bool
	switch ev.Kind {
	case KindSkill:
		var ver sql.NullString
		err := tx.QueryRowContext(ctx, `
SELECT s.id, v.version FROM skills s LEFT JOIN skill_versions v ON v.id = s.current_version_id
WHERE s.name = ? ORDER BY (s.scope='global') DESC, s.id LIMIT 1`, ev.Name).Scan(&skillID, &ver)
		if err == nil {
			registered = true
			if skillVersion == "" && ver.Valid {
				skillVersion = ver.String
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	case KindTool:
		err := tx.QueryRowContext(ctx, `SELECT id FROM tools WHERE name = ? ORDER BY (scope='global') DESC, id LIMIT 1`, ev.Name).Scan(&toolID)
		if err == nil {
			registered = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if !registered && ev.Source == SourceHook {
			// A hook sees the gateway's exposed name (forge_<tool>, <server>__<tool>, the
			// meta tools), not the registry name; resolve it so a registered tool is not a miss.
			reg, tid, err := hookGatewayRegistered(ctx, tx, ev.Name)
			if err != nil {
				return 0, err
			}
			registered, toolID = reg, tid
		}
	case KindMCP:
		// Claude Code normalizes '-' in server names to '_' inside mcp__<server>__<tool>.
		err := tx.QueryRowContext(ctx, `
SELECT id FROM mcp_servers WHERE replace(name,'-','_') = replace(?,'-','_')
ORDER BY (name = ?) DESC, (scope='global') DESC, id LIMIT 1`, ev.MCPServer, ev.MCPServer).Scan(&mcpID)
		if err == nil {
			registered = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	found := ev.Found
	if found == nil {
		found = Bool(registered)
		// "plugin:skill" names are owned by the client's plugin system, not by the Forge
		// registry, so "not registered" is not a miss: found stays NULL (unknown) and the
		// row never reaches v_misses.
		if ev.Kind == KindSkill && !registered && strings.Contains(ev.Name, ":") {
			found = nil
		}
		// An MCP server that is not in the Forge registry was attached by the user
		// directly (e.g. `claude mcp add`); the call itself proves it exists, so this is
		// not a miss: found stays NULL (unknown).
		if ev.Kind == KindMCP && !registered {
			found = nil
		}
	}

	var id int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO ledger(ts, kind, name, mcp_server, client, client_version, project, session_id, agent_id, tool_use_id,
                   source, invocation, ok, error_class, latency_ms, found, skill_version, skill_id, tool_id, mcp_server_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(client, session_id, tool_use_id, source) WHERE tool_use_id IS NOT NULL AND client IS NOT NULL AND session_id IS NOT NULL
DO UPDATE SET
  ok            = COALESCE(excluded.ok, ledger.ok),
  error_class   = COALESCE(excluded.error_class, ledger.error_class),
  latency_ms    = COALESCE(excluded.latency_ms, ledger.latency_ms),
  found         = COALESCE(excluded.found, ledger.found),
  skill_version = COALESCE(excluded.skill_version, ledger.skill_version),
  skill_id      = COALESCE(excluded.skill_id, ledger.skill_id),
  tool_id       = COALESCE(excluded.tool_id, ledger.tool_id),
  mcp_server_id = COALESCE(excluded.mcp_server_id, ledger.mcp_server_id),
  agent_id      = COALESCE(excluded.agent_id, ledger.agent_id),
  client_version= COALESCE(excluded.client_version, ledger.client_version),
  project       = COALESCE(excluded.project, ledger.project),
  recorded_at   = strftime('%s','now') * 1000
RETURNING id`,
		ts.UnixMilli(), ev.Kind, ev.Name, nstr(ev.MCPServer), client, nstr(ev.ClientVersion), nstr(ev.Project),
		nstr(ev.SessionID), nstr(ev.AgentID), nstr(ev.ToolUseID), ev.Source, nstr(ev.Invocation), nbool(ev.OK),
		nstr(ev.ErrorClass), nint(ev.LatencyMS), nbool(found), nstr(skillVersion), nid(skillID), nid(toolID), nid(mcpID),
	).Scan(&id)
	if err != nil {
		return 0, err
	}

	if clientKnown > 0 && ev.SessionID != "" {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO sessions(client, session_id, project, client_version, started_at) VALUES (?,?,?,?,?)
ON CONFLICT(client, session_id) DO UPDATE SET
  project = COALESCE(sessions.project, excluded.project),
  client_version = COALESCE(excluded.client_version, sessions.client_version)`,
			ev.Client, ev.SessionID, nstr(ev.Project), nstr(ev.ClientVersion), ts.UnixMilli()); err != nil {
			return 0, err
		}
	}

	if clientKnown > 0 {
		if err := linkCall(ctx, tx, id, ev, ts.UnixMilli()); err != nil {
			return 0, err
		}
	}

	if (len(ev.Input) > 0 || len(ev.Output) > 0) && payloadEnabled(ctx, tx) {
		if err := writePayload(ctx, tx, id, ev.Input, ev.Output); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// writePayload is the single place that writes ledger_payloads; it always masks.
func writePayload(ctx context.Context, tx *sql.Tx, id int64, in, out []byte) error {
	mi, err := maskPayload(in)
	if err != nil {
		return err
	}
	mo, err := maskPayload(out)
	if err != nil {
		return err
	}
	if mi == "" && mo == "" {
		return nil
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO ledger_payloads(ledger_id, input_json, output_json, redaction_version) VALUES (?,?,?,?)
ON CONFLICT(ledger_id) DO UPDATE SET
  input_json  = COALESCE(excluded.input_json, ledger_payloads.input_json),
  output_json = COALESCE(excluded.output_json, ledger_payloads.output_json),
  redaction_version = excluded.redaction_version,
  recorded_at = strftime('%s','now') * 1000`, id, nstr(mi), nstr(mo), RedactionVersion)
	return err
}

// NormalizeSkillName lowercases, trims and strips a leading "/" from a skill name
// (Claude Code accepts "/code-review" and plugin-qualified "plugin:skill").
func NormalizeSkillName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "/")
	return strings.ToLower(s)
}
