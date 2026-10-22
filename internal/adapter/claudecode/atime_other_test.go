//go:build !darwin && !linux

package claudecode

import (
	"os"
	"time"
)

func atimeOf(fi os.FileInfo) time.Time { return fi.ModTime() }
