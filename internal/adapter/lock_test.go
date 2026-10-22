package adapter

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A plan checks whether a sync is running without creating the lock file.
func TestLockHeldNeverCreatesAndSeesTheHolder(t *testing.T) {
	dir := t.TempDir()
	lockDir := filepath.Join(dir, "locks")
	key := LockKey("claude-code", ScopeProject, "/some/root")
	if LockHeld(lockDir, key) {
		t.Fatal("no lock file: nothing is held")
	}
	if _, err := os.Stat(lockDir); err == nil {
		t.Fatal("LockHeld created the lock directory")
	}
	release, err := AcquireLock(lockDir, key, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !LockHeld(lockDir, key) {
		t.Fatal("a held lock must be reported")
	}
	release()
	if LockHeld(lockDir, key) {
		t.Fatal("a released lock must not be reported")
	}
}

func TestTempNameShapeAndStaleDetection(t *testing.T) {
	name := tempName(tmpPrefix, ".mcp.json")
	if !tempRe.MatchString(name) {
		t.Fatalf("our own temp names must match the cleanup pattern: %s", name)
	}
	for _, n := range []string{".forge-tmp-notes.txt", "forge-tmp-1-deadbeef-x", ".forge-tmp-12345678-deadbeef-x", ".forge-tmp-1-DEADBEEF-x", ".forge-tmp-1-deadbeef-"} {
		if tempRe.MatchString(n) {
			t.Errorf("%s must not be treated as ours", n)
		}
	}
	dir := t.TempDir()
	// A temp file of this very process is stale at startup (nothing of ours is in flight).
	own := filepath.Join(dir, name)
	if err := os.WriteFile(own, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := FindStaleTemps([]string{dir}); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if got := CleanStaleTemps([]string{dir}); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if _, err := os.Stat(own); err == nil {
		t.Fatal("not removed")
	}
}
