package doctor

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/heonjinjeong/forge/internal/adapter"
)

const (
	walkLimit     = 20000
	backupAdvised = adapter.DefaultKeepBackups
)

// checkHygiene: backups (count, size, permissions) and the permissions of the
// client configuration files Forge writes. Metadata only (Lstat): no file is
// read, and nothing outside the managed files is looked at.
func checkHygiene(e *env) []Check {
	var out []Check
	out = append(out, e.backupHygiene()...)
	out = append(out, e.settingsPerms()...)
	return out
}

func (e *env) backupHygiene() []Check {
	dir := filepath.Join(e.o.Home, "backups")
	fi, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Check{mk("hygiene.backups", "hygiene", "백업", OK, "백업이 없습니다", "")}
		}
		return []Check{unavailable("hygiene.backups", "hygiene", "백업", err)}
	}
	if !fi.IsDir() {
		return []Check{mk("hygiene.backups", "hygiene", "백업", Warn, "backups 가 디렉터리가 아닙니다", "sync 백업과 복구가 동작하지 않습니다", "ls -ld "+e.q(dir))}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return []Check{unavailable("hygiene.backups", "hygiene", "백업", err)}
	}
	var syncN, dbN int
	for _, en := range ents {
		switch {
		case en.IsDir() && adapter.ValidBackupID(en.Name()):
			syncN++
		case !en.IsDir() && filepath.Ext(en.Name()) == ".db":
			dbN++
		}
	}
	var size int64
	var loose, visited int
	truncated := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if e.ctx.Err() != nil {
			truncated = true
			return fs.SkipAll
		}
		if err != nil {
			return nil
		}
		visited++
		if visited > walkLimit {
			truncated = true
			return fs.SkipAll
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.Mode().Perm()&0o077 != 0 && info.Mode()&os.ModeSymlink == 0 {
			loose++
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	more := ""
	if truncated {
		more = " 이상(항목이 많아 일부만 집계)"
	}
	var out []Check
	msg := fmt.Sprintf("sync 백업 %d개, DB 백업 %d개, 약 %s%s", syncN, dbN, humanBytes(size), more)
	switch {
	case syncN > backupAdvised:
		out = append(out, mk("hygiene.backups", "hygiene", "백업", Warn, msg+fmt.Sprintf(" (권장 상한 %d)", backupAdvised),
			"백업은 자동으로 지우지 않습니다. 오래된 백업이 쌓이면 설정 파일의 옛 사본이 오래 남습니다",
			"forge sync --prune-backups --dry-run   # 삭제 대상 확인", "forge sync --prune-backups --keep "+fmt.Sprint(backupAdvised)))
	default:
		out = append(out, mk("hygiene.backups", "hygiene", "백업", OK, msg, ""))
	}
	if loose > 0 {
		out = append(out, mk("hygiene.backups.perm", "hygiene", "백업 권한", Warn, fmt.Sprintf("다른 사용자가 읽을 수 있는 백업 항목이 %d개 있습니다", loose),
			"백업에는 클라이언트 설정 파일의 사본(환경변수 참조 등)이 들어 있습니다", "chmod -R go-rwx "+e.q(dir)))
	}
	return out
}

// settingsPerms warns about managed configuration files that other users can write.
func (e *env) settingsPerms() []Check {
	type f struct{ path, label string }
	var files []f
	for _, sc := range e.scope {
		switch {
		case sc == adapter.ScopeProject && e.project != "":
			files = append(files,
				f{filepath.Join(e.project, ".mcp.json"), ".mcp.json"},
				f{filepath.Join(e.project, ".claude", "settings.json"), ".claude/settings.json"},
				f{filepath.Join(e.project, "opencode.json"), "opencode.json"})
		case sc == adapter.ScopeUser && e.o.UserHome != "":
			// Only the Claude Code user settings Forge manages hooks in. Neither the
			// Claude Code user state file nor any OpenCode user file is named here.
			files = append(files, f{filepath.Join(e.o.UserHome, ".claude", "settings.json"), "~/.claude/settings.json"})
		}
	}
	var out []Check
	bad := 0
	for _, x := range files {
		fi, err := os.Lstat(x.path)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			continue
		}
		if fi.Mode().Perm()&0o022 != 0 {
			bad++
			out = append(out, mk("hygiene.perm."+x.label, "hygiene", "설정 파일 권한 "+x.label, Warn, fmt.Sprintf("%s 권한이 %04o 입니다 (그룹/다른 사용자가 쓸 수 있음)", x.label, fi.Mode().Perm()),
				"다른 사용자가 설정을 바꾸면 클라이언트가 임의 명령(MCP command, hook)을 실행할 수 있습니다", "chmod go-w "+e.q(x.path)))
		}
	}
	if bad == 0 {
		out = append(out, mk("hygiene.perm", "hygiene", "설정 파일 권한", OK, "관리 대상 설정 파일을 다른 사용자가 쓸 수 없습니다", ""))
	}
	return out
}
