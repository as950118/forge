package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrLocked is returned when another sync holds the lock past the timeout.
var ErrLocked = errors.New("다른 forge sync 가 같은 대상을 처리 중입니다 (잠시 후 다시 시도하세요)")

// LockKey names the lock of one target+root, so that two syncs of the same
// files serialize while syncs of different projects do not.
func LockKey(target string, scope Scope, root string) string {
	sum := sha256.Sum256([]byte(root))
	return fmt.Sprintf("%s-%s-%s", target, scope, hex.EncodeToString(sum[:])[:12])
}

// AcquireLock takes the exclusive lock file <lockDir>/<key>.lock, waiting up to
// timeout. The returned function releases it. Lock files are never deleted
// (deleting a lock file others may be waiting on is racy).
func AcquireLock(lockDir, key string, timeout time.Duration) (func(), error) {
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("잠금 디렉터리 생성 실패: %w", err)
	}
	path := filepath.Join(lockDir, key+".lock")
	deadline := time.Now().Add(timeout)
	for {
		rel, err := tryLock(path)
		if err == nil {
			return rel, nil
		}
		if !errors.Is(err, errWouldBlock) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, ErrLocked
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// LockHeld reports whether another process currently holds the lock <lockDir>/<key>.lock.
// It never creates anything.
func LockHeld(lockDir, key string) bool {
	return lockHeld(filepath.Join(lockDir, key+".lock"))
}
