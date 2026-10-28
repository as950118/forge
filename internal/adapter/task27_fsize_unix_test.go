//go:build unix

package adapter_test

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/heonjinjeong/forge/internal/adapter"
)

// A file size limit (RLIMIT_FSIZE) makes a large write fail with EFBIG: the
// closest thing to a full disk that a test can set up without privileges (a small
// tmpfs needs root). The limit is set only around the Apply call and SIGXFSZ is
// ignored for that time so the write returns the error instead of killing the test
// binary. The skill's 64 KiB file does not fit; everything before it (backup,
// journal, state) is smaller than the limit.
func TestSharedFileSizeLimitFailureRollsBack(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			sk := conformSkill(t, c.forge, "big-skill")
			if err := os.WriteFile(filepath.Join(sk.StoreDir, "big.bin"), make([]byte, 64<<10), 0o644); err != nil {
				t.Fatal(err)
			}
			h, _, err := adapter.TreeHash(sk.StoreDir, false)
			if err != nil {
				t.Fatal(err)
			}
			sk.Hash = h
			p, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			roots := []string{c.proj, filepath.Join(c.forge, "sync-state")}
			before := treeSig(t, roots...)

			var old syscall.Rlimit
			if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
				t.Skipf("RLIMIT_FSIZE unavailable: %v", err)
			}
			lim := syscall.Rlimit{Cur: 16 << 10, Max: old.Max}
			if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
				t.Skipf("cannot set RLIMIT_FSIZE: %v", err)
			}
			signal.Ignore(syscall.SIGXFSZ)
			_, aerr := adapter.Apply(context.Background(), c.ad, p, adapter.ApplyOptions{BackupDir: filepath.Join(c.forge, "backups")})
			_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
			signal.Reset(syscall.SIGXFSZ)

			if aerr == nil {
				t.Skip("the size limit did not stop the write (unverified on this platform)")
			}
			t.Logf("apply error: %v", aerr)
			after := treeSig(t, roots...)
			for path, v := range before {
				if after[path] != v {
					t.Errorf("not restored: %s: %q -> %q", path, v, after[path])
				}
			}
			for path := range after {
				if _, ok := before[path]; !ok {
					t.Errorf("left behind: %s", path)
				}
			}
			p2, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			if len(p2.Interrupted) != 0 || len(p2.StaleTemps) != 0 {
				t.Fatalf("a failed write left a trace: interrupted=%+v temps=%v (error: %v)", p2.Interrupted, p2.StaleTemps, aerr)
			}
			if _, err := adapter.Apply(context.Background(), c.ad, p2, adapter.ApplyOptions{BackupDir: filepath.Join(c.forge, "backups")}); err != nil {
				t.Fatalf("the same sync without the limit: %v", err)
			}
		})
	}
}
