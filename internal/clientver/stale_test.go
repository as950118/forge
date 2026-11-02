//go:build unix

package clientver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TASK-0032 AC-4: a killed forge's forge-ver-<pid>-<random> directory is removed only
// when every condition holds; each case below breaks exactly one.

func staleRoot(t *testing.T) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mkOld(t *testing.T, root, name string, mode os.FileMode, age time.Duration) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "cache"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCleanStaleRemovesOnlyProvenDeadOldOwnDirs(t *testing.T) {
	root := staleRoot(t)
	old := 3 * time.Hour
	dead, alive := 999001, 999002
	prev := processDead
	processDead = func(pid int) bool { return pid == dead || pid > maxPID } // dead for every candidate: only the cap keeps the big ones
	t.Cleanup(func() { processDead = prev })

	gone := mkOld(t, root, fmt.Sprintf("forge-ver-%d-123456", dead), 0o700, old)
	keep := map[string]string{
		"pid alive":       mkOld(t, root, fmt.Sprintf("forge-ver-%d-1", alive), 0o700, old),
		"too new":         mkOld(t, root, fmt.Sprintf("forge-ver-%d-2", dead), 0o700, time.Minute),
		"legacy no pid":   mkOld(t, root, "forge-ver-123456789", 0o700, old),
		"group readable":  mkOld(t, root, fmt.Sprintf("forge-ver-%d-3", dead), 0o750, old),
		"suffix not num":  mkOld(t, root, fmt.Sprintf("forge-ver-%d-12x", dead), 0o700, old),
		"other prefix":    mkOld(t, root, fmt.Sprintf("xforge-ver-%d-4", dead), 0o700, old),
		"leading zero":    mkOld(t, root, fmt.Sprintf("forge-ver-0%d-5", dead), 0o700, old),
		"own pid":         mkOld(t, root, fmt.Sprintf("forge-ver-%d-6", os.Getpid()), 0o700, old),
		"pid too large":   mkOld(t, root, fmt.Sprintf("forge-ver-%d-10", maxPID+1), 0o700, old),
		"pid beyond int":  mkOld(t, root, "forge-ver-4294967296-11", 0o700, old),
		"nested name":     mkOld(t, root, fmt.Sprintf("forge-ver-%d-7.bak", dead), 0o700, old),
		"mine but a file": filepath.Join(root, fmt.Sprintf("forge-ver-%d-8", dead)),
	}
	if err := os.WriteFile(keep["mine but a file"], []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a symbolic link with a matching name: neither the link nor its target may go
	outside := mkOld(t, staleRoot(t), "precious", 0o700, old)
	link := filepath.Join(root, fmt.Sprintf("forge-ver-%d-9", dead))
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	keep["symlink"] = link

	removed := CleanStale(root, time.Now(), time.Hour)
	if len(removed) != 1 || removed[0] != gone {
		t.Fatalf("removed %v, want only %s", removed, gone)
	}
	if _, err := os.Lstat(gone); !os.IsNotExist(err) {
		t.Errorf("the stale directory is still there: %v", err)
	}
	for why, p := range keep {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s: %s was removed", why, p)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "cache")); err != nil {
		t.Errorf("the target of a symbolic link was touched: %v", err)
	}
}

func TestCleanStaleRefusesUnsafeRoots(t *testing.T) {
	prev := processDead
	processDead = func(int) bool { return true }
	t.Cleanup(func() { processDead = prev })

	home := staleRoot(t)
	t.Setenv("HOME", home)
	inHome := mkOld(t, home, "forge-ver-999003-1", 0o700, 3*time.Hour)
	if got := CleanStale(home, time.Now(), time.Hour); len(got) != 0 {
		t.Errorf("the home directory was swept: %v", got)
	}
	if _, err := os.Lstat(inHome); err != nil {
		t.Errorf("a directory in HOME was removed: %v", err)
	}
	// a symbolic link to the home directory is the home directory too
	viaLink := filepath.Join(staleRoot(t), "tmp")
	if err := os.Symlink(home, viaLink); err != nil {
		t.Fatal(err)
	}
	if got := CleanStale(viaLink, time.Now(), time.Hour); len(got) != 0 {
		t.Errorf("a link to the home directory was swept: %v", got)
	}
	for _, r := range []string{"/", "", "relative/dir", filepath.Join(home, "x", "..")} {
		if safeStaleRoot(r) {
			t.Errorf("root %q accepted", r)
		}
	}
	if !safeStaleRoot(staleRoot(t)) {
		t.Error("an ordinary temp directory was refused")
	}
}

func TestCleanStaleRemovalCaps(t *testing.T) {
	root := staleRoot(t)
	prev := processDead
	processDead = func(int) bool { return true }
	t.Cleanup(func() { processDead = prev })
	for i := 0; i < maxStaleRemovals+10; i++ {
		mkOld(t, root, fmt.Sprintf("forge-ver-%d-%d", 999100+i, i), 0o700, 3*time.Hour)
	}
	if got := CleanStale(root, time.Now(), time.Hour); len(got) != maxStaleRemovals {
		t.Fatalf("removed %d, want the cap %d", len(got), maxStaleRemovals)
	}
	if left, _ := os.ReadDir(root); len(left) != 10 {
		t.Fatalf("%d left, want 10", len(left))
	}
}

func TestPidDeadRealProcesses(t *testing.T) {
	if pidDead(os.Getpid()) {
		t.Fatal("this process is alive")
	}
	if pidDead(0) || pidDead(-1) {
		t.Fatal("pid <= 0 must never be judged dead (kill(0)/kill(-1) address groups)")
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot run true: ", err)
	}
	if !pidDead(cmd.Process.Pid) {
		t.Skip("the pid of the finished child exists again (reused)")
	}
}

func TestRunVersionNamesTheDirectoryAfterItsPid(t *testing.T) {
	logf := filepath.Join(t.TempDir(), "pwd.log")
	dir := fake(t, "claude", `pwd >> `+logf+`; echo 2.1.305`)
	if r := check(t, dir); r.Status != StatusOK {
		t.Fatalf("%+v", r)
	}
	b, _ := os.ReadFile(logf)
	want := fmt.Sprintf("forge-ver-%d-", os.Getpid())
	if !strings.Contains(string(b), want) {
		t.Fatalf("working directory %q does not contain %q", strings.TrimSpace(string(b)), want)
	}
	if !staleNameRe.MatchString(filepath.Base(strings.TrimSpace(string(b)))) {
		t.Fatalf("the directory name does not match the sweep pattern: %q", b)
	}
}
