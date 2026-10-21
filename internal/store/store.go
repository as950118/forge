// Package store opens the Forge SQLite registry and applies embedded
// migrations (ADR-F0001 D5): numbered files, sha256 checksums, one
// transaction per file, forward-only, downgrade refused.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver

	"github.com/heonjinjeong/forge/db"
)

// EnvHome overrides the Forge home directory.
const EnvHome = "FORGE_HOME"

// Home resolves the Forge home directory: $FORGE_HOME or ~/.forge.
func Home() (string, error) {
	if h := os.Getenv(EnvHome); h != "" {
		return filepath.Abs(h)
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("홈 디렉터리를 알 수 없습니다 (FORGE_HOME 으로 지정하세요): %w", err)
	}
	return filepath.Join(u, ".forge"), nil
}

// EnsureLayout creates the home directory tree (0700).
func EnsureLayout(home string) error {
	for _, d := range []string{"", "skills", "tmp"} {
		p := filepath.Join(home, d)
		if err := os.MkdirAll(p, 0o700); err != nil {
			return fmt.Errorf("디렉터리 생성 실패 %s: %w", p, err)
		}
	}
	return nil
}

// DBPath returns the database file path under home.
func DBPath(home string) string { return filepath.Join(home, "forge.db") }

// Open opens (creating if needed) the database file, enabling foreign keys on
// every connection, and applies pending embedded migrations.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	conn, err := OpenRaw(path)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, conn, db.Migrations()); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if path != ":memory:" {
		_ = os.Chmod(path, 0o600)
	}
	return conn, nil
}

// OpenRaw opens the database without migrating. foreign_keys, busy_timeout,
// journal_mode=WAL and synchronous=NORMAL are applied to every connection via
// DSN pragmas and _txlock=immediate (ADR-F0001 D1). In-memory databases stay in journal_mode=memory.
func OpenRaw(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)") // ADR-F0001 D1; no-op for :memory:
	q.Add("_pragma", "synchronous(NORMAL)")
	// Every explicit transaction (BeginTx) starts with BEGIN IMMEDIATE. A deferred
	// transaction that reads and then writes cannot upgrade under WAL when another
	// writer committed meanwhile, and busy_timeout is not applied to the upgrade
	// (SQLITE_BUSY). Plain Query/Exec calls stay autocommit, so reads are not
	// serialized; only transactions (all of which write) take the write lock.
	q.Add("_txlock", "immediate")
	dsn := "file:" + filepath.ToSlash(path) + "?" + q.Encode()
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("DB 열기 실패: %w", err)
	}
	if path == ":memory:" {
		conn.SetMaxOpenConns(1) // each in-memory connection is its own database
	}
	if err := warmUp(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("DB 열기 실패: %w", err)
	}
	return conn, nil
}

// warmUp forces the first connection (and its pragmas) now. Switching a fresh
// file to journal_mode=WAL needs an exclusive lock that busy_timeout does not
// cover, so several processes initializing a new FORGE_HOME at once can see
// SQLITE_BUSY there; retry until it settles (the mode change is then a no-op).
func warmUp(conn *sql.DB) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := conn.PingContext(context.Background())
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10*time.Millisecond + time.Duration(rand.Intn(40))*time.Millisecond)
	}
}

func isBusy(err error) bool {
	m := err.Error()
	return strings.Contains(m, "SQLITE_BUSY") || strings.Contains(m, "database is locked")
}

var migNameRe = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

type migration struct {
	version  int
	name     string // file name without .sql, e.g. 0001_init
	sql      string
	checksum string
}

func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("마이그레이션 파일명이 NNNN_name.sql 형식이 아닙니다: %s", e.Name())
		}
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		v, _ := strconv.Atoi(m[1])
		sum := sha256.Sum256(b)
		ms = append(ms, migration{version: v, name: m[1] + "_" + m[2], sql: string(b), checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("마이그레이션 번호가 1부터 연속이어야 합니다: %d 번째 파일이 %s", i+1, m.name)
		}
	}
	return ms, nil
}

// ErrDowngrade is returned when the database is newer than this binary.
var ErrDowngrade = errors.New("DB 스키마가 이 forge 바이너리보다 최신입니다. forge 를 업그레이드하세요")

// ErrChecksum is returned when an applied migration file was modified.
var ErrChecksum = errors.New("이미 적용된 마이그레이션이 변경되었습니다")

// Migrate applies pending migrations from fsys in version order.
func Migrate(ctx context.Context, conn *sql.DB, fsys fs.FS) error {
	ms, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	applied := map[int]string{} // version -> checksum
	var hasTable int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&hasTable); err != nil {
		return err
	}
	maxApplied := 0
	if hasTable > 0 {
		rows, err := conn.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var v int
			var c string
			if err := rows.Scan(&v, &c); err != nil {
				_ = rows.Close()
				return err
			}
			applied[v] = c
			if v > maxApplied {
				maxApplied = v
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		_ = rows.Close()
	}
	if maxApplied > len(ms) {
		return fmt.Errorf("%w (DB v%d > 바이너리 v%d)", ErrDowngrade, maxApplied, len(ms))
	}
	for _, m := range ms {
		sum, ok := applied[m.version]
		if !ok {
			continue
		}
		if sum != m.checksum {
			return fmt.Errorf("%w: %s (기록 %.12s..., 현재 %.12s...)", ErrChecksum, m.name, sum, m.checksum)
		}
	}
	for _, m := range ms {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if err := applyOne(ctx, conn, m); err != nil {
			return fmt.Errorf("마이그레이션 %s 적용 실패: %w", m.name, err)
		}
	}
	return nil
}

func applyOne(ctx context.Context, conn *sql.DB, m migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Re-check under the write lock: a concurrent process may have applied this
	// migration while we waited (Migrate's pre-check ran outside the lock).
	var done int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&done); err != nil {
		return err
	}
	if done > 0 {
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM schema_migrations WHERE version=?`, m.version).Scan(&done); err != nil {
			return err
		}
		if done > 0 {
			return nil
		}
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	// Only the runner records schema_migrations rows (ADR-F0001 D5).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, name, checksum) VALUES (?, ?, ?)`,
		m.version, m.name, m.checksum); err != nil {
		return err
	}
	return tx.Commit()
}
