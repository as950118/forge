package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"

	_ "modernc.org/sqlite" // pure-Go driver (also registered by internal/store)

	"github.com/heonjinjeong/forge/internal/store"
)

// ErrNotInitialized is returned when forge.db does not exist. Nothing is created.
var ErrNotInitialized = errors.New("Forge 저장소가 초기화되지 않았습니다. forge init 을 먼저 실행하세요")

// Open opens the existing database under home strictly read-only: the file is
// opened with mode=ro and query_only, no directory/file is created and no
// migration runs. A missing database yields ErrNotInitialized; an unreadable or
// foreign file yields a "corrupt?" error.
func Open(ctx context.Context, home string) (*sql.DB, error) {
	path := store.DBPath(home)
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		return nil, fmt.Errorf("%w (%s)", ErrNotInitialized, path)
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(1)")
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	conn, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	// One connection: the read-only handle is cheap and queries run one by one.
	conn.SetMaxOpenConns(1)
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type IN ('table','view') AND name IN ('ledger','skills','tools','mcp_servers')`).Scan(&n); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("forge.db 를 읽을 수 없습니다(손상?): %w. 백업이 있으면 복원하거나 forge doctor 를 확인하세요", err)
	}
	if n != 4 {
		_ = conn.Close()
		return nil, errors.New("forge.db 에 원장/레지스트리 테이블이 없습니다(손상 또는 초기화 전). forge init 을 실행하거나 백업을 복원하세요")
	}
	return conn, nil
}
