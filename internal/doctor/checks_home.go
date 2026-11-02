package doctor

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/heonjinjeong/forge/internal/store"
)

const walWarnBytes = 64 << 20

// checkHome: FORGE_HOME existence and permissions.
func checkHome(e *env) []Check {
	home := e.o.Home
	fi, err := os.Lstat(home)
	if errors.Is(err, os.ErrNotExist) {
		return []Check{mk("home.dir", "home", "Forge 홈", Info, "FORGE_HOME 이 아직 없습니다: "+safe(home, 200),
			"forge init 을 실행한 적이 없습니다 (처음 쓰는 환경이면 정상)", "forge init")}
	}
	if err != nil {
		return []Check{unavailable("home.dir", "home", "Forge 홈", err)}
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		st, serr := os.Stat(home)
		if serr != nil {
			return []Check{mk("home.dir", "home", "Forge 홈", Fail, "FORGE_HOME 이 끊어진 심볼릭 링크입니다: "+safe(home, 200),
				"링크 대상이 없습니다", "ls -l "+e.q(home))}
		}
		fi = st
	}
	if !fi.IsDir() {
		return []Check{mk("home.dir", "home", "Forge 홈", Fail, "FORGE_HOME 이 디렉터리가 아닙니다: "+safe(home, 200),
			"같은 이름의 파일이 있습니다", "ls -ld "+e.q(home), "FORGE_HOME 환경변수를 확인하세요")}
	}
	perm := fi.Mode().Perm()
	if perm&0o007 != 0 {
		return []Check{mk("home.dir", "home", "Forge 홈 권한", Warn, fmt.Sprintf("FORGE_HOME 권한이 %04o 입니다 (다른 사용자가 읽을 수 있음: world-readable)", perm),
			"홈에는 DB, 스킬, 백업(설정 파일 사본)이 있어 0700 이어야 합니다", "chmod 700 "+e.q(home))}
	}
	if perm&0o077 != 0 {
		return []Check{mk("home.dir", "home", "Forge 홈 권한", Warn, fmt.Sprintf("FORGE_HOME 권한이 %04o 입니다 (같은 그룹이 접근 가능)", perm),
			"홈에는 DB, 스킬, 백업(설정 파일 사본)이 있어 0700 이어야 합니다", "chmod 700 "+e.q(home))}
	}
	return []Check{mk("home.dir", "home", "Forge 홈 권한", OK, fmt.Sprintf("%s (권한 %04o)", safe(home, 200), perm), "")}
}

// checkDatabase: forge.db file, integrity, migrations, WAL.
func checkDatabase(e *env) []Check {
	home := e.o.Home
	path := store.DBPath(home)
	var out []Check
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return []Check{mk("db.file", "home", "forge.db", Info, "forge.db 가 없습니다: "+safe(path, 200), "초기화 전입니다", "forge init")}
	case err != nil:
		return []Check{unavailable("db.file", "home", "forge.db", err)}
	case fi.IsDir():
		return []Check{mk("db.file", "home", "forge.db", Fail, "forge.db 가 파일이 아니라 디렉터리입니다: "+safe(path, 200),
			"DB 파일 위치에 디렉터리가 있습니다 (손상이 아닙니다)", "ls -ld "+e.q(path), "mv "+e.q(path)+" "+e.q(path+".moved")+" && forge init")}
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		out = append(out, mk("db.perm", "home", "forge.db 권한", Warn, fmt.Sprintf("forge.db 권한이 %04o 입니다", perm),
			"DB 에는 호출 이력(프로젝트 경로, 세션 id)이 있어 0600 이어야 합니다", "chmod 600 "+e.q(path)))
	} else {
		out = append(out, mk("db.perm", "home", "forge.db 권한", OK, fmt.Sprintf("%04o, %s", perm, humanBytes(fi.Size())), ""))
	}

	conn, err := e.database()
	if err != nil {
		switch {
		case errors.Is(err, errWALNoSHM):
			return append(out, mk("db.integrity", "home", "DB 무결성", Warn, "점검 불가: "+err.Error(), "forge 가 비정상 종료돼 WAL 이 남은 상태일 수 있습니다",
				"forge history --limit 1   # 쓰기 가능한 경로로 한 번 열면 WAL 이 정리됩니다"))
		case errors.Is(err, errNotForged):
			return append(out, mk("db.integrity", "home", "DB 무결성", Fail, "forge.db 에 레지스트리 테이블이 없습니다", "Forge DB 가 아니거나 손상되었습니다",
				"ls "+e.q(home+"/backups")+"   # forge-v<N>-<시각>.db 백업이 있으면 forge.db 로 복원하세요"))
		}
		if f, oerr := os.Open(path); oerr != nil && errors.Is(oerr, fs.ErrPermission) {
			return append(out, mk("db.integrity", "home", "DB 무결성", Fail, "forge.db 를 읽을 수 없습니다: 권한이 없습니다 ("+safe(oerr.Error(), 160)+")",
				"권한 문제입니다. 파일은 손상되지 않았을 수 있으며 현재 사용자에게 읽기 권한이 없습니다 (소유자나 모드가 바뀌었는지 확인)",
				"ls -l "+e.q(path), "chmod 600 "+e.q(path)+"   # 본인 소유일 때"))
		} else if f != nil {
			_ = f.Close()
		}
		return append(out, mk("db.integrity", "home", "DB 무결성", Fail, safe(err.Error(), 200), "파일이 손상되었거나 SQLite DB 가 아닙니다",
			"ls "+e.q(home+"/backups")+"   # forge-v<N>-<시각>.db 백업이 있으면 복원하세요"))
	}

	// quick_check with its own deadline: a very large database must not stall the run.
	qctx, cancel := contextWithTimeout(e, quickCheckLimit)
	var res string
	err = conn.QueryRowContext(qctx, `PRAGMA quick_check(1)`).Scan(&res)
	cancel()
	switch {
	case err != nil && isCtx(err):
		out = append(out, mk("db.integrity", "home", "DB 무결성", Warn, "점검 불가: quick_check 가 "+quickCheckLimit.String()+" 안에 끝나지 않았습니다 ("+humanBytes(fi.Size())+")",
			"DB 가 매우 큽니다", "sqlite3 "+e.q(path)+" 'PRAGMA quick_check'"))
	case err != nil:
		out = append(out, mk("db.integrity", "home", "DB 무결성", Fail, "quick_check 실패: "+safe(err.Error(), 200), "DB 파일이 손상되었습니다",
			"ls "+e.q(home+"/backups")+"   # 백업 DB 로 복원하세요"))
	case res != "ok":
		out = append(out, mk("db.integrity", "home", "DB 무결성", Fail, "quick_check: "+safe(res, 200), "DB 파일이 손상되었습니다",
			"ls "+e.q(home+"/backups")+"   # 백업 DB 로 복원하세요"))
	default:
		out = append(out, mk("db.integrity", "home", "DB 무결성", OK, "PRAGMA quick_check = ok", ""))
	}
	out = append(out, e.checkMigrations(conn))
	out = append(out, e.checkWAL(path))
	return out
}

func (e *env) checkMigrations(conn *sql.DB) Check {
	emb, err := store.EmbeddedMigrations()
	if err != nil {
		return unavailable("db.migrations", "home", "마이그레이션", err)
	}
	rows, err := conn.QueryContext(e.ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return mk("db.migrations", "home", "마이그레이션", Fail, "schema_migrations 를 읽을 수 없습니다: "+safe(err.Error(), 160), "DB 가 손상되었거나 Forge DB 가 아닙니다", "forge init")
	}
	defer rows.Close()
	applied := map[int]string{}
	max := 0
	for rows.Next() {
		var v int
		var c string
		if err := rows.Scan(&v, &c); err != nil {
			return unavailable("db.migrations", "home", "마이그레이션", err)
		}
		applied[v] = c
		if v > max {
			max = v
		}
	}
	if err := rows.Err(); err != nil {
		return unavailable("db.migrations", "home", "마이그레이션", err)
	}
	if max > len(emb) {
		return mk("db.migrations", "home", "마이그레이션", Fail, fmt.Sprintf("DB 스키마 v%d 가 이 forge(v%d)보다 최신입니다", max, len(emb)),
			"더 새로운 forge 로 만든 DB 입니다 (다운그레이드는 거부됩니다)", "forge 를 최신 버전으로 업그레이드하세요")
	}
	for _, m := range emb {
		if c, ok := applied[m.Version]; ok && c != m.Checksum {
			return mk("db.migrations", "home", "마이그레이션", Fail, "적용된 마이그레이션 "+m.Name+" 의 checksum 이 바이너리와 다릅니다",
				"마이그레이션 파일이 바뀌었거나 DB 가 다른 빌드의 것입니다 (적용된 마이그레이션은 불변)", "forge --version", "ls "+e.q(e.o.Home+"/backups"))
		}
	}
	if max < len(emb) {
		return mk("db.migrations", "home", "마이그레이션", Info, fmt.Sprintf("DB v%d, 이 forge v%d: 미적용 마이그레이션이 있습니다", max, len(emb)),
			"다음 쓰기 명령에서 자동 적용됩니다 (적용 전 DB 백업을 만듭니다)", "forge init")
	}
	return mk("db.migrations", "home", "마이그레이션", OK, fmt.Sprintf("v%d (checksum 일치)", max), "")
}

func (e *env) checkWAL(path string) Check {
	f, err := os.Open(path)
	if err != nil {
		return unavailable("db.wal", "home", "WAL", err)
	}
	defer f.Close()
	hdr := make([]byte, 20)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return unavailable("db.wal", "home", "WAL", err)
	}
	if hdr[18] != 2 || hdr[19] != 2 {
		return mk("db.wal", "home", "WAL", Info, "journal_mode 가 WAL 이 아닙니다", "DB 헤더가 WAL 모드가 아닙니다 (forge 가 다음에 열 때 WAL 로 전환합니다)", "forge init")
	}
	var size int64
	if fi, err := os.Stat(path + "-wal"); err == nil {
		size = fi.Size()
	}
	if size > walWarnBytes {
		return mk("db.wal", "home", "WAL", Warn, fmt.Sprintf("WAL 파일이 %s 입니다", humanBytes(size)), "체크포인트가 밀렸습니다 (오래 열려 있는 읽기 프로세스가 있을 수 있음)",
			"sqlite3 "+e.q(path)+" 'PRAGMA wal_checkpoint(TRUNCATE)'")
	}
	return mk("db.wal", "home", "WAL", OK, fmt.Sprintf("WAL 모드, -wal %s", humanBytes(size)), "")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
