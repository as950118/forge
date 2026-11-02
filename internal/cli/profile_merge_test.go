package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TASK-0032 AC-3: forge profile merge-projects.

type mergeEnv struct {
	a, b string // two spellings (symlink) of one directory
	home string
}

func newMergeEnv(t *testing.T) *mergeEnv {
	t.Helper()
	home := forgeHome(t)
	mustRun(t, "init")
	base, _ := filepath.EvalSymlinks(t.TempDir())
	a := filepath.Join(base, "proj")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(base, "link-to-proj")
	if err := os.Symlink(a, b); err != nil {
		t.Skip("symlinks unavailable")
	}
	c := pruneDB(t, home)
	for _, p := range []string{a, b} {
		if _, err := c.Exec(`INSERT INTO projects(path, name) VALUES (?, 'proj')`, p); err != nil {
			t.Fatal(err)
		}
	}
	return &mergeEnv{a: a, b: b, home: home}
}

func (m *mergeEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pruneDB(t, m.home).QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMergeProjectsDryRunThenYesMovesEverything(t *testing.T) {
	m := newMergeEnv(t)
	c := pruneDB(t, m.home)
	fromID := m.count(t, `SELECT id FROM projects WHERE path=?`, m.b)
	toID := m.count(t, `SELECT id FROM projects WHERE path=?`, m.a)
	// <from> owns a project profile (activated for opencode), a project skill and an activation;
	// <to> activates the same global profile for claude-code as <from> (identical: dropped)
	// and nothing for opencode.
	for _, q := range []string{
		`INSERT INTO profiles(id, scope, name) VALUES (10, 'global', 'shared')`,
		`INSERT INTO profiles(id, scope, project_id, name) VALUES (11, 'project', ` + itoa(fromID) + `, 'local')`,
		`INSERT INTO skills(scope, project_id, name, description) VALUES ('project', ` + itoa(fromID) + `, 'only-here', 'd')`,
		`INSERT INTO profile_activations(client, project_id, profile_id) VALUES ('opencode', ` + itoa(fromID) + `, 11)`,
		`INSERT INTO profile_activations(client, project_id, profile_id) VALUES ('claude-code', ` + itoa(fromID) + `, 10)`,
		`INSERT INTO profile_activations(client, project_id, profile_id) VALUES ('claude-code', ` + itoa(toID) + `, 10)`,
	} {
		if _, err := c.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	out := mustRun(t, "profile", "merge-projects", m.b, m.a)
	for _, w := range []string{"dry-run", "local", "only-here", "opencode -> local", "버릴 중복 활성화 1", "--yes"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	if n := m.count(t, `SELECT count(*) FROM projects`); n != 2 {
		t.Fatalf("dry-run deleted a row: %d", n)
	}
	mustRun(t, "profile", "merge-projects", m.b, m.a, "--yes")
	if n := m.count(t, `SELECT count(*) FROM projects`); n != 1 {
		t.Fatalf("projects rows: %d", n)
	}
	if n := m.count(t, `SELECT count(*) FROM profiles WHERE project_id=?`, toID); n != 1 {
		t.Errorf("project profile not moved: %d", n)
	}
	if n := m.count(t, `SELECT count(*) FROM skills WHERE project_id=? AND name='only-here'`, toID); n != 1 {
		t.Errorf("project skill not moved")
	}
	if n := m.count(t, `SELECT count(*) FROM profile_activations WHERE project_id=?`, toID); n != 2 {
		t.Errorf("want 2 activations on the kept row (claude-code kept once, opencode moved): %d", n)
	}
	if n := m.count(t, `SELECT count(*) FROM profiles`); n != 2 {
		t.Errorf("no profile may be deleted: %d", n)
	}
}

func TestMergeProjectsStopsOnConflictsWithoutChanging(t *testing.T) {
	m := newMergeEnv(t)
	c := pruneDB(t, m.home)
	fromID := itoa(m.count(t, `SELECT id FROM projects WHERE path=?`, m.b))
	toID := itoa(m.count(t, `SELECT id FROM projects WHERE path=?`, m.a))
	for _, q := range []string{
		`INSERT INTO profiles(id, scope, name) VALUES (10, 'global', 'p1'), (11, 'global', 'p2')`,
		`INSERT INTO profile_activations(client, project_id, profile_id) VALUES ('claude-code', ` + fromID + `, 10), ('claude-code', ` + toID + `, 11)`,
		`INSERT INTO tools(scope, project_id, name, kind, description, command) VALUES ('project', ` + fromID + `, 'dup', 'command', 'd', 'echo'), ('project', ` + toID + `, 'dup', 'command', 'd', 'echo')`,
	} {
		if _, err := c.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, args := range [][]string{{"profile", "merge-projects", m.b, m.a}, {"profile", "merge-projects", m.b, m.a, "--yes"}} {
		out, err := runErr(t, "", args...)
		if err == nil || !strings.Contains(err.Error(), "충돌") || !strings.Contains(out, "클라이언트 claude-code") || !strings.Contains(out, `도구 "dup"`) {
			t.Fatalf("%v: err=%v\n%s", args, err, out)
		}
	}
	if n := m.count(t, `SELECT count(*) FROM projects`); n != 2 {
		t.Fatalf("a conflict must change nothing: %d projects", n)
	}
	if n := m.count(t, `SELECT count(*) FROM profile_activations`); n != 2 {
		t.Fatalf("activations changed: %d", n)
	}
}

func TestMergeProjectsRefusesDifferentOrMissingDirectories(t *testing.T) {
	m := newMergeEnv(t)
	other := filepath.Join(filepath.Dir(m.a), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := pruneDB(t, m.home).Exec(`INSERT INTO projects(path, name) VALUES (?, 'o')`, other); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"profile", "merge-projects", other, m.a, "--yes"},
		{"profile", "merge-projects", m.a, filepath.Join(m.a, "nope"), "--yes"},
		{"profile", "merge-projects", m.a, m.a, "--yes"},
	} {
		if _, err := runErr(t, "", args...); err == nil {
			t.Errorf("%v must fail", args)
		}
	}
	if n := m.count(t, `SELECT count(*) FROM projects`); n != 3 {
		t.Fatalf("rows changed: %d", n)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
