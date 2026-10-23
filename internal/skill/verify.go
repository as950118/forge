package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Integrity states of an installed snapshot (forge skill check, doctor, gateway).
const (
	StoreOK         = "ok"
	StoreModified   = "modified"   // content differs from the registered content_hash
	StoreMissing    = "missing"    // the snapshot directory is gone
	StoreUnreadable = "unreadable" // could not be checked (permissions, I/O)
)

// VerifyStore re-hashes an installed snapshot and compares it with the content
// hash recorded when it was added (tamper detection of ~/.forge/skills after the
// fact). The walk is bounded by DefaultLimits: a snapshot with more files or
// bytes than an import may produce is "modified" without hashing it. It reads
// only below root, never follows symlinks (a symlink makes it "modified"), and
// changes nothing. got is the computed hash when one was computed.
func VerifyStore(root, want string) (state, got string, err error) {
	fi, err := os.Lstat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return StoreMissing, "", nil
	case err != nil:
		return StoreUnreadable, "", err
	case !fi.IsDir():
		return StoreModified, "", fmt.Errorf("스냅샷 경로가 디렉터리가 아닙니다")
	}
	files := 0
	var total int64
	werr := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("일반 파일이 아닌 항목(심볼릭 링크/특수 파일)이 있습니다: %s", filepath.Base(p))
		}
		files++
		if files > DefaultLimits.MaxFiles {
			return fmt.Errorf("파일 수가 가져오기 제한(%d개)을 넘습니다", DefaultLimits.MaxFiles)
		}
		if info, e := d.Info(); e == nil {
			total += info.Size()
			if total > DefaultLimits.MaxTotalSize {
				return fmt.Errorf("총 크기가 가져오기 제한을 넘습니다")
			}
		}
		return nil
	})
	if werr != nil {
		if errors.Is(werr, fs.ErrPermission) {
			return StoreUnreadable, "", werr
		}
		return StoreModified, "", werr
	}
	h, e := HashTree(root)
	if e != nil {
		if errors.Is(e, fs.ErrPermission) {
			return StoreUnreadable, "", e
		}
		return StoreModified, "", e
	}
	if h != want {
		return StoreModified, h, nil
	}
	return StoreOK, h, nil
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseSHA256 normalizes a user-given tree hash (surrounding whitespace and
// case are ignored, an optional "sha256:" prefix is accepted) and rejects
// anything that is not 64 hex digits.
func ParseSHA256(s string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(s))
	n = strings.TrimPrefix(n, "sha256:")
	if !sha256Re.MatchString(n) {
		return "", fmt.Errorf("--sha256 값이 올바르지 않습니다: 64자리 16진수(트리 해시)여야 합니다 (미리보기의 '트리 sha256' 을 복사하세요)")
	}
	return n, nil
}

// ErrHashMismatch is returned when the fetched tree is not the pinned one.
var ErrHashMismatch = errors.New("가져온 트리의 sha256 이 고정한 값과 다릅니다")
