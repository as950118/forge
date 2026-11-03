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
	"time"

	"github.com/as950118/forge/internal/adapter"
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
	fsizeEnvHome   = "FORGE_TEST_FSIZE_HOME"
	fsizeEnvVar    = "FORGE_TEST_FSIZE_VARIANT"
	fsizeApplied   = "FSIZE-RESULT-APPLY-OK"
	fsizeFailed    = "FSIZE-RESULT-APPLY-ERROR: "
	fsizeNoLimit   = "FSIZE-RESULT-NO-RLIMIT: "
)

// The two skill sets of the limited write. "single": one skill whose 64 KiB file does
// not fit. "small-first": a tiny skill whose directory is written successfully before
// the big one fails, so the rollback has a completed operation to undo (a rollback
// that is never called, or that only undoes the failing operation, leaves the tiny
// skill behind). Names sort a-small < z-big, the order the plan writes them in.
const (
	fsizeVarSingle     = "single"
	fsizeVarSmallFirst = "small-first"
)

func fsizeSkills(t *testing.T, forge, variant string) []adapter.SkillInput {
	t.Helper()
	names := []string{"big-skill"}
	if variant == fsizeVarSmallFirst {
		names = []string{"a-small", "z-big"}
	}
	var out []adapter.SkillInput
	for _, n := range names {
		sk := conformSkill(t, forge, n)
		if n == "big-skill" || n == "z-big" {
			if err := os.WriteFile(filepath.Join(sk.StoreDir, "big.bin"), make([]byte, 64<<10), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		h, _, err := adapter.TreeHash(sk.StoreDir, false)
		if err != nil {
			t.Fatal(err)
		}
		sk.Hash = h
		out = append(out, sk)
	}
	return out
}

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
	// The parent's c.home (HOME of the child is the same value; passed explicitly so the
	// helper never depends on the inherited environment).
	c := &conformProject{forge: forge, proj: proj, home: os.Getenv(fsizeEnvHome), ad: tg.adapter(forge), tg: tg}
	if c.home == "" {
		t.Fatal("no " + fsizeEnvHome)
	}
	p, err := c.plan(t, fsizeSkills(t, forge, os.Getenv(fsizeEnvVar)))
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
	for _, variant := range []string{fsizeVarSingle, fsizeVarSmallFirst} {
		for _, tg := range conformTargets {
			t.Run(variant+"/"+tg.name, func(t *testing.T) {
				runFsizeVariant(t, tg, variant)
			})
		}
	}
}

func runFsizeVariant(t *testing.T, tg conformTarget, variant string) {
	c := newConformProject(t, tg)
	skills := fsizeSkills(t, c.forge, variant)
	roots := []string{c.proj, filepath.Join(c.forge, "sync-state")}
	before := treeSig(t, roots...)
	if variant == fsizeVarSmallFirst {
		// The plan must write the tiny skill before the big one, or the scenario
		// (a completed operation followed by a failing one) does not exist.
		pp, err := c.plan(t, skills)
		if err != nil {
			t.Fatal(err)
		}
		small, big := -1, -1
		for i, op := range pp.Ops {
			switch filepath.Base(op.Path) {
			case "a-small":
				small = i
			case "z-big":
				big = i
			}
		}
		if small < 0 || big < 0 || small > big {
			t.Fatalf("plan order: a-small at %d, z-big at %d (want a-small first): %+v", small, big, pp.Ops)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFsizeHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HOME="+c.home, fsizeEnvTarget+"="+tg.name, fsizeEnvForge+"="+c.forge, fsizeEnvProj+"="+c.proj,
		fsizeEnvHome+"="+c.home, fsizeEnvVar+"="+variant)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("helper process timed out (60s): %v\n%s", ctx.Err(), out)
	}
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
	if !strings.Contains(aerr, "롤백했습니다") {
		t.Errorf("the failure was not an automatic rollback (a refusal before the first change does not exercise it): %s", aerr)
	}
	if variant == fsizeVarSmallFirst && !strings.Contains(aerr, "z-big") {
		t.Errorf("the failing operation should be the big skill, got: %s", aerr)
	}
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
	p2, err := c.plan(t, skills)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Interrupted) != 0 || len(p2.StaleTemps) != 0 {
		t.Fatalf("a failed write left a trace: interrupted=%+v temps=%v (error: %v)", p2.Interrupted, p2.StaleTemps, aerr)
	}
	if _, err := adapter.Apply(context.Background(), c.ad, p2, adapter.ApplyOptions{BackupDir: filepath.Join(c.forge, "backups")}); err != nil {
		t.Fatalf("the same sync without the limit: %v", err)
	}
}
