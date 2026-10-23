package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go driver (also registered by internal/store)

	"github.com/heonjinjeong/forge/internal/store"
)

var (
	errNoDB      = errors.New("forge.db 가 없습니다")
	errDBIsDir   = errors.New("forge.db 가 파일이 아니라 디렉터리입니다")
	errWALNoSHM  = errors.New("미반영 WAL(-wal)이 남아 있는데 -shm 이 없어, 읽기 전용으로 열면 보조 파일이 만들어질 수 있어 열지 않았습니다")
	errNotForged = errors.New("forge.db 에 레지스트리 테이블이 없습니다")
)

// openDB opens forge.db for reading without creating anything. When no WAL
// content is pending, the database is opened immutable=1: SQLite then neither
// uses nor creates the -wal/-shm files (a plain read-only open of a WAL
// database may create them). When WAL content is pending (-wal larger than 0)
// the file is not immutable (it must be read together with its WAL) and the
// -shm must already exist.
func openDB(home string) (*sql.DB, error) {
	path := store.DBPath(home)
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoDB
	}
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, errDBIsDir
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(2000)")
	q.Add("_pragma", "query_only(1)")
	if wal, err := os.Stat(path + "-wal"); err == nil && wal.Size() > 0 {
		if _, err := os.Stat(path + "-shm"); err != nil {
			return nil, errWALNoSHM
		}
	} else {
		q.Set("immutable", "1")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}
	conn, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	return conn, nil
}

// db returns the shared read-only handle (opened once).
func (e *env) database() (*sql.DB, error) {
	if e.db != nil || e.dbErr != nil {
		return e.db, e.dbErr
	}
	conn, err := openDB(e.o.Home)
	if err != nil {
		e.dbErr = err
		return nil, err
	}
	var n int
	if err := conn.QueryRowContext(e.ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('ledger','skills','tools','mcp_servers','profiles')`).Scan(&n); err != nil {
		_ = conn.Close()
		e.dbErr = fmt.Errorf("forge.db 를 읽을 수 없습니다: %w", err)
		return nil, e.dbErr
	}
	if n < 5 {
		_ = conn.Close()
		e.dbErr = errNotForged
		return nil, e.dbErr
	}
	e.db = conn
	return conn, nil
}

func isCtx(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
