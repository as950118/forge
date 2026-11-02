package clientver

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cleanup of the temporary directories that a killed forge left behind
// (TASK-0032 AC-4; docs/doctor.md).
//
// RunVersion creates "forge-ver-<pid>-<random>" under the temp directory (the pid
// is that of the creating forge process). A directory is removed at the start of a
// later --version run only when ALL of these hold, and only by its exact path:
//
//   - the name matches ^forge-ver-<pid>-<digits>$ exactly (the older
//     "forge-ver-<digits>" form carries no pid and is never touched);
//   - it is a real directory (lstat: a symbolic link is skipped, never followed),
//     owned by the current user, with no group/other permission bits (the mode
//     MkdirTemp gives);
//   - its modification time is older than StaleMinAge (a live run lasts seconds);
//   - the pid is in 1..2^22, is not this process and no process with that pid exists
//     (kill(pid, 0) = ESRCH; EPERM means "exists, not ours" and keeps it);
//   - the temp directory itself is absolute and is neither "/" nor the user's home.
//
// No pattern or wildcard deletion is used: the names come from one ReadDir, are
// checked one by one and removed with os.RemoveAll(<root>/<exact name>). A pid that
// was reused by an unrelated live process keeps the directory (a leak, not a
// deletion of something live).
var (
	// StaleMinAge is the minimum age of a directory before it can be removed (a
	// variable only so that tests can shorten it).
	StaleMinAge = time.Hour
	// processDead is replaced by tests.
	processDead = pidDead
)

const (
	// maxPID is the highest pid any supported system uses (Linux pid_max <= 2^22); a larger
	// number in a name is not a pid this code created.
	maxPID = 1 << 22
	// TempPrefix starts the name of every temporary directory of a --version run.
	TempPrefix       = "forge-ver-"
	maxStaleScan     = 5000 // directory entries looked at per sweep
	maxStaleRemovals = 50   // directories removed per sweep
)

var staleNameRe = regexp.MustCompile(`^forge-ver-([1-9][0-9]{0,9})-([0-9]{1,20})$`)

// tempPattern is the os.MkdirTemp pattern of this process.
func tempPattern() string { return TempPrefix + strconv.Itoa(os.Getpid()) + "-" }

// CleanStale removes the stale directories of root (see above) and returns the
// exact paths it removed. It never returns an error: a directory it cannot judge
// or remove is simply kept.
func CleanStale(root string, now time.Time, minAge time.Duration) []string {
	if !safeStaleRoot(root) {
		return nil
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var removed []string
	for i, e := range ents {
		if i >= maxStaleScan || len(removed) >= maxStaleRemovals {
			break
		}
		m := staleNameRe.FindStringSubmatch(e.Name())
		if m == nil || strings.ContainsRune(e.Name(), filepath.Separator) {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || pid <= 0 || pid > maxPID || pid == os.Getpid() {
			continue
		}
		path := filepath.Join(root, e.Name())
		fi, err := os.Lstat(path)
		if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o077 != 0 {
			continue
		}
		if mine, ok := ownedByCurrentUser(fi); !ok || !mine {
			continue
		}
		if now.Sub(fi.ModTime()) < minAge {
			continue
		}
		if !processDead(pid) {
			continue
		}
		// The directory must still be the one that was judged.
		if fi2, err := os.Lstat(path); err != nil || !os.SameFile(fi, fi2) {
			continue
		}
		if os.RemoveAll(path) == nil {
			removed = append(removed, path)
		}
	}
	return removed
}

// safeStaleRoot refuses a temp directory that is relative, the file system root or
// the user's home directory (a TMPDIR pointed at the wrong place must not turn the
// sweep into a deletion there).
func safeStaleRoot(root string) bool {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return false
	}
	if filepath.Dir(root) == root {
		return false
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		if filepath.Clean(h) == root {
			return false
		}
		if r, err := filepath.EvalSymlinks(root); err == nil {
			if hr, err := filepath.EvalSymlinks(h); err == nil && hr == r {
				return false
			}
		}
	}
	return true
}

var sweepOnce sync.Once

// sweepStaleOnce runs CleanStale on the temp directory once per process.
func sweepStaleOnce() {
	sweepOnce.Do(func() { CleanStale(os.TempDir(), time.Now(), StaleMinAge) })
}
