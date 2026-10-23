//go:build !darwin && !linux

package cli

import (
	"os"
	"time"
)

func atimeOf(fi os.FileInfo) time.Time { return fi.ModTime() }
