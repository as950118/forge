//go:build darwin

package opencode

import (
	"os"
	"syscall"
	"time"
)

func atimeOf(fi os.FileInfo) time.Time {
	st := fi.Sys().(*syscall.Stat_t)
	return time.Unix(int64(st.Atimespec.Sec), int64(st.Atimespec.Nsec))
}
