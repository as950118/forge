package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func mustExec(t *testing.T, c *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := c.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func seedRegistry(t *testing.T, c *sql.DB) {
	t.Helper()
	mustExec(t, c, `INSERT INTO projects(id, path, name) VALUES (1, '/work/app', 'app')`)
	mustExec(t, c, `INSERT INTO skills(id, scope, name, description) VALUES (1, 'global', 'review', 'd'), (2, 'global', 'lonely', 'd')`)
	mustExec(t, c, `INSERT INTO skills(id, scope, project_id, name, description) VALUES (3, 'project', 1, 'local-skill', 'd')`)
	mustExec(t, c, `INSERT INTO tools(id, scope, name, kind, description, command) VALUES (1, 'global', 'lint', 'command', 'd', 'echo')`)
	mustExec(t, c, `INSERT INTO mcp_servers(id, scope, name, transport, command) VALUES (1, 'global', 'docs', 'stdio', 'node')`)
	mustExec(t, c, `INSERT INTO profiles(id, scope, name) VALUES (1, 'global', 'dev'), (2, 'global', 'ci')`)
	mustExec(t, c, `INSERT INTO profiles(id, scope, project_id, name) VALUES (3, 'project', 1, 'dev')`)
	mustExec(t, c, `INSERT INTO profile_members(profile_id, kind, skill_id) VALUES (1,'skill',1), (2,'skill',1), (3,'skill',1), (1,'skill',3)`)
	mustExec(t, c, `INSERT INTO profile_members(profile_id, kind, tool_id) VALUES (1,'tool',1)`)
	mustExec(t, c, `INSERT INTO profile_members(profile_id, kind, mcp_server_id) VALUES (2,'mcp',1)`)
}

type deletion struct{ kind, name, scope, project, members string }

func deletions(t *testing.T, c *sql.DB) []deletion {
	t.Helper()
	rows, err := c.Query(`SELECT kind, name, scope, COALESCE(project,''), members FROM registry_deletions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []deletion
	for rows.Next() {
		var d deletion
		if err := rows.Scan(&d.kind, &d.name, &d.scope, &d.project, &d.members); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// TASK-0032 AC-1: deleting a skill / tool / MCP server records what it took out of
// which profiles, before the cascade removes the membership.
func TestMigration0005RecordsRegistryDeletions(t *testing.T) {
	c, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	seedRegistry(t, c)
	mustExec(t, c, `DELETE FROM skills WHERE name='review'`)
	mustExec(t, c, `DELETE FROM skills WHERE name='lonely'`)
	mustExec(t, c, `DELETE FROM tools WHERE name='lint'`)
	mustExec(t, c, `DELETE FROM mcp_servers WHERE name='docs'`)
	got := deletions(t, c)
	want := []deletion{
		{"skill", "review", "global", "", `["global:ci","global:dev","project:dev"]`},
		{"skill", "lonely", "global", "", `[]`},
		{"tool", "lint", "global", "", `["global:dev"]`},
		{"mcp", "docs", "global", "", `["global:ci"]`},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("deletion %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	var n int
	if err := c.QueryRow(`SELECT count(*) FROM profile_members`).Scan(&n); err != nil || n != 1 {
		t.Errorf("the cascade itself must be unchanged: %d members left (err %v), want 1", n, err)
	}
}

// A project-scoped skill removed by the cascade of its project is recorded once, with
// the project path and the profiles it was in, project profiles included (they are
// already gone when the child triggers run, so the projects trigger records it).
func TestMigration0005RecordsCascadeDeletion(t *testing.T) {
	c, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	seedRegistry(t, c)
	mustExec(t, c, `INSERT INTO profile_members(profile_id, kind, skill_id) VALUES (3,'skill',3)`) // project profile "dev" holds the project skill
	mustExec(t, c, `INSERT INTO tools(scope, project_id, name, kind, description, command) VALUES ('project', 1, 'proj-tool', 'command', 'd', 'echo')`)
	mustExec(t, c, `DELETE FROM projects WHERE id=1`)
	var skill, tool []deletion
	for _, d := range deletions(t, c) {
		if d.name == "local-skill" {
			skill = append(skill, d)
		}
		if d.name == "proj-tool" {
			tool = append(tool, d)
		}
	}
	if len(skill) != 1 || skill[0].scope != "project" || skill[0].project != "/work/app" || skill[0].members != `["global:dev","project:dev"]` {
		t.Fatalf("project skill: %+v (want one record with the path and both profiles)", skill)
	}
	if len(tool) != 1 || tool[0].project != "/work/app" {
		t.Fatalf("project tool: %+v", tool)
	}
}

// The audit table has no column that could hold a secret or a payload.
func TestMigration0005AuditHasNoSensitiveColumns(t *testing.T) {
	c, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rows, err := c.Query(`SELECT name FROM pragma_table_info('registry_deletions') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		cols = append(cols, n)
	}
	if got := strings.Join(cols, ","); got != "id,kind,name,scope,project,members,deleted_at" {
		t.Fatalf("columns: %s", got)
	}
}

// Upgrading a v4 database keeps every row and starts recording afterwards.
func TestMigration0005UpgradePreservesExistingDB(t *testing.T) {
	ctx := context.Background()
	c, _ := openRaw(t)
	if err := Migrate(ctx, c, realMigrationsUpTo(t, 4)); err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, c)
	mustExec(t, c, `INSERT INTO ledger(ts,kind,name,source) VALUES (1,'skill','review','hook')`)
	mustExec(t, c, `DELETE FROM skills WHERE name='lonely'`) // before the upgrade: nothing records it
	var has int
	_ = c.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='registry_deletions'`).Scan(&has)
	if has != 0 {
		t.Fatal("v4 database must not have the audit table")
	}
	if err := Migrate(ctx, c, realMigrationsUpTo(t, 5)); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"skills": 2, "tools": 1, "mcp_servers": 1, "profiles": 3, "profile_members": 6, "ledger": 1, "projects": 1} {
		var n int
		if err := c.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d rows after the upgrade (err %v), want %d", table, n, err, want)
		}
	}
	if d := deletions(t, c); len(d) != 0 {
		t.Fatalf("the upgrade must not invent deletions: %+v", d)
	}
	mustExec(t, c, `DELETE FROM tools WHERE name='lint'`)
	if d := deletions(t, c); len(d) != 1 || d[0].name != "lint" || d[0].members != `["global:dev"]` {
		t.Fatalf("after the upgrade deletions are recorded: %+v", d)
	}
}

// TASK-0034 AC-2: the MCP server rows get the same coverage as skills and tools —
// a project-scoped server removed by its project's cascade is recorded once, with the
// project path and its members (project profiles included), and the WHEN guard of the
// child trigger keeps the cascade from recording it a second time.
func TestMigration0005RecordsMCPCascadeDeletion(t *testing.T) {
	c, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	seedRegistry(t, c)
	mustExec(t, c, `INSERT INTO mcp_servers(id, scope, project_id, name, transport, command) VALUES (2, 'project', 1, 'proj-mcp', 'stdio', 'node')`)
	mustExec(t, c, `INSERT INTO mcp_servers(id, scope, project_id, name, transport, command) VALUES (3, 'project', 1, 'proj-mcp-lonely', 'stdio', 'node')`)
	mustExec(t, c, `INSERT INTO profile_members(profile_id, kind, mcp_server_id) VALUES (3,'mcp',2), (1,'mcp',2)`)
	mustExec(t, c, `DELETE FROM projects WHERE id=1`)
	byName := map[string][]deletion{}
	for _, d := range deletions(t, c) {
		byName[d.name] = append(byName[d.name], d)
	}
	if g := byName["proj-mcp"]; len(g) != 1 || g[0] != (deletion{"mcp", "proj-mcp", "project", "/work/app", `["global:dev","project:dev"]`}) {
		t.Fatalf("project mcp: %+v (want one record, path and both profiles)", g)
	}
	if g := byName["proj-mcp-lonely"]; len(g) != 1 || g[0].members != `[]` || g[0].project != "/work/app" {
		t.Fatalf("memberless project mcp: %+v", g)
	}
	var left int
	if err := c.QueryRow(`SELECT count(*) FROM mcp_servers WHERE project_id IS NOT NULL`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the cascade itself must be unchanged: %d project servers left (err %v)", left, err)
	}
}

// Deleting a project-scoped MCP server on its own (the project stays) is recorded by
// the child trigger with the project path; a project without children records nothing.
func TestMigration0005MCPDirectDeleteAndEmptyProject(t *testing.T) {
	c, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	seedRegistry(t, c)
	mustExec(t, c, `INSERT INTO mcp_servers(id, scope, project_id, name, transport, command) VALUES (2, 'project', 1, 'proj-mcp', 'stdio', 'node')`)
	mustExec(t, c, `INSERT INTO profile_members(profile_id, kind, mcp_server_id) VALUES (3,'mcp',2)`)
	mustExec(t, c, `DELETE FROM mcp_servers WHERE id=2`)
	if d := deletions(t, c); len(d) != 1 || d[0] != (deletion{"mcp", "proj-mcp", "project", "/work/app", `["project:dev"]`}) {
		t.Fatalf("direct delete: %+v", d)
	}
	mustExec(t, c, `INSERT INTO projects(id, path, name) VALUES (2, '/work/empty', 'empty')`)
	mustExec(t, c, `DELETE FROM projects WHERE id=2`)
	if d := deletions(t, c); len(d) != 1 {
		t.Fatalf("a project without registry items must record nothing: %+v", d)
	}
}

// The WHEN guard of the child triggers (N10): a child whose project row is already
// gone is not recorded by the child trigger. That state only exists with foreign keys
// OFF (the cascade is then skipped too), which Forge never uses; it pins the guard for
// all three kinds. Global rows (project_id IS NULL) are always recorded.
func TestMigration0005ChildTriggerGuardSkipsOrphanedProject(t *testing.T) {
	c, err := Open(context.Background(), filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	seedRegistry(t, c)
	mustExec(t, c, `INSERT INTO tools(id, scope, project_id, name, kind, description, command) VALUES (2, 'project', 1, 'proj-tool', 'command', 'd', 'echo')`)
	mustExec(t, c, `INSERT INTO mcp_servers(id, scope, project_id, name, transport, command) VALUES (2, 'project', 1, 'proj-mcp', 'stdio', 'node')`)
	conn, err := c.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exec := func(q string) {
		t.Helper()
		if _, err := conn.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`PRAGMA foreign_keys=OFF`)
	exec(`DELETE FROM projects WHERE id=1`) // projects trigger records the three children; nothing cascades
	if n := len(deletions(t, c)); n != 3 {
		t.Fatalf("project delete records %d, want 3 (skill, tool, mcp)", n)
	}
	exec(`DELETE FROM skills WHERE id=3`)
	exec(`DELETE FROM tools WHERE id=2`)
	exec(`DELETE FROM mcp_servers WHERE id=2`)
	if n := len(deletions(t, c)); n != 3 {
		t.Fatalf("child triggers must skip rows whose project is gone: %d records, want 3", n)
	}
	exec(`DELETE FROM mcp_servers WHERE id=1`) // global row: recorded
	if n := len(deletions(t, c)); n != 4 {
		t.Fatalf("a global row is always recorded: %d records, want 4", n)
	}
}

// TASK-0034 AC-3: with PRAGMA foreign_keys=OFF the ON DELETE CASCADE does not run, but
// the BEFORE DELETE trigger still records the members that "disappear". Forge always
// opens its database with foreign_keys ON (OpenRaw), so this only describes a
// hand-opened database; the record is then a list of memberships that are still there.
func TestMigration0005ForeignKeysOffRecordsMembersThatSurvive(t *testing.T) {
	c, err := Open(context.Background(), filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var fk int
	if err := c.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("Forge must open with foreign_keys ON: %d %v", fk, err)
	}
	seedRegistry(t, c)
	conn, err := c.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `DELETE FROM tools WHERE name='lint'`); err != nil {
		t.Fatal(err)
	}
	if d := deletions(t, c); len(d) != 1 || d[0].members != `["global:dev"]` {
		t.Fatalf("recorded: %+v", d)
	}
	var n int
	if err := conn.QueryRowContext(context.Background(), `SELECT count(*) FROM profile_members WHERE tool_id=1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("with foreign keys off the membership is not deleted: %d (err %v)", n, err)
	}
}
