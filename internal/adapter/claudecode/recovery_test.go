package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/mcp"
)

// Crash consistency (TASK-0010 fix round): a sync killed at ANY point must be
// recognizable by the next sync and converge to exactly the state a clean sync
// produces. The engine's Crash hook panics at every step boundary and inside the
// file primitives (temp file written, tree moved aside, ...), which models a
// SIGKILL: no deferred cleanup, no rollback.

var anyTempRe = regexp.MustCompile(`^\.forge-(tmp|old)-`)

// scenario builds an environment with a pending sync.
type scenario struct {
	name  string
	scope adapter.Scope
	build func(t *testing.T) *env
}

func scenarios() []scenario {
	fresh := func(t *testing.T) *env {
		e := newEnv(t)
		installFixture(t, e)
		standardRegistry(t, e)
		return e
	}
	update := func(t *testing.T) *env {
		e := fresh(t)
		e.mustSync(adapter.ScopeProject)
		e.addSkill("demo-skill", "1.1.0", map[string]string{"scripts/run.sh": "#!/bin/sh\necho hi v2\n"})
		must := func(err error) {
			if err != nil {
				t.Fatal(err)
			}
		}
		must(e.mreg.SetExposure(t.Context(), "remote-direct", "claude-code", mcp.ExposureGateway))
		e.addServer(mcp.Spec{Name: "late-add", Transport: "sse", URL: "https://example.com/sse"})
		must(e.mreg.SetEnabled(t.Context(), "fs-direct", false))
		e.addSkill("second-skill", "1.0.0", nil)
		return e
	}
	user := func(t *testing.T) *env {
		e := newEnv(t)
		standardRegistry(t, e)
		e.addSkill("second-skill", "1.0.0", nil)
		return e
	}
	return []scenario{
		{"fresh-project", adapter.ScopeProject, fresh},
		{"update-project", adapter.ScopeProject, update},
		{"fresh-user", adapter.ScopeUser, user},
	}
}

// finalTree hashes every file the sync may have produced or modified.
func finalTree(t *testing.T, e *env, scope adapter.Scope) map[string]string {
	t.Helper()
	out := map[string]string{}
	roots := map[string]string{"proj": e.proj, "home": e.home, "state": filepath.Join(e.forgeHome, "sync-state")}
	for label, root := range roots {
		if scope == adapter.ScopeProject && label == "home" || scope == adapter.ScopeUser && label == "proj" {
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == e.decoy {
				return nil
			}
			if d.IsDir() && d.Name() == ".git" {
				return fs.SkipDir
			}
			if d.Type().IsRegular() {
				b, _ := os.ReadFile(p)
				fi, _ := d.Info()
				rel, _ := filepath.Rel(root, p)
				out[label+"/"+filepath.ToSlash(rel)] = fmt.Sprintf("%o:%s", fi.Mode().Perm(), adapter.SHA256Hex(b))
			}
			return nil
		})
	}
	return out
}

func leftoverTemps(t *testing.T, roots ...string) []string {
	t.Helper()
	var out []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && anyTempRe.MatchString(d.Name()) {
				out = append(out, p)
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

func diffTrees(want, got map[string]string) string {
	var sb strings.Builder
	for k, v := range want {
		if got[k] != v {
			fmt.Fprintf(&sb, "  %s: want %q got %q\n", k, v, got[k])
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			fmt.Fprintf(&sb, "  unexpected %s\n", k)
		}
	}
	return sb.String()
}

func (e *env) applyCrashAt(p *adapter.Plan, k int) (point string) {
	e.t.Helper()
	n := 0
	defer func() {
		if r := recover(); r != nil {
			sc, ok := r.(adapter.SimulatedCrash)
			if !ok {
				panic(r)
			}
			point = sc.Point
		}
	}()
	_, _ = adapter.Apply(context.Background(), e.ad, p, adapter.ApplyOptions{BackupDir: e.backupDir(),
		Crash: func(string) bool { n++; return n-1 == k }})
	return ""
}

func renderPlan(p *adapter.Plan) string {
	var buf bytes.Buffer
	adapter.WriteText(&buf, p, nil, false)
	return buf.String()
}

func TestCrashAtEveryPointConverges(t *testing.T) {
	for _, sc := range scenarios() {
		t.Run(sc.name, func(t *testing.T) {
			// Reference: a clean run, and the list of crash points it passes.
			ref := sc.build(t)
			var points []string
			p := ref.mustPlan(sc.scope)
			if _, err := adapter.Apply(context.Background(), ref.ad, p, adapter.ApplyOptions{BackupDir: ref.backupDir(),
				Crash: func(pt string) bool { points = append(points, pt); return false }}); err != nil {
				t.Fatal(err)
			}
			want := finalTree(t, ref, sc.scope)
			if len(points) < 8 {
				t.Fatalf("suspiciously few crash points: %v", points)
			}
			jw, jc := -1, -1
			for i, pt := range points {
				if pt == "journal-written" {
					jw = i
				}
				if pt == "journal-complete" {
					jc = i
				}
			}
			if jw < 0 || jc < 0 {
				t.Fatalf("journal points missing: %v", points)
			}
			sawTmp := map[string]bool{}
			for k, pt := range points {
				e := sc.build(t)
				plan := e.mustPlan(sc.scope)
				if got := e.applyCrashAt(plan, k); got != pt {
					t.Fatalf("k=%d: crashed at %q, want %q", k, got, pt)
				}
				roots := []string{e.proj, e.home, e.forgeHome}
				tmps := leftoverTemps(t, roots...)
				if len(tmps) > 0 {
					sawTmp[pt[strings.Index(pt, ":")+1:]] = true
				}
				pending := k >= jw && k < jc

				// The next plan notices the interruption and writes nothing.
				before := snapshot(t, roots...)
				p2 := e.mustPlan(sc.scope)
				text := renderPlan(p2)
				if after := snapshot(t, roots...); after != before {
					t.Fatalf("k=%d (%s): the plan after a crash wrote files", k, pt)
				}
				if pending {
					if len(p2.Interrupted) != 1 || !strings.Contains(text, "이전 sync 가 중단되었습니다 (백업 "+p2.Interrupted[0].ID) {
						t.Fatalf("k=%d (%s): plan does not report the interruption:\n%s", k, pt, text)
					}
				} else if len(p2.Interrupted) != 0 || strings.Contains(text, "중단되었습니다") {
					t.Fatalf("k=%d (%s): reported an interruption although none is pending:\n%s", k, pt, text)
				}
				if len(tmps) > 0 && (len(p2.StaleTemps) != len(tmps) || !strings.Contains(text, "정리 예정")) {
					t.Fatalf("k=%d (%s): leftover temp files %v not reported (%v)", k, pt, tmps, p2.StaleTemps)
				}
				if p2.Conflicts() > 0 {
					t.Fatalf("k=%d (%s): a crash with no user edit must not produce conflicts:\n%s", k, pt, text)
				}

				// --apply converges to exactly the clean result.
				res, err := e.apply(p2)
				if err != nil {
					t.Fatalf("k=%d (%s): recovery apply: %v\n%s", k, pt, err, text)
				}
				if got := finalTree(t, e, sc.scope); diffTrees(want, got) != "" {
					t.Fatalf("k=%d (%s): not converged:\n%s\nplan:\n%s", k, pt, diffTrees(want, got), text)
				}
				if left := leftoverTemps(t, roots...); len(left) != 0 {
					t.Fatalf("k=%d (%s): temp files left after recovery: %v", k, pt, left)
				}
				if pending && len(res.Recovered) != 1 {
					t.Fatalf("k=%d (%s): interrupted sync not marked recovered: %+v", k, pt, res)
				}
				if det := adapter.DetectInterrupted(e.ad, e.backupDir(), sc.scope, p2.Root); len(det.Journals) != 0 || len(det.Invalid) != 0 {
					t.Fatalf("k=%d (%s): journals still pending after recovery: %+v", k, pt, det)
				}
				if _, err := os.Lstat(filepath.Join(e.proj, ".forge", "sync-journal.json")); err == nil {
					t.Fatalf("k=%d (%s): journal copy left in the project", k, pt)
				}
				if sc.scope == adapter.ScopeUser {
					if _, err := os.Lstat(filepath.Join(e.forgeHome, "sync-state", "claude-code-user.journal.json")); err == nil {
						t.Fatalf("k=%d (%s): journal copy left in the forge home", k, pt)
					}
				}
				// A second apply changes nothing.
				p3 := e.mustPlan(sc.scope)
				if p3.HasChanges() || len(p3.Interrupted) != 0 || len(p3.StaleTemps) != 0 {
					t.Fatalf("k=%d (%s): second plan not empty:\n%s", k, pt, renderPlan(p3))
				}
				// --restore latest still works after a recovery and the tree converges again.
				if id, err := adapter.ResolveBackup(e.backupDir(), "latest"); err != nil {
					t.Fatalf("k=%d (%s): latest: %v", k, pt, err)
				} else if _, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{
					ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: id, Force: true}); err != nil {
					t.Fatalf("k=%d (%s): restore latest: %v", k, pt, err)
				}
				if pr := e.mustPlan(sc.scope); pr.Conflicts() > 0 {
					t.Fatalf("k=%d (%s): conflicts after restore:\n%s", k, pt, renderPlan(pr))
				}
				e.mustSync(sc.scope)
				if got := finalTree(t, e, sc.scope); diffTrees(want, got) != "" {
					t.Fatalf("k=%d (%s): not converged after restore + sync:\n%s", k, pt, diffTrees(want, got))
				}
			}
			// The interesting intermediate states really occurred.
			for _, need := range []string{"file-tmp-written"} {
				if !sawTmp[need] {
					t.Errorf("no crash point left a temp file at %q (saw %v)", need, sawTmp)
				}
			}
		})
	}
}

func TestCrashLeavesTreeIntermediateStatesRecoverable(t *testing.T) {
	e := scenarios()[1].build(t) // update: an existing managed skill is replaced
	var points []string
	probe := scenarios()[1].build(t)
	if _, err := adapter.Apply(context.Background(), probe.ad, probe.mustPlan(adapter.ScopeProject),
		adapter.ApplyOptions{BackupDir: probe.backupDir(), Crash: func(pt string) bool { points = append(points, pt); return false }}); err != nil {
		t.Fatal(err)
	}
	k := -1
	for i, pt := range points {
		if strings.HasSuffix(pt, ":tree-old-renamed") {
			k = i
			break
		}
	}
	if k < 0 {
		t.Fatalf("no tree-old-renamed point in %v", points)
	}
	e.applyCrashAt(e.mustPlan(adapter.ScopeProject), k)
	// Intermediate state: the managed skill directory is gone, its old copy and the
	// new tree sit next to it under the .forge-tmp-/.forge-old- names.
	if e.exists(".claude/skills/demo-skill") {
		t.Fatal("expected the target to be missing between the two renames")
	}
	if got := leftoverTemps(t, filepath.Join(e.proj, ".claude", "skills")); len(got) != 2 {
		t.Fatalf("want a tmp and an old directory, got %v", got)
	}
	p := e.mustPlan(adapter.ScopeProject)
	if len(p.StaleTemps) != 2 {
		t.Fatalf("plan must list both leftovers: %v", p.StaleTemps)
	}
	res, err := e.apply(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CleanedTemps) != 2 || !e.exists(".claude/skills/demo-skill/SKILL.md") {
		t.Fatalf("recovery result: %+v", res)
	}
}

// A user edit made between the crash and the recovery is never overwritten: the
// item is reported as a conflict that names the interruption.
func TestRecoveryKeepsUserEdits(t *testing.T) {
	sc := scenarios()[0]
	probe := sc.build(t)
	var points []string
	if _, err := adapter.Apply(context.Background(), probe.ad, probe.mustPlan(sc.scope),
		adapter.ApplyOptions{BackupDir: probe.backupDir(), Crash: func(pt string) bool { points = append(points, pt); return false }}); err != nil {
		t.Fatal(err)
	}
	opIndex := func(e *env, p *adapter.Plan, suffix string) int {
		for i, op := range p.Ops {
			if strings.HasSuffix(op.Path, suffix) {
				return i
			}
		}
		t.Fatalf("no op for %s", suffix)
		return -1
	}
	crashAfterOp := func(e *env, suffix string) {
		p := e.mustPlan(sc.scope)
		want := fmt.Sprintf("op-%d:done", opIndex(e, p, suffix))
		for k, pt := range points {
			if pt == want {
				if got := e.applyCrashAt(p, k); got != want {
					t.Fatalf("crashed at %q", got)
				}
				return
			}
		}
		t.Fatalf("no point %s in %v", want, points)
	}

	t.Run("skill edited", func(t *testing.T) {
		e := sc.build(t)
		crashAfterOp(e, "/.claude/skills/demo-skill")
		edited := e.read(".claude/skills/demo-skill/SKILL.md") + "\nMY EDIT\n"
		e.write(".claude/skills/demo-skill/SKILL.md", edited, 0o644)
		before := snapshot(t, e.proj, e.forgeHome)
		p := e.mustPlan(sc.scope)
		it := itemFor(p, adapter.KindSkill, "demo-skill")
		if it == nil || it.Action != adapter.ActionConflict || !strings.Contains(it.Reason, "중단된 이전 sync") {
			t.Fatalf("want a conflict naming the interruption: %+v", it)
		}
		if _, err := e.apply(p); !errors.Is(err, adapter.ErrConflicts) {
			t.Fatalf("want ErrConflicts, got %v", err)
		}
		if snapshot(t, e.proj, e.forgeHome) != before {
			t.Fatal("a refused recovery changed files")
		}
		// --skip-conflicts finishes everything else and still leaves the edit alone.
		p.SkipConflicts = true
		if _, err := e.apply(p); err != nil {
			t.Fatal(err)
		}
		if e.read(".claude/skills/demo-skill/SKILL.md") != edited {
			t.Fatal("the user's edit was overwritten")
		}
		if !e.exists(".mcp.json") || !strings.Contains(e.read(".mcp.json"), `"forge"`) {
			t.Fatal("the rest of the sync was not completed")
		}
	})

	t.Run("mcp file gets an extra user server", func(t *testing.T) {
		e := sc.build(t)
		crashAfterOp(e, "/.mcp.json")
		cur := e.read(".mcp.json")
		edited := strings.Replace(cur, `"mcpServers": {`, `"mcpServers": {`+"\n    "+`"my-own": {"command": "mine"},`, 1)
		if edited == cur {
			t.Fatalf("test setup: could not edit %q", cur)
		}
		e.write(".mcp.json", edited, 0o644)
		p := e.mustPlan(sc.scope)
		if p.Conflicts() != 0 {
			t.Fatalf("an unrelated user server must not conflict: %s", renderPlan(p))
		}
		if _, err := e.apply(p); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(e.read(".mcp.json"), `"my-own": {"command": "mine"}`) {
			t.Fatal("the user's server was lost")
		}
		if e.mustPlan(sc.scope).HasChanges() {
			t.Fatal("not converged")
		}
	})

	t.Run("mcp entry edited", func(t *testing.T) {
		e := sc.build(t)
		crashAfterOp(e, "/.mcp.json")
		cur := e.read(".mcp.json")
		edited := strings.Replace(cur, `"mcp",`, `"mcp", "--my-flag",`, 1)
		if edited == cur {
			t.Fatalf("test setup: could not edit %q", cur)
		}
		e.write(".mcp.json", edited, 0o644)
		p := e.mustPlan(sc.scope)
		it := itemFor(p, adapter.KindMCP, "forge")
		if it == nil || it.Action != adapter.ActionConflict || !strings.Contains(it.Reason, "중단된 이전 sync") {
			t.Fatalf("want a conflict naming the interruption: %+v", it)
		}
	})
}

func TestLeftoverTempsOnlyOurShapeAndNeverThroughLinks(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	victimDir := filepath.Join(t.TempDir(), "victimdir")
	if err := os.MkdirAll(victimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victimDir, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	const dead = "9999999" // beyond any pid limit: no such process
	stale := []string{
		".forge-tmp-" + dead + "-deadbeef-.mcp.json",
		".claude/skills/.forge-old-" + dead + "-0badf00d-demo-skill/SKILL.md",
		".claude/.forge-tmp-" + dead + "-0badf00d-settings.json",
		".forge/.forge-tmp-" + dead + "-0badf00d-sync-state.json",
	}
	for _, rel := range stale {
		e.write(rel, "leftover", 0o600)
	}
	keep := []string{ // user files and look-alikes
		".forge-tmp-notes.txt",
		".forge-tmp-" + dead + "-nothex12-x",
		".forge-tmp-" + dead + "-deadbeef", // no target name
		"forge-tmp-" + dead + "-deadbeef-x",
		".forge-tmp-12ab-deadbeef-x",
		"keepme.txt",
		".claude/skills/user-skill/.forge-tmp-" + dead + "-deadbeef-x", // not a directory we manage
		fmt.Sprintf(".forge-tmp-%d-deadbeef-ours", os.Getppid()),       // a live process (our parent)
	}
	for _, rel := range keep {
		e.write(rel, "user", 0o644)
	}
	link := filepath.Join(e.proj, ".forge-tmp-"+dead+"-feedface-link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(e.proj, ".claude", "skills", ".forge-old-"+dead+"-feedface-dirlink")
	if err := os.Symlink(victimDir, dirLink); err != nil {
		t.Fatal(err)
	}

	before := snapshot(t, e.proj, e.home, e.forgeHome)
	p := e.mustPlan(adapter.ScopeProject)
	if snapshot(t, e.proj, e.home, e.forgeHome) != before {
		t.Fatal("plan wrote files")
	}
	if len(p.StaleTemps) != len(stale) || !strings.Contains(renderPlan(p), "정리 예정") {
		t.Fatalf("plan must list exactly the stale leftovers (%d): %v", len(stale), p.StaleTemps)
	}
	res, err := e.apply(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CleanedTemps) != len(stale) {
		t.Fatalf("cleaned %v", res.CleanedTemps)
	}
	for _, rel := range stale {
		if e.exists(rel) {
			t.Errorf("stale leftover %s survived", rel)
		}
	}
	for _, rel := range keep {
		if !e.exists(rel) {
			t.Errorf("%s was removed but is not ours to remove", rel)
		}
	}
	for _, l := range []string{link, dirLink} {
		if _, err := os.Lstat(l); err != nil {
			t.Errorf("symbolic link %s was removed", l)
		}
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "precious" {
		t.Fatal("a file behind a symbolic link was touched")
	}
	if _, err := os.Stat(filepath.Join(victimDir, "keep")); err != nil {
		t.Fatal("a directory behind a symbolic link was touched")
	}
}

func TestTempsBehindSymlinkedDirectoriesAreNotTouched(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, ".forge-tmp-9999999-deadbeef-x"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.proj, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	p := e.mustPlan(adapter.ScopeProject)
	if len(p.StaleTemps) != 0 {
		t.Fatalf("looked through a symlinked directory: %v", p.StaleTemps)
	}
	p.SkipConflicts = true
	if _, err := e.apply(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, ".forge-tmp-9999999-deadbeef-x")); err != nil {
		t.Fatal("a file behind a symlinked directory was removed")
	}
}

// ---- forged journals ----

func writeForgedJournal(t *testing.T, e *env, id string, j map[string]any) string {
	t.Helper()
	dir := filepath.Join(e.backupDir(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(j, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "journal.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func forged(e *env, ops []map[string]any, items []map[string]any) map[string]any {
	j := map[string]any{"version": 1, "id": "20260101T000000.000Z-abcdef", "created_at": "2026-01-01T00:00:00Z", "kind": "sync",
		"target": "claude-code", "scope": "project", "root": e.proj, "status": "pending", "ops": ops}
	if items != nil {
		j["items"] = items
		j["state_path"] = filepath.Join(e.proj, ".forge", "sync-state.json")
	}
	return j
}

func TestForgedJournalsCannotDeleteOrAdopt(t *testing.T) {
	const id = "20260101T000000.000Z-abcdef"
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	userUserFile := func(e *env) string { return e.proj + "/.claude/skills/user-skill/SKILL.md" }
	hash64 := strings.Repeat("ab", 32)
	op := func(path string) map[string]any {
		return map[string]any{"path": path, "op": "delete-tree", "before_exists": true, "before_hash": hash64}
	}
	cases := map[string]func(e *env) map[string]any{
		"path outside the project": func(e *env) map[string]any { return forged(e, []map[string]any{op(outside)}, nil) },
		"traversal": func(e *env) map[string]any {
			return forged(e, []map[string]any{op(e.proj + "/.claude/skills/../../keepme.txt")}, nil)
		},
		"user file in project": func(e *env) map[string]any { return forged(e, []map[string]any{op(e.proj + "/keepme.txt")}, nil) },
		"user skill file":      func(e *env) map[string]any { return forged(e, []map[string]any{op(userUserFile(e))}, nil) },
		"the Claude user file": func(e *env) map[string]any { return forged(e, []map[string]any{op(e.decoy)}, nil) },
		"relative path":        func(e *env) map[string]any { return forged(e, []map[string]any{op("keepme.txt")}, nil) },
		"bad op kind": func(e *env) map[string]any {
			r := op(e.proj + "/.mcp.json")
			r["op"] = "rm-rf"
			return forged(e, []map[string]any{r}, nil)
		},
		"bad hash": func(e *env) map[string]any {
			r := op(e.proj + "/.mcp.json")
			r["before_hash"] = "zz"
			return forged(e, []map[string]any{r}, nil)
		},
		"state hash mismatch": func(e *env) map[string]any {
			j := forged(e, nil, []map[string]any{{"kind": "mcp", "name": "forge", "hash": hash64, "applied_at": "x"}})
			j["state_hash"] = hash64
			return j
		},
		"item kind": func(e *env) map[string]any {
			return forged(e, nil, []map[string]any{{"kind": "shell", "name": "x", "hash": hash64, "applied_at": "x"}})
		},
		"wrong target": func(e *env) map[string]any { j := forged(e, nil, nil); j["target"] = "opencode"; return j },
		"id mismatch": func(e *env) map[string]any {
			j := forged(e, nil, nil)
			j["id"] = "20260101T000000.000Z-ffffff"
			return j
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			installFixture(t, e)
			standardRegistry(t, e)
			e.write("keepme.txt", "mine", 0o644)
			writeForgedJournal(t, e, id, mk(e))
			userBefore := finalTree(t, e, adapter.ScopeProject)
			sig := e.decoySig()

			p := e.mustPlan(adapter.ScopeProject)
			if len(p.Interrupted) != 0 {
				t.Fatalf("a forged journal was believed: %+v", p.Interrupted)
			}
			if name != "wrong target" && !strings.Contains(strings.Join(p.Notes, "\n"), id) && len(p.InvalidJournals) == 0 {
				// "wrong target" is another adapter's journal (ignored silently); the others are reported.
				t.Fatalf("forged journal not reported: notes=%v", p.Notes)
			}
			if _, err := e.apply(p); err != nil {
				t.Fatal(err)
			}
			after := finalTree(t, e, adapter.ScopeProject)
			for _, rel := range []string{"proj/keepme.txt", "proj/.claude/skills/user-skill/SKILL.md"} {
				if after[rel] != userBefore[rel] || after[rel] == "" {
					t.Errorf("%s was changed or deleted", rel)
				}
			}
			if b, err := os.ReadFile(outside); err != nil || string(b) != "precious" {
				t.Fatal("a file outside the project was touched")
			}
			e.checkDecoy(sig)
		})
	}
}

// A well-formed journal that lies about content cannot make Forge adopt (or
// remove) user-owned items: adoption needs the exact content hash, skills also
// need the management marker.
func TestForgedJournalCannotAdoptUserItems(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	// A user skill with the same name as a registry skill, no marker.
	e.write(".claude/skills/demo-skill/SKILL.md", "---\nname: demo-skill\ndescription: mine\n---\nuser\n", 0o644)
	userHash, _, err := adapter.TreeHash(filepath.Join(e.proj, ".claude/skills/demo-skill"), true)
	if err != nil {
		t.Fatal(err)
	}
	// And a user-defined server nobody wanted removed.
	mcpPath := filepath.Join(e.proj, ".mcp.json")
	userMCP := e.read(".mcp.json")
	items := []map[string]any{
		{"kind": "skill", "name": "demo-skill", "hash": userHash, "applied_at": "x"},
		{"kind": "mcp", "name": "mine-srv", "hash": strings.Repeat("cd", 32), "applied_at": "x"},
		{"kind": "skill", "name": "user-skill", "hash": strings.Repeat("ef", 32), "applied_at": "x"},
	}
	j := forged(e, nil, items)
	st := &adapter.State{Version: adapter.StateVersion, Target: Name, Scope: adapter.ScopeProject, Items: []adapter.StateItem{
		{Kind: "skill", Name: "demo-skill", Hash: userHash, AppliedAt: "x"},
		{Kind: "mcp", Name: "mine-srv", Hash: strings.Repeat("cd", 32), AppliedAt: "x"},
		{Kind: "skill", Name: "user-skill", Hash: strings.Repeat("ef", 32), AppliedAt: "x"},
	}}
	j["state_hash"] = adapter.SHA256Hex(st.Marshal())
	writeForgedJournal(t, e, "20260101T000000.000Z-abcdef", j)
	p := e.mustPlan(adapter.ScopeProject)
	wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
	if it := itemFor(p, adapter.KindSkill, "user-skill"); it != nil && it.Action != adapter.ActionSkip && it.Action != adapter.ActionConflict {
		t.Fatalf("user-skill: %+v", it)
	}
	p.SkipConflicts = true
	if _, err := e.apply(p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.read(".claude/skills/demo-skill/SKILL.md"), "user\n") {
		t.Fatal("the user's skill was overwritten")
	}
	if !e.exists(".claude/skills/user-skill/SKILL.md") {
		t.Fatal("the user's other skill was removed")
	}
	_ = mcpPath
	_ = userMCP
}

// The journal copy inside a repository is never trusted: only the journal in the
// backup directory counts.
func TestJournalCopyInTheProjectIsNotTrusted(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	forgedCopy, _ := json.Marshal(forged(e, []map[string]any{{"path": e.proj + "/.mcp.json", "op": "delete-file", "before_exists": true}}, nil))
	e.write(".forge/sync-journal.json", string(forgedCopy), 0o644)
	p := e.mustPlan(adapter.ScopeProject)
	if len(p.Interrupted) != 0 || len(p.Notes) == 0 {
		t.Fatalf("%+v", p.Interrupted)
	}
	if _, err := e.apply(p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.read(".mcp.json"), `"mcpServers"`) {
		t.Fatal(".mcp.json was damaged")
	}
}

func TestJournalsOfOtherRootsAreIgnored(t *testing.T) {
	a := newEnv(t)
	installFixture(t, a)
	standardRegistry(t, a)
	plan := a.mustPlan(adapter.ScopeProject)
	if got := a.applyCrashAt(plan, 12); got == "" {
		t.Fatal("expected a crash")
	}
	// Same forge home, another project: the pending journal belongs to project a.
	b := newEnv(t)
	other := filepath.Join(b.proj, "..", "proj2")
	if err := os.MkdirAll(filepath.Join(other, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.ad = New(a.forgeHome)
	p, err := b.ad.Plan(context.Background(), adapter.Inputs{Client: Name}, adapter.Request{Scope: adapter.ScopeProject, ProjectDir: other,
		Home: b.home, ForgeBin: "forge", BackupDir: a.backupDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Interrupted) != 0 {
		t.Fatalf("another project's interrupted sync leaked into this plan: %+v", p.Interrupted)
	}
}

func TestSyncInProgressIsNotAnInterruption(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	e.applyCrashAt(e.mustPlan(adapter.ScopeProject), 12)
	p := e.mustPlan(adapter.ScopeProject, func(r *adapter.Request) { r.SyncInProgress = true })
	if len(p.Interrupted) != 0 || !strings.Contains(strings.Join(p.Notes, "\n"), "진행 중") {
		t.Fatalf("a running sync must not be reported as interrupted: %+v %v", p.Interrupted, p.Notes)
	}
}

func TestRolledBackAndCompletedJournalsAreNotInterruptions(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	p := e.mustPlan(adapter.ScopeProject)
	_, err := adapter.Apply(context.Background(), e.ad, p, adapter.ApplyOptions{BackupDir: e.backupDir(),
		Inject: func(step int, _ adapter.Op) error {
			if step == 2 {
				return errors.New("injected")
			}
			return nil
		}})
	if err == nil {
		t.Fatal("want failure")
	}
	if p2 := e.mustPlan(adapter.ScopeProject); len(p2.Interrupted) != 0 {
		t.Fatalf("a rolled-back apply is not an interruption: %+v", p2.Interrupted)
	}
	if _, err := e.apply(p); err != nil {
		t.Fatal(err)
	}
	if p3 := e.mustPlan(adapter.ScopeProject); len(p3.Interrupted) != 0 || p3.HasChanges() {
		t.Fatal("completed apply must leave no pending journal")
	}
	// No journal copies remain.
	if e.exists(".forge/sync-journal.json") {
		t.Fatal("journal copy left behind")
	}
	// The journal in the backup directory records the outcome.
	list, _ := adapter.ListBackups(e.backupDir())
	var states []string
	for _, b := range list {
		raw, _ := os.ReadFile(filepath.Join(b.Path, "journal.json"))
		var j struct{ Status string }
		_ = json.Unmarshal(raw, &j)
		states = append(states, j.Status)
	}
	if strings.Join(states, ",") != "rolled_back,complete" {
		t.Fatalf("journal statuses: %v", states)
	}
}

func TestJournalContentIsRecorded(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	p := e.mustPlan(adapter.ScopeProject)
	var jsonSeen []byte
	_, err := adapter.Apply(context.Background(), e.ad, p, adapter.ApplyOptions{BackupDir: e.backupDir(),
		Crash: func(pt string) bool {
			if pt == "journal-copy-written" {
				list, _ := adapter.ListBackups(e.backupDir())
				jsonSeen, _ = os.ReadFile(filepath.Join(list[0].Path, "journal.json"))
				cp, err := os.ReadFile(filepath.Join(e.proj, ".forge", "sync-journal.json"))
				if err != nil || !bytes.Equal(cp, jsonSeen) {
					t.Errorf("journal copy missing or different: %v", err)
				}
			}
			return false
		}})
	if err != nil {
		t.Fatal(err)
	}
	var j adapter.Journal
	if err := json.Unmarshal(jsonSeen, &j); err != nil {
		t.Fatal(err)
	}
	last := j.Ops[len(j.Ops)-1]
	if j.Status != "pending" || j.StatePath != filepath.Join(e.proj, ".forge", "sync-state.json") || last.Path != j.StatePath {
		t.Fatalf("the state file must be the last operation and recorded: %+v", j)
	}
	stateBytes := e.read(".forge/sync-state.json")
	if j.StateHash != adapter.SHA256Hex([]byte(stateBytes)) || last.AfterHash != j.StateHash {
		t.Fatal("journal state hash does not match the state file that was written")
	}
	if len(j.Items) == 0 || j.ID == "" {
		t.Fatalf("journal lacks items or id: %+v", j)
	}
}

// Rolling back an interrupted sync by hand makes it a non-event for the next sync.
func TestRestoringAnInterruptedBackupClearsTheInterruption(t *testing.T) {
	e := scenarios()[0].build(t)
	p := e.mustPlan(adapter.ScopeProject)
	if got := e.applyCrashAt(p, 8); got == "" {
		t.Fatal("expected a crash")
	}
	p2 := e.mustPlan(adapter.ScopeProject)
	if len(p2.Interrupted) != 1 {
		t.Fatalf("setup: %+v", p2.Interrupted)
	}
	id := p2.Interrupted[0].ID
	if _, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{
		ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: id, Force: true}); err != nil {
		t.Fatal(err)
	}
	p3 := e.mustPlan(adapter.ScopeProject)
	if len(p3.Interrupted) != 0 {
		t.Fatalf("restored backup still reported as interrupted: %+v", p3.Interrupted)
	}
	if _, err := e.apply(p3); err != nil {
		t.Fatal(err)
	}
	if e.mustPlan(adapter.ScopeProject).HasChanges() {
		t.Fatal("not converged")
	}
}
