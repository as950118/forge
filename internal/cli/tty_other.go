//go:build !darwin && !linux

package cli

import "os"

// isTerminalFd is a conservative fallback: a character device that is not the
// null device. (Windows and the BSDs are not verified.)
func isTerminalFd(fd uintptr) bool {
	f := os.NewFile(fd, "stdin")
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}
