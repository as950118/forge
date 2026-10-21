package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"path/filepath"
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
	// 1) schema.sql bytes == 0001_init.sql (sync rule while 0001 is the only migration)
	b, err := fs.ReadFile(db.Migrations(), "0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != db.SchemaSQL {
		t.Fatal("db/schema.sql and db/migrations/0001_init.sql differ; keep them in sync (cp db/schema.sql db/migrations/0001_init.sql)")
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
	if err := c.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != 1 {
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
