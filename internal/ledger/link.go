package ledger

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Record-time linking of a hook row and a gateway row of the same call (TASK-0021 AC-1,
// ADR-F0003).
//
// The merge key of a call is (kind, name, mcp_server, client, session_id,
// tool_use_id). The gateway does not know the client's tool_use_id and its session_id
// is its own ("gw-..."), and it names the call differently from the hook (the hook
// sees "mcp__forge__forge_greet" = kind tool "forge_greet"; the gateway records kind
// tool "greet", or kind mcp for a proxied server tool). Without help the two rows are
// never merged and the call is counted twice.
//
// Whichever row arrives second looks for exactly one partner and, if it finds one,
// gives the gateway row the hook's session_id and tool_use_id and gives the hook row
// the gateway's kind, name and registry links (the gateway classifies better). After
// that the ordinary key merges the pair and queries need no special rule.
//
// Rules (all must hold; anything else leaves both rows as they are):
//   - same registered client, source hook vs gateway, no row of the pair already
//     linked to another row (1:1; a partner is never reused);
//   - hook row has a tool_use_id and a real session, gateway row has neither
//     (tool_use_id NULL, session_id "gw-...");
//   - the names correspond (hookIdentities) after the gateway's sanitizing;
//   - the timestamps differ by at most LinkWindow; the nearest candidate wins.
//
// Direct merge (TASK-0030, ADR-F0004): when the gateway row itself carries a
// tool_use_id (Claude Code sends it in tools/call params._meta), the two rows are
// joined by that id (same client, names correspond, 1:1) instead of the time window.
// A gateway row with an id is never window-linked; a gateway row without one still is.
//
// Not linked on purpose: meta tools (forge_skill/forge_list: the gateway records the
// served skill, not the meta tool), names the gateway shortened or suffixed with a
// hash, and direct (non-gateway) MCP servers.
const (
	// LinkWindow bounds the timestamp distance of a linked pair. Hook (PreToolUse)
	// and gateway Start are milliseconds apart; a post-only hook is as late as the
	// call ran, so longer calls without a pre hook stay unlinked.
	LinkWindow = 5 * time.Second

	// GatewaySessionPrefix starts the session_id the gateway generates for itself.
	GatewaySessionPrefix = "gw-"

	metaSkill = "forge_skill"
	metaList  = "forge_list"
)

type ident struct{ kind, server, tool string }

// hookIdentities maps the name a hook sees under the gateway's MCP server
// (kind=tool, name = the exposed tool name) to the (kind, server, tool) the gateway
// records. A name can be read two ways (custom "forge_<tool>" or proxied
// "<server>__<tool>"); both are tried.
func hookIdentities(name string) []ident {
	if name == metaSkill || name == metaList {
		return nil
	}
	var out []ident
	if strings.HasPrefix(name, "forge_") && len(name) > len("forge_") {
		out = append(out, ident{KindTool, "", name[len("forge_"):]})
	}
	if i := strings.Index(name, "__"); i > 0 && i+2 < len(name) {
		out = append(out, ident{KindMCP, normServer(name[:i]), name[i+2:]})
	}
	return out
}

func normServer(s string) string { return strings.ReplaceAll(s, "-", "_") }

// sanitizeToolName mirrors the gateway's exposed-name rule: every character outside
// [A-Za-z0-9_-] becomes '_'.
func sanitizeToolName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (i ident) matches(kind, server, name string) bool {
	return i.kind == kind && i.server == normServer(server) && i.tool == sanitizeToolName(name)
}

type linkTx interface {
	queryer
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// linkCall tries to link the row id just written for ev. Errors are returned to the
// caller (the transaction rolls back); "no partner" is not an error.
func linkCall(ctx context.Context, tx linkTx, id int64, ev Event, ts int64) error {
	win := LinkWindow.Milliseconds()
	switch {
	case ev.Source == SourceHook && ev.Kind == KindTool && ev.ToolUseID != "" && ev.SessionID != "" &&
		!strings.HasPrefix(ev.SessionID, GatewaySessionPrefix):
		ids := hookIdentities(ev.Name)
		if len(ids) == 0 {
			return nil
		}
		var linked int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ledger WHERE source='gateway' AND client=? AND session_id=? AND tool_use_id=?`,
			ev.Client, ev.SessionID, ev.ToolUseID).Scan(&linked); err != nil || linked > 0 {
			return err
		}
		// Direct merge: a gateway row that carries this very tool_use_id (the client sent
		// it in params._meta). It still has to be unlinked (gw- session) and the names
		// must correspond; no time window applies.
		if partner, err := directPartner(ctx, tx, `
SELECT id, kind, name, COALESCE(mcp_server,'') FROM ledger
WHERE source='gateway' AND tool_use_id=? AND client=? AND substr(session_id,1,3)=? AND kind IN ('tool','mcp')
ORDER BY abs(ts - ?), id`, func(kind, server, name string) bool {
			for _, h := range ids {
				if h.matches(kind, server, name) {
					return true
				}
			}
			return false
		}, ev.ToolUseID, ev.Client, GatewaySessionPrefix, ts); err != nil {
			return err
		} else if partner != 0 {
			return joinRows(ctx, tx, id, partner)
		}
		rows, err := tx.QueryContext(ctx, `
SELECT id, kind, name, COALESCE(mcp_server,'') FROM ledger
WHERE source='gateway' AND tool_use_id IS NULL AND client=? AND substr(session_id,1,3)=? AND kind IN ('tool','mcp')
  AND ts BETWEEN ? AND ? ORDER BY abs(ts - ?), id`, ev.Client, GatewaySessionPrefix, ts-win, ts+win, ts)
		if err != nil {
			return err
		}
		partner := int64(0)
		for rows.Next() {
			var gid int64
			var kind, name, server string
			if err := rows.Scan(&gid, &kind, &name, &server); err != nil {
				_ = rows.Close()
				return err
			}
			for _, h := range ids {
				if partner == 0 && h.matches(kind, server, name) {
					partner = gid
				}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		if partner == 0 {
			return nil
		}
		return joinRows(ctx, tx, id, partner)

	case ev.Source == SourceGateway && ev.ToolUseID != "" && (ev.Kind == KindTool || ev.Kind == KindMCP) &&
		strings.HasPrefix(ev.SessionID, GatewaySessionPrefix):
		// The gateway knows the client's tool_use_id (TASK-0030): merge with the hook row
		// of the same id, never by time window (a different id means a different call).
		var hid int64
		var name string
		rows, err := tx.QueryContext(ctx, `
SELECT h.id, h.name FROM ledger h
WHERE h.source='hook' AND h.kind='tool' AND h.tool_use_id=? AND h.client=? AND h.session_id IS NOT NULL
  AND substr(h.session_id,1,3) != ?
  AND NOT EXISTS (SELECT 1 FROM ledger g WHERE g.source='gateway' AND g.client=h.client AND g.session_id=h.session_id AND g.tool_use_id=h.tool_use_id)
ORDER BY abs(h.ts - ?), h.id`, ev.ToolUseID, ev.Client, GatewaySessionPrefix, ts)
		if err != nil {
			return err
		}
		partner := int64(0)
		for rows.Next() {
			if err := rows.Scan(&hid, &name); err != nil {
				_ = rows.Close()
				return err
			}
			for _, h := range hookIdentities(name) {
				if partner == 0 && h.matches(ev.Kind, ev.MCPServer, ev.Name) {
					partner = hid
				}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		if partner == 0 {
			return nil
		}
		return joinRows(ctx, tx, partner, id)

	case ev.Source == SourceGateway && ev.ToolUseID == "" && (ev.Kind == KindTool || ev.Kind == KindMCP) &&
		strings.HasPrefix(ev.SessionID, GatewaySessionPrefix):
		rows, err := tx.QueryContext(ctx, `
SELECT h.id, h.name FROM ledger h
WHERE h.source='hook' AND h.kind='tool' AND h.tool_use_id IS NOT NULL AND h.client=? AND h.session_id IS NOT NULL
  AND substr(h.session_id,1,3) != ? AND h.ts BETWEEN ? AND ?
  AND NOT EXISTS (SELECT 1 FROM ledger g WHERE g.source='gateway' AND g.client=h.client AND g.session_id=h.session_id AND g.tool_use_id=h.tool_use_id)
ORDER BY abs(h.ts - ?), h.id`, ev.Client, GatewaySessionPrefix, ts-win, ts+win, ts)
		if err != nil {
			return err
		}
		partner := int64(0)
		for rows.Next() {
			var hid int64
			var name string
			if err := rows.Scan(&hid, &name); err != nil {
				_ = rows.Close()
				return err
			}
			for _, h := range hookIdentities(name) {
				if partner == 0 && h.matches(ev.Kind, ev.MCPServer, ev.Name) {
					partner = hid
				}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		if partner == 0 {
			return nil
		}
		return joinRows(ctx, tx, partner, id)
	}
	return nil
}

// directPartner runs q (columns id, kind, name, mcp_server) and returns the first row
// accepted by ok, or 0.
func directPartner(ctx context.Context, tx linkTx, q string, ok func(kind, server, name string) bool, args ...any) (int64, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var kind, name, server string
		if err := rows.Scan(&id, &kind, &name, &server); err != nil {
			return 0, err
		}
		if ok(kind, server, name) {
			return id, nil
		}
	}
	return 0, rows.Err()
}

// joinRows makes the hook row and the gateway row one call: the gateway row takes the
// hook's session_id and tool_use_id, the hook row takes the gateway's identity.
func joinRows(ctx context.Context, tx linkTx, hookID, gwID int64) error {
	if _, err := tx.ExecContext(ctx, `
UPDATE ledger SET session_id=(SELECT session_id FROM ledger WHERE id=?), tool_use_id=(SELECT tool_use_id FROM ledger WHERE id=?)
WHERE id=?`, hookID, hookID, gwID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
UPDATE ledger SET (kind, name, mcp_server, found, skill_id, tool_id, mcp_server_id) =
  (SELECT kind, name, mcp_server, found, skill_id, tool_id, mcp_server_id FROM ledger WHERE id=?)
WHERE id=?`, gwID, hookID)
	return err
}

// hookGatewayRegistered reports whether a hook-observed tool name under the gateway's
// MCP server refers to something registered: a meta tool, a custom tool
// (forge_<registered name>), or a tool of a registered MCP server (<server>__<tool>,
// server compared with '-' = '_'). tool_id is set for custom tools only.
func hookGatewayRegistered(ctx context.Context, tx linkTx, name string) (bool, sql.NullInt64, error) {
	var tid sql.NullInt64
	if name == metaSkill || name == metaList {
		return true, tid, nil
	}
	for _, h := range hookIdentities(name) {
		switch h.kind {
		case KindTool:
			// The exposed name is sanitized; compare against the sanitized registry names.
			rows, err := tx.QueryContext(ctx, `SELECT id, name FROM tools ORDER BY (scope='global') DESC, id`)
			if err != nil {
				return false, tid, err
			}
			for rows.Next() {
				var id int64
				var n string
				if err := rows.Scan(&id, &n); err != nil {
					_ = rows.Close()
					return false, tid, err
				}
				if sanitizeToolName(n) == h.tool {
					_ = rows.Close()
					return true, sql.NullInt64{Int64: id, Valid: true}, nil
				}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return false, tid, err
			}
			_ = rows.Close()
		case KindMCP:
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mcp_servers WHERE replace(name,'-','_') = ?`, h.server).Scan(&n); err != nil {
				return false, tid, err
			}
			if n > 0 {
				return true, tid, nil
			}
		}
	}
	return false, tid, nil
}
