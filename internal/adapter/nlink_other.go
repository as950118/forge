//go:build !unix

package adapter

import "os"

func linkCount(os.FileInfo) uint64 { return 1 }
