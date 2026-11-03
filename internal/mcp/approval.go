package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/as950118/forge/internal/textsafe"
)

// Tool definition approval (TASK-0013). A tool's definition (name + description
// + input schema, hashed as mcp_tools.schema_hash) is what a model reads and
// trusts. `forge mcp approve` records the current cached definition as approved
// (mcp_tool_approvals, migration 0002); a later refresh that returns a
// different definition is reported as a change ("rug pull" defence), and with
// enforcement on (settings key mcp.enforce.<server>) the gateway exposes only
// tools whose current hash equals the approved one.

// Approval states of one cached tool.
const (
	ApprovalApproved = "approved"   // approved and the definition is unchanged
	ApprovalChanged  = "changed"    // approved earlier, the definition differs now
	ApprovalNew      = "unapproved" // never approved
)

func enforceKey(server string) string { return "mcp.enforce." + server }

// allowPrivateKey is the settings key of the per-server "may connect to private
// addresses" opt-in (TASK-0020 AC-12; no row = off).
func allowPrivateKey(server string) string { return "mcp.allow_private." + server }

// ToolChange is one finding of a refresh. Description texts are sanitized
// (textsafe) and cut to the changed span, so a long description cannot hide an
// appended instruction behind the cut.
type ToolChange struct {
	Name string `json:"name"`
	// Kind: "new" (first seen, unapproved), "changed", "removed".
	Kind string `json:"kind"`
	// Baseline of a "changed": "approved" (the approved definition) or "previous"
	// (the previous cache, when nothing was approved).
	Baseline           string   `json:"baseline,omitempty"`
	Approved           bool     `json:"approved"`
	DescriptionChanged bool     `json:"description_changed,omitempty"`
	SchemaChanged      bool     `json:"schema_changed,omitempty"`
	OldSpan            string   `json:"old_description_span,omitempty"`
	NewSpan            string   `json:"new_description_span,omitempty"`
	SchemaDiff         []string `json:"schema_diff,omitempty"`
	// Hidden: the gateway will not expose this tool (enforcement on).
	Hidden bool `json:"hidden,omitempty"`
}

type approvalRow struct {
	hash, desc, schema string
	at                 int64
}

func loadApprovals(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id int64) (map[string]approvalRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT tool_name, approved_hash, COALESCE(description,''), input_schema, approved_at FROM mcp_tool_approvals WHERE server_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]approvalRow{}
	for rows.Next() {
		var n string
		var a approvalRow
		if err := rows.Scan(&n, &a.hash, &a.desc, &a.schema, &a.at); err != nil {
			return nil, err
		}
		m[n] = a
	}
	return m, rows.Err()
}

const maxSpan = 300

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// changedSpan returns the differing middle of old and new (common prefix and
// suffix removed, with a little context), sanitized and bounded.
func changedSpan(old, nw string) (string, string) {
	o, n := []rune(old), []rune(nw)
	p := 0
	for p < len(o) && p < len(n) && o[p] == n[p] {
		p++
	}
	s := 0
	for s < len(o)-p && s < len(n)-p && o[len(o)-1-s] == n[len(n)-1-s] {
		s++
	}
	ctx := 20
	from := p - ctx
	if from < 0 {
		from = 0
	}
	mk := func(r []rune) string {
		toEnd := len(r) - s + ctx
		if toEnd > len(r) {
			toEnd = len(r)
		}
		seg := string(r[from:toEnd])
		pre, post := "", ""
		if from > 0 {
			pre = "…"
		}
		if toEnd < len(r) {
			post = "…"
		}
		return pre + textsafe.Display(clipRunes(strings.Join(strings.Fields(seg), " "), maxSpan), 4*maxSpan) + post
	}
	return mk(o), mk(n)
}

// schemaDiff summarizes how two input schemas differ (top-level properties,
// required list and per-property definitions); names are sanitized.
func schemaDiff(old, nw string) []string {
	var a, b map[string]any
	_ = json.Unmarshal([]byte(old), &a)
	_ = json.Unmarshal([]byte(nw), &b)
	props := func(m map[string]any) map[string]any {
		p, _ := m["properties"].(map[string]any)
		return p
	}
	pa, pb := props(a), props(b)
	var added, removed, changed []string
	for k, v := range pb {
		ov, ok := pa[k]
		switch {
		case !ok:
			added = append(added, k)
		default:
			x, _ := json.Marshal(ov)
			y, _ := json.Marshal(v)
			if string(x) != string(y) {
				changed = append(changed, k)
			}
		}
	}
	for k := range pa {
		if _, ok := pb[k]; !ok {
			removed = append(removed, k)
		}
	}
	list := func(label string, xs []string) string {
		sort.Strings(xs)
		if len(xs) > 8 {
			xs = append(xs[:8], fmt.Sprintf("… 외 %d개", len(xs)-8))
		}
		for i := range xs {
			xs[i] = textsafe.Display(xs[i], 60)
		}
		return label + ": " + strings.Join(xs, ", ")
	}
	var out []string
	if len(added) > 0 {
		out = append(out, list("입력 속성 추가", added))
	}
	if len(removed) > 0 {
		out = append(out, list("입력 속성 삭제", removed))
	}
	if len(changed) > 0 {
		out = append(out, list("입력 속성 정의 변경", changed))
	}
	ra, _ := json.Marshal(a["required"])
	rb, _ := json.Marshal(b["required"])
	if string(ra) != string(rb) {
		out = append(out, "필수(required) 항목 변경")
	}
	if len(out) == 0 {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		if string(x) != string(y) {
			out = append(out, "스키마 세부 변경 (속성 목록은 같음)")
		}
	}
	return out
}

// ApprovedTool is one tool recorded by Approve.
type ApprovedTool struct {
	Name string `json:"name"`
	// Previous: "none" (first approval), "same" (already approved, unchanged) or
	// "changed" (replaces an older approved definition).
	Previous string `json:"previous"`
	// Description is the sanitized, shortened description that was approved.
	Description string      `json:"description"`
	Change      *ToolChange `json:"change,omitempty"`
}

// ApproveResult reports what Approve recorded.
type ApproveResult struct {
	Server   string         `json:"server"`
	Approved []ApprovedTool `json:"approved"`
	Pruned   []string       `json:"pruned,omitempty"` // approvals of tools no longer offered (full approve only)
}

// Approve records the currently cached definitions of server as approved. With
// no tools given it approves every non-stale cached tool (and drops approvals of
// tools the server no longer offers); with tools it approves just those, which
// must be cached and not stale. It never talks to the server: what is approved
// is exactly what `forge mcp show` displays.
func (r *Registry) Approve(ctx context.Context, name string, tools []string) (*ApproveResult, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM mcp_servers WHERE scope='global' AND project_id IS NULL AND name=?`, name).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	} else if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT name, COALESCE(description,''), input_schema, schema_hash, stale FROM mcp_tools WHERE server_id=? ORDER BY name`, id)
	if err != nil {
		return nil, err
	}
	type cached struct {
		name, desc, schema, hash string
		stale                    bool
	}
	var all []cached
	for rows.Next() {
		var c cached
		var st int
		if err := rows.Scan(&c.name, &c.desc, &c.schema, &c.hash, &st); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c.stale = st == 1
		all = append(all, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if len(all) == 0 {
		return nil, fmt.Errorf("MCP 서버 %q 에 캐시된 도구가 없습니다. 먼저 forge mcp refresh %s 로 도구 목록을 가져오세요", name, name)
	}
	want := map[string]bool{}
	for _, t := range tools {
		want[t] = true
	}
	live := map[string]bool{}
	var sel []cached
	for _, c := range all {
		if c.stale {
			continue
		}
		live[c.name] = true
		if len(tools) == 0 || want[c.name] {
			sel = append(sel, c)
			delete(want, c.name)
		}
	}
	if len(want) > 0 {
		var miss []string
		for t := range want {
			miss = append(miss, textsafe.Display(t, 80))
		}
		sort.Strings(miss)
		return nil, fmt.Errorf("MCP 서버 %q 의 캐시에 (사라지지 않은) 도구가 없습니다: %s", name, strings.Join(miss, ", "))
	}
	prev, err := loadApprovals(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	res := &ApproveResult{Server: name}
	now := r.now()
	for _, c := range sel {
		at := ApprovedTool{Name: c.name, Previous: "none", Description: textsafe.Display(clipRunes(strings.Join(strings.Fields(c.desc), " "), 160), 700)}
		if p, ok := prev[c.name]; ok {
			if p.hash == c.hash {
				at.Previous = "same"
			} else {
				at.Previous = "changed"
				ch := &ToolChange{Name: c.name, Kind: "changed", Baseline: "approved", Approved: true}
				if p.desc != c.desc {
					ch.DescriptionChanged = true
					ch.OldSpan, ch.NewSpan = changedSpan(p.desc, c.desc)
				}
				if sd := schemaDiff(p.schema, c.schema); len(sd) > 0 {
					ch.SchemaChanged, ch.SchemaDiff = true, sd
				}
				at.Change = ch
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_tool_approvals(server_id, tool_name, approved_hash, description, input_schema, approved_at) VALUES (?,?,?,?,?,?)
			ON CONFLICT(server_id, tool_name) DO UPDATE SET approved_hash=excluded.approved_hash, description=excluded.description, input_schema=excluded.input_schema, approved_at=excluded.approved_at`,
			id, c.name, c.hash, nullStr(c.desc), c.schema, now); err != nil {
			return nil, err
		}
		res.Approved = append(res.Approved, at)
	}
	if len(tools) == 0 {
		for n := range prev {
			if !live[n] {
				if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_tool_approvals WHERE server_id=? AND tool_name=?`, id, n); err != nil {
					return nil, err
				}
				res.Pruned = append(res.Pruned, textsafe.Display(n, 80))
			}
		}
		sort.Strings(res.Pruned)
	}
	return res, tx.Commit()
}

// SetEnforce turns "gateway exposes only approved, unchanged tools" on or off
// for one server (settings key mcp.enforce.<server>; no row = off).
func (r *Registry) SetEnforce(ctx context.Context, name string, on bool) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mcp_servers WHERE scope='global' AND project_id IS NULL AND name=?`, name).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if on {
		_, err = tx.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES (?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, enforceKey(name), "1", r.now())
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, enforceKey(name))
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SetAllowPrivate allows (or revokes) connections of a remote server to loopback,
// private and CGNAT addresses, including addresses a DNS name resolves to and
// redirect targets. Link-local, multicast, unspecified and cloud metadata addresses
// are blocked regardless (settings key mcp.allow_private.<server>; no row = off).
func (r *Registry) SetAllowPrivate(ctx context.Context, name string, on bool) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mcp_servers WHERE scope='global' AND project_id IS NULL AND name=?`, name).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if on {
		_, err = tx.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES (?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, allowPrivateKey(name), "1", r.now())
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, allowPrivateKey(name))
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ToolState is the approval state of a cached tool for display and the gateway.
func ToolState(t Tool) string {
	if t.Approval == "" {
		return ApprovalNew
	}
	return t.Approval
}

// Exposable reports whether the gateway may expose t when enforcement is on.
func (t Tool) Exposable(enforce bool) bool {
	return !enforce || t.Approval == ApprovalApproved
}
