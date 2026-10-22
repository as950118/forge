//go:build !darwin && !linux

package opencode

import (
	"os"
	"time"
)

func atimeOf(fi os.FileInfo) time.Time { return fi.ModTime() }
