package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/heonjinjeong/forge/db"
)

func openRaw(t *testing.T) (*sql.DB, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.db")
	c, err := OpenRaw(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, p
}

func mfs(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for k, v := range files {
		m[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return m
}

const m1 = "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at INTEGER);"

func TestMigrateAppliesAndIsIdempotent(t *testing.T) {
	c, _ := openRaw(t)
	ctx := context.Background()
	fsys := mfs(map[string]string{"0001_init.sql": m1, "0002_more.sql": "CREATE TABLE t2(x);"})
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, c, fsys); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = c.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n)
	if n != 2 {
		t.Errorf("rows=%d", n)
	}
}

func TestMigrateChecksumTamper(t *testing.T) {
	c, _ := openRaw(t)
	ctx := context.Background()
	if err := Migrate(ctx, c, mfs(map[string]string{"0001_init.sql": m1})); err != nil {
		t.Fatal(err)
	}
	err := Migrate(ctx, c, mfs(map[string]string{"0001_init.sql": m1 + " -- tampered"}))
	if !errors.Is(err, ErrChecksum) {
		t.Errorf("err=%v", err)
	}
}

func TestMigrateRefusesDowngrade(t *testing.T) {
	c, _ := openRaw(t)
	ctx := context.Background()
	newer := mfs(map[string]string{"0001_init.sql": m1, "0002_more.sql": "CREATE TABLE t2(x);"})
	if err := Migrate(ctx, c, newer); err != nil {
		t.Fatal(err)
	}
	err := Migrate(ctx, c, mfs(map[string]string{"0001_init.sql": m1}))
	if !errors.Is(err, ErrDowngrade) {
		t.Errorf("err=%v", err)
	}
}

func TestMigrateTransactionPerFile(t *testing.T) {
	c, _ := openRaw(t)
	ctx := context.Background()
	fsys := mfs(map[string]string{"0001_init.sql": m1, "0002_bad.sql": "CREATE TABLE ok(x); CREATE TABLE ok(x);"})
	if err := Migrate(ctx, c, fsys); err == nil {
		t.Fatal("expected failure")
	}
	var n int
	_ = c.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='ok'`).Scan(&n)
	if n != 0 {
		t.Error("failed migration must roll back")
	}
	_ = c.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n)
	if n != 1 {
		t.Errorf("0001 should stay applied, rows=%d", n)
	}
}

func TestMigrateBadNumbering(t *testing.T) {
	c, _ := openRaw(t)
	err := Migrate(context.Background(), c, mfs(map[string]string{"0002_x.sql": "SELECT 1;"}))
	if err == nil || !strings.Contains(err.Error(), "연속") {
		t.Errorf("err=%v", err)
	}
}

func TestForeignKeysOnEveryConnection(t *testing.T) {
	c, _ := openRaw(t)
	c.SetMaxOpenConns(3)
	ctx := context.Background()
	var conns []*sql.Conn
	for i := 0; i < 3; i++ {
		cn, err := c.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, cn)
		var fk int
		if err := cn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
			t.Errorf("conn %d fk=%d err=%v", i, fk, err)
		}
	}
	for _, cn := range conns {
		cn.Close()
	}
}

func TestEmbeddedSchemaMatchesMigrations(t *testing.T) {
	ctx := context.Background()
	// 1) schema.sql bytes == every migration concatenated in version order
	// (TASK-0013: the snapshot rule changed from "== 0001" once 0002 existed)
	names, err := fs.Glob(db.Migrations(), "*.sql")
	if err != nil || len(names) < 2 {
		t.Fatalf("migrations=%v err=%v", names, err)
	}
	sort.Strings(names)
	var all strings.Builder
	for _, n := range names {
		b, err := fs.ReadFile(db.Migrations(), n)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
	}
	if all.String() != db.SchemaSQL {
		t.Fatal("db/schema.sql differs from db/migrations/*.sql concatenated; regenerate: cat db/migrations/*.sql > db/schema.sql")
	}
	// 2) sqlite_master equivalence: migrations vs schema.sql applied directly
	a, _ := openRaw(t)
	if err := Migrate(ctx, a, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	s, _ := openRaw(t)
	if _, err := s.ExecContext(ctx, db.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	if x, y := dump(t, a), dump(t, s); x != y {
		t.Errorf("sqlite_master differs:\n%s\n---\n%s", x, y)
	}
}

func dump(t *testing.T, c *sql.DB) string {
	t.Helper()
	rows, err := c.Query(`SELECT type, name, COALESCE(sql,'') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var a, b, d string
		_ = rows.Scan(&a, &b, &d)
		sb.WriteString(a + "|" + b + "|" + d + "\n")
	}
	return sb.String()
}

func TestOpenCreatesDBAndHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if err := EnsureLayout(home); err != nil {
		t.Fatal(err)
	}
	c, err := Open(context.Background(), DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var n int
	_ = c.QueryRow(`SELECT count(*) FROM clients`).Scan(&n)
	if n != 2 {
		t.Errorf("clients seeded=%d", n)
	}
}

func TestHomeEnv(t *testing.T) {
	d := t.TempDir()
	t.Setenv(EnvHome, d)
	h, err := Home()
	if err != nil || h != d {
		t.Errorf("%q %v", h, err)
	}
}

func pragma(t *testing.T, c *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := c.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOpenAppliesPragmasOnFileDB(t *testing.T) {
	c, err := Open(context.Background(), filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Hold several connections at once so the check covers more than one.
	ctx := context.Background()
	var conns []*sql.Conn
	for i := 0; i < 3; i++ {
		cn, err := c.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, cn)
	}
	for i, cn := range conns {
		for pragma, want := range map[string]string{"journal_mode": "wal", "busy_timeout": "5000", "foreign_keys": "1", "synchronous": "1"} {
			var got string
			if err := cn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("conn %d: PRAGMA %s = %q, want %q", i, pragma, got, want)
			}
		}
		cn.Close()
	}
}

func TestOpenMemoryDB(t *testing.T) {
	c, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := pragma(t, c, "journal_mode"); got != "memory" {
		t.Errorf("journal_mode=%q (WAL is not available for :memory:)", got)
	}
	if got := pragma(t, c, "foreign_keys"); got != "1" {
		t.Errorf("foreign_keys=%q", got)
	}
	var n int
	if err := c.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != 3 {
		t.Errorf("migrations not applied: n=%d err=%v", n, err)
	}
}

func TestRunnerRecordsRealChecksum(t *testing.T) {
	c, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var sum string
	if err := c.QueryRow(`SELECT checksum FROM schema_migrations WHERE version=1`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	b, _ := fs.ReadFile(db.Migrations(), "0001_init.sql")
	h := sha256.Sum256(b)
	if sum != hex.EncodeToString(h[:]) {
		t.Errorf("checksum=%q, want sha256 of 0001_init.sql", sum)
	}
}

// realMigrationsUpTo returns the embedded migrations with version <= n.
func realMigrationsUpTo(t *testing.T, n int) fstest.MapFS {
	t.Helper()
	names, err := fs.Glob(db.Migrations(), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	m := fstest.MapFS{}
	for i, name := range names {
		if i+1 > n {
			break
		}
		b, _ := fs.ReadFile(db.Migrations(), name)
		m[name] = &fstest.MapFile{Data: b}
	}
	return m
}

// TestMigration0002UpgradePath: a database created by the 0001-only binary is
// upgraded in place, existing rows survive, the approvals table appears and
// cascades with its server (TASK-0013).
func TestMigration0002UpgradePath(t *testing.T) {
	ctx := context.Background()
	c, _ := openRaw(t)
	if err := Migrate(ctx, c, realMigrationsUpTo(t, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, `INSERT INTO mcp_servers(scope,name,transport,command,created_at,updated_at) VALUES('global','keep','stdio','x',1,1)`); err != nil {
		t.Fatal(err)
	}
	var has int
	_ = c.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='mcp_tool_approvals'`).Scan(&has)
	if has != 0 {
		t.Fatal("0001 database must not have the approvals table")
	}
	if err := Migrate(ctx, c, db.Migrations()); err != nil {
		t.Fatalf("upgrade 0001->0002: %v", err)
	}
	_ = c.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='mcp_tool_approvals'`).Scan(&has)
	if has != 1 {
		t.Fatal("approvals table missing after upgrade")
	}
	var n int
	if err := c.QueryRow(`SELECT count(*) FROM mcp_servers WHERE name='keep'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("existing row lost: n=%d err=%v", n, err)
	}
	var sid int64
	_ = c.QueryRow(`SELECT id FROM mcp_servers WHERE name='keep'`).Scan(&sid)
	if _, err := c.ExecContext(ctx, `INSERT INTO mcp_tool_approvals(server_id,tool_name,approved_hash,input_schema,approved_at) VALUES(?,?,?,?,1)`, sid, "t", strings.Repeat("a", 64), "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, `INSERT INTO mcp_tool_approvals(server_id,tool_name,approved_hash,input_schema,approved_at) VALUES(?,?,?,?,1)`, sid, "bad", "short", "{}"); err == nil {
		t.Error("approved_hash CHECK (64 chars) not enforced")
	}
	if _, err := c.ExecContext(ctx, `DELETE FROM mcp_servers WHERE id=?`, sid); err != nil {
		t.Fatal(err)
	}
	_ = c.QueryRow(`SELECT count(*) FROM mcp_tool_approvals`).Scan(&n)
	if n != 0 {
		t.Error("approvals must cascade with the server")
	}
	// idempotent re-run
	if err := Migrate(ctx, c, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	rows, _ := c.Query(`SELECT version, checksum FROM schema_migrations ORDER BY version`)
	defer rows.Close()
	got := 0
	for rows.Next() {
		var v int
		var sum string
		_ = rows.Scan(&v, &sum)
		name := map[int]string{1: "0001_init.sql", 2: "0002_mcp_tool_approvals.sql", 3: "0003_misses_mcp_server.sql"}[v]
		b, _ := fs.ReadFile(db.Migrations(), name)
		h := sha256.Sum256(b)
		if sum != hex.EncodeToString(h[:]) {
			t.Errorf("version %d checksum mismatch", v)
		}
		got++
	}
	if got != 3 {
		t.Errorf("schema_migrations rows=%d", got)
	}
}

// TestMigration0002TamperAndDowngrade: a modified 0002 file is refused (checksum)
// and a 0001-only binary refuses a v2 database (downgrade).
func TestMigration0002TamperAndDowngrade(t *testing.T) {
	ctx := context.Background()
	c, _ := openRaw(t)
	if err := Migrate(ctx, c, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	tampered := realMigrationsUpTo(t, 3)
	tampered["0002_mcp_tool_approvals.sql"] = &fstest.MapFile{Data: append(append([]byte{}, tampered["0002_mcp_tool_approvals.sql"].Data...), []byte("\n-- x\n")...)}
	if err := Migrate(ctx, c, tampered); !errors.Is(err, ErrChecksum) {
		t.Errorf("tampered 0002: err=%v, want ErrChecksum", err)
	}
	if err := Migrate(ctx, c, realMigrationsUpTo(t, 1)); !errors.Is(err, ErrDowngrade) {
		t.Errorf("v2 database with a 0001-only binary: err=%v, want ErrDowngrade", err)
	}
}
