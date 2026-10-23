//go:build linux

package cli

import (
	"syscall"
	"unsafe"
)

// isTerminalFd asks the kernel (TCGETS); /dev/null and other character devices
// that are not terminals fail the ioctl.
func isTerminalFd(fd uintptr) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return e == 0
}
