//go:build unix

package mcp

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the child in its own process group so that wrappers such
// as npx can be cleaned up together with the server they spawn.
func setProcGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killGroup force-kills any process still left in the child's group.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
