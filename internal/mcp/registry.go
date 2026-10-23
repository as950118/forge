package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Exposure modes (schema profile_member_exposure.exposure).
const (
	ExposureDirect  = "direct"
	ExposureGateway = "gateway"
	ExposureHidden  = "hidden"
)

// Registry stores MCP server definitions in the DB.
type Registry struct {
	DB  *sql.DB
	Now func() time.Time
}

func (r *Registry) now() int64 {
	if r.Now != nil {
		return r.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

// Discovery is the latest discovery attempt (one row per server).
type Discovery struct {
	LastAttemptAt   int64  `json:"last_attempt_at"`
	OK              bool   `json:"ok"`
	Error           string `json:"error,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
	ServerName      string `json:"server_name,omitempty"`
	ServerVersion   string `json:"server_version,omitempty"`
}

// Tool is one cached tools/list entry.
type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	SchemaHash   string          `json:"schema_hash"`
	Stale        bool            `json:"stale"`
	DiscoveredAt int64           `json:"discovered_at"`
	// Approval is approved|changed|unapproved (see approval.go); set by Get.
	Approval   string `json:"approval"`
	ApprovedAt int64  `json:"approved_at,omitempty"`
}

// Info is a registered server. Values carry references only, never secrets.
type Info struct {
	Name      string            `json:"name"`
	Scope     string            `json:"scope"`
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	URL       string            `json:"url,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
	Enabled   bool              `json:"enabled"`
	Values    []Value           `json:"values,omitempty"`
	Exposure  map[string]string `json:"exposure"`
	Discovery *Discovery        `json:"discovery,omitempty"`
	ToolCount int               `json:"tool_count"`
	// Enforce: the gateway exposes only approved, unchanged tools (settings mcp.enforce.<server>).
	Enforce   bool   `json:"enforce_pinned"`
	Tools     []Tool `json:"tools,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Add registers a validated server definition in one write transaction.
func (r *Registry) Add(ctx context.Context, s Spec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	args, _ := json.Marshal(append([]string{}, s.Args...))
	now := r.now()
	tx, err := r.DB.BeginTx(ctx, nil) // BEGIN IMMEDIATE (store.OpenRaw _txlock)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM mcp_servers WHERE scope='global' AND project_id IS NULL AND name=?`, s.Name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %s (다시 등록하려면 먼저 forge mcp remove %s)", ErrExists, s.Name, s.Name)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO mcp_servers(scope, project_id, name, transport, command, args, cwd, url, timeout_ms, enabled, created_at, updated_at)
		VALUES ('global', NULL, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		s.Name, s.Transport, nullStr(s.Command), string(args), nullStr(s.Cwd), nullStr(s.URL), nullInt(s.TimeoutMS), now, now)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	for _, v := range s.Values {
		secret := 0
		if v.IsSecret {
			secret = 1
		}
		var ref, lit any
		switch v.RefType {
		case RefLiteral:
			lit = v.Literal
		case RefEnv, RefKeychain:
			ref = v.Ref
		default:
			return fmt.Errorf("알 수 없는 ref_type: %s", v.RefType)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mcp_server_values(server_id, slot, key, is_secret, ref_type, ref, value) VALUES (?,?,?,?,?,?,?)`,
			id, v.Slot, v.Key, secret, v.RefType, ref, lit); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

const infoCols = `id, name, scope, transport, COALESCE(command,''), args, COALESCE(cwd,''), COALESCE(url,''), COALESCE(timeout_ms,0), enabled, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanInfo(sc scanner) (int64, *Info, error) {
	var id int64
	var i Info
	var en int
	var args string
	if err := sc.Scan(&id, &i.Name, &i.Scope, &i.Transport, &i.Command, &args, &i.Cwd, &i.URL, &i.TimeoutMS, &en, &i.CreatedAt, &i.UpdatedAt); err != nil {
		return 0, nil, err
	}
	i.Enabled = en == 1
	_ = json.Unmarshal([]byte(args), &i.Args)
	i.Exposure = map[string]string{}
	return id, &i, nil
}

// List returns all global servers ordered by name (without tools/values detail).
func (r *Registry) List(ctx context.Context) ([]Info, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+infoCols+` FROM mcp_servers WHERE scope='global' AND project_id IS NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	type pair struct {
		id int64
		i  *Info
	}
	var ps []pair
	for rows.Next() {
		id, i, err := scanInfo(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		ps = append(ps, pair{id, i})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	out := []Info{}
	for _, p := range ps {
		if err := r.fill(ctx, p.id, p.i, false); err != nil {
			return nil, err
		}
		out = append(out, *p.i)
	}
	return out, nil
}

// Get returns one server with values (references), discovery state and cached tools.
func (r *Registry) Get(ctx context.Context, name string) (*Info, error) {
	id, i, err := scanInfo(r.DB.QueryRowContext(ctx,
		`SELECT `+infoCols+` FROM mcp_servers WHERE scope='global' AND project_id IS NULL AND name=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	if err := r.fill(ctx, id, i, true); err != nil {
		return nil, err
	}
	return i, nil
}

func (r *Registry) fill(ctx context.Context, id int64, i *Info, detail bool) error {
	// exposure: default direct for every known client, overridden by settings rows.
	crows, err := r.DB.QueryContext(ctx, `SELECT id FROM clients ORDER BY id`)
	if err != nil {
		return err
	}
	for crows.Next() {
		var c string
		if err := crows.Scan(&c); err != nil {
			_ = crows.Close()
			return err
		}
		i.Exposure[c] = ExposureDirect
	}
	_ = crows.Close()
	prefix := exposurePrefix(i.Name)
	srows, err := r.DB.QueryContext(ctx, `SELECT substr(key, ?), value FROM settings WHERE substr(key,1,?)=?`,
		len(prefix)+1, len(prefix), prefix)
	if err != nil {
		return err
	}
	for srows.Next() {
		var c, m string
		if err := srows.Scan(&c, &m); err != nil {
			_ = srows.Close()
			return err
		}
		i.Exposure[c] = m
	}
	_ = srows.Close()

	var ev string
	if err := r.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, enforceKey(i.Name)).Scan(&ev); err == nil {
		i.Enforce = ev == "1"
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	var d Discovery
	var ok int
	var e, pv, sn, sv sql.NullString
	err = r.DB.QueryRowContext(ctx, `SELECT last_attempt_at, ok, error, protocol_version, server_name, server_version FROM mcp_discovery WHERE server_id=?`, id).
		Scan(&d.LastAttemptAt, &ok, &e, &pv, &sn, &sv)
	switch {
	case err == nil:
		d.OK, d.Error, d.ProtocolVersion, d.ServerName, d.ServerVersion = ok == 1, e.String, pv.String, sn.String, sv.String
		i.Discovery = &d
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if err := r.DB.QueryRowContext(ctx, `SELECT count(*) FROM mcp_tools WHERE server_id=? AND stale=0`, id).Scan(&i.ToolCount); err != nil {
		return err
	}
	if !detail {
		return nil
	}
	vrows, err := r.DB.QueryContext(ctx, `SELECT slot, key, is_secret, ref_type, COALESCE(ref,''), COALESCE(value,'') FROM mcp_server_values WHERE server_id=? ORDER BY slot, key`, id)
	if err != nil {
		return err
	}
	for vrows.Next() {
		var v Value
		var sec int
		if err := vrows.Scan(&v.Slot, &v.Key, &sec, &v.RefType, &v.Ref, &v.Literal); err != nil {
			_ = vrows.Close()
			return err
		}
		v.IsSecret = sec == 1
		if v.RefType == RefLiteral {
			v.Display = v.Literal
		} else {
			v.Literal = ""
			v.Display = "${" + v.Ref + "}"
			if v.RefType == RefKeychain {
				v.Display = "keychain:" + v.Ref
			}
		}
		i.Values = append(i.Values, v)
	}
	_ = vrows.Close()
	approvals, err := loadApprovals(ctx, r.DB, id)
	if err != nil {
		return err
	}
	trows, err := r.DB.QueryContext(ctx, `SELECT name, COALESCE(description,''), input_schema, schema_hash, stale, discovered_at FROM mcp_tools WHERE server_id=? ORDER BY name`, id)
	if err != nil {
		return err
	}
	defer trows.Close()
	for trows.Next() {
		var t Tool
		var st int
		var sch string
		if err := trows.Scan(&t.Name, &t.Description, &sch, &t.SchemaHash, &st, &t.DiscoveredAt); err != nil {
			return err
		}
		t.Stale = st == 1
		t.InputSchema = json.RawMessage(sch)
		t.Approval = ApprovalNew
		if a, ok := approvals[t.Name]; ok {
			t.ApprovedAt = a.at
			if a.hash == t.SchemaHash {
				t.Approval = ApprovalApproved
			} else {
				t.Approval = ApprovalChanged
			}
		}
		i.Tools = append(i.Tools, t)
	}
	return trows.Err()
}

// SetEnabled toggles a server globally (idempotent).
func (r *Registry) SetEnabled(ctx context.Context, name string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := r.DB.ExecContext(ctx,
		`UPDATE mcp_servers SET enabled=?, updated_at=? WHERE scope='global' AND project_id IS NULL AND name=?`, v, r.now(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return nil
}

// exposurePrefix is the settings key prefix holding per-client exposure for a server.
//
// Profiles (package profile, TASK-0012) are the formal store of exposure
// (profile_member_exposure). This settings key is the LEGACY per-client default
// that applies while no profile is active for the client (rule "legacy"); an
// active profile ignores it, and `forge mcp expose|enable|disable --client`
// refuse to change it then (internal/cli/profile.go). `forge profile create
// --from-current` copies these settings into a profile.
// "mcp.exposure.<server>.<client>" = direct|gateway|hidden (no row = direct).
func exposurePrefix(name string) string { return "mcp.exposure." + name + "." }

// SetExposure sets how a server is exposed to one client.
func (r *Registry) SetExposure(ctx context.Context, name, client, mode string) error {
	switch mode {
	case ExposureDirect, ExposureGateway, ExposureHidden:
	default:
		return fmt.Errorf("노출 방식은 direct|gateway|hidden 중 하나여야 합니다: %q", mode)
	}
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
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM clients WHERE id=?`, client).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		ids, _ := r.clientIDs(ctx)
		return fmt.Errorf("알 수 없는 클라이언트: %q (지원: %s)", client, strings.Join(ids, ", "))
	}
	key := exposurePrefix(name) + client
	if mode == ExposureDirect {
		_, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, key)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES (?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, key, mode, r.now())
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Registry) clientIDs(ctx context.Context) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT id FROM clients ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		ids = append(ids, s)
	}
	sort.Strings(ids)
	return ids, rows.Err()
}

// Remove deletes a server; values, discovery and tool cache cascade.
func (r *Registry) Remove(ctx context.Context, name string) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM mcp_servers WHERE scope='global' AND project_id IS NULL AND name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	p := exposurePrefix(name)
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE substr(key,1,?)=?`, len(p), p); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, enforceKey(name)); err != nil {
		return err
	}
	return tx.Commit()
}
