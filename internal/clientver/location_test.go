package clientver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// W: a client binary that other users could have planted is never run. Owners
// cannot be changed by a test, so these cases use mode bits (the owner rule is a
// plain comparison with the current uid and root).

// markerScript is a fake claude that records that it ran.
func markerScript(marker string) string {
	return "#!/bin/sh\ntouch " + marker + "\necho 2.1.305\n"
}

// fakeGroups injects the group trust decision (real chown is impossible in a test).
type fakeGroups struct{ trust func(gid uint32) bool }

func (f fakeGroups) Trusted(fi os.FileInfo) bool {
	_, gid, ok := ids(fi)
	return ok && f.trust(gid)
}

func withGroups(t *testing.T, trust func(gid uint32) bool) {
	t.Helper()
	old := groups
	groups = fakeGroups{trust}
	t.Cleanup(func() { groups = old })
}

func ranMarker(t *testing.T, marker string) bool {
	t.Helper()
	_, err := os.Stat(marker)
	return err == nil
}

func mkdirMode(t *testing.T, mode os.FileMode) string {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	d := filepath.Join(dir, "bin")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, mode); err != nil { // umask must not weaken the test
		t.Fatal(err)
	}
	return d
}

func writeBin(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBinaryDirectoryModes(t *testing.T) {
	skipWindows(t)
	withGroups(t, func(uint32) bool { return false }) // no group is trusted here
	cases := []struct {
		mode   os.FileMode
		unsafe bool
	}{
		{0o777, true},
		{0o775, true},
		{0o770, true}, // group write alone is enough
		{0o757, true},
		{0o755, false}, // Homebrew-like: owned by the user, not writable by others
		{0o750, false},
		{0o700, false},
	}
	for _, c := range cases {
		marker := filepath.Join(t.TempDir(), "ran")
		dir := mkdirMode(t, c.mode)
		writeBin(t, dir, "claude", markerScript(marker), 0o755)
		r := check(t, dir)
		if c.unsafe {
			if r.Code != CodeUnsafeLoc || r.Status != StatusWarn || r.UnsafePath != dir {
				t.Errorf("mode %04o: %+v", c.mode, r)
			}
			if ranMarker(t, marker) {
				t.Errorf("mode %04o: the binary was executed", c.mode)
			}
			if !strings.Contains(r.Message, "다른 사용자가 수정할 수 있는 위치라 실행하지 않았습니다") || !strings.Contains(r.Message, "chmod go-w "+dir) {
				t.Errorf("mode %04o message: %s", c.mode, r.Message)
			}
		} else if r.Status != StatusOK || !ranMarker(t, marker) {
			t.Errorf("mode %04o must be accepted: %+v", c.mode, r)
		}
	}
}

func TestBinaryFileModes(t *testing.T) {
	skipWindows(t)
	withGroups(t, func(uint32) bool { return false }) // no group is trusted here
	for _, c := range []struct {
		mode   os.FileMode
		unsafe bool
	}{{0o775, true}, {0o757, true}, {0o777, true}, {0o755, false}, {0o750, false}, {0o700, false}} {
		marker := filepath.Join(t.TempDir(), "ran")
		dir := mkdirMode(t, 0o755)
		bin := writeBin(t, dir, "claude", markerScript(marker), c.mode)
		r := check(t, dir)
		if c.unsafe && (r.Code != CodeUnsafeLoc || r.UnsafePath != bin || ranMarker(t, marker)) {
			t.Errorf("file mode %04o: %+v ran=%v", c.mode, r, ranMarker(t, marker))
		}
		if !c.unsafe && (r.Status != StatusOK || !ranMarker(t, marker)) {
			t.Errorf("file mode %04o must be accepted: %+v", c.mode, r)
		}
	}
}

func TestSymlinkIntoAnUnsafeDirectoryIsNotRun(t *testing.T) {
	skipWindows(t)
	withGroups(t, func(uint32) bool { return false }) // no group is trusted here
	// link in a safe directory -> real file in a world-writable one
	marker := filepath.Join(t.TempDir(), "ran")
	bad := mkdirMode(t, 0o777)
	writeBin(t, bad, "claude-real", markerScript(marker), 0o755)
	safe := mkdirMode(t, 0o755)
	if err := os.Symlink(filepath.Join(bad, "claude-real"), filepath.Join(safe, "claude")); err != nil {
		t.Fatal(err)
	}
	r := check(t, safe)
	if r.Code != CodeUnsafeLoc || r.UnsafePath != bad || ranMarker(t, marker) {
		t.Fatalf("%+v", r)
	}
	// link in a world-writable directory -> real file in a safe one (the link itself can be swapped)
	marker2 := filepath.Join(t.TempDir(), "ran")
	good := mkdirMode(t, 0o755)
	writeBin(t, good, "claude-real", markerScript(marker2), 0o755)
	badLinkDir := mkdirMode(t, 0o777)
	if err := os.Symlink(filepath.Join(good, "claude-real"), filepath.Join(badLinkDir, "claude")); err != nil {
		t.Fatal(err)
	}
	r = check(t, badLinkDir)
	if r.Code != CodeUnsafeLoc || r.UnsafePath != badLinkDir || ranMarker(t, marker2) {
		t.Fatalf("%+v", r)
	}
	// a PATH entry that is itself a symlink to a safe directory is fine
	entry := filepath.Join(t.TempDir(), "entry")
	if err := os.Symlink(good, entry); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(good, "claude"))
	writeBin(t, good, "claude", markerScript(marker2), 0o755)
	if r = check(t, entry); r.Status != StatusOK {
		t.Fatalf("%+v", r)
	}
}

func TestAncestorDirectoryRules(t *testing.T) {
	skipWindows(t)
	withGroups(t, func(uint32) bool { return false }) // no group is trusted here
	build := func(parentMode os.FileMode) (string, string) {
		base, _ := filepath.EvalSymlinks(t.TempDir())
		parent := filepath.Join(base, "parent")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(parent, "bin")
		if err := os.Mkdir(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(base, "ran")
		writeBin(t, bin, "claude", markerScript(marker), 0o755)
		if err := os.Chmod(parent, parentMode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
		return bin, marker
	}
	// a world-writable ancestor lets anyone rename the directory below it
	bin, marker := build(0o777)
	if r := check(t, bin); r.Code != CodeUnsafeLoc || ranMarker(t, marker) {
		t.Fatalf("world-writable ancestor: %+v", r)
	}
	// a sticky ancestor (like /tmp above your own directory) only lets owners rename: accepted
	bin, marker = build(0o777 | os.ModeSticky)
	if r := check(t, bin); r.Status != StatusOK || !ranMarker(t, marker) {
		t.Fatalf("sticky ancestor: %+v", r)
	}
	// but a binary directly inside a sticky world-writable directory is never run
	marker = filepath.Join(t.TempDir(), "ran")
	d := mkdirMode(t, 0o777|os.ModeSticky)
	writeBin(t, d, "claude", markerScript(marker), 0o755)
	if r := check(t, d); r.Code != CodeUnsafeLoc || ranMarker(t, marker) {
		t.Fatalf("sticky directory holding the binary: %+v", r)
	}
}

func TestUnsafeCandidateStopsTheSearch(t *testing.T) {
	skipWindows(t)
	withGroups(t, func(uint32) bool { return false }) // no group is trusted here
	// The shell would start the first claude in PATH: a safe one further down
	// must not hide that the first one is unsafe.
	m1 := filepath.Join(t.TempDir(), "ran1")
	m2 := filepath.Join(t.TempDir(), "ran2")
	bad := mkdirMode(t, 0o777)
	writeBin(t, bad, "claude", markerScript(m1), 0o755)
	good := mkdirMode(t, 0o755)
	writeBin(t, good, "claude", markerScript(m2), 0o755)
	r := check(t, bad+string(os.PathListSeparator)+good)
	if r.Code != CodeUnsafeLoc || ranMarker(t, m1) || ranMarker(t, m2) {
		t.Fatalf("%+v", r)
	}
	// an unsafe directory without the binary does not matter
	empty := mkdirMode(t, 0o777)
	if r := check(t, empty+string(os.PathListSeparator)+good); r.Status != StatusOK {
		t.Fatalf("%+v", r)
	}
	// Locate (no version run) reports the same
	if p, _, _ := (&Checker{PathEnv: bad}).Locate("claude"); p != "" {
		t.Fatalf("an unsafe binary is not located: %q", p)
	}
}

// Homebrew on Apple Silicon: /opt/homebrew/bin is user:admin 0775. Group write
// of a TRUSTED group (admin gid 80, wheel gid 0, the owner's private group) is
// accepted; staff-like broad groups and other-write are not.
func TestGroupWritableIsAcceptedOnlyForTrustedGroups(t *testing.T) {
	skipWindows(t)
	gid := func(dir string) uint32 {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, g, _ := ids(fi)
		return g
	}
	for _, c := range []struct {
		name    string
		mode    os.FileMode
		trusted bool
		unsafe  bool
	}{
		{"0775 admin-like trusted group", 0o775, true, false},
		{"0770 trusted group", 0o770, true, false},
		{"0775 staff-like broad group", 0o775, false, true},
		{"0757 other write, trusted group", 0o757, true, true},
		{"0777 trusted group", 0o777, true, true},
		{"0777|sticky trusted group, binary directly inside", 0o777 | os.ModeSticky, true, true},
	} {
		marker := filepath.Join(t.TempDir(), "ran")
		dir := mkdirMode(t, c.mode)
		writeBin(t, dir, "claude", markerScript(marker), 0o755)
		g := gid(dir)
		withGroups(t, func(x uint32) bool { return c.trusted && x == g })
		r := check(t, dir)
		if c.unsafe && (r.Code != CodeUnsafeLoc || ranMarker(t, marker)) {
			t.Errorf("%s: %+v", c.name, r)
		}
		if !c.unsafe && (r.Status != StatusOK || !ranMarker(t, marker)) {
			t.Errorf("%s must be accepted: %+v", c.name, r)
		}
	}
	// the same rule for the file and for an ancestor, and through a symlink chain
	// (/opt/homebrew/bin/claude -> /opt/homebrew/Caskroom/...)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	cask := filepath.Join(base, "Caskroom", "claude")
	bin := filepath.Join(base, "bin")
	for _, d := range []string{cask, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(base, "ran")
	writeBin(t, cask, "claude", markerScript(marker), 0o775) // group-writable file
	if err := os.Symlink(filepath.Join(cask, "claude"), filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(base, "Caskroom"), 0o775)
	_ = os.Chmod(cask, 0o775)
	_ = os.Chmod(bin, 0o775)
	g := gid(bin)
	withGroups(t, func(x uint32) bool { return x == g })
	if r := check(t, bin); r.Status != StatusOK || !ranMarker(t, marker) {
		t.Fatalf("trusted group along the whole chain: %+v", r)
	}
	os.Remove(marker)
	withGroups(t, func(uint32) bool { return false })
	if r := check(t, bin); r.Code != CodeUnsafeLoc || ranMarker(t, marker) {
		t.Fatalf("untrusted group: %+v", r)
	}
	// an untrusted-group ancestor high in the chain is found too (not sticky)
	withGroups(t, func(x uint32) bool { return x == g })
	_ = os.Chmod(filepath.Join(base, "Caskroom"), 0o757)
	if r := check(t, bin); r.Code != CodeUnsafeLoc {
		t.Fatalf("other-writable ancestor in the symlink target chain: %+v", r)
	}
}

// ---- W-1: every hop of a symlink chain is judged ----------------------------

func noGroups(t *testing.T) { withGroups(t, func(uint32) bool { return false }) }

func TestIntermediateHopInAnUnsafeDirectoryIsFound(t *testing.T) {
	skipWindows(t)
	noGroups(t)
	// safe link -> link inside a 0777 directory -> safe file: the final file and
	// the first link are in safe directories, the middle hop is not.
	marker := filepath.Join(t.TempDir(), "ran")
	good := mkdirMode(t, 0o755)
	writeBin(t, good, "claude-real", markerScript(marker), 0o755)
	bad := mkdirMode(t, 0o777)
	if err := os.Symlink(filepath.Join(good, "claude-real"), filepath.Join(bad, "hop")); err != nil {
		t.Fatal(err)
	}
	entry := mkdirMode(t, 0o755)
	if err := os.Symlink(filepath.Join(bad, "hop"), filepath.Join(entry, "claude")); err != nil {
		t.Fatal(err)
	}
	r := check(t, entry)
	if r.Code != CodeUnsafeLoc || r.UnsafePath != bad || ranMarker(t, marker) {
		t.Fatalf("%+v", r)
	}
	// an unsafe (non-sticky, world-writable) ANCESTOR of the intermediate hop's directory
	base, _ := filepath.EvalSymlinks(t.TempDir())
	parent := filepath.Join(base, "parent")
	_ = os.Mkdir(parent, 0o700)
	hopDir := filepath.Join(parent, "h")
	_ = os.Mkdir(hopDir, 0o755)
	g2 := mkdirMode(t, 0o755)
	m2 := filepath.Join(base, "ran")
	writeBin(t, g2, "real", markerScript(m2), 0o755)
	if err := os.Symlink(filepath.Join(g2, "real"), filepath.Join(hopDir, "hop")); err != nil {
		t.Fatal(err)
	}
	entry2 := mkdirMode(t, 0o755)
	if err := os.Symlink(filepath.Join(hopDir, "hop"), filepath.Join(entry2, "claude")); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(parent, 0o777)
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	if r := check(t, entry2); r.Code != CodeUnsafeLoc || ranMarker(t, m2) {
		t.Fatalf("unsafe ancestor of an intermediate hop: %+v", r)
	}
}

func TestIntermediateHopLinkFileModeIsJudged(t *testing.T) {
	skipWindows(t)
	noGroups(t)
	marker := filepath.Join(t.TempDir(), "ran")
	good := mkdirMode(t, 0o755)
	writeBin(t, good, "real", markerScript(marker), 0o755)
	hopDir := mkdirMode(t, 0o755)
	hop := filepath.Join(hopDir, "hop")
	if err := os.Symlink(filepath.Join(good, "real"), hop); err != nil {
		t.Fatal(err)
	}
	entry := mkdirMode(t, 0o755)
	if err := os.Symlink(hop, filepath.Join(entry, "claude")); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("chmod", "-h", "757", hop).Run(); err != nil {
		t.Skip("cannot set the mode of a symlink here")
	}
	if fi, _ := os.Lstat(hop); fi.Mode().Perm()&0o002 == 0 {
		t.Skip("symlink modes are not honoured on this system")
	}
	r := check(t, entry)
	if r.Code != CodeUnsafeLoc || r.UnsafePath != hop || ranMarker(t, marker) {
		t.Fatalf("%+v", r)
	}
}

func TestLinkChainCyclesAndLimit(t *testing.T) {
	skipWindows(t)
	noGroups(t)
	dir := mkdirMode(t, 0o755)
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	_ = os.Symlink(b, a)
	_ = os.Symlink(a, b)
	// the checker cannot resolve a cycle: not installed, never run
	if r := check(t, dir); r.Code != CodeNotInstalled {
		t.Fatalf("%+v", r)
	}
	// the policy itself reports a cycle as unsafe
	if off, why := unsafeLocation(a, a); off == "" || !strings.Contains(why, "링크") {
		t.Fatalf("cycle: %q %q", off, why)
	}
	build := func(n int) (string, string) {
		d := mkdirMode(t, 0o755)
		marker := filepath.Join(t.TempDir(), "ran")
		writeBin(t, d, "real", markerScript(marker), 0o755)
		prev := filepath.Join(d, "real")
		for i := 0; i < n; i++ {
			l := filepath.Join(d, fmt.Sprintf("l%d", i))
			if err := os.Symlink(prev, l); err != nil {
				t.Fatal(err)
			}
			prev = l
		}
		_ = os.Rename(prev, filepath.Join(d, "claude"))
		return d, marker
	}
	d, marker := build(10)
	if r := check(t, d); r.Status != StatusOK || !ranMarker(t, marker) {
		t.Fatalf("short chain: %+v", r)
	}
	// (the kernel itself refuses chains over its own limit, e.g. 32 hops on macOS,
	// so the checker may report "not installed"; it must never run it, and the
	// policy function refuses the chain on its own)
	d, marker = build(maxLinkHops + 5)
	if r := check(t, d); (r.Code != CodeUnsafeLoc && r.Code != CodeNotInstalled) || ranMarker(t, marker) {
		t.Fatalf("long chain: %+v", r)
	}
	if off, why := unsafeLocation(filepath.Join(d, "claude"), filepath.Join(d, "real")); off == "" || !strings.Contains(why, "홉") {
		t.Fatalf("hop limit: %q %q", off, why)
	}
}

func TestMixedRelativeAndAbsoluteChainWithoutFalsePositive(t *testing.T) {
	skipWindows(t)
	withGroups(t, func(uint32) bool { return true }) // admin-like group everywhere
	// bin/claude -> ../Caskroom/claude/1.0/claude (relative) -> <abs>/real/claude-real (absolute)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	for _, d := range []string{"bin", "Caskroom/claude/1.0", "real"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o775); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(filepath.Join(base, d), 0o775)
	}
	marker := filepath.Join(base, "ran")
	writeBin(t, filepath.Join(base, "real"), "claude-real", markerScript(marker), 0o755)
	if err := os.Symlink(filepath.Join(base, "real", "claude-real"), filepath.Join(base, "Caskroom/claude/1.0/claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Caskroom/claude/1.0/claude", filepath.Join(base, "bin", "claude")); err != nil {
		t.Fatal(err)
	}
	if r := check(t, filepath.Join(base, "bin")); r.Status != StatusOK || !ranMarker(t, marker) {
		t.Fatalf("a normal Homebrew-like chain must be accepted: %+v", r)
	}
}

// ---- W-1b: ".." in a link target is walked, not normalized -------------------

// g3: a link target "<0777 dir>/sub/../../safe/claude-real". Normalized it is
// "<P>/safe/claude-real" (all safe), but the real walk goes through the
// world-writable directory, where an attacker can turn "sub" into a symlink so
// that ".." leads somewhere else.
func TestDotDotInALinkTargetIsWalkedNotNormalized(t *testing.T) {
	skipWindows(t)
	noGroups(t)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	mk := func(rel string, mode os.FileMode) string {
		d := filepath.Join(base, rel)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
		return d
	}
	mk("P", 0o755)
	open := mk("P/D", 0o777) // world-writable
	mk("P/D/sub", 0o755)
	safe := mk("P/safe", 0o755)
	legit := filepath.Join(base, "legit-ran")
	writeBin(t, safe, "claude-real", markerScript(legit), 0o755)
	entry := mk("entry", 0o755)
	target := base + "/P/D/sub/../../safe/claude-real" // not filepath.Join: that would clean it
	if err := os.Symlink(target, filepath.Join(entry, "claude")); err != nil {
		t.Fatal(err)
	}
	// (1) as built: the walk passes through the world-writable directory
	r := check(t, entry)
	if r.Code != CodeUnsafeLoc || r.UnsafePath != open || ranMarker(t, legit) {
		t.Fatalf("walk through a world-writable directory: %+v", r)
	}
	// (2) the attack: sub becomes a symlink, ".." now leads into the attacker's tree
	attacker := filepath.Join(base, "attacker-ran")
	mk("A/x/deep", 0o755)
	mk("A/safe", 0o755)
	writeBin(t, filepath.Join(base, "A", "safe"), "claude-real", markerScript(attacker), 0o755)
	if err := os.Remove(filepath.Join(open, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "A", "x", "deep"), filepath.Join(open, "sub")); err != nil {
		t.Fatal(err)
	}
	r = check(t, entry)
	if r.Code != CodeUnsafeLoc || ranMarker(t, attacker) {
		t.Fatalf("the attacker's binary must not run: %+v ran=%v", r, ranMarker(t, attacker))
	}
}

func TestDotDotWalkAcceptsNormalChainsAndOddNames(t *testing.T) {
	skipWindows(t)
	withGroups(t, func(uint32) bool { return true })
	base, _ := filepath.EvalSymlinks(t.TempDir())
	for _, d := range []string{"bin", "Cellar/claude/1.0/bin", "a..b", "..."} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Homebrew: bin/claude -> ../Cellar/claude/1.0/bin/claude (a relative target with "..")
	marker := filepath.Join(base, "ran")
	writeBin(t, filepath.Join(base, "Cellar/claude/1.0/bin"), "claude", markerScript(marker), 0o755)
	if err := os.Symlink("../Cellar/claude/1.0/bin/claude", filepath.Join(base, "bin", "claude")); err != nil {
		t.Fatal(err)
	}
	if r := check(t, filepath.Join(base, "bin")); r.Status != StatusOK || !ranMarker(t, marker) {
		t.Fatalf("a Homebrew-like ../Cellar link must not be a false positive: %+v", r)
	}
	// "a..b" and "..." are ordinary names, not parent references
	for _, name := range []string{"a..b", "..."} {
		m := filepath.Join(base, "ran-"+name)
		writeBin(t, filepath.Join(base, name), "claude", markerScript(m), 0o755)
		if r := check(t, filepath.Join(base, name)); r.Status != StatusOK || !ranMarker(t, m) {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	// an explicit "dir/../dir" in the PATH entry is walked too (not cleaned away)
	m := filepath.Join(base, "ran-dd")
	writeBin(t, filepath.Join(base, "a..b"), "claude", markerScript(m), 0o755)
	if r := check(t, base+"/bin/../a..b"); r.Status != StatusOK || !ranMarker(t, m) {
		t.Fatalf("PATH entry with ..: %+v", r)
	}
	// ".." that only goes up to a safe directory and returns is accepted: the walk
	// judges every directory it passes, it does not reject ".." as such
	if err := os.Symlink("../bin/../Cellar/claude/1.0/bin/claude", filepath.Join(base, "bin", "claude2")); err != nil {
		t.Fatal(err)
	}
	os.Remove(marker)
	_ = os.Rename(filepath.Join(base, "bin", "claude"), filepath.Join(base, "bin", "claude-old"))
	_ = os.Rename(filepath.Join(base, "bin", "claude2"), filepath.Join(base, "bin", "claude"))
	if r := check(t, filepath.Join(base, "bin")); r.Status != StatusOK || !ranMarker(t, marker) {
		t.Fatalf("up-and-down through safe directories: %+v", r)
	}
	// but passing through a world-writable directory is refused even when the path ends safe
	bad := filepath.Join(base, "bad")
	_ = os.Mkdir(bad, 0o700)
	_ = os.Chmod(bad, 0o777)
	withGroups(t, func(uint32) bool { return false })
	_ = os.Remove(filepath.Join(base, "bin", "claude"))
	if err := os.Symlink("../bad/../Cellar/claude/1.0/bin/claude", filepath.Join(base, "bin", "claude")); err != nil {
		t.Fatal(err)
	}
	os.Remove(marker)
	if r := check(t, filepath.Join(base, "bin")); r.Code != CodeUnsafeLoc || ranMarker(t, marker) {
		t.Fatalf("%+v", r)
	}
}

// T-A: ".." in a PATH ENTRY is walked too (through the public Check path
// resolution). If the entry were cleaned first ("<0777>/../safe" -> "<base>/safe")
// the world-writable directory would never be judged.
func TestDotDotInAPathEntryIsWalked(t *testing.T) {
	skipWindows(t)
	noGroups(t)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	mk := func(rel string, mode os.FileMode) string {
		d := filepath.Join(base, rel)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
		return d
	}
	d777 := mk("d777", 0o777)
	mk("d777/sub", 0o755)
	mk("d777/x", 0o755)
	safe := mk("safe", 0o755)
	okBin := mk("ok/bin", 0o755)
	markers := map[string]string{}
	put := func(dir, key string) {
		markers[key] = filepath.Join(base, "ran-"+key)
		writeBin(t, dir, "claude", markerScript(markers[key]), 0o755)
	}
	put(safe, "safe")
	put(okBin, "ok")
	put(filepath.Join(d777, "x"), "x")

	for _, c := range []struct{ name, entry, key string }{
		{"up out of the writable directory", d777 + "/../safe", "safe"},
		{"down, up, up", d777 + "/sub/../../safe", "safe"},
		{"up to a safe sibling", d777 + "/../ok/bin", "ok"},
		{"in through ../.. and into the writable directory", base + "/ok/bin/../../d777/x", "x"},
	} {
		r := check(t, c.entry)
		if r.Code != CodeUnsafeLoc || ranMarker(t, markers[c.key]) {
			t.Errorf("%s (%s): %+v ran=%v", c.name, c.entry, r, ranMarker(t, markers[c.key]))
		}
	}
	// g3 as a PATH entry: the attacker turns "sub" into a link, ".." leads into their tree
	attacker := filepath.Join(base, "attacker-ran")
	mk("A/x/deep", 0o755)
	a := mk("A/safe", 0o755)
	writeBin(t, a, "claude", markerScript(attacker), 0o755)
	if err := os.Remove(filepath.Join(d777, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "A", "x", "deep"), filepath.Join(d777, "sub")); err != nil {
		t.Fatal(err)
	}
	if r := check(t, d777+"/sub/../../safe"); r.Code != CodeUnsafeLoc || ranMarker(t, attacker) {
		t.Fatalf("attacker via a swapped sub: %+v ran=%v", r, ranMarker(t, attacker))
	}
	// harmless spellings of safe entries stay accepted
	for _, entry := range []string{base + "/ok/bin/../bin", base + "/./ok//bin/"} {
		os.Remove(markers["ok"])
		if r := check(t, entry); r.Status != StatusOK || !ranMarker(t, markers["ok"]) {
			t.Errorf("%s must be accepted: %+v", entry, r)
		}
	}
}
