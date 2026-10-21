package skill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ScopeGlobal is the only scope implemented so far.
// TODO(project-scope): `--project` (skills.scope='project', projects row,
// <repo>/.forge/skills vendoring, forge.yaml reconcile) is deferred to a later
// task; ADR-F0001 D2 defines the schema but the manifest format is open (O-2).
const ScopeGlobal = "global"

// Sentinel errors.
var (
	ErrNotFound = errors.New("등록된 스킬이 없습니다")
)

// Registry stores skills in the DB plus immutable snapshots under Home/skills.
type Registry struct {
	DB   *sql.DB
	Home string
	Now  func() time.Time
}

// Version is one immutable stored version.
type Version struct {
	Version    string `json:"version"`
	Hash       string `json:"content_hash"`
	SourceType string `json:"source_type"`
	SourceURI  string `json:"source_uri,omitempty"`
	SourceRef  string `json:"source_ref,omitempty"`
	StorePath  string `json:"store_path"`
	CreatedAt  int64  `json:"created_at"`
	Current    bool   `json:"current"`
}

// Info is a registered skill.
type Info struct {
	Name           string    `json:"name"`
	Scope          string    `json:"scope"`
	Description    string    `json:"description"`
	Enabled        bool      `json:"enabled"`
	CurrentVersion string    `json:"current_version"`
	Versions       []Version `json:"versions,omitempty"`
	VersionCount   int       `json:"version_count"`
	UpdatedAt      int64     `json:"updated_at"`
}

// AddResult reports what Add did.
type AddResult struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Hash      string `json:"content_hash"`
	StorePath string `json:"store_path"`
	Status    string `json:"status"` // "added" | "new_version" | "unchanged"
	// PreviousVersion is the current version replaced by a "new_version" add.
	PreviousVersion string `json:"previous_version,omitempty"`
}

func (r *Registry) now() int64 {
	if r.Now != nil {
		return r.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

// Add stores a prepared skill as an immutable, content-addressed version.
// Re-adding identical content is a no-op ("unchanged").
func (r *Registry) Add(ctx context.Context, p *Prepared) (res *AddResult, err error) {
	hash, err := HashTree(p.Root)
	if err != nil {
		return nil, fmt.Errorf("해시 계산 실패: %w", err)
	}
	version := p.Meta.Version
	if version == "" {
		version = "h-" + hash[:12]
	}
	name := p.Meta.Name
	rel := filepath.ToSlash(filepath.Join("skills", name+"@"+version))
	res = &AddResult{Name: name, Version: version, Hash: hash, StorePath: rel}
	fmJSON, err := p.Meta.FrontmatterJSON()
	if err != nil {
		return nil, err
	}

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var skillID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM skills WHERE scope='global' AND project_id IS NULL AND name=?`, name).Scan(&skillID)
	isNewSkill := errors.Is(err, sql.ErrNoRows)
	if err != nil && !isNewSkill {
		return nil, err
	}
	if !isNewSkill {
		var exVersion, exStore string
		e := tx.QueryRowContext(ctx,
			`SELECT version, store_path FROM skill_versions WHERE skill_id=? AND content_hash=?`, skillID, hash).Scan(&exVersion, &exStore)
		if e == nil {
			res.Version, res.StorePath, res.Status = exVersion, exStore, "unchanged"
			return res, nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		var n int
		if e := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM skill_versions WHERE skill_id=? AND version=?`, skillID, version).Scan(&n); e != nil {
			return nil, e
		}
		if n > 0 {
			return nil, fmt.Errorf("스킬 %q 버전 %s 은 이미 다른 내용으로 등록되어 있습니다 (버전은 불변입니다: SKILL.md 의 version 을 올리세요)", name, version)
		}
	}

	now := r.now()
	if isNewSkill {
		rs, err := tx.ExecContext(ctx,
			`INSERT INTO skills(scope, project_id, name, description, enabled, created_at, updated_at) VALUES ('global', NULL, ?, ?, 1, ?, ?)`,
			name, p.Meta.Description, now, now)
		if err != nil {
			return nil, err
		}
		skillID, _ = rs.LastInsertId()
		res.Status = "added"
	} else {
		res.Status = "new_version"
		_ = tx.QueryRowContext(ctx,
			`SELECT COALESCE(v.version,'') FROM skills s LEFT JOIN skill_versions v ON v.id=s.current_version_id WHERE s.id=?`,
			skillID).Scan(&res.PreviousVersion)
	}
	rv, err := tx.ExecContext(ctx,
		`INSERT INTO skill_versions(skill_id, version, content_hash, source_type, source_uri, source_ref, store_path, frontmatter, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		skillID, version, hash, p.SourceType, nullStr(p.SourceURI), nullStr(p.SourceRef), rel, fmJSON, now)
	if err != nil {
		return nil, err
	}
	vid, _ := rv.LastInsertId()
	if _, err := tx.ExecContext(ctx,
		`UPDATE skills SET current_version_id=?, description=?, updated_at=? WHERE id=?`,
		vid, p.Meta.Description, now, skillID); err != nil {
		return nil, err
	}

	final := filepath.Join(r.Home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return nil, err
	}
	if _, statErr := os.Lstat(final); statErr == nil {
		// Orphan snapshot without a DB row (crashed earlier add): replace it.
		if err := RemoveAllForce(final); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(p.Root, final); err != nil {
		return nil, fmt.Errorf("스킬 파일 저장 실패: %w", err)
	}
	if err := makeReadOnly(final); err != nil {
		_ = RemoveAllForce(final)
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		_ = RemoveAllForce(final)
		return nil, err
	}
	return res, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// List returns all global skills ordered by name.
func (r *Registry) List(ctx context.Context) ([]Info, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT s.name, s.scope, s.description, s.enabled, COALESCE(v.version,''), s.updated_at,
		       (SELECT count(*) FROM skill_versions x WHERE x.skill_id = s.id)
		FROM skills s LEFT JOIN skill_versions v ON v.id = s.current_version_id
		WHERE s.scope='global' AND s.project_id IS NULL
		ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Info{}
	for rows.Next() {
		var i Info
		var en int
		if err := rows.Scan(&i.Name, &i.Scope, &i.Description, &en, &i.CurrentVersion, &i.UpdatedAt, &i.VersionCount); err != nil {
			return nil, err
		}
		i.Enabled = en == 1
		out = append(out, i)
	}
	return out, rows.Err()
}

// Get returns one skill with all versions (newest first).
func (r *Registry) Get(ctx context.Context, name string) (*Info, error) {
	var i Info
	var en int
	var curID sql.NullInt64
	var id int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, name, scope, description, enabled, current_version_id, updated_at
		FROM skills WHERE scope='global' AND project_id IS NULL AND name=?`, name).
		Scan(&id, &i.Name, &i.Scope, &i.Description, &en, &curID, &i.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	i.Enabled = en == 1
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, version, content_hash, source_type, COALESCE(source_uri,''), COALESCE(source_ref,''), store_path, created_at
		FROM skill_versions WHERE skill_id=? ORDER BY id DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v Version
		var vid int64
		if err := rows.Scan(&vid, &v.Version, &v.Hash, &v.SourceType, &v.SourceURI, &v.SourceRef, &v.StorePath, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.Current = curID.Valid && curID.Int64 == vid
		if v.Current {
			i.CurrentVersion = v.Version
		}
		i.Versions = append(i.Versions, v)
	}
	i.VersionCount = len(i.Versions)
	return &i, rows.Err()
}

// SetEnabled toggles a skill (idempotent).
func (r *Registry) SetEnabled(ctx context.Context, name string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := r.DB.ExecContext(ctx,
		`UPDATE skills SET enabled=?, updated_at=? WHERE scope='global' AND project_id IS NULL AND name=?`, v, r.now(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return nil
}

// Remove deletes the skill, all its versions and their stored files.
func (r *Registry) Remove(ctx context.Context, name string) error {
	info, err := r.Get(ctx, name)
	if err != nil {
		return err
	}
	if _, err := r.DB.ExecContext(ctx,
		`DELETE FROM skills WHERE scope='global' AND project_id IS NULL AND name=?`, name); err != nil {
		return fmt.Errorf("스킬 %q 삭제 실패 (프로필에서 고정(pin)되어 사용 중일 수 있습니다): %w", name, err)
	}
	var firstErr error
	for _, v := range info.Versions {
		if err := RemoveAllForce(filepath.Join(r.Home, filepath.FromSlash(v.StorePath))); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
