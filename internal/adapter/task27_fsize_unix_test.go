//go:build unix

package adapter_test

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/heonjinjeong/forge/internal/adapter"
)

// The limit is process wide, and go test appends to its own testlog.txt (a cached
// run, already larger than the limit), so setting it in the test binary makes the
// package fail with "can't write testlog.txt: file too large". The limited write
// therefore runs in a child process: the test binary executes itself, running only
// TestFsizeHelperProcess, which is the only place that sets the limit and ignores
// SIGXFSZ. The child has no -test.testlogfile. It reports through marker lines.
const (
	fsizeEnvTarget = "FORGE_TEST_FSIZE_TARGET"
	fsizeEnvForge  = "FORGE_TEST_FSIZE_FORGE"
	fsizeEnvProj   = "FORGE_TEST_FSIZE_PROJ"
	fsizeApplied   = "FSIZE-RESULT-APPLY-OK"
	fsizeFailed    = "FSIZE-RESULT-APPLY-ERROR: "
	fsizeNoLimit   = "FSIZE-RESULT-NO-RLIMIT: "
)

// TestFsizeHelperProcess is the child half of TestSharedFileSizeLimitFailureRollsBack;
// it does nothing in a normal run.
func TestFsizeHelperProcess(t *testing.T) {
	name := os.Getenv(fsizeEnvTarget)
	if name == "" {
		t.Skip("helper process for TestSharedFileSizeLimitFailureRollsBack")
	}
	var tg conformTarget
	for _, c := range conformTargets {
		if c.name == name {
			tg = c
		}
	}
	if tg.adapter == nil {
		t.Fatalf("unknown target %q", name)
	}
	forge, proj := os.Getenv(fsizeEnvForge), os.Getenv(fsizeEnvProj)
	c := &conformProject{forge: forge, proj: proj, home: os.Getenv("HOME"), ad: tg.adapter(forge), tg: tg}
	sk := conformSkill(t, forge, "big-skill")
	h, _, err := adapter.TreeHash(sk.StoreDir, false)
	if err != nil {
		t.Fatal(err)
	}
	sk.Hash = h
	p, err := c.plan(t, []adapter.SkillInput{sk})
	if err != nil {
		t.Fatal(err)
	}
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		os.Stdout.WriteString("\n" + fsizeNoLimit + err.Error() + "\n")
		return
	}
	lim := syscall.Rlimit{Cur: 16 << 10, Max: old.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		os.Stdout.WriteString("\n" + fsizeNoLimit + err.Error() + "\n")
		return
	}
	signal.Ignore(syscall.SIGXFSZ)
	_, aerr := adapter.Apply(context.Background(), c.ad, p, adapter.ApplyOptions{BackupDir: filepath.Join(forge, "backups")})
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	signal.Reset(syscall.SIGXFSZ)
	if aerr == nil {
		os.Stdout.WriteString("\n" + fsizeApplied + "\n")
		return
	}
	os.Stdout.WriteString("\n" + fsizeFailed + strings.ReplaceAll(aerr.Error(), "\n", " ") + "\n")
}

// A file size limit (RLIMIT_FSIZE) makes a large write fail with EFBIG: the
// closest thing to a full disk that a test can set up without privileges (a small
// tmpfs needs root). The skill's 64 KiB file does not fit; everything before it
// (backup, journal, state) is smaller than the limit.
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
			roots := []string{c.proj, filepath.Join(c.forge, "sync-state")}
			before := treeSig(t, roots...)

			cmd := exec.Command(os.Args[0], "-test.run=^TestFsizeHelperProcess$", "-test.count=1")
			cmd.Env = append(os.Environ(), fsizeEnvTarget+"="+tg.name, fsizeEnvForge+"="+c.forge, fsizeEnvProj+"="+c.proj)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("helper process: %v\n%s", err, out)
			}
			var aerr string
			var failed bool
			for _, line := range strings.Split(string(out), "\n") {
				switch {
				case strings.HasPrefix(line, fsizeNoLimit):
					t.Skipf("RLIMIT_FSIZE unavailable: %s", strings.TrimPrefix(line, fsizeNoLimit))
				case strings.HasPrefix(line, fsizeFailed):
					failed = true
					aerr = strings.TrimPrefix(line, fsizeFailed)
				}
			}
			if !failed {
				if strings.Contains(string(out), fsizeApplied) {
					t.Skip("the size limit did not stop the write (unverified on this platform)")
				}
				t.Fatalf("helper process gave no result:\n%s", out)
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
