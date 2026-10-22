//go:build !unix

package adapter

import (
	"errors"
	"os"
)

var errWouldBlock = errors.New("would block")

// tryLock without flock: the lock is the exclusive existence of <path>.held.
func tryLock(path string) (func(), error) {
	held := path + ".held"
	f, err := os.OpenFile(held, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errWouldBlock
		}
		return nil, err
	}
	f.Close()
	return func() { _ = os.Remove(held) }, nil
}

// lockHeld reports whether the lock is currently held (the .held file exists).
func lockHeld(path string) bool {
	_, err := os.Lstat(path + ".held")
	return err == nil
}
