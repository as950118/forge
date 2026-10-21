//go:build unix

package tool

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the child in its own process group so that grandchildren
// can be killed together with it (timeout / finish).
func setProcGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// KNOWN LIMIT: only the child's process group is killed. A descendant that
// calls setsid()/setpgid() (a script can; the `setsid` binary is deny-listed
// for command tools) leaves the group and survives timeout/cleanup.
// TODO(linux): contain descendants with a cgroup (v2 cgroup.kill) or
// prctl(PR_SET_CHILD_SUBREAPER) + reaping/killing the whole subtree.
//
// killGroup force-kills every process still left in the child's group.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
