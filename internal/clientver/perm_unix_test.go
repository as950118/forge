//go:build unix

package clientver

import (
	"errors"
	"os"
	"os/user"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// statInfo is a FileInfo whose owner and group are chosen by the test.
type statInfo struct{ st syscall.Stat_t }

func (statInfo) Name() string        { return "x" }
func (statInfo) Size() int64         { return 0 }
func (statInfo) Mode() os.FileMode   { return 0o775 }
func (statInfo) ModTime() time.Time  { return time.Time{} }
func (statInfo) IsDir() bool         { return true }
func (s statInfo) Sys() any          { return &s.st }
func owned(uid, gid uint32) statInfo { return statInfo{st: syscall.Stat_t{Uid: uid, Gid: gid}} }

func fakeLookups(t *testing.T, users map[string]user.User, grps map[string]user.Group) {
	t.Helper()
	ou, og := lookupUser, lookupGroup
	lookupUser = func(id string) (*user.User, error) {
		if u, ok := users[id]; ok {
			return &u, nil
		}
		return nil, errors.New("no such user")
	}
	lookupGroup = func(id string) (*user.Group, error) {
		if g, ok := grps[id]; ok {
			return &g, nil
		}
		return nil, errors.New("no such group")
	}
	t.Cleanup(func() { lookupUser, lookupGroup = ou, og })
}

// T2: Trusted is asserted for known gids, not against its own lookup result.
func TestSystemGroupsTrustedForKnownGids(t *testing.T) {
	fakeLookups(t,
		map[string]user.User{"1234": {Uid: "1234", Gid: "1234", Username: "alice"}, "501": {Uid: "501", Gid: "20", Username: "bob"},
			"1500": {Uid: "1500", Gid: "1500", Username: "carol"}},
		map[string]user.Group{"1234": {Gid: "1234", Name: "alice"}, "20": {Gid: "20", Name: "staff"}, "10": {Gid: "10", Name: "wheel"},
			"27": {Gid: "27", Name: "sudo"}, "4": {Gid: "4", Name: "adm"}, "1500": {Gid: "1500", Name: "shared"}})
	g := systemGroups{}
	if !g.Trusted(owned(501, 0)) {
		t.Error("gid 0 (wheel/root) is trusted")
	}
	if runtime.GOOS == "darwin" {
		if !g.Trusted(owned(501, 80)) {
			t.Error("macOS gid 80 (admin) is trusted")
		}
	} else if g.Trusted(owned(501, 80)) {
		t.Error("gid 80 is only admin on macOS")
	}
	if g.Trusted(owned(501, 20)) {
		t.Error("gid 20 (staff on macOS, not trusted anywhere) must not be trusted")
	}
	if !g.Trusted(owned(1234, 1234)) {
		t.Error("the owner's user-private group (primary, named like the user) is trusted")
	}
	if g.Trusted(owned(1500, 1500)) {
		t.Error("a primary group with another name than the user is not a private group")
	}
	if g.Trusted(owned(1234, 20)) {
		t.Error("a group that is not the owner's primary group is not trusted")
	}
	for _, gid := range []uint32{10, 27, 4} { // wheel/sudo/adm by NAME are never trusted
		if g.Trusted(owned(501, gid)) {
			t.Errorf("gid %d must not be trusted by its name", gid)
		}
	}
	if g.Trusted(owned(9999, 9999)) {
		t.Error("a failed user lookup is not trusted")
	}
	if g.Trusted(owned(1234, 777)) {
		t.Error("an unknown group is not trusted")
	}
	// a FileInfo without owner information is not trusted
	if g.Trusted(noSys{}) {
		t.Error("no owner information: not trusted")
	}
}

type noSys struct{ statInfo }

func (noSys) Sys() any { return nil }
