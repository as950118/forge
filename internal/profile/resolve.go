// Package profile implements Forge profiles (TASK-0012): named allow-lists of
// skills, custom tools and MCP servers with a per-client exposure, plus the
// activation of a profile for a (client, project) pair. Storage is the schema
// of migration 0001 (profiles, profile_members, profile_member_exposure,
// profile_activations); no schema change was needed.
//
// PRECEDENCE (the one rule every consumer uses, see Resolve):
//
//  1. a profile activated for (client, this project)         -> rule "project"
//  2. else a profile activated for (client, all projects)    -> rule "global"
//  3. else no profile: the legacy behaviour (enabled items +
//     "mcp.exposure.<server>.<client>" settings)             -> rule "legacy"
//
// With a profile the meaning is an ALLOW LIST: an item that is not a member of
// the active profile is not exposed, even when it is enabled. Without a profile
// everything enabled is exposed (the old behaviour). The enabled flag still
// wins over membership: a disabled item is never exposed.
package profile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/as950118/forge/internal/textsafe"
)

// Rules (which precedence step applied).
const (
	RuleProject = "project"
	RuleGlobal  = "global"
	RuleLegacy  = "legacy"
)

// Exposure modes, same words as mcp.Exposure*.
const (
	ExposureDirect  = "direct"
	ExposureGateway = "gateway"
	ExposureHidden  = "hidden"
)

// Member kinds.
const (
	KindSkill = "skill"
	KindTool  = "tool"
	KindMCP   = "mcp"
)

// Scopes of a profile.
const (
	ScopeGlobal  = "global"
	ScopeProject = "project"
)

// Sentinel errors.
var (
	ErrNotFound = errors.New("프로필을 찾을 수 없습니다")
	ErrExists   = errors.New("같은 이름의 프로필이 이미 있습니다")
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidName checks a profile name (also used before anything is printed).
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("프로필 이름이 올바르지 않습니다: %q (영문·숫자·._- , 1~64자, 영문/숫자로 시작)", textsafe.Display(name, 70))
	}
	return nil
}

// Querier is satisfied by *sql.DB and *sql.Tx (and by the read-only handle).
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DefaultExposure is the exposure of a member without an explicit row.
func DefaultExposure(kind string) string {
	if kind == KindTool {
		return ExposureGateway // custom tools are only reachable through the gateway
	}
	return ExposureDirect
}

// Meta describes one profile.
type Meta struct {
	ID          int64  `json:"-"`
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	Project     string `json:"project,omitempty"` // project path for scope=project
	Description string `json:"description,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	Members     int    `json:"members"`
}

// Member is one profile member with its explicit per-client exposure rows.
type Member struct {
	// ID is the profile_members row id (`forge profile remove --member-id`).
	ID       int64             `json:"id"`
	Kind     string            `json:"kind"`
	Name     string            `json:"name"`
	Enabled  bool              `json:"enabled"`
	Exposure map[string]string `json:"exposure,omitempty"` // explicit rows only
	// PinnedVersion is the immutable skill version id string ("" = follow current).
	PinnedVersion string `json:"pinned_version,omitempty"`
	// Broken marks rows that point at nothing (only a damaged database has them).
	Broken string `json:"broken,omitempty"`
	pinID  int64
}

// ExposureFor returns the effective exposure of the member for a client.
func (m Member) ExposureFor(client string) string {
	if e, ok := m.Exposure[client]; ok {
		return e
	}
	return DefaultExposure(m.Kind)
}

// Detail is a profile with all members.
type Detail struct {
	Meta
	MemberList []Member `json:"member_list"`
}

// Resolved is the outcome of Resolve for one client.
type Resolved struct {
	Rule    string
	Profile *Meta
	// Skills/Tools/MCP map the member name to its exposure for the client.
	Skills map[string]string
	Tools  map[string]string
	MCP    map[string]string
	// Pins maps a skill name to the pinned version string.
	Pins map[string]string
}

// Active reports whether a profile (not the legacy behaviour) applies.
func (r *Resolved) Active() bool { return r != nil && r.Rule != RuleLegacy }

// Describe is the one-line statement of the applied rule (plan, doctor).
func (r *Resolved) Describe(client string) string {
	if !r.Active() {
		return "프로필 없음: 레거시 동작(활성화된 항목 전체 + mcp.exposure.* 설정)을 적용합니다"
	}
	where := "이 프로젝트에 활성화"
	if r.Rule == RuleGlobal {
		where = "전역 활성화"
	}
	return fmt.Sprintf("프로필 %q 적용 (%s, 클라이언트 %s): 프로필에 없는 항목은 노출되지 않습니다(허용 목록)", textsafe.Display(r.Profile.Name, 70), where, client)
}

// ProjectRoot canonicalizes a directory the way sync does: absolute, symlinks
// resolved, then the nearest ancestor holding ".git" (else the directory itself).
func ProjectRoot(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("프로젝트 디렉터리를 찾을 수 없습니다: %w", err)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", errors.New("프로젝트 디렉터리가 디렉터리가 아닙니다")
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return abs, nil
		}
	}
}

// Resolve applies the precedence rule for (client, project). project is a
// canonical path (ProjectRoot) or "". It only reads. Errors mean the database
// could not be read: callers must then fail closed (expose nothing new).
func Resolve(ctx context.Context, q Querier, client, project string) (*Resolved, error) {
	projID, projFound := int64(0), false
	if project != "" {
		var err error
		if projID, _, projFound, err = FindProject(ctx, q, project); err != nil {
			return nil, err
		}
	}
	for _, step := range []struct {
		rule string
		sqlq string
		args []any
	}{
		{RuleProject, `SELECT profile_id FROM profile_activations WHERE client = ? AND project_id = ?`, []any{client, projID}},
		{RuleGlobal, `SELECT profile_id FROM profile_activations WHERE client = ? AND project_id IS NULL`, []any{client}},
	} {
		if step.rule == RuleProject && (project == "" || !projFound) {
			continue
		}
		var id int64
		err := q.QueryRowContext(ctx, step.sqlq, step.args...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		d, err := GetDetail(ctx, q, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return d.resolved(step.rule, client), nil
	}
	return &Resolved{Rule: RuleLegacy}, nil
}

func (d *Detail) resolved(rule, client string) *Resolved {
	meta := d.Meta
	r := &Resolved{Rule: rule, Profile: &meta,
		Skills: map[string]string{}, Tools: map[string]string{}, MCP: map[string]string{}, Pins: map[string]string{}}
	for _, m := range d.MemberList {
		if m.Broken != "" {
			continue
		}
		switch m.Kind {
		case KindSkill:
			r.Skills[m.Name] = m.ExposureFor(client)
			if m.PinnedVersion != "" {
				r.Pins[m.Name] = m.PinnedVersion
			}
		case KindTool:
			r.Tools[m.Name] = m.ExposureFor(client)
		case KindMCP:
			r.MCP[m.Name] = m.ExposureFor(client)
		}
	}
	return r
}

// GetDetail loads one profile by id with its members (read-only).
func GetDetail(ctx context.Context, q Querier, id int64) (*Detail, error) {
	d := &Detail{}
	var proj sql.NullString
	var desc sql.NullString
	err := q.QueryRowContext(ctx, `SELECT p.id, p.name, p.scope, pr.path, p.description, p.created_at
		FROM profiles p LEFT JOIN projects pr ON pr.id = p.project_id WHERE p.id = ?`, id).
		Scan(&d.ID, &d.Name, &d.Scope, &proj, &desc, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Project, d.Description = proj.String, desc.String
	rows, err := q.QueryContext(ctx, `SELECT m.id, m.kind, s.name, s.enabled, t.name, t.enabled, ms.name, ms.enabled,
			m.pinned_version_id, v.version, v.skill_id, m.skill_id
		FROM profile_members m
		LEFT JOIN skills s ON s.id = m.skill_id
		LEFT JOIN tools t ON t.id = m.tool_id
		LEFT JOIN mcp_servers ms ON ms.id = m.mcp_server_id
		LEFT JOIN skill_versions v ON v.id = m.pinned_version_id
		WHERE m.profile_id = ? ORDER BY m.kind, m.id`, id)
	if err != nil {
		return nil, err
	}
	byID := map[int64]int{}
	for rows.Next() {
		var mid int64
		var kind string
		var sn, tn, mn sql.NullString
		var se, te, me sql.NullInt64
		var pin, pinSkill, memSkill sql.NullInt64
		var pv sql.NullString
		if err := rows.Scan(&mid, &kind, &sn, &se, &tn, &te, &mn, &me, &pin, &pv, &pinSkill, &memSkill); err != nil {
			_ = rows.Close()
			return nil, err
		}
		m := Member{ID: mid, Kind: kind}
		switch kind {
		case KindSkill:
			m.Name, m.Enabled = sn.String, se.Int64 == 1
			if !sn.Valid {
				m.Broken = "스킬 행이 없습니다"
			}
		case KindTool:
			m.Name, m.Enabled = tn.String, te.Int64 == 1
			if !tn.Valid {
				m.Broken = "도구 행이 없습니다"
			}
		case KindMCP:
			m.Name, m.Enabled = mn.String, me.Int64 == 1
			if !mn.Valid {
				m.Broken = "MCP 서버 행이 없습니다"
			}
		}
		if pin.Valid {
			m.pinID = pin.Int64
			switch {
			case !pv.Valid:
				m.Broken = "고정한 버전 행이 없습니다"
			case !pinSkill.Valid || !memSkill.Valid || pinSkill.Int64 != memSkill.Int64:
				m.Broken = "고정한 버전이 이 스킬의 버전이 아닙니다"
			default:
				m.PinnedVersion = pv.String
			}
		}
		byID[mid] = len(d.MemberList)
		d.MemberList = append(d.MemberList, m)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	erows, err := q.QueryContext(ctx, `SELECT e.member_id, e.client, e.exposure FROM profile_member_exposure e
		JOIN profile_members m ON m.id = e.member_id WHERE m.profile_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer erows.Close()
	for erows.Next() {
		var mid int64
		var c, e string
		if err := erows.Scan(&mid, &c, &e); err != nil {
			return nil, err
		}
		if i, ok := byID[mid]; ok {
			if d.MemberList[i].Exposure == nil {
				d.MemberList[i].Exposure = map[string]string{}
			}
			d.MemberList[i].Exposure[c] = e
		}
	}
	d.Members = len(d.MemberList)
	return d, erows.Err()
}

// SortMembers orders members by kind then name (stable display).
func SortMembers(ms []Member) {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Kind != ms[j].Kind {
			return ms[i].Kind < ms[j].Kind
		}
		return ms[i].Name < ms[j].Name
	})
}

// Activation is one row of profile_activations.
type Activation struct {
	Client  string `json:"client"`
	Project string `json:"project,omitempty"` // "" = all projects
	Profile string `json:"profile"`
	Scope   string `json:"profile_scope"`
}

// Activations lists every activation (read-only).
func Activations(ctx context.Context, q Querier) ([]Activation, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.client, pr.path, p.name, p.scope
		FROM profile_activations a JOIN profiles p ON p.id = a.profile_id
		LEFT JOIN projects pr ON pr.id = a.project_id ORDER BY a.client, pr.path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activation
	for rows.Next() {
		var a Activation
		var proj sql.NullString
		if err := rows.Scan(&a.Client, &proj, &a.Profile, &a.Scope); err != nil {
			return nil, err
		}
		a.Project = proj.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListMeta lists all profiles (read-only).
func ListMeta(ctx context.Context, q Querier) ([]Meta, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.id, p.name, p.scope, pr.path, p.description, p.created_at,
		(SELECT count(*) FROM profile_members m WHERE m.profile_id = p.id)
		FROM profiles p LEFT JOIN projects pr ON pr.id = p.project_id ORDER BY p.scope, pr.path, p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Meta
	for rows.Next() {
		var m Meta
		var proj, desc sql.NullString
		if err := rows.Scan(&m.ID, &m.Name, &m.Scope, &proj, &desc, &m.CreatedAt, &m.Members); err != nil {
			return nil, err
		}
		m.Project, m.Description = proj.String, desc.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// Clients lists the known client ids.
func Clients(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM clients ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func joinClients(ids []string) string { return strings.Join(ids, ", ") }
