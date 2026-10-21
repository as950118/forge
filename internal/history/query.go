package history

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Limits for --limit.
const (
	DefaultLimit = 50
	MaxLimit     = 10000
)

// Kinds and sources accepted by the filters.
var (
	Kinds   = []string{"skill", "tool", "mcp"}
	Sources = []string{"hook", "gateway", "transcript"}
)

// Filter narrows the ledger. Zero values match everything. Every value reaches
// SQL only as a bound parameter.
type Filter struct {
	Kind    string
	Client  string
	Session string
	Project string // exact, as stored (callers normalize paths)
	Server  string // MCP server name; '-' and '_' are the same
	Source  string

	Name         string
	NameContains bool // false: exact match, true: case-insensitive (ASCII) substring

	Since, Until *int64 // UTC ms; Since inclusive, Until exclusive
	OK           *bool  // true: ok=1, false: ok=0 (unknown results match neither)

	Raw bool // list only: skip the tool_use_id merge (one row per ledger row)
}

// Call is one call (merged across sources unless Filter.Raw).
type Call struct {
	ID         int64    `json:"id"`
	Time       string   `json:"time"`  // UTC RFC3339
	TS         int64    `json:"ts_ms"` // UTC epoch ms
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	MCPServer  string   `json:"mcp_server"`
	Client     string   `json:"client"`
	Project    string   `json:"project"`
	SessionID  string   `json:"session_id"`
	Sources    []string `json:"sources"`
	OK         *bool    `json:"ok"`
	ErrorClass string   `json:"error_class"`
	LatencyMS  *int64   `json:"latency_ms"`
	Found      *bool    `json:"found"`
	Invocation string   `json:"invocation"`
	ToolUseID  string   `json:"tool_use_id,omitempty"` // raw mode only
}

type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

func (w *where) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// normServer is the server-name rule used everywhere: Claude Code writes '-' as
// '_' inside mcp__<server>__<tool>, so the two spellings are one server.
func normServer(s string) string { return strings.ReplaceAll(s, "-", "_") }

// mergeSlack bounds how far apart two sources' timestamps of one call may be
// when the time window is applied before merging (see rawWhere). A hook and the
// gateway record the same call within milliseconds; an hour is far beyond that.
const mergeSlack = int64(time.Hour / time.Millisecond)

// rawWhere holds the conditions that are exact on single ledger rows and on the
// merge key (kind, name, mcp_server, client, session_id), so they may be applied
// before merging; they use the ledger indexes. The time window is applied here
// too, widened by mergeSlack, so a recent window does not scan the whole ledger
// (the exact test on the merged call's first timestamp is in outerWhere). In raw
// mode the window is exact.
func (f Filter) rawWhere() where {
	var w where
	slack := mergeSlack
	if f.Raw {
		slack = 0
	}
	if f.Since != nil {
		w.add("ts >= ?", *f.Since-slack)
	}
	if f.Until != nil {
		w.add("ts < ?", *f.Until+slack)
	}
	if f.Kind != "" {
		w.add("kind = ?", f.Kind)
	}
	if f.Client != "" {
		w.add("client = ?", f.Client)
	}
	if f.Session != "" {
		w.add("session_id = ?", f.Session)
	}
	if f.Server != "" {
		w.add("replace(mcp_server,'-','_') = ?", normServer(f.Server))
	}
	if f.Name != "" {
		if f.NameContains {
			w.add("instr(lower(name), lower(?)) > 0", f.Name)
		} else {
			w.add("name = ?", f.Name)
		}
	}
	return w
}

// outerWhere holds the conditions on the (merged) call: time, project, source, ok.
func (f Filter) outerWhere() where {
	var w where
	if f.Since != nil {
		w.add("ts >= ?", *f.Since)
	}
	if f.Until != nil {
		w.add("ts < ?", *f.Until)
	}
	if f.Project != "" {
		w.add("project = ?", f.Project)
	}
	if f.Source != "" {
		w.add("instr(',' || sources || ',', ',' || ? || ',') > 0", f.Source)
	}
	if f.OK != nil {
		if *f.OK {
			w.add("ok = 1")
		} else {
			w.add("ok = 0")
		}
	}
	return w
}

// mergedCTE is `calls`: v_ledger_calls' merge rule (rows of the same kind, name,
// mcp_server, client, session_id and tool_use_id are one call; rows without a
// tool_use_id are never merged), except that mcp_server is compared with '-' and
// '_' equal (the hook sees the client's spelling, the gateway the registered
// one; the view would keep such a pair as two calls) with deterministic aggregates and the extra
// columns the CLI shows. The view itself is not used because it cannot be
// narrowed before grouping and exposes neither error_class nor invocation.
func mergedCTE(f Filter) (string, []any) {
	return mergedCTELean(f, cteCols{project: true, sources: true, detail: true})
}

// cteCols selects the optional columns of the merged CTE. Aggregates compute
// only what they group or filter by, which makes the scan of a large ledger
// noticeably cheaper; unselected columns are empty strings.
type cteCols struct {
	project, sources bool
	detail           bool // session_id, error_class, invocation, tool_use_id
}

// mergedCTELean is mergedCTE with a column selection. A scan without any row
// filter ignores the secondary indexes (their lookups cost more than reading the
// table in order).
func mergedCTELean(f Filter, c cteCols) (string, []any) {
	w := f.rawWhere()
	if f.Raw {
		return `calls AS (SELECT id, ts, kind, name, COALESCE(mcp_server,'') AS mcp_server, COALESCE(client,'') AS client,
 COALESCE(project,'') AS project, COALESCE(session_id,'') AS session_id, ok, latency_ms, found, source AS sources,
 COALESCE(error_class,'') AS error_class, COALESCE(invocation,'') AS invocation, COALESCE(tool_use_id,'') AS tool_use_id
 FROM ledger` + w.sql() + `)`, w.args
	}
	proj, src := `'' AS project`, `'' AS sources`
	sess, ec, inv := `'' AS session_id`, `'' AS error_class`, `'' AS invocation`
	if c.project {
		proj = `COALESCE(MAX(project),'') AS project`
	}
	if c.sources {
		src = `ltrim(CASE WHEN MAX(source='gateway') THEN ',gateway' ELSE '' END || CASE WHEN MAX(source='hook') THEN ',hook' ELSE '' END
   || CASE WHEN MAX(source='transcript') THEN ',transcript' ELSE '' END, ',') AS sources`
	}
	if c.detail {
		sess, ec, inv = `COALESCE(MAX(session_id),'') AS session_id`, `COALESCE(MAX(error_class),'') AS error_class`, `COALESCE(MAX(invocation),'') AS invocation`
	}
	from := "ledger"
	if len(w.conds) == 0 {
		from = "ledger NOT INDEXED"
	}
	return `calls AS (SELECT MIN(id) AS id, MIN(ts) AS ts, kind, name, COALESCE(MAX(mcp_server),'') AS mcp_server,
 COALESCE(MAX(client),'') AS client, ` + proj + `, ` + sess + `,
 MAX(ok) AS ok, MAX(latency_ms) AS latency_ms, MIN(found) AS found,
 ` + src + `,
 ` + ec + `, ` + inv + `, '' AS tool_use_id
 FROM ` + from + w.sql() + `
 GROUP BY kind, name, replace(COALESCE(mcp_server,''),'-','_'), COALESCE(client,''), COALESCE(session_id,''), COALESCE(tool_use_id, 'row:' || id))`, w.args
}

func fmtTime(ms int64) string { return msToTime(ms) }

func nb(n sql.NullInt64) *bool {
	if !n.Valid {
		return nil
	}
	b := n.Int64 != 0
	return &b
}

// listWindows are the widths of the staged look-back of List (before the newest
// timestamp), so that "the latest N calls" does not group the whole ledger.
var listWindows = []int64{int64(6 * time.Hour / time.Millisecond), int64(48 * time.Hour / time.Millisecond),
	int64(16 * 24 * time.Hour / time.Millisecond), int64(128 * 24 * time.Hour / time.Millisecond)}

// List returns calls, newest first (ties: highest id first), at most limit.
// Without --since it first looks at a recent window and widens it only while
// fewer than limit calls were found; the result equals the unwindowed query.
func List(ctx context.Context, db *sql.DB, f Filter, limit int) ([]Call, error) {
	if f.Since == nil {
		var anchor sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT MAX(ts) FROM ledger`).Scan(&anchor); err != nil {
			return nil, err
		}
		if !anchor.Valid {
			return []Call{}, nil
		}
		hi := anchor.Int64
		if f.Until != nil && *f.Until < hi {
			hi = *f.Until
		}
		for _, w := range listWindows {
			lo := hi - w
			g := f
			g.Since = &lo
			cs, err := listWindow(ctx, db, g, limit)
			if err != nil || len(cs) >= limit {
				return cs, err
			}
		}
	}
	return listWindow(ctx, db, f, limit)
}

func listWindow(ctx context.Context, db *sql.DB, f Filter, limit int) ([]Call, error) {
	cte, args := mergedCTE(f)
	ow := f.outerWhere()
	args = append(args, ow.args...)
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, `WITH `+cte+`
SELECT id, ts, kind, name, mcp_server, client, project, session_id, sources, ok, error_class, latency_ms, found, invocation, tool_use_id
FROM calls`+ow.sql()+` ORDER BY ts DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Call{}
	for rows.Next() {
		var c Call
		var src string
		var ok, found, lat sql.NullInt64
		if err := rows.Scan(&c.ID, &c.TS, &c.Kind, &c.Name, &c.MCPServer, &c.Client, &c.Project, &c.SessionID, &src,
			&ok, &c.ErrorClass, &lat, &found, &c.Invocation, &c.ToolUseID); err != nil {
			return nil, err
		}
		c.Time = fmtTime(c.TS)
		c.Sources = strings.Split(src, ",")
		c.OK, c.Found = nb(ok), nb(found)
		if lat.Valid {
			v := lat.Int64
			c.LatencyMS = &v
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Group-by modes of Summary.
var GroupBys = []string{"kind", "name", "client", "project", "day", "source"}

// Group is one aggregate row. Key holds the grouping columns (see KeyCols).
type Group struct {
	Key       map[string]string `json:"key"`
	Calls     int64             `json:"calls"`
	OK        int64             `json:"ok"`
	Failed    int64             `json:"failed"`
	Unknown   int64             `json:"unknown"` // calls whose result was not observed
	AvgMS     *float64          `json:"avg_latency_ms"`
	MaxMS     *int64            `json:"max_latency_ms"`
	LastTS    int64             `json:"last_ts_ms"`
	LastTime  string            `json:"last_time"`
	keyValues []string
}

// KeyCols returns the grouping columns of a group-by mode.
func KeyCols(groupBy string) []string {
	switch groupBy {
	case "name":
		return []string{"kind", "mcp_server", "name"}
	default:
		return []string{groupBy}
	}
}

func groupExprs(groupBy string) ([]string, error) {
	switch groupBy {
	case "kind":
		return []string{"kind"}, nil
	case "name":
		return []string{"kind", "replace(mcp_server,'-','_')", "name"}, nil
	case "client":
		return []string{"client"}, nil
	case "project":
		return []string{"project"}, nil
	case "day":
		return []string{"strftime('%Y-%m-%d', ts/1000, 'unixepoch')"}, nil
	case "source":
		return []string{"sources"}, nil
	}
	return nil, fmt.Errorf("--group-by 값 %q 은(는) 지원하지 않습니다 (kind|name|client|project|day|source)", groupBy)
}

// Summary aggregates merged calls. Ordering is stable: most calls first, ties by
// the key columns ascending (day: newest first). It is also the engine of Top.
func Summary(ctx context.Context, db *sql.DB, f Filter, groupBy string, limit int) ([]Group, error) {
	exprs, err := groupExprs(groupBy)
	if err != nil {
		return nil, err
	}
	f.Raw = false
	cte, args := mergedCTELean(f, cteCols{project: groupBy == "project" || f.Project != "", sources: groupBy == "source" || f.Source != ""})
	ow := f.outerWhere()
	args = append(args, ow.args...)
	args = append(args, limit)
	keys := strings.Join(exprs, ", ")
	order := "COUNT(*) DESC, " + keys
	if groupBy == "day" {
		order = keys + " DESC"
	}
	rows, err := db.QueryContext(ctx, `WITH `+cte+`
SELECT `+keys+`, COUNT(*), COALESCE(SUM(ok=1),0), COALESCE(SUM(ok=0),0), AVG(latency_ms), MAX(latency_ms), MAX(ts)
FROM calls`+ow.sql()+` GROUP BY `+keys+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := KeyCols(groupBy)
	out := []Group{}
	for rows.Next() {
		g := Group{Key: map[string]string{}}
		vals := make([]string, len(exprs))
		dest := make([]any, 0, len(exprs)+6)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		var avg sql.NullFloat64
		var mx sql.NullInt64
		dest = append(dest, &g.Calls, &g.OK, &g.Failed, &avg, &mx, &g.LastTS)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		for i, c := range cols {
			g.Key[c] = vals[i]
		}
		g.keyValues = vals
		g.Unknown = g.Calls - g.OK - g.Failed
		if avg.Valid {
			v := math.Round(avg.Float64*10) / 10
			g.AvgMS = &v
		}
		if mx.Valid {
			v := mx.Int64
			g.MaxMS = &v
		}
		g.LastTime = fmtTime(g.LastTS)
		out = append(out, g)
	}
	return out, rows.Err()
}

// Unused is a registered skill, tool or MCP server without a call in the period.
type Unused struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Scope   string `json:"scope"`
	Enabled bool   `json:"enabled"`
	LastTS  *int64 `json:"last_ts_ms"` // last call ever (not only in the period); null = never called
	Last    string `json:"last_time"`
}

// UnusedItems lists registered items (skills, tools, MCP servers; kind "" = all)
// with no call in [Since, Until). Matching rules: skills and tools by exact name,
// MCP servers by name with '-' and '_' equal (the ledger stores the client's
// spelling). A call recorded with found=0 does not count: the item did not exist
// then. Disabled items are listed too (Enabled=false).
func UnusedItems(ctx context.Context, db *sql.DB, kind string, since, until *int64) ([]Unused, error) {
	lo, hi := int64(math.MinInt64), int64(math.MaxInt64)
	if since != nil {
		lo = *since
	}
	if until != nil {
		hi = *until
	}
	var out []Unused
	for _, k := range Kinds {
		if kind != "" && kind != k {
			continue
		}
		var regQ, usedQ, lastQ string
		switch k {
		case "skill":
			regQ = `SELECT name, scope, enabled FROM skills`
			usedQ = `SELECT DISTINCT name FROM ledger WHERE kind='skill' AND found IS NOT 0 AND ts >= ? AND ts < ?`
			lastQ = `SELECT name, MAX(ts) FROM ledger WHERE kind='skill' AND found IS NOT 0 GROUP BY name`
		case "tool":
			regQ = `SELECT name, scope, enabled FROM tools`
			usedQ = `SELECT DISTINCT name FROM ledger WHERE kind='tool' AND found IS NOT 0 AND ts >= ? AND ts < ?`
			lastQ = `SELECT name, MAX(ts) FROM ledger WHERE kind='tool' AND found IS NOT 0 GROUP BY name`
		default:
			regQ = `SELECT name, scope, enabled FROM mcp_servers`
			usedQ = `SELECT DISTINCT replace(mcp_server,'-','_') FROM ledger WHERE kind='mcp' AND mcp_server IS NOT NULL AND found IS NOT 0 AND ts >= ? AND ts < ?`
			lastQ = `SELECT replace(mcp_server,'-','_'), MAX(ts) FROM ledger WHERE kind='mcp' AND mcp_server IS NOT NULL AND found IS NOT 0 GROUP BY replace(mcp_server,'-','_')`
		}
		norm := func(s string) string { return s }
		if k == "mcp" {
			norm = normServer
		}
		used := map[string]bool{}
		if err := scanStrings(ctx, db, usedQ, []any{lo, hi}, func(s string) { used[s] = true }); err != nil {
			return nil, err
		}
		last := map[string]int64{}
		lr, err := db.QueryContext(ctx, lastQ)
		if err != nil {
			return nil, err
		}
		for lr.Next() {
			var n string
			var ts int64
			if err := lr.Scan(&n, &ts); err != nil {
				_ = lr.Close()
				return nil, err
			}
			last[n] = ts
		}
		if err := lr.Err(); err != nil {
			return nil, err
		}
		_ = lr.Close()

		rr, err := db.QueryContext(ctx, regQ+` ORDER BY name, scope, id`)
		if err != nil {
			return nil, err
		}
		for rr.Next() {
			var u Unused
			var en int
			if err := rr.Scan(&u.Name, &u.Scope, &en); err != nil {
				_ = rr.Close()
				return nil, err
			}
			key := norm(u.Name)
			if used[key] {
				continue
			}
			u.Kind, u.Enabled = k, en != 0
			if ts, ok := last[key]; ok {
				v := ts
				u.LastTS, u.Last = &v, fmtTime(ts)
			}
			out = append(out, u)
		}
		if err := rr.Err(); err != nil {
			return nil, err
		}
		_ = rr.Close()
	}
	return out, nil
}

func scanStrings(ctx context.Context, db *sql.DB, q string, args []any, fn func(string)) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		fn(s)
	}
	return rows.Err()
}

// Miss is one "unregistered call name": calls with found=0 grouped by kind,
// (MCP) server and name. Names of unknown tools are the sanitized, cut values
// the recorder stored, not necessarily real tool names.
type Miss struct {
	Kind      string         `json:"kind"`
	MCPServer string         `json:"mcp_server"`
	Name      string         `json:"name"`
	Count     int64          `json:"count"`
	FirstTS   int64          `json:"first_ts_ms"`
	First     string         `json:"first_time"`
	LastTS    int64          `json:"last_ts_ms"`
	Last      string         `json:"last_time"`
	Clients   map[string]int `json:"clients"` // calls per client; "" client is reported as UnknownClient
}

// UnknownClient labels calls whose client was not identified (ledger.client NULL).
const UnknownClient = "(unknown)"

// Misses aggregates merged calls with found=0 (a source that said "not in the
// registry" wins, as MIN(found) does in v_ledger_calls). It reads the ledger
// directly so MCP misses stay separated per server (v_misses folds them by
// name only). Only kind, client, since and until of f are used. Rows with
// found NULL (unknown, e.g. a plugin:skill name) are never misses. Ordered by
// count, then kind, server, name.
func Misses(ctx context.Context, db *sql.DB, f Filter, limit int) ([]Miss, error) {
	w := where{}
	w.add("found = 0") // uses ix_ledger_miss; MIN(found)=0 iff any row is 0
	if f.Kind != "" {
		w.add("kind = ?", f.Kind)
	}
	if f.Client != "" {
		w.add("client = ?", f.Client)
	}
	args := w.args
	ow := where{}
	if f.Since != nil {
		ow.add("ts >= ?", *f.Since)
	}
	if f.Until != nil {
		ow.add("ts < ?", *f.Until)
	}
	args = append(args, ow.args...)
	rows, err := db.QueryContext(ctx, `
WITH m AS (SELECT MIN(ts) AS ts, kind, name, replace(COALESCE(MAX(mcp_server),''),'-','_') AS srv, COALESCE(MAX(client),'') AS client
 FROM ledger`+w.sql()+`
 GROUP BY kind, name, replace(COALESCE(mcp_server,''),'-','_'), COALESCE(client,''), COALESCE(session_id,''), COALESCE(tool_use_id, 'row:' || id))
SELECT kind, srv, name, client, COUNT(*), MIN(ts), MAX(ts) FROM m`+ow.sql()+` GROUP BY kind, srv, name, client`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx := map[[3]string]*Miss{}
	var out []*Miss
	for rows.Next() {
		var kind, srv, name, client string
		var n, first, last int64
		if err := rows.Scan(&kind, &srv, &name, &client, &n, &first, &last); err != nil {
			return nil, err
		}
		k := [3]string{kind, srv, name}
		m := idx[k]
		if m == nil {
			m = &Miss{Kind: kind, MCPServer: srv, Name: name, FirstTS: first, LastTS: last, Clients: map[string]int{}}
			idx[k] = m
			out = append(out, m)
		}
		m.Count += n
		if first < m.FirstTS {
			m.FirstTS = first
		}
		if last > m.LastTS {
			m.LastTS = last
		}
		if client == "" {
			client = UnknownClient
		}
		m.Clients[client] += int(n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.MCPServer != b.MCPServer {
			return a.MCPServer < b.MCPServer
		}
		return a.Name < b.Name
	})
	if len(out) > limit {
		out = out[:limit]
	}
	res := make([]Miss, len(out))
	for i, m := range out {
		m.First, m.Last = fmtTime(m.FirstTS), fmtTime(m.LastTS)
		res[i] = *m
	}
	return res, nil
}
