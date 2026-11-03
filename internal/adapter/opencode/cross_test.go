package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/as950118/forge/internal/adapter"
	"github.com/as950118/forge/internal/adapter/claudecode"
)

// syncClaude runs a claude-code sync (hooks off) in the same project/home.
func (e *env) syncClaude(scope adapter.Scope) *adapter.Plan {
	e.t.Helper()
	cc := claudecode.New(e.forgeHome)
	in, err := adapter.LoadInputs(context.Background(), e.forgeHome, claudecode.Name)
	if err != nil {
		e.t.Fatal(err)
	}
	p, err := cc.Plan(context.Background(), in, e.req(scope, func(r *adapter.Request) { r.NoHooks = true }))
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), cc, p, adapter.ApplyOptions{BackupDir: e.backupDir()}); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func fileSigOf(t *testing.T, p string) string {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		return "absent"
	}
	b, _ := os.ReadFile(p)
	return adapter.SHA256Hex(b) + "|" + fi.ModTime().String()
}

func TestSkillAlreadyExposedViaClaudeIsNotWrittenAgain(t *testing.T) {
	for _, scope := range []adapter.Scope{adapter.ScopeProject, adapter.ScopeUser} {
		t.Run(string(scope), func(t *testing.T) {
			e := newEnv(t)
			standardRegistry(t, e)
			e.addSkill("second-skill", "1.0.0", nil)
			e.syncClaude(scope)
			ccState := e.ccStatePath(scope)
			before := fileSigOf(t, ccState)

			p := e.mustPlan(scope)
			for _, n := range []string{"demo-skill", "second-skill"} {
				it := itemFor(p, adapter.KindSkill, n)
				if it == nil || it.Action != adapter.ActionSkip || !strings.Contains(it.Reason, ".claude/skills 로 이미 노출됨") || !strings.Contains(it.Reason, "[미확인]") {
					t.Fatalf("%s: %+v", n, it)
				}
			}
			for _, op := range p.Ops {
				if strings.Contains(op.Path, "demo-skill") || strings.Contains(op.Path, "second-skill") {
					t.Fatalf("op for a claimed skill: %s", op.Path)
				}
			}
			if _, err := e.apply(p); err != nil {
				t.Fatal(err)
			}
			if fileSigOf(t, ccState) != before {
				t.Fatal("the claude-code state file was modified by the opencode target")
			}
			if e.existsAbs(filepath.Join(p.Root, relSkills(scope), "demo-skill")) {
				t.Fatal("skill written to the OpenCode directory")
			}
			if scope == adapter.ScopeProject && e.mcpEntry("forge") == nil {
				t.Fatal("MCP is still rendered")
			}
		})
	}
}

func relSkills(scope adapter.Scope) string {
	if scope == adapter.ScopeUser {
		return ".config/opencode/skills"
	}
	return ".opencode/skills"
}

func (e *env) ccStatePath(scope adapter.Scope) string {
	root := e.proj
	if scope == adapter.ScopeUser {
		root = e.home
	}
	return claudecode.New(e.forgeHome).StatePath(scope, root)
}

// OpenCode synced first, claude-code second: the next opencode sync removes its
// own (managed, unmodified) copies so that the skill is exposed once.
func TestOrderIndependentConvergence(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	e.mustSync(adapter.ScopeProject)
	if !e.exists(".opencode/skills/demo-skill/SKILL.md") {
		t.Fatal("setup")
	}
	userSkill := e.read(".opencode/skills/user-skill/SKILL.md")
	e.syncClaude(adapter.ScopeProject)
	if !e.exists(".claude/skills/demo-skill/SKILL.md") {
		t.Fatal("claude-code did not write its copy")
	}
	p, _ := e.mustSync(adapter.ScopeProject)
	it := itemFor(p, adapter.KindSkill, "demo-skill")
	if it == nil || it.Action != adapter.ActionRemove || !strings.Contains(it.Reason, "중복") {
		t.Fatalf("expected removal of the duplicate: %+v", it)
	}
	if e.exists(".opencode/skills/demo-skill") || !e.exists(".claude/skills/demo-skill/SKILL.md") {
		t.Fatal("skill must end up exposed exactly once")
	}
	if e.read(".opencode/skills/user-skill/SKILL.md") != userSkill {
		t.Fatal("user skill touched")
	}
	if e.mustPlan(adapter.ScopeProject).HasChanges() {
		t.Fatal("not converged")
	}
	// A hand-edited managed copy is NOT removed silently: it becomes a conflict.
	e2 := newEnv(t)
	standardRegistry(t, e2)
	e2.mustSync(adapter.ScopeProject)
	e2.write(".opencode/skills/demo-skill/extra.txt", "mine\n", 0o644)
	e2.syncClaude(adapter.ScopeProject)
	p2 := e2.mustPlan(adapter.ScopeProject)
	wantAction(t, p2, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
}

func TestTwoTargetsAreIndependent(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	e.write(".mcp.json", `{"mcpServers":{"mine":{"type":"stdio","command":"x"}}}`+"\n", 0o644)
	standardRegistry(t, e)
	sig := e.decoySig()

	// OpenCode first, then claude-code, then OpenCode again, alternating.
	e.mustSync(adapter.ScopeProject)
	ocState := fileSigOf(t, filepath.Join(e.proj, ".forge", "sync-state.opencode.json"))
	ocJSON := e.read("opencode.json")
	e.syncClaude(adapter.ScopeProject)
	if fileSigOf(t, filepath.Join(e.proj, ".forge", "sync-state.opencode.json")) != ocState || e.read("opencode.json") != ocJSON {
		t.Fatal("claude-code sync touched the opencode state or config")
	}
	ccState := fileSigOf(t, filepath.Join(e.proj, ".forge", "sync-state.json"))
	ccMCP := e.read(".mcp.json")
	e.mustSync(adapter.ScopeProject)
	e.mustSync(adapter.ScopeProject)
	if fileSigOf(t, filepath.Join(e.proj, ".forge", "sync-state.json")) != ccState || e.read(".mcp.json") != ccMCP {
		t.Fatal("opencode sync touched the claude-code state or config")
	}
	// State files hold only their own target; the other target's skills/servers stay.
	for file, target := range map[string]string{"sync-state.json": "claude-code", "sync-state.opencode.json": "opencode"} {
		if !strings.Contains(e.read(".forge/"+file), `"target": "`+target+`"`) {
			t.Fatalf("%s has the wrong target", file)
		}
	}
	if !strings.Contains(e.read(".mcp.json"), `"mine"`) || !strings.Contains(e.read(".mcp.json"), `"--client",`) {
		t.Fatal("claude .mcp.json")
	}
	if !strings.Contains(e.read("opencode.json"), `"command": [`) {
		t.Fatal("opencode.json")
	}
	if !strings.Contains(e.read(".mcp.json"), "claude-code") || strings.Contains(e.read(".mcp.json"), "{env:") {
		t.Fatal("claude config must keep ${VAR} and the claude-code client")
	}
	if strings.Contains(e.read("opencode.json"), "${") || strings.Contains(e.read("opencode.json"), "claude-code") {
		t.Fatal("opencode config must use {env:VAR} and the opencode client")
	}
	// Journals/backups of each target are recognized only by that target.
	ids, _ := adapter.ListBackups(e.backupDir())
	targets := map[string]int{}
	for _, b := range ids {
		targets[b.Target]++
	}
	if targets["opencode"] == 0 || targets["claude-code"] == 0 {
		t.Fatalf("backups by target: %v", targets)
	}
	var ccBackup string
	for _, b := range ids {
		if b.Target == "claude-code" {
			ccBackup = b.ID
		}
	}
	if _, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: ccBackup, Force: true}); err == nil {
		t.Fatal("opencode adapter restored a claude-code backup")
	}
	e.checkDecoy(sig)
}

func TestCorruptClaudeStateOnlyDisablesTheDuplicateCheck(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.write(".forge/sync-state.json", "not json", 0o644)
	p := e.mustPlan(adapter.ScopeProject)
	wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionAdd)
	if !strings.Contains(renderPlan(p), "중복 노출 검사를 건너뜁니다") {
		t.Fatal("no note")
	}
	if e.read(".forge/sync-state.json") != "not json" {
		t.Fatal("claude state touched")
	}
}
