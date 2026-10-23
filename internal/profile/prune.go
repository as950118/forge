package profile

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// Stale activations (ADR-F0001 O-4). A project that was deleted or moved leaves
// its profile_activations rows behind: they affect nothing (no directory can
// match them) but cannot be reached with the normal commands either. Stale and
// PruneStale find and remove exactly those, and nothing else.
//
// A project is stale only when stat says its path does not exist (ENOENT or
// ENOTDIR). Any other error (permission denied, I/O error, a slow mount) is "not
// proven gone" and the project is kept. A moved project looks the same as a
// deleted one: that is why the listing shows the path and the last activation
// time and why the default is a dry run.

// StaleActivation is one activation of a stale project.
type StaleActivation struct {
	Client       string `json:"client"`
	Profile      string `json:"profile"`
	ProfileScope string `json:"profile_scope"`
	UpdatedAt    int64  `json:"updated_at"` // ms since the epoch
}

// StaleProject is a projects row whose path no longer exists and that has
// activations to prune and/or is referenced by nothing at all.
type StaleProject struct {
	ID          int64             `json:"-"`
	Path        string            `json:"path"`
	Activations []StaleActivation `json:"activations"`
	// LastActivation is the newest UpdatedAt (0 when there is none).
	LastActivation int64 `json:"last_activation"`
	// KeptProfiles are project profiles of this project; they are never deleted
	// (so the row is kept too).
	KeptProfiles []string `json:"kept_profiles,omitempty"`
	// RemoveRow reports that the projects row itself is removed (nothing else
	// references it once the activations are gone).
	RemoveRow bool `json:"remove_row"`
}

func gone(path string) bool {
	_, err := os.Stat(path)
	return err != nil && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR))
}

// Stale lists the stale projects (read-only; works on a *sql.DB, Tx or the
// read-only handle). Projects with no activation that other things still
// reference (profiles, skills, tools, MCP servers) are not listed: nothing to prune.
func Stale(ctx context.Context, q Querier) ([]StaleProject, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, path FROM projects ORDER BY path, id`)
	if err != nil {
		return nil, err
	}
	var cand []StaleProject
	for rows.Next() {
		var p StaleProject
		if err := rows.Scan(&p.ID, &p.Path); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if gone(p.Path) {
			cand = append(cand, p)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	var out []StaleProject
	for _, p := range cand {
		arows, err := q.QueryContext(ctx, `SELECT a.client, pr.name, pr.scope, a.updated_at FROM profile_activations a
			JOIN profiles pr ON pr.id = a.profile_id WHERE a.project_id = ? ORDER BY a.client`, p.ID)
		if err != nil {
			return nil, err
		}
		for arows.Next() {
			var a StaleActivation
			if err := arows.Scan(&a.Client, &a.Profile, &a.ProfileScope, &a.UpdatedAt); err != nil {
				_ = arows.Close()
				return nil, err
			}
			p.Activations = append(p.Activations, a)
			if a.UpdatedAt > p.LastActivation {
				p.LastActivation = a.UpdatedAt
			}
		}
		if err := arows.Err(); err != nil {
			_ = arows.Close()
			return nil, err
		}
		_ = arows.Close()
		prows, err := q.QueryContext(ctx, `SELECT name FROM profiles WHERE project_id = ? ORDER BY name`, p.ID)
		if err != nil {
			return nil, err
		}
		for prows.Next() {
			var n string
			if err := prows.Scan(&n); err != nil {
				_ = prows.Close()
				return nil, err
			}
			p.KeptProfiles = append(p.KeptProfiles, n)
		}
		if err := prows.Err(); err != nil {
			_ = prows.Close()
			return nil, err
		}
		_ = prows.Close()
		var other int
		if err := q.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM skills WHERE project_id = ?) + (SELECT count(*) FROM tools WHERE project_id = ?) +
			(SELECT count(*) FROM mcp_servers WHERE project_id = ?)`, p.ID, p.ID, p.ID).Scan(&other); err != nil {
			return nil, err
		}
		p.RemoveRow = len(p.KeptProfiles) == 0 && other == 0
		if len(p.Activations) == 0 && !p.RemoveRow {
			continue // nothing to prune and still referenced
		}
		out = append(out, p)
	}
	return out, nil
}

// PruneStale removes the activations of stale projects and the projects rows that
// nothing references afterwards, in ONE transaction. The staleness is decided
// again inside the transaction (a directory re-created since the listing is
// kept). Profiles are never deleted, and a row that a project profile, skill,
// tool or MCP server still references is never deleted (its cascade would take
// those with it). It returns what it removed.
func (s *Store) PruneStale(ctx context.Context) ([]StaleProject, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	list, err := Stale(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, p := range list {
		if _, err := tx.ExecContext(ctx, `DELETE FROM profile_activations WHERE project_id = ?`, p.ID); err != nil {
			return nil, err
		}
		if p.RemoveRow {
			if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, p.ID); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return list, nil
}
