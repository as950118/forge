//go:build unix

package tool

import (
	"fmt"
	"os"
	"syscall"
)

// checkPrivate requires path to be owned by the current user, not a symlink,
// and not accessible to group/other (defense in depth against a swapped or
// world-writable script store; a same-user attacker is out of scope).
func checkPrivate(path string, dir bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s 가 심볼릭 링크입니다", path)
	}
	if dir != fi.IsDir() || (!dir && !fi.Mode().IsRegular()) {
		return fmt.Errorf("%s 의 파일 종류가 올바르지 않습니다", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s 의 권한이 너무 넓습니다 (%04o, 그룹/기타 접근 불가여야 함)", path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s 의 소유자가 현재 사용자가 아닙니다", path)
	}
	return nil
}
