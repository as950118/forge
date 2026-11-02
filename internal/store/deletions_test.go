package store

import (
	"context"
	"database/sql"
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
