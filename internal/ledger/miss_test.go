package ledger

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/heonjinjeong/forge/db"
	"github.com/heonjinjeong/forge/internal/store"
)

// A user-attached MCP server (not in the Forge registry) is not a miss, and an
// explicit found=false from the gateway still is. v_misses separates MCP servers.
func TestMissUserAttachedMCPIsNotAMiss(t *testing.T) {
	rec, _ := newRec(t)
	ctx := context.Background()
	for _, e := range []Event{
		{Kind: KindMCP, Name: "list", MCPServer: "user_attached", Source: SourceHook},
		{Kind: KindSkill, Name: "no-such-skill", Source: SourceHook},
		{Kind: KindMCP, Name: "ghost", MCPServer: "srv_a", Source: SourceGateway, Found: Bool(false)},
		{Kind: KindMCP, Name: "ghost", MCPServer: "srv_a", Source: SourceGateway, Found: Bool(false)},
		{Kind: KindMCP, Name: "ghost", MCPServer: "srv_b", Source: SourceGateway, Found: Bool(false)},
	} {
		if _, err := rec.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := rec.Misses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, m := range ms {
		got[m.Kind+"/"+m.MCPServer+"/"+m.Name] = m.Count
	}
	want := map[string]int{"skill//no-such-skill": 1, "mcp/srv_a/ghost": 2, "mcp/srv_b/ghost": 1}
	if len(got) != len(want) {
		t.Fatalf("misses=%v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s=%d want %d (all: %v)", k, got[k], v, got)
		}
	}
}

// Upgrading a database created with migrations 0001+0002: old false-positive rows
// are repaired and the view gains the mcp_server column.
func TestMigration0003UpgradesExistingDB(t *testing.T) {
	ctx := context.Background()
	oldFS := fstest.MapFS{}
	for _, n := range []string{"0001_init.sql", "0002_mcp_tool_approvals.sql"} {
		b, err := fs.ReadFile(db.Migrations(), n)
		if err != nil {
			t.Fatal(err)
		}
		oldFS[n] = &fstest.MapFile{Data: b}
	}
	path := filepath.Join(t.TempDir(), "forge.db")
	conn, err := store.OpenRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := store.Migrate(ctx, conn, oldFS); err != nil {
		t.Fatal(err)
	}
	exec(t, conn, `INSERT INTO mcp_servers(id,scope,name,transport,command) VALUES (1,'global','reg','stdio','x')`)
	ins := `INSERT INTO ledger(ts,kind,name,mcp_server,source,found,mcp_server_id) VALUES (?,?,?,?,?,?,?)`
	exec(t, conn, ins, 1, "mcp", "t", "user_attached", "hook", 0, nil) // false positive -> NULL
	exec(t, conn, ins, 2, "mcp", "t", "gw", "gateway", 0, nil)         // explicit gateway miss stays
	exec(t, conn, ins, 3, "skill", "nope", nil, "hook", 0, nil)        // real miss stays
	exec(t, conn, ins, 4, "mcp", "t", "reg", "hook", 0, 1)             // linked to registry stays
	var cols int
	_ = conn.QueryRow(`SELECT count(*) FROM pragma_table_info('v_misses') WHERE name='mcp_server'`).Scan(&cols)
	if cols != 0 {
		t.Fatal("v_misses already has mcp_server before the upgrade")
	}

	if err := store.Migrate(ctx, conn, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	var found *int
	if err := conn.QueryRow(`SELECT found FROM ledger WHERE ts=1`).Scan(&found); err != nil || found != nil {
		t.Errorf("user-attached row found=%v err=%v, want NULL", found, err)
	}
	rows, err := conn.Query(`SELECT kind, COALESCE(mcp_server,''), name FROM v_misses ORDER BY kind, mcp_server, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var k, sv, n string
		if err := rows.Scan(&k, &sv, &n); err != nil {
			t.Fatal(err)
		}
		got = append(got, k+"/"+sv+"/"+n)
	}
	want := []string{"mcp/gw/t", "mcp/reg/t", "skill//nope"}
	if len(got) != len(want) {
		t.Fatalf("v_misses=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("v_misses=%v want %v", got, want)
		}
	}
}

// Upgrading from 0001-0003: v_ledger_calls merges ok failure-first (TASK-0021 AC-2).
func TestMigration0004UpgradesMergedOK(t *testing.T) {
	ctx := context.Background()
	oldFS := fstest.MapFS{}
	for _, n := range []string{"0001_init.sql", "0002_mcp_tool_approvals.sql", "0003_misses_mcp_server.sql"} {
		b, err := fs.ReadFile(db.Migrations(), n)
		if err != nil {
			t.Fatal(err)
		}
		oldFS[n] = &fstest.MapFile{Data: b}
	}
	conn, err := store.OpenRaw(filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := store.Migrate(ctx, conn, oldFS); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO ledger(ts,kind,name,client,session_id,tool_use_id,source,ok) VALUES (1,'tool','t','claude-code','s','u',?,?)`
	exec(t, conn, ins, "hook", 1)
	exec(t, conn, ins, "gateway", 0)
	var ok int
	if err := conn.QueryRow(`SELECT ok FROM v_ledger_calls`).Scan(&ok); err != nil || ok != 1 {
		t.Fatalf("before the upgrade the view used MAX(ok): ok=%d err=%v", ok, err)
	}
	if err := store.Migrate(ctx, conn, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(`SELECT ok FROM v_ledger_calls`).Scan(&ok); err != nil || ok != 0 {
		t.Errorf("after 0004 the merged ok must be 0 (failure first): ok=%d err=%v", ok, err)
	}
}
