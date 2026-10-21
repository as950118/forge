package ledger

import (
	"context"
	"database/sql"
)

// Minimal read helpers for tests and internal use. The user-facing
// `forge history` command is TASK-0009.

// Row is one ledger row.
type Row struct {
	ID            int64
	TS            int64 // UTC ms
	Kind, Name    string
	MCPServer     string
	Client        string
	ClientVersion string
	Project       string
	SessionID     string
	AgentID       string
	ToolUseID     string
	Source        string
	Invocation    string
	OK            *bool
	ErrorClass    string
	LatencyMS     *int64
	Found         *bool
	SkillVersion  string
	SkillID       *int64
	ToolID        *int64
	MCPServerID   *int64
}

// Filter narrows Recent. Zero values match everything.
type Filter struct {
	Kind, Name, Client, Source string
}

// Count is a per kind/name call count.
type Count struct {
	Kind, Name string
	Calls      int
	OK, Failed int // ok=1 / ok=0 (rows with unknown result are in neither)
}

// Miss mirrors v_misses.
type Miss struct {
	Kind, Name      string
	Count           int
	FirstTS, LastTS int64
}

const rowCols = `id, ts, kind, name, COALESCE(mcp_server,''), COALESCE(client,''), COALESCE(client_version,''),
COALESCE(project,''), COALESCE(session_id,''), COALESCE(agent_id,''), COALESCE(tool_use_id,''), source,
COALESCE(invocation,''), ok, COALESCE(error_class,''), latency_ms, found, COALESCE(skill_version,''), skill_id, tool_id, mcp_server_id`

func (r *SQLite) Recent(ctx context.Context, n int, f Filter) ([]Row, error) {
	if n <= 0 {
		n = 20
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT `+rowCols+` FROM ledger
WHERE (?1='' OR kind=?1) AND (?2='' OR name=?2) AND (?3='' OR client=?3) AND (?4='' OR source=?4)
ORDER BY ts DESC, id DESC LIMIT ?5`, f.Kind, f.Name, f.Client, f.Source, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var x Row
		var ok, found, lat, sid, tid, mid sql.NullInt64
		if err := rows.Scan(&x.ID, &x.TS, &x.Kind, &x.Name, &x.MCPServer, &x.Client, &x.ClientVersion, &x.Project,
			&x.SessionID, &x.AgentID, &x.ToolUseID, &x.Source, &x.Invocation, &ok, &x.ErrorClass, &lat, &found,
			&x.SkillVersion, &sid, &tid, &mid); err != nil {
			return nil, err
		}
		if ok.Valid {
			x.OK = Bool(ok.Int64 == 1)
		}
		if found.Valid {
			x.Found = Bool(found.Int64 == 1)
		}
		if lat.Valid {
			x.LatencyMS = Int64(lat.Int64)
		}
		for _, p := range []struct {
			n sql.NullInt64
			d **int64
		}{{sid, &x.SkillID}, {tid, &x.ToolID}, {mid, &x.MCPServerID}} {
			if p.n.Valid {
				*p.d = Int64(p.n.Int64)
			}
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Counts returns call counts per kind/name, most called first.
func (r *SQLite) Counts(ctx context.Context) ([]Count, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT kind, name, count(*), COALESCE(sum(ok=1),0), COALESCE(sum(ok=0),0)
FROM ledger GROUP BY kind, name ORDER BY count(*) DESC, kind, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Count
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Kind, &c.Name, &c.Calls, &c.OK, &c.Failed); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Misses lists requested-but-unregistered skills/tools (v_misses).
func (r *SQLite) Misses(ctx context.Context) ([]Miss, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT kind, name, miss_count, first_ts, last_ts FROM v_misses ORDER BY miss_count DESC, kind, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Miss
	for rows.Next() {
		var m Miss
		if err := rows.Scan(&m.Kind, &m.Name, &m.Count, &m.FirstTS, &m.LastTS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Payload returns the stored (masked) payload of a row; found=false when none.
func (r *SQLite) Payload(ctx context.Context, id int64) (in, out string, found bool, err error) {
	var i, o sql.NullString
	err = r.DB.QueryRowContext(ctx, `SELECT input_json, output_json FROM ledger_payloads WHERE ledger_id=?`, id).Scan(&i, &o)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return i.String, o.String, err == nil, err
}
