// Package testenv holds helpers that make tests independent of the machine they
// run on. It is imported only from _test.go files.
package testenv

import (
	"os"
	"testing"
)

// maxTempDirLen is the longest os.TempDir() tests accept as is. SQLite's unix VFS
// refuses database paths of 512 characters or more, unix sockets are limited to
// about 104, and doctor / prune-stale cut long paths when they print them; a
// TMPDIR of 400 characters (some CI and sandbox setups) breaks all of these for
// reasons unrelated to what a test checks.
const maxTempDirLen = 100

// Run runs the package's tests and returns the exit code. When TMPDIR is long it
// first points TMPDIR at a short private directory under /tmp (removed afterwards)
// so that t.TempDir() paths stay short. Only the test process is affected; the
// production code reads TMPDIR as usual.
func Run(m *testing.M) int {
	if len(os.TempDir()) > maxTempDirLen {
		if d, err := os.MkdirTemp("/tmp", "forge-t-"); err == nil {
			defer os.RemoveAll(d)
			old, had := os.LookupEnv("TMPDIR")
			_ = os.Setenv("TMPDIR", d)
			defer func() {
				if had {
					_ = os.Setenv("TMPDIR", old)
				} else {
					_ = os.Unsetenv("TMPDIR")
				}
			}()
		}
	}
	return m.Run()
}
