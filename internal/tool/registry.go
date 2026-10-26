package tool

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/heonjinjeong/forge/internal/mcp"
)

// Registry stores tool definitions in the DB; scripts live under Home/scripts
// (ADR-F0001 layout: scripts/<name>@<revision>/script, verified by script_hash).
type Registry struct {
	DB   *sql.DB
	Home string
	Now  func() time.Time
}

func (r *Registry) now() int64 {
	if r.Now != nil {
		return r.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

// extKey is the settings key holding the policy parts the v1 schema has no
// columns for. TODO(schema-0002): replace by real columns.
func extKey(name string) string { return "tool.ext." + name }

type ext struct {
	Shell            bool     `json:"shell,omitempty"`
	Body             string   `json:"body,omitempty"`
	Allow            []string `json:"allow,omitempty"`
	AllowLeadingDash bool     `json:"allow_leading_dash,omitempty"`
	AllowVarSubcmd   bool     `json:"allow_var_subcommand,omitempty"`
	AllowPrivate     bool     `json:"allow_private,omitempty"`
	AllowHTTP        bool     `json:"allow_http,omitempty"`
	MaxOutputBytes   int      `json:"max_output_bytes,omitempty"`
	MaxRedirects     int      `json:"max_redirects,omitempty"`
}

func scriptRel(name string, rev int) string {
	return "scripts/" + name + "@" + fmt.Sprint(rev) + "/script"
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Add registers a validated definition (replace=true updates an existing one and
// bumps its revision; there is no history, ADR-F0001 D3). One BEGIN IMMEDIATE
// transaction covers existence check, rows and the script file.
func (r *Registry) Add(ctx context.Context, d Definition, replace bool) (*Definition, error) {
	if err := d.ValidateNew(); err != nil {
		return nil, err
	}
	if d.Kind == KindScript && r.Home == "" {
		return nil, errors.New("script 도구에는 Registry.Home 이 필요합니다")
	}
	now := r.now()
	args, _ := json.Marshal(append([]string{}, d.Args...))
	ex, _ := json.Marshal(ext{Shell: d.Shell, Body: d.BodyTemplate, Allow: d.Policy.Allow, AllowLeadingDash: d.Policy.AllowLeadingDash, AllowVarSubcmd: d.Policy.AllowVarSubcommand,
		AllowPrivate: d.Policy.AllowPrivate, AllowHTTP: d.Policy.AllowHTTP, MaxOutputBytes: d.Policy.MaxOutputBytes, MaxRedirects: d.Policy.MaxRedirects})

	tx, err := r.DB.BeginTx(ctx, nil) // BEGIN IMMEDIATE (store.OpenRaw _txlock)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	var rev, enabled int
	var created int64
	err = tx.QueryRowContext(ctx, `SELECT id, revision, enabled, created_at FROM tools WHERE scope='global' AND project_id IS NULL AND name=?`, d.Name).
		Scan(&id, &rev, &enabled, &created)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if exists && !replace {
		return nil, fmt.Errorf("%w: %s (정의를 바꾸려면 --replace, 지우려면 forge tool remove %s)", ErrExists, d.Name, d.Name)
	}
	newRev := 1
	if exists {
		newRev = rev + 1
		d.Enabled = enabled == 1
	} else {
		d.Enabled, created = true, now
	}
	d.Revision, d.CreatedAt, d.UpdatedAt = newRev, created, now

	var scriptDir string
	if d.Kind == KindScript {
		if d.ScriptBody != "" {
			d.ScriptHash = hashHex([]byte(d.ScriptBody))
			d.ScriptPath = scriptRel(d.Name, newRev)
			scriptDir = filepath.Join(r.Home, filepath.Dir(filepath.FromSlash(d.ScriptPath)))
		} else {
			return nil, errors.New("script 도구는 스크립트 본문이 있어야 등록할 수 있습니다")
		}
	}
	var cmd any
	if d.Kind == KindCommand {
		cmd = d.Command
	}
	cols := []any{d.Description, string(d.InputSchema), cmd, string(args), nullStr(d.ScriptPath), nullStr(d.ScriptHash),
		nullStr(d.Interpreter), nullStr(d.Method), nullStr(d.URL), nullStr(d.Policy.WorkingDir), d.Policy.TimeoutMS, d.Policy.Confirm, newRev, b2i(d.Enabled), now}
	if d.Kind == KindHTTP {
		cols[2] = nil
	}
	if exists {
		_, err = tx.ExecContext(ctx, `UPDATE tools SET kind=?, description=?, input_schema=?, command=?, args=?, script_path=?, script_hash=?, interpreter=?,
			http_method=?, http_url=?, working_dir=?, timeout_ms=?, confirm=?, revision=?, enabled=?, updated_at=? WHERE id=?`,
			append(append([]any{d.Kind}, cols...), id)...)
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM tool_values WHERE tool_id=?`, id)
		}
	} else {
		var res sql.Result
		res, err = tx.ExecContext(ctx, `INSERT INTO tools(scope, project_id, name, kind, description, input_schema, command, args, script_path, script_hash, interpreter,
			http_method, http_url, working_dir, timeout_ms, confirm, revision, enabled, created_at, updated_at)
			VALUES ('global', NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			append(append([]any{d.Name, d.Kind}, cols[:len(cols)-1]...), now, now)...)
		if err == nil {
			id, _ = res.LastInsertId()
		}
	}
	if err != nil {
		return nil, err
	}
	for _, v := range d.Values {
		var ref, lit any
		switch v.RefType {
		case mcp.RefLiteral:
			lit = v.Literal
		case mcp.RefEnv, mcp.RefKeychain:
			ref = v.Ref
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tool_values(tool_id, slot, key, is_secret, ref_type, ref, value) VALUES (?,?,?,?,?,?,?)`,
			id, v.Slot, v.Key, b2i(v.IsSecret), v.RefType, ref, lit); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, extKey(d.Name), string(ex), now); err != nil {
		return nil, err
	}
	if scriptDir != "" {
		if err := writeScript(scriptDir, d.ScriptBody); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		if scriptDir != "" {
			_ = os.RemoveAll(scriptDir)
		}
		return nil, err
	}
	r.pruneScripts(d.Name, scriptDir)
	d.ScriptBody = ""
	return &d, nil
}

func writeScript(dir, body string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	_ = os.RemoveAll(dir) // stale leftover from a failed earlier attempt
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "script"), []byte(body), 0o600); err != nil {
		return err
	}
	// sidecar hash recorded next to the file: Resolve cross-checks it with the
	// DB's script_hash, so editing only one of the two is detected.
	return os.WriteFile(filepath.Join(dir, "script.sha256"), []byte(hashHex([]byte(body))+"\n"), 0o600)
}

// pruneScripts removes every stored script dir of the tool except keep.
func (r *Registry) pruneScripts(name, keep string) {
	if r.Home == "" {
		return
	}
	dirs, _ := filepath.Glob(filepath.Join(r.Home, "scripts", name+"@*"))
	for _, d := range dirs {
		if d != keep {
			_ = os.RemoveAll(d)
		}
	}
}

const cols = `id, name, scope, kind, description, input_schema, COALESCE(command,''), args, COALESCE(script_path,''), COALESCE(script_hash,''),
	COALESCE(interpreter,''), COALESCE(http_method,''), COALESCE(http_url,''), COALESCE(working_dir,''), timeout_ms, confirm, revision, enabled, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanDef(sc scanner) (int64, *Definition, error) {
	var id int64
	var d Definition
	var schema, args string
	var en int
	if err := sc.Scan(&id, &d.Name, &d.Scope, &d.Kind, &d.Description, &schema, &d.Command, &args, &d.ScriptPath, &d.ScriptHash,
		&d.Interpreter, &d.Method, &d.URL, &d.Policy.WorkingDir, &d.Policy.TimeoutMS, &d.Policy.Confirm, &d.Revision, &en, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return 0, nil, err
	}
	d.Enabled = en == 1
	d.InputSchema = json.RawMessage(schema)
	_ = json.Unmarshal([]byte(args), &d.Args)
	return id, &d, nil
}

func (r *Registry) fill(ctx context.Context, id int64, d *Definition, values bool) error {
	var ex ext
	var s string
	err := r.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, extKey(d.Name)).Scan(&s)
	if err == nil {
		_ = json.Unmarshal([]byte(s), &ex)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	d.Shell, d.BodyTemplate = ex.Shell, ex.Body
	d.Policy.Allow, d.Policy.AllowLeadingDash, d.Policy.AllowPrivate, d.Policy.AllowHTTP = ex.Allow, ex.AllowLeadingDash, ex.AllowPrivate, ex.AllowHTTP
	d.Policy.AllowVarSubcommand = ex.AllowVarSubcmd
	d.Policy.MaxOutputBytes, d.Policy.MaxRedirects = ex.MaxOutputBytes, ex.MaxRedirects
	if d.Policy.MaxOutputBytes == 0 {
		d.Policy.MaxOutputBytes = DefaultMaxOutput
	}
	if d.Policy.MaxRedirects == 0 {
		d.Policy.MaxRedirects = DefaultMaxRedirects
	}
	if !values {
		return nil
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT slot, key, is_secret, ref_type, COALESCE(ref,''), COALESCE(value,'') FROM tool_values WHERE tool_id=? ORDER BY slot, key`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v mcp.Value
		var sec int
		if err := rows.Scan(&v.Slot, &v.Key, &sec, &v.RefType, &v.Ref, &v.Literal); err != nil {
			return err
		}
		v.IsSecret = sec == 1
		if v.RefType == mcp.RefLiteral {
			v.Display = v.Literal
		} else {
			v.Literal, v.Display = "", "${"+v.Ref+"}"
			if v.RefType == mcp.RefKeychain {
				v.Display = "keychain:" + v.Ref
			}
		}
		d.Values = append(d.Values, v)
	}
	return rows.Err()
}

// List returns all global tools ordered by name.
func (r *Registry) List(ctx context.Context) ([]Definition, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+cols+` FROM tools WHERE scope='global' AND project_id IS NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	type pair struct {
		id int64
		d  *Definition
	}
	var ps []pair
	for rows.Next() {
		id, d, err := scanDef(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		ps = append(ps, pair{id, d})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	out := []Definition{}
	for _, p := range ps {
		if err := r.fill(ctx, p.id, p.d, true); err != nil {
			return nil, err
		}
		out = append(out, *p.d)
	}
	return out, nil
}

// Get returns one tool with value references (no script body).
func (r *Registry) Get(ctx context.Context, name string) (*Definition, error) {
	id, d, err := scanDef(r.DB.QueryRowContext(ctx, `SELECT `+cols+` FROM tools WHERE scope='global' AND project_id IS NULL AND name=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	if err := r.fill(ctx, id, d, true); err != nil {
		return nil, err
	}
	return d, nil
}

// Resolve is Get plus, for script tools, the verified script body so the
// definition can be passed to Run.
func (r *Registry) Resolve(ctx context.Context, name string) (*Definition, error) {
	d, err := r.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	// The DB is not trusted: a row edited behind our back (e.g. command=/bin/sh,
	// shell off, allowlist removed) must fail the same policy as a fresh Add.
	// Limit: whoever can write the DB already runs as the user; this is defense in depth.
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("저장된 도구 정의가 현재 정책을 위반합니다 (DB 변조 의심): %w", err)
	}
	if d.Kind == KindScript {
		p := filepath.Join(r.Home, filepath.FromSlash(d.ScriptPath))
		if !strings.HasPrefix(filepath.Clean(p), filepath.Join(r.Home, "scripts")+string(filepath.Separator)) {
			return nil, errors.New("스크립트 경로가 저장소 밖을 가리킵니다")
		}
		for _, c := range []struct {
			path string
			dir  bool
		}{{filepath.Dir(p), true}, {p, false}, {p + ".sha256", false}} {
			if err := checkPrivate(c.path, c.dir); err != nil {
				return nil, fmt.Errorf("스크립트 저장소 검사 실패: %w. forge tool add --replace 로 다시 등록하세요", err)
			}
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("스크립트 파일을 읽을 수 없습니다: %w", err)
		}
		side, err := os.ReadFile(p + ".sha256")
		if hashHex(b) != d.ScriptHash || err != nil || strings.TrimSpace(string(side)) != d.ScriptHash {
			return nil, errors.New("스크립트 파일이 변조되었거나 손상되었습니다 (해시 불일치: DB/파일/기록 해시). forge tool add --replace 로 다시 등록하세요")
		}
		d.ScriptBody = string(b)
	}
	return d, nil
}

// SetEnabled toggles a tool (idempotent).
func (r *Registry) SetEnabled(ctx context.Context, name string, enabled bool) error {
	res, err := r.DB.ExecContext(ctx, `UPDATE tools SET enabled=?, updated_at=? WHERE scope='global' AND project_id IS NULL AND name=?`, b2i(enabled), r.now(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return nil
}

// Remove deletes a tool, its values, policy extension and stored scripts.
func (r *Registry) Remove(ctx context.Context, name string) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM tools WHERE scope='global' AND project_id IS NULL AND name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, extKey(name)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.pruneScripts(name, "")
	return nil
}
