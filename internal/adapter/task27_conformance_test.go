package adapter_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/as950118/forge/internal/adapter"
	"github.com/as950118/forge/internal/mcp"
)

// TASK-0027 AC-15 / AC-16 / AC-17: the same scenarios against every target.

// forgeJournal writes <backups>/<id>/journal.json from a map (a forged journal).
func forgeJournal(t *testing.T, c *conformProject, id string, j map[string]any) {
	t.Helper()
	dir := filepath.Join(c.forge, "backups", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(j, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "journal.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// AC-15: restore_of of a journal is validated (only a restore journal may carry
// one, and it must have the shape of a backup id), so a forged value is reported
// as an invalid journal and never reaches the warning.
func TestSharedJournalRestoreOfIsValidated(t *testing.T) {
	const id = "20260101T000000.000Z-abcdef"
	const target = "20260101T000001.000Z-123456"
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			ad := c.ad
			mk := func(kind, restoreOf string) map[string]any {
				j := map[string]any{"version": 1, "id": id, "created_at": "2026-01-01T00:00:00Z", "kind": kind, "target": tg.name,
					"scope": "project", "root": c.proj, "status": "pending", "ops": []any{}}
				if restoreOf != "" {
					j["restore_of"] = restoreOf
				}
				return j
			}
			detect := func(j map[string]any) *adapter.Detection {
				if err := os.RemoveAll(filepath.Join(c.forge, "backups", id)); err != nil {
					t.Fatal(err)
				}
				forgeJournal(t, c, id, j)
				return adapter.DetectInterrupted(ad, filepath.Join(c.forge, "backups"), adapter.ScopeProject, c.proj)
			}
			if d := detect(mk("restore", target)); len(d.Infos) != 1 || d.Infos[0].RestoreOf != target || len(d.Invalid) != 0 {
				t.Fatalf("control: a restore journal with a valid restore_of must be believed: %+v", d)
			}
			if d := detect(mk("restore", "")); len(d.Infos) != 1 || d.Infos[0].RestoreOf != "" {
				t.Fatalf("a legacy restore journal (no restore_of) must be believed: %+v", d)
			}
			for name, j := range map[string]map[string]any{
				"restore_of on a sync journal": mk("sync", target),
				"path-like restore_of":         mk("restore", "../../etc/passwd"),
				"escape in restore_of":         mk("restore", "x\x1b[31m"),
				"long restore_of":              mk("restore", strings.Repeat("a", 500)),
				"upper-case id":                mk("restore", "20260101T000001.000Z-ABCDEF"),
			} {
				d := detect(j)
				if len(d.Infos) != 0 || len(d.Invalid) != 1 || d.Invalid[0] != id {
					t.Errorf("%s: want the journal rejected, got infos=%+v invalid=%v", name, d.Infos, d.Invalid)
				}
			}
		})
	}
}

// AC-16: the journal copy next to the state file only explains a lost backup when
// it is believable in every respect. Each subtest changes ONE thing and expects
// that nothing is reported (the control reports it).
func TestSharedLostBackupJournalCopyIsValidated(t *testing.T) {
	for _, tg := range conformTargets {
		tg := tg
		setup := func(t *testing.T) (c *conformProject, plan func() *adapter.Plan, id, copyPath string) {
			c = newConformProject(t, tg)
			skills := []adapter.SkillInput{conformSkill(t, c.forge, "lost-skill")}
			plan = func() *adapter.Plan {
				p, err := c.plan(t, skills)
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			p0 := plan()
			copyPath = p0.JournalPath
			id = c.applyCrashAt(t, p0, "op-0:done")
			if _, err := os.Stat(copyPath); err != nil {
				t.Fatalf("the crash left no journal copy at %s: %v", copyPath, err)
			}
			return
		}
		edit := func(t *testing.T, path string, f func(m map[string]any)) {
			t.Helper()
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			f(m)
			out, _ := json.MarshalIndent(m, "", "  ")
			if err := os.WriteFile(path, out, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		removeBackup := func(t *testing.T, c *conformProject, id string) {
			if err := os.RemoveAll(filepath.Join(c.forge, "backups", id)); err != nil {
				t.Fatal(err)
			}
		}
		t.Run(tg.name+"/control", func(t *testing.T) {
			c, plan, id, _ := setup(t)
			removeBackup(t, c, id)
			if p := plan(); len(p.LostBackups) != 1 || p.LostBackups[0] != id {
				t.Fatalf("control: want %s reported, got %v", id, p.LostBackups)
			}
		})
		mutations := map[string]func(m map[string]any){
			"status complete":    func(m map[string]any) { m["status"] = "complete" },
			"status rolled_back": func(m map[string]any) { m["status"] = "rolled_back" },
			"other root":         func(m map[string]any) { m["root"] = "/somewhere/else" },
			"other scope":        func(m map[string]any) { m["scope"] = "user" },
			"other target":       func(m map[string]any) { m["target"] = "someone-else" },
			"state hash":         func(m map[string]any) { m["state_hash"] = strings.Repeat("0", 64) },
			"op outside": func(m map[string]any) {
				m["ops"] = []any{map[string]any{"path": "/etc/hosts", "op": "write_file", "before_exists": true, "after_exists": true}}
			},
		}
		// The same single-field changes on a journal copy that has no operations or
		// items: the path validation (which also fails for a copy of another root,
		// scope or target) cannot be what refuses it, so the explicit comparisons of
		// detectLostBackup are what is tested.
		emptied := func(m map[string]any) {
			m["ops"], m["items"] = []any{}, nil
			delete(m, "state_path")
			delete(m, "state_hash")
		}
		t.Run(tg.name+"/emptied control", func(t *testing.T) {
			c, plan, id, copyPath := setup(t)
			removeBackup(t, c, id)
			edit(t, copyPath, emptied)
			if p := plan(); len(p.LostBackups) != 1 {
				t.Fatalf("control: want the lost backup reported, got %v", p.LostBackups)
			}
		})
		for name, mut := range map[string]func(m map[string]any){
			"root":   func(m map[string]any) { m["root"] = "/somewhere/else" },
			"scope":  func(m map[string]any) { m["scope"] = "user" },
			"target": func(m map[string]any) { m["target"] = "someone-else" },
			"status": func(m map[string]any) { m["status"] = "complete" },
		} {
			mut := mut
			t.Run(tg.name+"/emptied "+name, func(t *testing.T) {
				c, plan, id, copyPath := setup(t)
				removeBackup(t, c, id)
				edit(t, copyPath, func(m map[string]any) { emptied(m); mut(m) })
				if p := plan(); len(p.LostBackups) != 0 {
					t.Fatalf("a journal copy of another %s explained a lost backup: %v", name, p.LostBackups)
				}
			})
		}
		for name, mut := range mutations {
			mut := mut
			t.Run(tg.name+"/"+name, func(t *testing.T) {
				c, plan, id, copyPath := setup(t)
				removeBackup(t, c, id)
				edit(t, copyPath, mut)
				if p := plan(); len(p.LostBackups) != 0 {
					t.Fatalf("a journal copy that is not believable explained a lost backup: %v notes=%v", p.LostBackups, p.Notes)
				}
			})
		}
		t.Run(tg.name+"/symlinked copy", func(t *testing.T) {
			c, plan, id, copyPath := setup(t)
			removeBackup(t, c, id)
			real := filepath.Join(filepath.Dir(copyPath), "elsewhere.json")
			if err := os.Rename(copyPath, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, copyPath); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if p := plan(); len(p.LostBackups) != 0 {
				t.Fatalf("a symlinked journal copy was believed: %v", p.LostBackups)
			}
		})
		t.Run(tg.name+"/backup directory still there", func(t *testing.T) {
			// The backup exists but its journal is no longer pending (finished or
			// rolled back): not a lost backup, and not an interruption either.
			c, plan, id, _ := setup(t)
			edit(t, filepath.Join(c.forge, "backups", id, "journal.json"), func(m map[string]any) { m["status"] = "rolled_back" })
			if p := plan(); len(p.LostBackups) != 0 || len(p.Interrupted) != 0 {
				t.Fatalf("lost=%v interrupted=%+v", p.LostBackups, p.Interrupted)
			}
		})
		t.Run(tg.name+"/backup journal forged", func(t *testing.T) {
			// The backup's own journal is not believable (reported as invalid): the
			// copy must not claim the backup is lost.
			c, plan, id, _ := setup(t)
			edit(t, filepath.Join(c.forge, "backups", id, "journal.json"), func(m map[string]any) { m["state_hash"] = strings.Repeat("0", 64) })
			p := plan()
			if len(p.LostBackups) != 0 || len(p.InvalidJournals) != 1 {
				t.Fatalf("lost=%v invalid=%v", p.LostBackups, p.InvalidJournals)
			}
		})
	}
}

// AC-17: the lost-backup explanation says what happens once the copy is cleaned
// up after a --skip-conflicts apply (the conflicts come back in generic words).
func TestSharedLostBackupNoteWarnsAboutGenericWordingAfterSkip(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			skills := []adapter.SkillInput{conformSkill(t, c.forge, "lost-skill")}
			p0, err := c.plan(t, skills)
			if err != nil {
				t.Fatal(err)
			}
			id := c.applyCrashAt(t, p0, "op-0:done")
			if err := os.RemoveAll(filepath.Join(c.forge, "backups", id)); err != nil {
				t.Fatal(err)
			}
			p, err := c.plan(t, skills)
			if err != nil {
				t.Fatal(err)
			}
			// Full text of the report (notes are no longer cut at 300 bytes).
			var sb strings.Builder
			adapter.WriteText(&sb, p, nil, false)
			for _, want := range []string{"--skip-conflicts", "--force-managed", "일반 문구의 충돌로 다시 나타납니다", "저널 사본은 정리됩니다", "--skip-conflicts 로 제외한 항목은 다음 실행에서"} {
				if !strings.Contains(sb.String(), want) {
					t.Errorf("the report lacks %q:\n%s", want, sb.String())
				}
			}
			// The documented limit is real: skip the conflict, apply, and the item
			// comes back with the generic reason.
			if _, err := adapter.Apply(context.Background(), c.ad, mustSkip(p), adapter.ApplyOptions{BackupDir: filepath.Join(c.forge, "backups")}); err != nil {
				t.Fatalf("apply --skip-conflicts: %v", err)
			}
			p2, err := c.plan(t, skills)
			if err != nil {
				t.Fatal(err)
			}
			if len(p2.LostBackups) != 0 {
				t.Fatalf("the journal copy survived the apply: %v", p2.LostBackups)
			}
			it := conformItem(t, p2, adapter.KindSkill, "lost-skill")
			if it.Action != adapter.ActionConflict || !strings.Contains(it.Reason, "복사된 디렉터리") {
				t.Fatalf("documented limit changed (update docs/sync.md): %s %q", it.Action, it.Reason)
			}
		})
	}
}

func mustSkip(p *adapter.Plan) *adapter.Plan {
	p.SkipConflicts = true
	return p
}

// AC-17: restoring a backup closes a restore journal that predates restore_of (it
// names no backup) when everything it did was putting back that backup's state.
func TestSharedLegacyRestoreJournalIsClosedByTheFinishingRestore(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			v1 := conformSkillV(t, c.forge, "legacy-skill", "1.0.0")
			v2 := conformSkillV(t, c.forge, "legacy-skill", "2.0.0")
			for _, v := range []adapter.SkillInput{v1, v2} {
				p, err := c.plan(t, []adapter.SkillInput{v})
				if err != nil {
					t.Fatal(err)
				}
				c.apply(t, p, nil)
			}
			b2 := c.lastBackup(t) // v1 -> v2
			bdir := filepath.Join(c.forge, "backups")
			// Kill a restore of b2, then strip restore_of from its journal and manifest
			// (what an older Forge wrote).
			func() {
				defer func() {
					if r := recover(); r != nil {
						if _, ok := r.(adapter.SimulatedCrash); !ok {
							panic(r)
						}
					}
				}()
				_, _ = adapter.Restore(context.Background(), c.ad, adapter.RestoreOptions{
					ApplyOptions: adapter.ApplyOptions{BackupDir: bdir, Crash: func(pt string) bool { return pt == "op-0:done" }}, ID: b2, Force: true})
			}()
			safety := c.lastBackup(t)
			if safety == b2 {
				t.Fatal("no safety backup")
			}
			strip := func(file string) {
				b, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				delete(m, "restore_of")
				out, _ := json.MarshalIndent(m, "", "  ")
				if err := os.WriteFile(file, out, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			strip(filepath.Join(bdir, safety, "journal.json"))
			strip(filepath.Join(bdir, safety, "manifest.json"))
			p, err := c.plan(t, []adapter.SkillInput{v2})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Interrupted) != 1 || p.Interrupted[0].Kind != "restore" || p.Interrupted[0].RestoreOf != "" {
				t.Fatalf("control: want a legacy restore interruption, got %+v", p.Interrupted)
			}
			if _, err := adapter.Restore(context.Background(), c.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: bdir}, ID: b2, Force: true}); err != nil {
				t.Fatalf("restore: %v", err)
			}
			p, err = c.plan(t, []adapter.SkillInput{v1})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Interrupted) != 0 {
				t.Fatalf("the finished restore left the legacy interruption: %+v", p.Interrupted)
			}
		})
	}
}

// AC-19: a long note is shown in full in the text and the JSON report.
func TestSharedLongNotesAreNotCutInTheReport(t *testing.T) {
	long := strings.Repeat("가나다라마바사 ", 60) + "끝까지-보여야-하는-안내"
	p := &adapter.Plan{Target: "claude-code", Scope: adapter.ScopeProject, Root: "/r", Notes: []string{long},
		Items: []adapter.Item{{Kind: adapter.KindMCP, Name: "srv", Action: adapter.ActionConflict, Reason: strings.Repeat("가나다 ", 60) + "사유-끝"}}}
	var sb strings.Builder
	adapter.WriteText(&sb, p, nil, false)
	r := adapter.BuildReport(p, nil, false)
	if !strings.Contains(sb.String(), "끝까지-보여야-하는-안내") || !strings.Contains(r.Notes[0], "끝까지-보여야-하는-안내") {
		t.Errorf("the note was cut:\n%s", sb.String())
	}
	if !strings.Contains(r.Items[0].Reason, "사유-끝") || !strings.Contains(sb.String(), "사유-끝") {
		t.Errorf("the reason was cut:\n%s", sb.String())
	}
	_ = mcp.SlotEnv
}

// ---- AC-6: a real file system error in the middle of an apply ----

// treeSig describes every file (mode, content hash) and directory below roots.
func treeSig(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && d.Name() == ".git" {
				return filepath.SkipDir
			}
			if d.IsDir() {
				out[p] = "dir"
				return nil
			}
			b, _ := os.ReadFile(p)
			fi, _ := d.Info()
			out[p] = fi.Mode().String() + ":" + adapter.SHA256Hex(b)
			return nil
		})
	}
	return out
}

// The directory that receives operation k is made read-only AFTER planning (the
// plan's read-only check has passed): the write fails with a real EACCES from the
// operating system, not through the Inject hook. The automatic rollback must put
// every file back, leave no temporary files and no pending journal, and the same
// plan must apply cleanly once the directory is writable again. (A genuinely full
// disk cannot be produced in a test: a tmpfs needs privileges and `ulimit -f` only
// limits file size in a child process; EACCES exercises the same code path: the
// first failed write of executeOp. Docs: docs/sync.md.)
func TestSharedRealWriteFailureRollsBackEverything(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	rolledBack := map[string]int{}
	defer func() {
		for _, tg := range conformTargets {
			if rolledBack[tg.name] == 0 {
				t.Errorf("%s: no operation failed in the middle of the writes (only refusals before them)", tg.name)
			}
		}
	}()
	for _, tg := range conformTargets {
		probe := newConformProject(t, tg)
		skills := []adapter.SkillInput{conformSkill(t, probe.forge, "fail-skill")}
		servers := []*mcp.Info{conformServer("fail-srv", "node-fail")}
		pp, err := probe.ad.Plan(context.Background(), adapter.Inputs{Client: tg.name, Skills: skills, Servers: servers}, probe.req())
		if err != nil {
			t.Fatal(err)
		}
		nOps := len(pp.Ops)
		if nOps < 3 {
			t.Fatalf("%s: want at least 3 operations to fail in the middle, got %d", tg.name, nOps)
		}
		for k := 0; k < nOps; k++ {
			k := k
			t.Run(tg.name+"/op-"+string(rune('0'+k)), func(t *testing.T) {
				c := newConformProject(t, tg)
				sk := []adapter.SkillInput{conformSkill(t, c.forge, "fail-skill")}
				p, err := c.ad.Plan(context.Background(), adapter.Inputs{Client: tg.name, Skills: sk, Servers: servers}, c.req())
				if err != nil {
					t.Fatal(err)
				}
				bad := filepath.Dir(p.Ops[k].Path)
				if err := os.MkdirAll(bad, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(bad, 0o755) })
				roots := []string{c.proj, filepath.Join(c.forge, "sync-state")}
				before := treeSig(t, roots...)
				if err := os.Chmod(bad, 0o555); err != nil {
					t.Fatal(err)
				}
				_, aerr := adapter.Apply(context.Background(), c.ad, p, adapter.ApplyOptions{BackupDir: filepath.Join(c.forge, "backups")})
				if err := os.Chmod(bad, 0o755); err != nil {
					t.Fatal(err)
				}
				if aerr == nil {
					t.Fatalf("an apply into a read-only directory (%s) succeeded", bad)
				}
				switch {
				case strings.Contains(aerr.Error(), "롤백했습니다"):
					rolledBack[tg.name]++
				case strings.Contains(aerr.Error(), "아무것도 변경하지 않았습니다"):
					// the journal copy (or its directory) could not be written: refused before the first change
				default:
					t.Fatalf("neither a rollback nor a refusal before the first change: %v", aerr)
				}
				after := treeSig(t, roots...)
				for path, v := range before {
					if after[path] != v {
						t.Errorf("not restored: %s: %q -> %q", path, v, after[path])
					}
				}
				for path := range after {
					if _, ok := before[path]; !ok {
						t.Errorf("left behind after the rollback: %s", path)
					}
				}
				p2, err := c.ad.Plan(context.Background(), adapter.Inputs{Client: tg.name, Skills: sk, Servers: servers}, c.req())
				if err != nil {
					t.Fatal(err)
				}
				if len(p2.Interrupted) != 0 || len(p2.LostBackups) != 0 || len(p2.StaleTemps) != 0 {
					t.Fatalf("the rollback left a trace: interrupted=%+v lost=%v temps=%v", p2.Interrupted, p2.LostBackups, p2.StaleTemps)
				}
				if _, err := adapter.Apply(context.Background(), c.ad, p2, adapter.ApplyOptions{BackupDir: filepath.Join(c.forge, "backups")}); err != nil {
					t.Fatalf("the same sync after fixing the permissions: %v", err)
				}
			})
		}
	}
}

// AC-19: a registry snapshot with a hard-linked file is refused with its own
// reason, not as a hash mismatch ("tampered or damaged").
func TestSharedHardLinkedRegistrySnapshotHasItsOwnReason(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			sk := conformSkill(t, c.forge, "linked-snap")
			outside := filepath.Join(filepath.Dir(c.forge), "outside.txt")
			if err := os.WriteFile(outside, []byte("not a skill file\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(outside, filepath.Join(sk.StoreDir, "linked.txt")); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			p, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			it := conformItem(t, p, adapter.KindSkill, "linked-snap")
			if it.Action != adapter.ActionConflict || it.Overridable || !strings.Contains(it.Reason, "하드링크") || strings.Contains(it.Reason, "변조·손상") {
				t.Fatalf("want the hard-link reason, got %s overridable=%v %q", it.Action, it.Overridable, it.Reason)
			}
			for _, op := range p.Ops {
				if strings.HasSuffix(op.Path, "linked-snap") {
					t.Fatal("the skill with a hard-linked snapshot file is in the operations")
				}
			}
			// A genuine content mismatch keeps the tampered wording.
			sk2 := conformSkill(t, c.forge, "edited-snap")
			if err := os.WriteFile(filepath.Join(sk2.StoreDir, "SKILL.md"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			p, err = c.plan(t, []adapter.SkillInput{sk2})
			if err != nil {
				t.Fatal(err)
			}
			if it := conformItem(t, p, adapter.KindSkill, "edited-snap"); !strings.Contains(it.Reason, "변조·손상(해시 불일치)") {
				t.Fatalf("a hash mismatch must keep the tampered wording: %q", it.Reason)
			}
		})
	}
}

// AC-12: the note for an invalid journal names the XDG_CONFIG_HOME possibility.
func TestSharedInvalidJournalNoteMentionsXDG(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			const id = "20260101T000000.000Z-abcdef"
			forgeJournal(t, c, id, map[string]any{"version": 1, "id": id, "created_at": "2026-01-01T00:00:00Z", "kind": "sync", "target": tg.name,
				"scope": "project", "root": c.proj, "status": "pending", "ops": []any{}, "restore_of": id})
			p, err := c.plan(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			notes := strings.Join(p.Notes, "\n")
			if len(p.InvalidJournals) != 1 || !strings.Contains(notes, "XDG_CONFIG_HOME") || !strings.Contains(notes, id) {
				t.Fatalf("invalid=%v notes=%s", p.InvalidJournals, notes)
			}
		})
	}
}
