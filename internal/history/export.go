package history

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// WriteFile writes data to path as a new file with mode 0600.
//
//   - An existing file is never overwritten unless force is set.
//   - A symbolic link at path is always refused (dangling or not), with or
//     without force; so is anything that is not a regular file.
//   - With force the data goes to a temporary file in the same directory and is
//     renamed over the old file, so a failure leaves the old file intact.
//   - protect lists files (the registry database and its WAL/shm) that must never
//     be replaced, even with force.
//
// The parent directory must exist; it is not created.
func WriteFile(path string, data []byte, force bool, protect ...string) error {
	if path == "" {
		return errors.New("--output 경로가 비어 있습니다")
	}
	dir := filepath.Dir(path)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("--output 의 디렉터리가 없습니다: %s", filepath.ToSlash(dir))
	}
	st, err := os.Lstat(path)
	switch {
	case err == nil:
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("--output 대상이 심볼릭 링크입니다. 거부합니다: %s", path)
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("--output 대상이 일반 파일이 아닙니다: %s", path)
		}
		for _, p := range protect {
			if ps, err := os.Stat(p); err == nil && os.SameFile(ps, st) {
				return fmt.Errorf("--output 대상이 Forge 저장소 파일입니다. 거부합니다: %s", path)
			}
		}
		if !force {
			return fmt.Errorf("--output 파일이 이미 있습니다: %s (덮어쓰려면 --force)", path)
		}
	case errors.Is(err, os.ErrNotExist):
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("--output 파일을 만들 수 없습니다: %w", err)
		}
		if _, err := bytes.NewReader(data).WriteTo(f); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return err
		}
		return nil
	default:
		return err
	}
	tmp := filepath.Join(dir, ".forge-history-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("임시 파일을 만들 수 없습니다: %w", err)
	}
	if _, err := bytes.NewReader(data).WriteTo(f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
