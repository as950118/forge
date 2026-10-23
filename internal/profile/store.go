package profile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/heonjinjeong/forge/internal/textsafe"
)

// Store writes profiles. Every method is one transaction; the handle comes from
// store.Open (BEGIN IMMEDIATE), so concurrent writers queue instead of racing.
type Store struct {
	DB  *sql.DB
	Now func() time.Time
}

func (s *Store) now() int64 {
	if s.Now != nil {
		return s.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

// Ref names a profile: global, or inside one project (canonical path).
type Ref struct {
	Name    string
	Scope   string // ScopeGlobal | ScopeProject
	Project string // required for ScopeProject
}

func (r Ref) validate() error {
	if err := ValidName(r.Name); err != nil {
		return err
	}
	switch r.Scope {
	case ScopeGlobal:
		if r.Project != "" {
			return errors.New("전역 프로필에는 프로젝트를 지정할 수 없습니다")
		}
	case ScopeProject:
		if r.Project == "" || !filepath.IsAbs(r.Project) {
			return errors.New("프로젝트 프로필에는 프로젝트 경로가 필요합니다")
		}
	default:
		return fmt.Errorf("알 수 없는 범위: %q", textsafe.Display(r.Scope, 30))
	}
	return nil
}

// ensureProject returns the row of the project directory, reusing a row that
// refers to the same directory under another spelling (FindProject); a new row
// stores the on-disk spelling of the path.
func ensureProject(ctx context.Context, tx *sql.Tx, path string, now int64) (int64, error) {
	id, _, ok, err := FindProject(ctx, tx, path)
	if err != nil {
		return 0, err
	}
	if ok {
		return id, nil
	}
	path = canonicalCase(path)
	res, err := tx.ExecContext(ctx, `INSERT INTO projects(path, name, created_at) VALUES (?,?,?)`, path, filepath.Base(path), now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func projectID(ctx context.Context, q Querier, path string) (int64, bool, error) {
	id, _, ok, err := FindProject(ctx, q, path)
	return id, ok, err
}

// lookup finds a profile id for a Ref (read; works on a Tx).
func lookup(ctx context.Context, q Querier, r Ref) (int64, error) {
	var id int64
	var err error
	if r.Scope == ScopeGlobal {
		err = q.QueryRowContext(ctx, `SELECT id FROM profiles WHERE scope='global' AND project_id IS NULL AND name=?`, r.Name).Scan(&id)
	} else {
		pid, _, ok, perr := FindProject(ctx, q, r.Project)
		if perr != nil {
			return 0, perr
		}
		if !ok {
			return 0, fmt.Errorf("%w: %s", ErrNotFound, textsafe.Display(r.Name, 70))
		}
		err = q.QueryRowContext(ctx, `SELECT id FROM profiles WHERE scope='project' AND project_id=? AND name=?`, pid, r.Name).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, textsafe.Display(r.Name, 70))
	}
	return id, err
}

// Get loads a profile by Ref (read-only on whatever handle is given).
func Get(ctx context.Context, q Querier, r Ref) (*Detail, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	id, err := lookup(ctx, q, r)
	if err != nil {
		return nil, err
	}
	return GetDetail(ctx, q, id)
}

// Create makes an empty profile, or (fromCurrent) one holding a snapshot of what
// is exposed today: every enabled skill (following the current version), custom
// tool and MCP server, with the legacy "mcp.exposure.<server>.<client>"
// settings copied as member exposure. The legacy settings stay untouched.
func (s *Store) Create(ctx context.Context, r Ref, desc string, fromCurrent bool) (*Detail, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	if len(desc) > 500 {
		return nil, errors.New("설명은 500자 이하여야 합니다")
	}
	for _, c := range desc {
		if textsafe.NeedsEscape(c) {
			return nil, errors.New("설명에 제어/비가시 문자를 넣을 수 없습니다")
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now()
	var pid any
	if r.Scope == ScopeProject {
		id, err := ensureProject(ctx, tx, r.Project, now)
		if err != nil {
			return nil, err
		}
		pid = id
	}
	if _, err := lookup(ctx, tx, r); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrExists, textsafe.Display(r.Name, 70))
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var d sql.NullString
	if desc != "" {
		d = sql.NullString{String: desc, Valid: true}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO profiles(scope, project_id, name, description, created_at) VALUES (?,?,?,?,?)`,
		r.Scope, pid, r.Name, d, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if fromCurrent {
		if err := snapshotCurrent(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GetDetail(ctx, s.DB, id)
}

// snapshotCurrent copies the enabled registry items into profile id.
func snapshotCurrent(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO profile_members(profile_id, kind, skill_id)
		SELECT ?, 'skill', id FROM skills WHERE enabled=1 AND scope='global' AND project_id IS NULL AND current_version_id IS NOT NULL`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO profile_members(profile_id, kind, tool_id)
		SELECT ?, 'tool', id FROM tools WHERE enabled=1 AND scope='global' AND project_id IS NULL`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO profile_members(profile_id, kind, mcp_server_id)
		SELECT ?, 'mcp', id FROM mcp_servers WHERE enabled=1 AND scope='global' AND project_id IS NULL`, id); err != nil {
		return err
	}
	// Legacy exposure settings -> member exposure rows (only non-default values).
	_, err := tx.ExecContext(ctx, `INSERT INTO profile_member_exposure(member_id, client, exposure)
		SELECT m.id, c.id, st.value
		FROM profile_members m
		JOIN mcp_servers ms ON ms.id = m.mcp_server_id
		JOIN clients c ON 1=1
		JOIN settings st ON st.key = 'mcp.exposure.' || ms.name || '.' || c.id
		WHERE m.profile_id = ? AND st.value IN ('gateway','hidden')`, id)
	return err
}

// Delete removes a profile. A profile that is activated somewhere is refused:
// deleting it would silently switch those (client, project) pairs back to the
// legacy behaviour.
func (s *Store) Delete(ctx context.Context, r Ref) error {
	if err := r.validate(); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	id, err := lookup(ctx, tx, r)
	if err != nil {
		return err
	}
	acts, err := activationsOf(ctx, tx, id)
	if err != nil {
		return err
	}
	if len(acts) > 0 {
		return fmt.Errorf("프로필 %q 은(는) 활성화되어 있어 삭제하지 않았습니다 (%s). 먼저 forge profile unuse 로 해제하세요",
			textsafe.Display(r.Name, 70), describeActs(acts))
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM profiles WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func activationsOf(ctx context.Context, q Querier, profileID int64) ([]Activation, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.client, pr.path FROM profile_activations a LEFT JOIN projects pr ON pr.id=a.project_id WHERE a.profile_id=?`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activation
	for rows.Next() {
		var a Activation
		var p sql.NullString
		if err := rows.Scan(&a.Client, &p); err != nil {
			return nil, err
		}
		a.Project = p.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// ActivationsOfProfile lists where a profile is activated (read-only).
func ActivationsOfProfile(ctx context.Context, q Querier, profileID int64) ([]Activation, error) {
	return activationsOf(ctx, q, profileID)
}

func describeActs(acts []Activation) string {
	s := ""
	for i, a := range acts {
		if i > 0 {
			s += ", "
		}
		if i == 3 {
			s += fmt.Sprintf("외 %d건", len(acts)-3)
			break
		}
		where := "전역"
		if a.Project != "" {
			where = textsafe.Display(a.Project, 80)
		}
		s += a.Client + "@" + where
	}
	return s
}

// AddSpec describes a member to add or update.
type AddSpec struct {
	Kind, Name string
	// Exposure with Clients sets the per-client exposure ("" = leave as is).
	Exposure string
	Clients  []string
	// Pin fixes a skill to an immutable registered version (skill only).
	Pin string
}

// ValidExposure checks a mode for a member kind.
func ValidExposure(kind, mode string) error {
	switch mode {
	case ExposureDirect, ExposureGateway, ExposureHidden:
	default:
		return fmt.Errorf("노출 방식은 direct|gateway|hidden 중 하나여야 합니다: %q", textsafe.Display(mode, 30))
	}
	if kind == KindTool && mode == ExposureDirect {
		return errors.New("커스텀 도구는 게이트웨이로만 노출됩니다: 노출 방식은 gateway|hidden 이어야 합니다")
	}
	return nil
}

// Add adds a member (or updates the exposure/pin of an existing one).
func (s *Store) Add(ctx context.Context, r Ref, a AddSpec) error {
	if err := r.validate(); err != nil {
		return err
	}
	if a.Exposure != "" {
		if err := ValidExposure(a.Kind, a.Exposure); err != nil {
			return err
		}
		if len(a.Clients) == 0 {
			return errors.New("--exposure 에는 --client 가 필요합니다")
		}
	} else if len(a.Clients) > 0 {
		return errors.New("--client 는 --exposure 와 함께 써야 합니다")
	}
	if a.Pin != "" && a.Kind != KindSkill {
		return errors.New("--pin 은 스킬에만 쓸 수 있습니다")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	pid, err := lookup(ctx, tx, r)
	if err != nil {
		return err
	}
	clients, err := resolveClients(ctx, tx, a.Clients)
	if err != nil {
		return err
	}
	var col, tbl string
	switch a.Kind {
	case KindSkill:
		col, tbl = "skill_id", "skills"
	case KindTool:
		col, tbl = "tool_id", "tools"
	case KindMCP:
		col, tbl = "mcp_server_id", "mcp_servers"
	default:
		return fmt.Errorf("종류는 skill|tool|mcp 중 하나여야 합니다: %q", textsafe.Display(a.Kind, 30))
	}
	var refID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM `+tbl+` WHERE scope='global' AND project_id IS NULL AND name=?`, a.Name).Scan(&refID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("레지스트리에 %s %q 이(가) 없습니다", a.Kind, textsafe.Display(a.Name, 70))
	}
	if err != nil {
		return err
	}
	var pin any
	if a.Pin != "" {
		var vid int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM skill_versions WHERE skill_id=? AND version=?`, refID, a.Pin).Scan(&vid)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("스킬 %q 에 등록된 버전 %q 이(가) 없습니다 (forge skill show %s 로 버전 확인)",
				textsafe.Display(a.Name, 70), textsafe.Display(a.Pin, 70), textsafe.Display(a.Name, 70))
		}
		if err != nil {
			return err
		}
		pin = vid
	}
	var mid int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM profile_members WHERE profile_id=? AND `+col+`=?`, pid, refID).Scan(&mid)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx, `INSERT INTO profile_members(profile_id, kind, `+col+`, pinned_version_id) VALUES (?,?,?,?)`, pid, a.Kind, refID, pin)
		if err != nil {
			return err
		}
		mid, _ = res.LastInsertId()
	case err != nil:
		return err
	case pin != nil:
		if _, err := tx.ExecContext(ctx, `UPDATE profile_members SET pinned_version_id=? WHERE id=?`, pin, mid); err != nil {
			return err
		}
	}
	for _, c := range clients {
		if a.Exposure == DefaultExposure(a.Kind) {
			_, err = tx.ExecContext(ctx, `DELETE FROM profile_member_exposure WHERE member_id=? AND client=?`, mid, c)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO profile_member_exposure(member_id, client, exposure) VALUES (?,?,?)
				ON CONFLICT(member_id, client) DO UPDATE SET exposure=excluded.exposure`, mid, c, a.Exposure)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Remove removes a member from a profile.
func (s *Store) Remove(ctx context.Context, r Ref, kind, name string) error {
	if err := r.validate(); err != nil {
		return err
	}
	col := map[string]string{KindSkill: "skill_id", KindTool: "tool_id", KindMCP: "mcp_server_id"}[kind]
	tbl := map[string]string{KindSkill: "skills", KindTool: "tools", KindMCP: "mcp_servers"}[kind]
	if col == "" {
		return fmt.Errorf("종류는 skill|tool|mcp 중 하나여야 합니다: %q", textsafe.Display(kind, 30))
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	pid, err := lookup(ctx, tx, r)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM profile_members WHERE profile_id=? AND `+col+` IN
		(SELECT id FROM `+tbl+` WHERE scope='global' AND project_id IS NULL AND name=?)`, pid, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("프로필 %q 에 %s %q 멤버가 없습니다", textsafe.Display(r.Name, 70), kind, textsafe.Display(name, 70))
	}
	return tx.Commit()
}

// RemoveMemberID removes the member row with that id from the profile. It is the
// way to clean up a damaged member (a row that points at a deleted skill, tool or
// server has no name that `remove --skill|--tool|--mcp` could use). The id must
// belong to this profile.
func (s *Store) RemoveMemberID(ctx context.Context, r Ref, id int64) error {
	if err := r.validate(); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	pid, err := lookup(ctx, tx, r)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM profile_members WHERE id=? AND profile_id=?`, id, pid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("프로필 %q 에 멤버 번호 %d 이(가) 없습니다 (forge profile show %s 로 확인)", textsafe.Display(r.Name, 70), id, textsafe.Display(r.Name, 70))
	}
	return tx.Commit()
}

// Membership is a profile that holds a registry item, for the warning that
// removing the item (ON DELETE CASCADE) takes it out of these profiles.
type Membership struct {
	Profile string   `json:"profile"`
	Scope   string   `json:"scope"`
	Project string   `json:"project,omitempty"`
	Active  []string `json:"active,omitempty"` // "client@전역" / "client@<project>"
}

// MembershipsOf lists the profiles that contain the registry item (kind, name).
func MembershipsOf(ctx context.Context, q Querier, kind, name string) ([]Membership, error) {
	col := map[string]string{KindSkill: "skill_id", KindTool: "tool_id", KindMCP: "mcp_server_id"}[kind]
	tbl := map[string]string{KindSkill: "skills", KindTool: "tools", KindMCP: "mcp_servers"}[kind]
	if col == "" {
		return nil, fmt.Errorf("종류는 skill|tool|mcp 중 하나여야 합니다: %q", textsafe.Display(kind, 30))
	}
	rows, err := q.QueryContext(ctx, `SELECT p.id, p.name, p.scope, pr.path FROM profile_members m
		JOIN `+tbl+` i ON i.id = m.`+col+`
		JOIN profiles p ON p.id = m.profile_id LEFT JOIN projects pr ON pr.id = p.project_id
		WHERE i.scope='global' AND i.project_id IS NULL AND i.name = ? ORDER BY p.scope, pr.path, p.name`, name)
	if err != nil {
		return nil, err
	}
	var out []Membership
	var ids []int64
	for rows.Next() {
		var m Membership
		var id int64
		var proj sql.NullString
		if err := rows.Scan(&id, &m.Profile, &m.Scope, &proj); err != nil {
			_ = rows.Close()
			return nil, err
		}
		m.Project = proj.String
		out = append(out, m)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	for i, id := range ids {
		acts, err := activationsOf(ctx, q, id)
		if err != nil {
			return nil, err
		}
		for _, a := range acts {
			where := "전역"
			if a.Project != "" {
				where = a.Project
			}
			out[i].Active = append(out[i].Active, a.Client+"@"+where)
		}
	}
	return out, nil
}

func resolveClients(ctx context.Context, q Querier, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	all, err := Clients(ctx, q)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, c := range all {
		known[c] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range in {
		if c == "all" {
			for _, k := range all {
				if !seen[k] {
					seen[k] = true
					out = append(out, k)
				}
			}
			continue
		}
		if !known[c] {
			return nil, fmt.Errorf("알 수 없는 클라이언트: %q (지원: %s, all)", textsafe.Display(c, 40), joinClients(all))
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// Use activates a profile for the clients. project == "" activates it for all
// projects (the client's global default); otherwise only for that project, where
// a project profile named name wins over a global one. A project-scoped profile
// can only be activated for its own project.
func (s *Store) Use(ctx context.Context, name, project string, clients []string) (*Meta, []string, error) {
	if err := ValidName(name); err != nil {
		return nil, nil, err
	}
	if len(clients) == 0 {
		return nil, nil, errors.New("--client 가 필요합니다 (claude-code|opencode|all)")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	cl, err := resolveClients(ctx, tx, clients)
	if err != nil {
		return nil, nil, err
	}
	var id int64
	found := false
	if project != "" {
		if pid, err := lookup(ctx, tx, Ref{Name: name, Scope: ScopeProject, Project: project}); err == nil {
			id, found = pid, true
		} else if !errors.Is(err, ErrNotFound) {
			return nil, nil, err
		}
	}
	if !found {
		gid, err := lookup(ctx, tx, Ref{Name: name, Scope: ScopeGlobal})
		if err != nil {
			if errors.Is(err, ErrNotFound) && project == "" {
				var n int
				_ = tx.QueryRowContext(ctx, `SELECT count(*) FROM profiles WHERE scope='project' AND name=?`, name).Scan(&n)
				if n > 0 {
					return nil, nil, fmt.Errorf("프로필 %q 은(는) 프로젝트 프로필이라 --project-dir 로 그 프로젝트를 지정해야 활성화할 수 있습니다", textsafe.Display(name, 70))
				}
			}
			return nil, nil, err
		}
		id = gid
	}
	now := s.now()
	var pidArg any
	if project != "" {
		p, err := ensureProject(ctx, tx, project, now)
		if err != nil {
			return nil, nil, err
		}
		pidArg = p
	}
	for _, c := range cl {
		if _, err := tx.ExecContext(ctx, `DELETE FROM profile_activations WHERE client=? AND COALESCE(project_id,0)=COALESCE(?,0)`, c, pidArg); err != nil {
			return nil, nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO profile_activations(client, project_id, profile_id, updated_at) VALUES (?,?,?,?)`, c, pidArg, id, now); err != nil {
			return nil, nil, err
		}
	}
	d, err := GetDetail(ctx, tx, id)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	m := d.Meta
	return &m, cl, nil
}

// Unuse removes the activation of the clients for project ("" = the global default).
// It returns the clients that had one.
func (s *Store) Unuse(ctx context.Context, project string, clients []string) ([]string, error) {
	if len(clients) == 0 {
		return nil, errors.New("--client 가 필요합니다 (claude-code|opencode|all)")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	cl, err := resolveClients(ctx, tx, clients)
	if err != nil {
		return nil, err
	}
	var pidArg any
	if project != "" {
		id, ok, err := projectID(ctx, tx, project)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, tx.Commit()
		}
		pidArg = id
	}
	var removed []string
	for _, c := range cl {
		res, err := tx.ExecContext(ctx, `DELETE FROM profile_activations WHERE client=? AND COALESCE(project_id,0)=COALESCE(?,0)`, c, pidArg)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed = append(removed, c)
		}
	}
	return removed, tx.Commit()
}
