//go:build linux

package cli

import (
	"os"
	"syscall"
	"time"
)

func atimeOf(fi os.FileInfo) time.Time {
	st := fi.Sys().(*syscall.Stat_t)
	return time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec))
}
