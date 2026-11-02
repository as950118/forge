//go:build !unix

package clientver

import (
	"os"
	"os/exec"
)

func setProcGroup(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// No process table or owner information here: the stale-directory sweep never
// removes anything on this platform.
func pidDead(int) bool { return false }

func ownedByCurrentUser(os.FileInfo) (mine, ok bool) { return false, false }
