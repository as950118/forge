package profile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
)

// Merging duplicate projects rows (TASK-0032 AC-3; docs/profile.md).
//
// Two rows of the same directory (spellings written before FindProject folded
// them) each own activations and project-scoped profiles, skills, tools and MCP
// servers. MergeProjects moves everything of the `from` row to the `to` row and
// deletes `from`, in ONE transaction, only when no rule is violated:
//
//   - both rows exist (exact stored path strings) and are different rows;
//   - both paths exist and are the SAME directory (os.SameFile); a row whose
//     directory is gone is the business of `forge profile prune-stale`;
//   - no conflict: both rows activate a DIFFERENT profile for the same client; or a
//     project-scoped profile, skill, tool or MCP server of the same name exists on
//     both rows (the unique indexes would collide, and silently dropping one side
//     would change what is exposed).
//
// Activations that point at the same profile on both rows are not a conflict: the
// duplicate of `from` is dropped. A conflict stops the merge before anything changes
// and is reported in full. ledger.project / sessions.project are history text, not
// foreign keys, and keep the spelling they were recorded with.

// MergePlan is what a merge does (or would do), or why it cannot.
type MergePlan struct {
	From, To   string   `json:"-"`
	FromID     int64    `json:"-"`
	ToID       int64    `json:"-"`
	Profiles   []string `json:"profiles"`
	Skills     []string `json:"skills"`
	Tools      []string `json:"tools"`
	MCP        []string `json:"mcp"`
	Moved      []string `json:"moved_activations"`   // "<client> -> <profile>" activations that move to the kept row
	Dropped    []string `json:"dropped_activations"` // identical activations of the removed row
	Conflicts  []string `json:"conflicts"`
	NotSameDir string   `json:"not_same_dir,omitempty"` // non-empty: why the two rows are not provably one directory
}

// OK reports that the merge may run.
func (p *MergePlan) OK() bool { return len(p.Conflicts) == 0 && p.NotSameDir == "" }

func projectRow(ctx context.Context, q Querier, path string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM projects WHERE path = ?`, path).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("projects 에 경로 %q 행이 없습니다 (forge doctor 가 보여 준 철자 그대로 입력하세요)", path)
	}
	return id, err
}

func names(ctx context.Context, q Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	return out, nil
}

// PlanMerge computes the merge without changing anything.
func PlanMerge(ctx context.Context, q Querier, from, to string) (*MergePlan, error) {
	p := &MergePlan{From: from, To: to}
	var err error
	if p.FromID, err = projectRow(ctx, q, from); err != nil {
		return nil, err
	}
	if p.ToID, err = projectRow(ctx, q, to); err != nil {
		return nil, err
	}
	if p.FromID == p.ToID {
		return nil, errors.New("<from> 과 <to> 가 같은 행입니다")
	}
	fa, ferr := os.Stat(from)
	ta, terr := os.Stat(to)
	switch {
	case ferr != nil || terr != nil:
		p.NotSameDir = "두 경로 중 하나가 존재하지 않습니다 (없어진 프로젝트는 forge profile prune-stale 로 정리하세요)"
	case !fa.IsDir() || !ta.IsDir() || !os.SameFile(fa, ta):
		p.NotSameDir = "두 경로가 같은 디렉터리가 아닙니다 (같은 디렉터리의 다른 철자만 합칠 수 있습니다)"
	}
	for _, x := range []struct {
		dst   *[]string
		query string
	}{
		{&p.Profiles, `SELECT name FROM profiles WHERE project_id = ? ORDER BY name`},
		{&p.Skills, `SELECT name FROM skills WHERE project_id = ? ORDER BY name`},
		{&p.Tools, `SELECT name FROM tools WHERE project_id = ? ORDER BY name`},
		{&p.MCP, `SELECT name FROM mcp_servers WHERE project_id = ? ORDER BY name`},
	} {
		if *x.dst, err = names(ctx, q, x.query, p.FromID); err != nil {
			return nil, err
		}
	}
	// name collisions
	for _, c := range []struct{ label, table string }{{"프로필", "profiles"}, {"스킬", "skills"}, {"도구", "tools"}, {"MCP 서버", "mcp_servers"}} {
		dup, err := names(ctx, q, `SELECT a.name FROM `+c.table+` a JOIN `+c.table+` b ON b.name = a.name AND b.project_id = ? WHERE a.project_id = ? ORDER BY a.name`, p.ToID, p.FromID)
		if err != nil {
			return nil, err
		}
		for _, n := range dup {
			p.Conflicts = append(p.Conflicts, fmt.Sprintf("%s %q 가 양쪽 행에 모두 있습니다 (이름 충돌: 하나의 이름을 바꾸거나 지운 뒤 다시 시도)", c.label, n))
		}
	}
	// activations
	rows, err := q.QueryContext(ctx, `SELECT a.client, a.profile_id, pf.name, b.profile_id, pb.name
		FROM profile_activations a JOIN profiles pf ON pf.id = a.profile_id
		LEFT JOIN profile_activations b ON b.client = a.client AND b.project_id = ?
		LEFT JOIN profiles pb ON pb.id = b.profile_id
		WHERE a.project_id = ? ORDER BY a.client`, p.ToID, p.FromID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var client, pfName string
		var pfID int64
		var bID sql.NullInt64
		var pbName sql.NullString
		if err := rows.Scan(&client, &pfID, &pfName, &bID, &pbName); err != nil {
			_ = rows.Close()
			return nil, err
		}
		switch {
		case !bID.Valid:
			p.Moved = append(p.Moved, client+" -> "+pfName)
		case bID.Int64 == pfID:
			p.Dropped = append(p.Dropped, client+" -> "+pfName)
		default:
			p.Conflicts = append(p.Conflicts, fmt.Sprintf("클라이언트 %s 의 활성 프로필이 다릅니다 (<from>: %q, <to>: %q). 하나를 forge profile unuse 로 해제한 뒤 다시 시도", client, pfName, pbName.String))
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	sort.Strings(p.Conflicts)
	return p, nil
}

// MergeProjects runs PlanMerge and, when it is clean, applies it in one
// transaction (nothing changes on any error). The plan is returned either way; a
// plan that is not OK was NOT applied.
func (s *Store) MergeProjects(ctx context.Context, from, to string) (*MergePlan, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := PlanMerge(ctx, tx, from, to)
	if err != nil {
		return nil, err
	}
	if !p.OK() {
		return p, nil
	}
	for _, q := range []string{
		`DELETE FROM profile_activations WHERE project_id = ?1 AND EXISTS (SELECT 1 FROM profile_activations b WHERE b.client = profile_activations.client AND b.project_id = ?2 AND b.profile_id = profile_activations.profile_id)`,
		`UPDATE profile_activations SET project_id = ?2 WHERE project_id = ?1`,
		`UPDATE profiles SET project_id = ?2 WHERE project_id = ?1`,
		`UPDATE skills SET project_id = ?2 WHERE project_id = ?1`,
		`UPDATE tools SET project_id = ?2 WHERE project_id = ?1`,
		`UPDATE mcp_servers SET project_id = ?2 WHERE project_id = ?1`,
	} {
		if _, err := tx.ExecContext(ctx, q, p.FromID, p.ToID); err != nil {
			return nil, fmt.Errorf("병합 실패(아무것도 바뀌지 않았습니다): %w", err)
		}
	}
	var left int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM profile_activations WHERE project_id=?1) + (SELECT count(*) FROM profiles WHERE project_id=?1)
		+ (SELECT count(*) FROM skills WHERE project_id=?1) + (SELECT count(*) FROM tools WHERE project_id=?1) + (SELECT count(*) FROM mcp_servers WHERE project_id=?1)`, p.FromID).Scan(&left); err != nil {
		return nil, err
	}
	if left != 0 { // never delete a row that still has children: its cascade would take them
		return nil, fmt.Errorf("병합 중단: <from> 행을 아직 %d개가 참조합니다 (아무것도 바뀌지 않았습니다)", left)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, p.FromID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}
