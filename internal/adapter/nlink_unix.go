//go:build unix

package adapter

import (
	"os"
	"syscall"
)

// linkCount returns st_nlink of a file (1 when unknown).
func linkCount(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}
