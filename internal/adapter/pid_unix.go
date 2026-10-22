//go:build unix

package adapter

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process with this pid exists (signal 0).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
