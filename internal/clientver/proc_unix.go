//go:build unix

package clientver

import (
	"os"
	"os/exec"
	"syscall"
)

func setProcGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// pidDead reports that no process with this pid exists (kill(pid, 0) = ESRCH).
// EPERM (exists, owned by someone else) and every other result mean "not proven dead".
func pidDead(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

// ownedByCurrentUser reports whether fi belongs to the current user (ok is false
// when the owner cannot be determined).
func ownedByCurrentUser(fi os.FileInfo) (mine, ok bool) {
	uid, _, isStat := ids(fi)
	if !isStat {
		return false, false
	}
	return uid == uint32(os.Getuid()), true
}
