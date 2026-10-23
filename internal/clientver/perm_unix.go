//go:build unix

package clientver

import (
	"os"
	"os/user"
	"runtime"
	"strconv"
	"syscall"
)

func ids(fi os.FileInfo) (uid, gid uint32, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return st.Uid, st.Gid, true
}

// trustedOwner reports whether fi is owned by the current user or by root. ok is
// false when the owner cannot be determined (then the owner is not judged).
func trustedOwner(fi os.FileInfo) (trusted, ok bool) {
	uid, _, isStat := ids(fi)
	if !isStat {
		return true, false
	}
	return uid == uint32(os.Getuid()) || uid == 0, true
}

// Lookups (replaced by tests).
var (
	lookupUser  = user.LookupId
	lookupGroup = user.LookupGroupId
)

// systemGroups is the production groupResolver (see location.go).
type systemGroups struct{}

// Trusted: gid 0 (wheel/root), gid 80 on macOS (admin: the administrator
// accounts' group), or the owner's user-private group (primary group of the
// owner whose name equals the owner's user name, the Linux/UPG convention).
// Group names such as wheel/sudo/adm/staff/users are never trusted by name, and
// any lookup failure means "not trusted".
func (systemGroups) Trusted(fi os.FileInfo) bool {
	uid, gid, ok := ids(fi)
	if !ok {
		return false
	}
	if gid == 0 || (runtime.GOOS == "darwin" && gid == 80) {
		return true
	}
	u, err := lookupUser(strconv.FormatUint(uint64(uid), 10))
	if err != nil || u.Gid != strconv.FormatUint(uint64(gid), 10) {
		return false
	}
	g, err := lookupGroup(u.Gid)
	return err == nil && g.Name == u.Username
}
