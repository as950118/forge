package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver (also registered by internal/store)

	"github.com/as950118/forge/internal/store"
)

// ErrNotInitialized is returned when forge.db does not exist. Nothing is created.
var ErrNotInitialized = errors.New("Forge 저장소가 초기화되지 않았습니다. forge init 을 먼저 실행하세요")

// ImmutableNote is the notice printed when the database had to be opened with the
// immutable fallback (see OpenNote).
const ImmutableNote = "알림: forge.db 가 있는 위치가 읽기 전용이라 immutable 모드로 읽었습니다. 다른 프로세스가 동시에 기록 중이면 최신 호출이 보이지 않거나 일시적으로 불일치할 수 있습니다."

// Open opens the existing database under home strictly read-only: the file is
// opened with mode=ro and query_only, no directory/file is created and no
// migration runs. A missing database yields ErrNotInitialized; an unreadable or
// foreign file yields a "corrupt?" error.
//
// Side effect of a WAL database: SQLite itself may create the (empty) forge.db-wal
// and forge.db-shm next to the database while it is open, and they can stay after
// Close because a read-only connection may not delete them; the database content is
// never changed. If the directory is read-only so that these files cannot be
// created, see OpenNote.
func Open(ctx context.Context, home string) (*sql.DB, error) {
	db, _, err := OpenNote(ctx, home)
	return db, err
}

// OpenNote is Open that also returns a notice for the user ("" = none). When the
// normal read-only open fails because the -wal/-shm files cannot be created (read-only
// file system or directory), it retries once with SQLite's immutable=1 (no locks, no
// auxiliary files, WAL contents are ignored) and returns ImmutableNote; the data of
// writes still sitting in the WAL is then not visible. Any other failure is returned.
func OpenNote(ctx context.Context, home string) (*sql.DB, string, error) {
	path := store.DBPath(home)
	if err := store.CheckDBLocation(path); err != nil {
		return nil, "", err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, "", fmt.Errorf("%w (%s)", ErrNotInitialized, path)
	}
	db, err := openMode(ctx, path, false)
	if err == nil {
		return db, "", nil
	}
	if !isReadOnlyFSError(err) {
		return nil, "", wrapOpen(err)
	}
	db, err2 := openMode(ctx, path, true)
	if err2 != nil {
		return nil, "", wrapOpen(err)
	}
	return db, ImmutableNote, nil
}

func isReadOnlyFSError(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "readonly database") || strings.Contains(m, "unable to open database file")
}

type openErr struct{ error }

func wrapOpen(err error) error {
	var oe openErr
	if errors.As(err, &oe) {
		return oe.error
	}
	return fmt.Errorf("forge.db 를 읽을 수 없습니다(손상?): %w. 백업이 있으면 복원하거나 forge doctor 를 확인하세요", err)
}

// roDSN builds the connection string. Open always sets both defenses: mode=ro (the
// file is opened read-only, SQLite refuses every write at the VFS level) and
// query_only (the connection refuses every statement that would write). The flags
// exist so that tests can prove each defense on its own (TASK-0021 AC-5).
func roDSN(path string, modeRO, queryOnly, immutable bool) string {
	q := url.Values{}
	if modeRO {
		q.Set("mode", "ro")
	}
	if immutable {
		q.Set("immutable", "1")
	}
	q.Add("_pragma", "busy_timeout(5000)")
	if queryOnly {
		q.Add("_pragma", "query_only(1)")
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
}

func openMode(ctx context.Context, path string, immutable bool) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", roDSN(path, true, true, immutable))
	if err != nil {
		return nil, openErr{err}
	}
	// One connection: the read-only handle is cheap and queries run one by one.
	conn.SetMaxOpenConns(1)
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type IN ('table','view') AND name IN ('ledger','skills','tools','mcp_servers')`).Scan(&n); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if n != 4 {
		_ = conn.Close()
		return nil, openErr{errors.New("forge.db 에 원장/레지스트리 테이블이 없습니다(손상 또는 초기화 전). forge init 을 실행하거나 백업을 복원하세요")}
	}
	return conn, nil
}
