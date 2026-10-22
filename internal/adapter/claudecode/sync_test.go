package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/mcp"
)

func TestPlanWritesNothing(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	for _, scope := range []adapter.Scope{adapter.ScopeProject, adapter.ScopeUser} {
		before := snapshot(t, e.proj, e.home, e.forgeHome)
		sig := e.decoySig()
		p := e.mustPlan(scope)
		adapter.BuildDiffs(p)
		var buf bytes.Buffer
		adapter.WriteText(&buf, p, nil, true)
		if err := adapter.WriteJSON(&buf, adapter.BuildReport(p, nil, true)); err != nil {
			t.Fatal(err)
		}
		if after := snapshot(t, e.proj, e.home, e.forgeHome); after != before {
			t.Fatalf("%s: plan changed files on disk", scope)
		}
		e.checkDecoy(sig)
	}
	if _, err := os.Stat(e.backupDir()); err == nil {
		t.Fatal("plan must not create a backup directory")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	e.mustSync(adapter.ScopeProject)
	files := []string{".mcp.json", ".claude/settings.json", ".forge/sync-state.json", ".claude/skills/demo-skill/SKILL.md", ".claude/skills/demo-skill/.forge-managed"}
	mt := map[string]time.Time{}
	bytesBefore := map[string]string{}
	for _, f := range files {
		fi, err := os.Stat(filepath.Join(e.proj, f))
		if err != nil {
			t.Fatal(err)
		}
		mt[f], bytesBefore[f] = fi.ModTime(), e.read(f)
	}
	nBackups, _ := adapter.ListBackups(e.backupDir())
	time.Sleep(20 * time.Millisecond)
	p, res := e.mustSync(adapter.ScopeProject)
	if p.HasChanges() || len(res.Changed) != 0 || res.BackupID != "" {
		t.Fatalf("second apply must be a no-op: %+v", res)
	}
	for _, f := range files {
		fi, _ := os.Stat(filepath.Join(e.proj, f))
		if !fi.ModTime().Equal(mt[f]) || e.read(f) != bytesBefore[f] {
			t.Errorf("%s changed by the second apply", f)
		}
	}
	if n, _ := adapter.ListBackups(e.backupDir()); len(n) != len(nBackups) {
		t.Fatal("a no-op apply must not create a backup")
	}
}

func TestUserItemsAreNeverTouched(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	e.mustSync(adapter.ScopeProject)
	var mc, st map[string]any
	_ = json.Unmarshal([]byte(e.read(".mcp.json")), &mc)
	servers := mc["mcpServers"].(map[string]any)
	for _, n := range []string{"github", "local-db", "forge", "fs-direct", "remote-direct"} {
		if _, ok := servers[n]; !ok {
			t.Errorf("server %s missing", n)
		}
	}
	if _, ok := servers["gw-proxied"]; ok {
		t.Error("gateway server must not be written to .mcp.json")
	}
	if _, ok := servers["hidden-one"]; ok {
		t.Error("hidden server must not be written")
	}
	_ = json.Unmarshal([]byte(e.read(".claude/settings.json")), &st)
	if st["model"] != "sonnet" || st["permissions"] == nil || st["env"] == nil {
		t.Fatal("unrelated settings keys lost")
	}
	// Forge-specific fields are never added to the Claude files.
	for _, f := range []string{".mcp.json", ".claude/settings.json"} {
		low := strings.ToLower(e.read(f))
		for _, bad := range []string{"managed", "x-forge", "_forge"} {
			if strings.Contains(low, bad) {
				t.Errorf("%s contains forge marker %q", f, bad)
			}
		}
	}
	if !strings.Contains(e.read(".claude/skills/user-skill/SKILL.md"), "Written by the user") {
		t.Fatal("user skill changed")
	}
	// Types are always explicit.
	for name, v := range servers {
		if _, ok := v.(map[string]any)["type"]; !ok {
			t.Errorf("%s has no explicit type", name)
		}
	}
}

func TestConflictUserSkillBlocksEverythingByDefault(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	e.write(".claude/skills/demo-skill/SKILL.md", "mine\n", 0o644) // same name, no marker
	p := e.mustPlan(adapter.ScopeProject)
	wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
	if p.Conflicts() != 1 {
		t.Fatalf("conflicts=%d", p.Conflicts())
	}
	before := snapshot(t, e.proj, e.home)
	_, err := e.apply(p)
	if !errors.Is(err, adapter.ErrConflicts) {
		t.Fatalf("want ErrConflicts, got %v", err)
	}
	if snapshot(t, e.proj, e.home) != before {
		t.Fatal("apply with conflicts must write nothing")
	}
	if _, serr := os.Stat(e.backupDir()); serr == nil {
		t.Fatal("no backup may be created when apply is refused")
	}
	// --skip-conflicts applies the rest and leaves the user's skill alone.
	p2 := e.mustPlan(adapter.ScopeProject)
	p2.SkipConflicts = true
	if _, err := e.apply(p2); err != nil {
		t.Fatal(err)
	}
	if e.read(".claude/skills/demo-skill/SKILL.md") != "mine\n" || e.exists(".claude/skills/demo-skill/.forge-managed") {
		t.Fatal("user skill must be untouched")
	}
	if !strings.Contains(e.read(".mcp.json"), `"forge"`) || strings.Contains(e.read(".forge/sync-state.json"), "demo-skill") {
		t.Fatal("rest must be applied and the conflicting skill must not be recorded as managed")
	}
}

func TestConflictUserServerAndGatewayName(t *testing.T) {
	e := newEnv(t)
	e.write(".mcp.json", `{"mcpServers":{"fs-direct":{"type":"stdio","command":"mine"},"forge":{"type":"stdio","command":"my-forge"}}}`+"\n", 0o644)
	standardRegistry(t, e)
	p := e.mustPlan(adapter.ScopeProject)
	wantAction(t, p, adapter.KindMCP, "fs-direct", adapter.ActionConflict)
	wantAction(t, p, adapter.KindMCP, "forge", adapter.ActionConflict)
	wantAction(t, p, adapter.KindMCP, "remote-direct", adapter.ActionAdd)
	p.SkipConflicts = true
	if _, err := e.apply(p); err != nil {
		t.Fatal(err)
	}
	s := e.read(".mcp.json")
	if !strings.Contains(s, `"command":"mine"`) || !strings.Contains(s, `"command":"my-forge"`) || !strings.Contains(s, "remote-direct") {
		t.Fatalf("user servers must be byte-preserved and the rest added:\n%s", s)
	}
	if strings.Contains(e.read(".forge/sync-state.json"), `"fs-direct"`) {
		t.Fatal("a user server must never be recorded as managed")
	}
}

func TestConflictRegistryServerNamedForge(t *testing.T) {
	e := newEnv(t)
	e.addServer(mcp.Spec{Name: "forge", Transport: "stdio", Command: "x"})
	p := e.mustPlan(adapter.ScopeProject)
	it := itemFor(p, adapter.KindMCP, "forge")
	if it == nil || it.Action != adapter.ActionConflict {
		t.Fatalf("registry server named forge must conflict: %+v", p.Items)
	}
}

func TestManagedItemsEditedByHandAreConflicts(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	e.mustSync(adapter.ScopeProject)

	// skill: add a file; mcp: change a command; hook: change the timeout
	e.write(".claude/skills/demo-skill/extra.txt", "mine\n", 0o644)
	mcpJSON := strings.Replace(e.read(".mcp.json"), `"fs-server"`, `"evil-server"`, 1)
	e.write(".mcp.json", mcpJSON, 0o644)
	settings := strings.Replace(e.read(".claude/settings.json"), `"timeout": 5`, `"timeout": 99`, 1)
	e.write(".claude/settings.json", settings, 0o644)

	p := e.mustPlan(adapter.ScopeProject)
	wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
	wantAction(t, p, adapter.KindMCP, "fs-direct", adapter.ActionConflict)
	conflictHooks := 0
	for _, it := range p.Items {
		if it.Kind == adapter.KindHook && it.Action == adapter.ActionConflict {
			conflictHooks++
			if !it.Overridable {
				t.Error("a modified managed hook is overridable with --force-managed")
			}
		}
	}
	if conflictHooks != 1 {
		t.Fatalf("want 1 hook conflict, got %d", conflictHooks)
	}
	before := snapshot(t, e.proj)
	if _, err := e.apply(p); !errors.Is(err, adapter.ErrConflicts) {
		t.Fatalf("got %v", err)
	}
	if snapshot(t, e.proj) != before {
		t.Fatal("conflicting apply wrote something")
	}

	// --force-managed overwrites the managed items (backed up), not user items.
	p2 := e.mustPlan(adapter.ScopeProject, func(r *adapter.Request) { r.ForceManaged = true })
	if p2.Conflicts() != 0 {
		t.Fatalf("force-managed leaves conflicts: %+v", p2.Items)
	}
	res, err := e.apply(p2)
	if err != nil {
		t.Fatal(err)
	}
	if e.exists(".claude/skills/demo-skill/extra.txt") || strings.Contains(e.read(".mcp.json"), "evil-server") || strings.Contains(e.read(".claude/settings.json"), `"timeout": 99`) {
		t.Fatal("managed items must be restored")
	}
	if !strings.Contains(e.read(".claude/settings.json"), "my-skill-audit.sh") || !strings.Contains(e.read(".mcp.json"), "local-db") {
		t.Fatal("user items must survive --force-managed")
	}
	m, _, err := adapter.LoadManifest(e.backupDir(), res.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	foundExtra := false
	for _, en := range m.Entries {
		if filepath.Base(en.Path) == "demo-skill" && en.Before.Exists {
			foundExtra = true
		}
	}
	if !foundExtra {
		t.Fatal("overwritten managed skill must be in the backup")
	}
}

func TestMarkerWithoutStateAndStateWithoutMarker(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.mustSync(adapter.ScopeProject)
	// state lost: marker present, no state record -> conflict
	if err := os.Remove(filepath.Join(e.proj, ".forge/sync-state.json")); err != nil {
		t.Fatal(err)
	}
	p := e.mustPlan(adapter.ScopeProject)
	wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
	wantAction(t, p, adapter.KindMCP, "forge", adapter.ActionConflict)
}

func TestMarkerRemovedMeansUserDirectory(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.mustSync(adapter.ScopeProject)
	if err := os.Remove(filepath.Join(e.proj, ".claude/skills/demo-skill/.forge-managed")); err != nil {
		t.Fatal(err)
	}
	p := e.mustPlan(adapter.ScopeProject)
	wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
}

func TestUnparsableJSONAbortsWithoutWriting(t *testing.T) {
	secret := "sk-test-LEAKLEAKLEAKLEAK9999"
	cases := map[string]struct{ file, content string }{
		"mcp comment":    {".mcp.json", "{\n  // my servers\n  \"mcpServers\": {}\n}\n"},
		"mcp trailing":   {".mcp.json", `{"mcpServers":{},}`},
		"mcp array":      {".mcp.json", `[]`},
		"mcp empty":      {".mcp.json", ``},
		"mcp bad type":   {".mcp.json", `{"mcpServers":[]}`},
		"settings bad":   {".claude/settings.json", "{\"env\":{\"K\":\"" + secret + "\"}, oops}"},
		"settings array": {".claude/settings.json", `[1]`},
		"hooks not obj":  {".claude/settings.json", `{"hooks":[]}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			standardRegistry(t, e)
			e.write(c.file, c.content, 0o644)
			before := snapshot(t, e.proj, e.home)
			_, err := e.plan(adapter.ScopeProject)
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaks file content: %v", err)
			}
			if snapshot(t, e.proj, e.home) != before {
				t.Fatal("files changed")
			}
		})
	}
}

func TestHookMergeIsHandlerLevelAndRemovalRestoresBytes(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	orig := e.read(".claude/settings.json")
	e.mustSync(adapter.ScopeProject)
	merged := e.read(".claude/settings.json")
	var doc struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal([]byte(merged), &doc); err != nil {
		t.Fatal(err)
	}
	pre := doc.Hooks["PreToolUse"]
	if len(pre) != 2 || pre[0].Matcher != "Bash" || len(pre[1].Hooks) != 2 ||
		pre[1].Hooks[0].Command != "./scripts/my-skill-audit.sh" || !strings.HasPrefix(pre[1].Hooks[1].Command, "forge hook ingest") {
		t.Fatalf("PreToolUse must keep the user group and append one handler: %+v", pre)
	}
	if len(doc.Hooks["Notification"]) != 1 {
		t.Fatal("Notification hook lost")
	}
	// --no-hooks (changed semantics, TASK-0010 fix round) neither installs nor
	// removes: already installed managed hooks and their state stay exactly as they are.
	stateBefore := e.read(".forge/sync-state.json")
	pn, rn := e.mustSync(adapter.ScopeProject, func(r *adapter.Request) { r.NoHooks = true })
	if pn.HasChanges() || rn.BackupID != "" {
		t.Fatalf("--no-hooks must not touch installed managed hooks: %+v", pn.Items)
	}
	if e.read(".claude/settings.json") != merged || e.read(".forge/sync-state.json") != stateBefore {
		t.Fatal("--no-hooks changed settings.json or the state file")
	}
	// --remove-hooks removes exactly the managed handlers; the file returns to its original bytes.
	p, _ := e.mustSync(adapter.ScopeProject, func(r *adapter.Request) { r.RemoveHooks = true })
	nRemoved := 0
	for _, it := range p.Items {
		if it.Kind == adapter.KindHook {
			if it.Action != adapter.ActionRemove {
				t.Errorf("hook %s: %s", it.Name, it.Action)
			}
			nRemoved++
		}
	}
	if nRemoved == 0 {
		t.Fatal("--remove-hooks removed nothing")
	}
	if got := e.read(".claude/settings.json"); got != orig {
		t.Fatalf("settings.json not restored byte-for-byte:\n%s", got)
	}
	if strings.Contains(e.read(".forge/sync-state.json"), `"hook"`) {
		t.Fatal("hook state entries must be gone")
	}
}

func TestNoHooksAndRemoveHooksAreExclusive(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	if _, err := e.plan(adapter.ScopeProject, func(r *adapter.Request) { r.NoHooks, r.RemoveHooks = true, true }); err == nil {
		t.Fatal("--no-hooks together with --remove-hooks must be an error")
	}
}

func TestUserHookWithSameCommandIsNotAdopted(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.write(".claude/settings.json", `{"hooks":{"PreToolUse":[{"matcher":"Skill|mcp__.*","hooks":[{"type":"command","command":"forge hook ingest --client claude-code --event pre","timeout":5}]}]}}`+"\n", 0o644)
	p := e.mustPlan(adapter.ScopeProject)
	for _, it := range p.Items {
		if it.Kind == adapter.KindHook && it.Name == "PreToolUse" {
			if it.Action != adapter.ActionConflict {
				t.Fatalf("user hook with the same command must be a conflict: %+v", it)
			}
		}
	}
	p.SkipConflicts = true
	if _, err := e.apply(p); err != nil {
		t.Fatal(err)
	}
	if strings.Count(e.read(".claude/settings.json"), "--event pre") != 1 {
		t.Fatal("user hook duplicated or removed")
	}
}

func TestHooksTouchNothingWhenDisabledAndUnmanaged(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.write(".claude/settings.json", "{ \"theme\":   \"dark\" }\n", 0o644)
	e.mustSync(adapter.ScopeProject, func(r *adapter.Request) { r.NoHooks = true })
	if e.read(".claude/settings.json") != "{ \"theme\":   \"dark\" }\n" {
		t.Fatal("--no-hooks must not modify settings.json")
	}
}

func TestSecretsAreMaskedAndRefused(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	// Bypass the registry's own checks: a server whose literal value is a secret.
	in := adapter.Inputs{Client: Name, Servers: []*mcp.Info{{
		Name: "leaky", Scope: "global", Transport: mcp.TransportStdio, Command: "x", Enabled: true,
		Exposure: map[string]string{Name: mcp.ExposureDirect},
		Values:   []mcp.Value{{Slot: mcp.SlotEnv, Key: "SERVICE_URL", RefType: mcp.RefLiteral, Literal: "sk-test-LEAKLEAKLEAKLEAK9999", Display: "sk-test-LEAKLEAKLEAKLEAK9999"}},
	}, {
		Name: "keyed", Scope: "global", Transport: mcp.TransportHTTP, URL: "https://example.com/x", Enabled: true,
		Exposure: map[string]string{Name: mcp.ExposureDirect},
		Values:   []mcp.Value{{Slot: mcp.SlotHeader, Key: "X-Api-Key", IsSecret: true, RefType: mcp.RefLiteral, Literal: "plain-secret-value", Display: "plain-secret-value"}},
	}}}
	p, err := e.ad.Plan(context.Background(), in, e.req(adapter.ScopeProject))
	if err != nil {
		t.Fatal(err)
	}
	wantAction(t, p, adapter.KindMCP, "leaky", adapter.ActionConflict)
	wantAction(t, p, adapter.KindMCP, "keyed", adapter.ActionConflict)
	adapter.BuildDiffs(p)
	var out bytes.Buffer
	adapter.WriteText(&out, p, nil, true)
	_ = adapter.WriteJSON(&out, adapter.BuildReport(p, nil, true))
	for _, leak := range []string{"LEAKLEAK", "plain-secret-value", "FAKEFAKE", "Bearer ${GITHUB_TOKEN}"} {
		if strings.Contains(out.String(), leak) {
			t.Fatalf("output leaks %q:\n%s", leak, out.String())
		}
	}
	for _, op := range p.Ops {
		if bytes.Contains(op.Data, []byte("LEAKLEAK")) || bytes.Contains(op.Data, []byte("plain-secret-value")) {
			t.Fatal("secret rendered into a file")
		}
	}
}

func TestUserFileContextLinesAreMaskedInDiff(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	// A user secret right next to the lines Forge edits.
	e.write(".claude/settings.json", "{\n  \"env\": {\n    \"ANTHROPIC_API_KEY\": \"sk-test-FAKEFAKEFAKEFAKE0000\",\n    \"DB_PASSWORD\": \"hunter2hunter2\"\n  }\n}\n", 0o644)
	p := e.mustPlan(adapter.ScopeProject)
	adapter.BuildDiffs(p)
	var out bytes.Buffer
	adapter.WriteText(&out, p, nil, true)
	if strings.Contains(out.String(), "FAKEFAKE") || strings.Contains(out.String(), "hunter2") {
		t.Fatalf("diff context leaks user secrets:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"ANTHROPIC_API_KEY": "***"`) {
		t.Fatalf("expected masked context line:\n%s", out.String())
	}
}

func TestDecoyUserFileIsNeverTouched(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	sig := e.decoySig()
	// project flow
	p, r := e.mustSync(adapter.ScopeProject)
	_ = p
	// user flow (skills, settings.json, commands only for MCP)
	e.mustSync(adapter.ScopeUser)
	// restore both
	if _, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: r.BackupID, Force: true}); err != nil {
		t.Fatal(err)
	}
	e.checkDecoy(sig)
	// the guard refuses the name wherever it appears
	for _, path := range []string{e.decoy, filepath.Join(e.proj, ".claude"+".json")} {
		if _, err := adapter.Snapshot(path); err == nil {
			t.Fatalf("Snapshot must refuse %s", path)
		}
		if _, _, _, err := adapter.ReadFileSafe(path); err == nil {
			t.Fatalf("ReadFileSafe must refuse %s", path)
		}
		if err := adapter.WriteFileAtomic(path, []byte("x"), 0o600); err == nil {
			t.Fatalf("WriteFileAtomic must refuse %s", path)
		}
		if _, err := e.ad.AllowedPath(adapter.ScopeUser, e.home, path); err == nil {
			t.Fatal("AllowedPath must refuse it")
		}
	}
	e.checkDecoy(sig)
}

// The only place that may spell the Claude Code user file name in non-test
// sync code is adapter/guard.go (the refusal itself).
func TestSourcesNeverMentionTheUserFile(t *testing.T) {
	name := "." + "claude" + ".json"
	for _, dir := range []string{".", "../", "../jsonedit", "../../cli"} {
		ents, _ := os.ReadDir(dir)
		for _, en := range ents {
			n := en.Name()
			if en.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || n == "guard.go" {
				continue
			}
			if dir == "../../cli" && n != "sync.go" {
				continue
			}
			b, _ := os.ReadFile(filepath.Join(dir, n))
			if strings.Contains(string(b), name) {
				t.Errorf("%s/%s mentions the Claude Code user file", dir, n)
			}
		}
	}
}

func TestSymlinksAreRefused(t *testing.T) {
	t.Run("linked .claude directory", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		other := filepath.Join(filepath.Dir(e.proj), "elsewhere")
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, filepath.Join(e.proj, ".claude")); err != nil {
			t.Fatal(err)
		}
		p := e.mustPlan(adapter.ScopeProject)
		wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
		for _, it := range p.Items {
			if it.Kind == adapter.KindHook && it.Action != adapter.ActionConflict {
				t.Errorf("hook through a linked directory must conflict: %+v", it)
			}
		}
		p.SkipConflicts = true
		if _, err := e.apply(p); err != nil {
			t.Fatal(err)
		}
		if ents, _ := os.ReadDir(other); len(ents) != 0 {
			t.Fatalf("wrote through the symlink: %v", ents)
		}
	})
	t.Run("linked .mcp.json", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		target := filepath.Join(filepath.Dir(e.proj), "shared.json")
		if err := os.WriteFile(target, []byte(`{"mcpServers":{}}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(e.proj, ".mcp.json")); err != nil {
			t.Fatal(err)
		}
		p := e.mustPlan(adapter.ScopeProject)
		wantAction(t, p, adapter.KindMCP, "forge", adapter.ActionConflict)
		p.SkipConflicts = true
		if _, err := e.apply(p); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(target); string(b) != `{"mcpServers":{}}`+"\n" {
			t.Fatal("linked file target modified")
		}
	})
	t.Run("linked skill dir", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		other := filepath.Join(filepath.Dir(e.proj), "skillsrc")
		_ = os.MkdirAll(other, 0o755)
		_ = os.MkdirAll(filepath.Join(e.proj, ".claude/skills"), 0o755)
		if err := os.Symlink(other, filepath.Join(e.proj, ".claude/skills/demo-skill")); err != nil {
			t.Fatal(err)
		}
		p := e.mustPlan(adapter.ScopeProject)
		wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
	})
	t.Run("apply re-checks links created after planning", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		p := e.mustPlan(adapter.ScopeProject)
		other := filepath.Join(filepath.Dir(e.proj), "late")
		_ = os.MkdirAll(other, 0o755)
		if err := os.Symlink(other, filepath.Join(e.proj, ".claude")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.apply(p); err == nil {
			t.Fatal("apply must refuse a path that became a symlink")
		}
		if ents, _ := os.ReadDir(other); len(ents) != 0 {
			t.Fatal("wrote through a late symlink")
		}
	})
}

func TestPlanToApplyChangeIsDetected(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	p := e.mustPlan(adapter.ScopeProject)
	e.write(".mcp.json", e.read(".mcp.json")+"\n", 0o644) // someone else edits after planning
	before := snapshot(t, e.proj, e.home)
	_, err := e.apply(p)
	if !errors.Is(err, adapter.ErrStale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
	if snapshot(t, e.proj, e.home) != before {
		t.Fatal("stale apply wrote something")
	}
	if _, serr := os.Stat(e.backupDir()); serr == nil {
		t.Fatal("stale apply must not create a backup")
	}
	// A file created after planning is detected too.
	e2 := newEnv(t)
	standardRegistry(t, e2)
	p2 := e2.mustPlan(adapter.ScopeProject)
	e2.write(".mcp.json", `{"mcpServers":{}}`+"\n", 0o644)
	if _, err := e2.apply(p2); !errors.Is(err, adapter.ErrStale) {
		t.Fatalf("want ErrStale for a new file, got %v", err)
	}
}

func TestRollbackOnMidwayFailure(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	before := snapshot(t, e.proj, e.home)
	p := e.mustPlan(adapter.ScopeProject)
	if len(p.Ops) < 3 {
		t.Fatalf("need several ops, got %d", len(p.Ops))
	}
	for failAt := 1; failAt < len(p.Ops); failAt++ {
		_, err := adapter.Apply(context.Background(), e.ad, p, adapter.ApplyOptions{BackupDir: e.backupDir(),
			Inject: func(step int, op adapter.Op) error {
				if step == failAt {
					return errors.New("injected failure")
				}
				return nil
			}})
		if err == nil || !strings.Contains(err.Error(), "롤백") {
			t.Fatalf("failAt=%d: want rollback error, got %v", failAt, err)
		}
		if snapshot(t, e.proj, e.home) != before {
			t.Fatalf("failAt=%d: files differ from the original after rollback", failAt)
		}
	}
	list, _ := adapter.ListBackups(e.backupDir())
	for _, b := range list {
		if b.Status != adapter.StatusRolledBack {
			t.Fatalf("backup %s status %s", b.ID, b.Status)
		}
	}
	// A rolled-back backup cannot be restored; "latest" skips it.
	if _, err := adapter.ResolveBackup(e.backupDir(), "latest"); err == nil {
		t.Fatal("latest must skip rolled-back backups")
	}
	// The same plan still applies cleanly afterwards.
	if _, err := e.apply(p); err != nil {
		t.Fatal(err)
	}
}

func TestBackupManifestAndRestore(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	origMCP := e.read(".mcp.json")
	_, res := e.mustSync(adapter.ScopeProject)
	m, dir, err := adapter.LoadManifest(e.backupDir(), res.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != adapter.StatusApplied || m.Target != Name || m.Scope != adapter.ScopeProject || m.Root != e.proj {
		t.Fatalf("manifest: %+v", m)
	}
	byPath := map[string]adapter.Entry{}
	for _, en := range m.Entries {
		byPath[en.Path] = en
	}
	mcpEntry := byPath[filepath.Join(e.proj, ".mcp.json")]
	if !mcpEntry.Before.Exists || mcpEntry.Before.Hash != adapter.SHA256Hex([]byte(origMCP)) || mcpEntry.Before.Mode != 0o644 {
		t.Fatalf("mcp entry: %+v", mcpEntry)
	}
	if st := byPath[filepath.Join(e.proj, ".forge/sync-state.json")]; st.Before.Exists {
		t.Fatal("a file created by sync must be recorded as not existing before")
	}
	// permissions: backup dir 0700, files 0600
	for _, p := range []string{dir, filepath.Join(dir, "manifest.json")} {
		fi, _ := os.Stat(p)
		want := os.FileMode(0o600)
		if fi.IsDir() {
			want = 0o700
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %o want %o", p, fi.Mode().Perm(), want)
		}
	}
	_ = filepath.WalkDir(filepath.Join(dir, "files"), func(p string, d os.DirEntry, err error) error {
		if err == nil {
			fi, _ := d.Info()
			if fi.IsDir() && fi.Mode().Perm() != 0o700 || fi.Mode().IsRegular() && fi.Mode().Perm()&0o077 != 0 {
				t.Errorf("backup path %s has mode %o", p, fi.Mode().Perm())
			}
		}
		return nil
	})

	// restore: dry run writes nothing; real restore reverts + deletes created files.
	snap := snapshot(t, e.proj)
	opts := adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: res.BackupID, DryRun: true}
	rr, err := adapter.Restore(context.Background(), e.ad, opts)
	if err != nil || len(rr.Entries) != len(m.Entries) {
		t.Fatalf("dry run: %v %+v", err, rr)
	}
	if snapshot(t, e.proj) != snap {
		t.Fatal("restore --dry-run wrote files")
	}
	opts.DryRun = false
	rr, err = adapter.Restore(context.Background(), e.ad, opts)
	if err != nil {
		t.Fatal(err)
	}
	if e.read(".mcp.json") != origMCP || e.exists(".forge/sync-state.json") || e.exists(".claude/skills/demo-skill") {
		t.Fatal("restore did not revert the project")
	}
	if rr.NewBackupID == "" {
		t.Fatal("restore must preserve the current state as a new backup")
	}
	// ... and that backup brings the synced state back (undo of the restore).
	if _, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: rr.NewBackupID}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.read(".mcp.json"), `"forge"`) || !e.exists(".claude/skills/demo-skill/SKILL.md") {
		t.Fatal("undoing the restore must bring the synced files back")
	}
	if got := e.read(".claude/skills/demo-skill/scripts/run.sh"); got == "" {
		t.Fatal("skill tree not restored")
	}
	if fi, _ := os.Stat(filepath.Join(e.proj, ".claude/skills/demo-skill/scripts/run.sh")); fi.Mode().Perm() != 0o755 {
		t.Fatalf("exec bit lost: %o", fi.Mode().Perm())
	}
}

func TestRestoreDriftNeedsForce(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	_, res := e.mustSync(adapter.ScopeProject)
	edited := e.read(".mcp.json") + "\n"
	e.write(".mcp.json", edited, 0o644)
	opts := adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: res.BackupID}
	before := snapshot(t, e.proj)
	rr, err := adapter.Restore(context.Background(), e.ad, opts)
	if !errors.Is(err, adapter.ErrDrift) || rr.Drift != 1 {
		t.Fatalf("want ErrDrift with 1 drift, got %v %+v", err, rr)
	}
	if snapshot(t, e.proj) != before {
		t.Fatal("refused restore wrote files")
	}
	opts.Force = true
	rr, err = adapter.Restore(context.Background(), e.ad, opts)
	if err != nil {
		t.Fatal(err)
	}
	// The user's later edit is preserved in the pre-restore backup.
	m, dir, _ := adapter.LoadManifest(e.backupDir(), rr.NewBackupID)
	found := false
	for _, en := range m.Entries {
		if filepath.Base(en.Path) == ".mcp.json" {
			b, _ := os.ReadFile(filepath.Join(dir, "files", strings.TrimPrefix(en.Path, "/")))
			found = string(b) == edited
		}
	}
	if !found {
		t.Fatal("current (user-edited) state was not preserved by the pre-restore backup")
	}
}

func TestRestoreRejectsTamperedManifests(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	_, res := e.mustSync(adapter.ScopeProject)
	m, dir, _ := adapter.LoadManifest(e.backupDir(), res.BackupID)
	outside := filepath.Join(filepath.Dir(e.proj), "victim.txt")
	_ = os.WriteFile(outside, []byte("precious"), 0o644)
	link := filepath.Join(e.proj, ".claude", "skills", "linked")
	_ = os.Symlink(filepath.Dir(e.proj), link)
	bad := []string{
		outside, // outside the project
		filepath.Join(e.proj, "..", "victim.txt"),                     // not clean
		filepath.Join(e.proj, "README.md"),                            // inside but not a managed path
		filepath.Join(e.proj, ".claude/skills/demo-skill/SKILL.md"),   // deeper than a skill dir
		filepath.Join(e.proj, ".claude", "skills", "..", "..", "etc"), // traversal
		e.decoy,                  // the user file
		filepath.Join(link, "x"), // below a symlink
		"relative/path.json",     // not absolute
	}
	for _, p := range bad {
		mm := *m
		mm.Entries = append([]adapter.Entry{}, m.Entries...)
		mm.Entries = append(mm.Entries, adapter.Entry{Path: p, Before: adapter.FileState{}})
		if err := adapter.ValidateManifest(e.ad, &mm); err == nil {
			t.Errorf("manifest path %q must be rejected", p)
		}
	}
	// Tampered manifest on disk: the restore refuses before touching anything.
	m.Entries = append(m.Entries, adapter.Entry{Path: outside, Before: adapter.FileState{Exists: true, Kind: "file", Hash: adapter.SHA256Hex([]byte("x")), Mode: 0o644}})
	b, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o600)
	before := snapshot(t, e.proj)
	_, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: res.BackupID, Force: true})
	if err == nil {
		t.Fatal("tampered manifest must be refused")
	}
	if b, _ := os.ReadFile(outside); string(b) != "precious" || snapshot(t, e.proj) != before {
		t.Fatal("files changed by a refused restore")
	}
	// ids with path syntax are refused
	for _, id := range []string{"../x", "latest/../..", "20261022T000000.000Z-aaaaaa/../.."} {
		if _, _, err := adapter.LoadManifest(e.backupDir(), id); err == nil {
			t.Errorf("id %q must be refused", id)
		}
	}
}

func TestResolveBackupAndPrune(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	var ids []string
	for i := 0; i < 4; i++ {
		e.write(".mcp.json", fmt.Sprintf("{\"n\": %d}\n", i), 0o644)
		_, res := e.mustSync(adapter.ScopeProject, func(r *adapter.Request) { r.NoHooks = true })
		if res.BackupID == "" {
			t.Fatal("expected a backup")
		}
		ids = append(ids, res.BackupID)
		// make the next plan non-empty again
		_ = os.Remove(filepath.Join(e.proj, ".forge/sync-state.json"))
		_ = os.RemoveAll(filepath.Join(e.proj, ".claude"))
		time.Sleep(2 * time.Millisecond)
	}
	got, err := adapter.ResolveBackup(e.backupDir(), "latest")
	if err != nil || got != ids[3] {
		t.Fatalf("latest=%s err=%v want %s", got, err, ids[3])
	}
	if got, err := adapter.ResolveBackup(e.backupDir(), ids[1][:20]); err != nil || got != ids[1] {
		t.Fatalf("prefix: %s %v", got, err)
	}
	if _, err := adapter.ResolveBackup(e.backupDir(), "2026"); err == nil {
		t.Fatal("ambiguous prefix must fail")
	}
	// dry run deletes nothing; prune keeps the newest N and only touches backups
	stray := filepath.Join(e.backupDir(), "keep-me.txt")
	_ = os.WriteFile(stray, []byte("x"), 0o600)
	removed, err := adapter.PruneBackups(e.backupDir(), 2, true)
	if err != nil || len(removed) != 2 {
		t.Fatalf("dry run: %v %v", removed, err)
	}
	if l, _ := adapter.ListBackups(e.backupDir()); len(l) != 4 {
		t.Fatal("dry run deleted backups")
	}
	removed, err = adapter.PruneBackups(e.backupDir(), 2, false)
	if err != nil || len(removed) != 2 || removed[0] != ids[0] {
		t.Fatalf("prune: %v %v", removed, err)
	}
	if l, _ := adapter.ListBackups(e.backupDir()); len(l) != 2 {
		t.Fatal("prune did not keep 2")
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("prune deleted a non-backup file")
	}
}

func TestPermissionsPreserved(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.write(".mcp.json", "{\"mcpServers\": {}}\n", 0o600)
	e.write(".claude/settings.json", "{}\n", 0o664)
	e.mustSync(adapter.ScopeProject)
	for f, want := range map[string]os.FileMode{".mcp.json": 0o600, ".claude/settings.json": 0o664, ".forge/sync-state.json": 0o644} {
		fi, err := os.Stat(filepath.Join(e.proj, f))
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v want %o", f, fi, err, want)
		}
	}
	// skill: files 0644/0755, dirs 0755 (read-only registry snapshot modes are not inherited)
	filepath.WalkDir(filepath.Join(e.proj, ".claude/skills/demo-skill"), func(p string, d os.DirEntry, err error) error {
		fi, _ := d.Info()
		if d.IsDir() && fi.Mode().Perm() != 0o755 {
			t.Errorf("dir %s %o", p, fi.Mode().Perm())
		}
		if fi.Mode().IsRegular() && fi.Mode().Perm() != 0o644 && fi.Mode().Perm() != 0o755 {
			t.Errorf("file %s %o", p, fi.Mode().Perm())
		}
		return nil
	})
}

func TestLargeSkillTree(t *testing.T) {
	e := newEnv(t)
	files := map[string]string{}
	for i := 0; i < 800; i++ {
		files[fmt.Sprintf("data/d%02d/f%04d.txt", i%20, i)] = strings.Repeat("x", 200+i)
	}
	e.addSkill("big-skill", "1.0.0", files)
	e.mustSync(adapter.ScopeProject, func(r *adapter.Request) { r.NoHooks = true })
	in, err := adapter.LoadInputs(context.Background(), e.forgeHome, Name)
	if err != nil {
		t.Fatal(err)
	}
	want, _, _ := adapter.TreeHash(in.Skills[0].StoreDir, false)
	got, fl, err := adapter.TreeHash(filepath.Join(e.proj, ".claude/skills/big-skill"), true)
	if err != nil || got != want || len(fl) != 801 {
		t.Fatalf("copied tree differs from the snapshot: %v files=%d", err, len(fl))
	}
	if p := e.mustPlan(adapter.ScopeProject, func(r *adapter.Request) { r.NoHooks = true }); p.HasChanges() {
		t.Fatal("not idempotent for a large tree")
	}
}

func TestReadOnlyTargetsAreConflictsNotErrors(t *testing.T) {
	t.Run("read-only file", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		e.write(".mcp.json", "{\"mcpServers\": {}}\n", 0o444)
		p := e.mustPlan(adapter.ScopeProject)
		wantAction(t, p, adapter.KindMCP, "forge", adapter.ActionConflict)
		before := snapshot(t, e.proj)
		if _, err := e.apply(p); !errors.Is(err, adapter.ErrConflicts) {
			t.Fatal(err)
		}
		if snapshot(t, e.proj) != before {
			t.Fatal("wrote to a read-only target")
		}
	})
	t.Run("read-only directory", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		e.write(".claude/keep.txt", "x", 0o644)
		if err := os.Chmod(filepath.Join(e.proj, ".claude"), 0o555); err != nil {
			t.Fatal(err)
		}
		p := e.mustPlan(adapter.ScopeProject)
		wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
		before := snapshot(t, e.proj)
		if _, err := e.apply(p); !errors.Is(err, adapter.ErrConflicts) {
			t.Fatal(err)
		}
		if snapshot(t, e.proj) != before {
			t.Fatal("wrote into a read-only directory")
		}
	})
}

func TestDisabledSkillIsRemovedWithBackup(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.mustSync(adapter.ScopeProject)
	if err := e.sreg.SetEnabled(context.Background(), "demo-skill", false); err != nil {
		t.Fatal(err)
	}
	p, res := e.mustSync(adapter.ScopeProject)
	wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionRemove)
	if e.exists(".claude/skills/demo-skill") {
		t.Fatal("managed skill must be removed")
	}
	if strings.Contains(e.read(".forge/sync-state.json"), "demo-skill") {
		t.Fatal("state not cleaned")
	}
	if _, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: res.BackupID}); err != nil {
		t.Fatal(err)
	}
	if !e.exists(".claude/skills/demo-skill/SKILL.md") {
		t.Fatal("restore must bring the removed skill back")
	}
}

func TestRemovedManagedSkillEditedByUserIsConflict(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.mustSync(adapter.ScopeProject)
	e.write(".claude/skills/demo-skill/SKILL.md", "user rewrote this\n", 0o644)
	_ = e.sreg.SetEnabled(context.Background(), "demo-skill", false)
	p := e.mustPlan(adapter.ScopeProject)
	wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
}

func TestUserScope(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.addServer(mcp.Spec{Name: "quoted", Transport: "stdio", Command: "run it", Args: []string{"--name", "o'brien"}})
	e.write("sentinel", "x", 0o644)
	sig := e.decoySig()
	p, _ := e.mustSync(adapter.ScopeUser)
	if p.Root != e.home {
		t.Fatalf("root %s", p.Root)
	}
	for _, f := range []string{".claude/skills/demo-skill/SKILL.md", ".claude/settings.json"} {
		if _, err := os.Stat(filepath.Join(e.home, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.forgeHome, "sync-state", "claude-code-user.json")); err != nil {
		t.Fatal("user state file missing")
	}
	if _, err := os.Stat(filepath.Join(e.home, ".mcp.json")); err == nil {
		t.Fatal("user scope must not write .mcp.json")
	}
	if fi, _ := os.Stat(filepath.Join(e.home, ".claude/settings.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("new user settings mode %o", fi.Mode().Perm())
	}
	cmds := strings.Join(p.Commands, "\n")
	for _, want := range []string{
		"claude mcp add --scope user --transport stdio forge -- forge mcp serve --client claude-code",
		"--env 'API_TOKEN=${FS_TOKEN}'",
		"--header 'Authorization: ${REMOTE_AUTH}'",
		`'run it' --name 'o'\''brien'`,
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("commands miss %q:\n%s", want, cmds)
		}
	}
	if strings.Contains(cmds, "gw-proxied") || strings.Contains(cmds, "hidden-one") {
		t.Error("gateway/hidden servers must not get commands")
	}
	e.checkDecoy(sig)
	// project files untouched by the user scope
	if e.exists(".mcp.json") || e.exists(".claude") {
		t.Fatal("user scope wrote into the project")
	}
}

func TestExposureTransitionsRemoveManagedServers(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.mustSync(adapter.ScopeProject)
	if !strings.Contains(e.read(".mcp.json"), "fs-direct") {
		t.Fatal("setup")
	}
	if err := e.mreg.SetExposure(context.Background(), "fs-direct", Name, mcp.ExposureGateway); err != nil {
		t.Fatal(err)
	}
	p, _ := e.mustSync(adapter.ScopeProject)
	wantAction(t, p, adapter.KindMCP, "fs-direct", adapter.ActionRemove)
	if strings.Contains(e.read(".mcp.json"), "fs-direct") {
		t.Fatal("server moved to gateway must be removed from .mcp.json")
	}
}

func TestConcurrentSyncsSerialize(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	changed := make([]int, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rel, err := adapter.AcquireLock(filepath.Join(e.forgeHome, "locks"), adapter.LockKey(Name, adapter.ScopeProject, e.proj), 10*time.Second)
			if err != nil {
				errs[i] = err
				return
			}
			defer rel()
			in, err := adapter.LoadInputs(context.Background(), e.forgeHome, Name)
			if err != nil {
				errs[i] = err
				return
			}
			p, err := e.ad.Plan(context.Background(), in, e.req(adapter.ScopeProject))
			if err != nil {
				errs[i] = err
				return
			}
			res, err := e.apply(p)
			errs[i] = err
			if res != nil {
				changed[i] = len(res.Changed)
			}
		}(i)
	}
	wg.Wait()
	applied := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
		if changed[i] > 0 {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("exactly one sync must write, %d did (changed=%v)", applied, changed)
	}
	if l, _ := adapter.ListBackups(e.backupDir()); len(l) != 1 {
		t.Fatalf("backups: %d", len(l))
	}
	if p := e.mustPlan(adapter.ScopeProject); p.HasChanges() {
		t.Fatal("final state not converged")
	}
	// The lock really excludes: a second holder times out.
	rel, err := adapter.AcquireLock(filepath.Join(e.forgeHome, "locks"), "x", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.AcquireLock(filepath.Join(e.forgeHome, "locks"), "x", 100*time.Millisecond); !errors.Is(err, adapter.ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	rel()
}

func TestJSONReportSchemaAndSanitizing(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	e.addServer(mcp.Spec{Name: "weird", Transport: "stdio", Command: "x", Args: []string{"a\u202eb", "line1\nline2"}})
	p := e.mustPlan(adapter.ScopeProject)
	adapter.BuildDiffs(p)
	var buf bytes.Buffer
	if err := adapter.WriteJSON(&buf, adapter.BuildReport(p, nil, true)); err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema", "target", "scope", "root", "mode", "applied", "conflicts", "summary", "items", "files", "commands", "notes", "diff"} {
		if _, ok := r[k]; !ok {
			t.Errorf("report misses %q", k)
		}
	}
	if r["mode"] != "plan" || r["applied"] != false || r["schema"] != float64(1) {
		t.Fatalf("%v", r)
	}
	if strings.ContainsRune(buf.String(), '\u202e') {
		t.Fatal("bidi control character reached the report")
	}
	var text bytes.Buffer
	adapter.WriteText(&text, p, nil, true)
	if strings.ContainsRune(text.String(), '\u202e') {
		t.Fatal("bidi control character reached the terminal output")
	}
}

func TestBinValidationAndCommands(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	if _, err := e.plan(adapter.ScopeProject, func(r *adapter.Request) { r.ForgeBin = "./bin/forge" }); err == nil {
		t.Fatal("relative --bin must be refused")
	}
	if _, err := e.plan(adapter.ScopeProject, func(r *adapter.Request) { r.ForgeBin = "/opt/f\x1borge" }); err == nil {
		t.Fatal("control characters in --bin must be refused")
	}
	e.mustSync(adapter.ScopeProject, func(r *adapter.Request) { r.ForgeBin = "/opt/my tools/forge" })
	if !strings.Contains(e.read(".mcp.json"), `"command": "/opt/my tools/forge"`) {
		t.Fatalf("gateway command:\n%s", e.read(".mcp.json"))
	}
	if !strings.Contains(e.read(".claude/settings.json"), `'/opt/my tools/forge' hook ingest`) {
		t.Fatalf("hook command must be shell-quoted:\n%s", e.read(".claude/settings.json"))
	}
	// Changing --bin replaces the managed handlers (handler-level) and keeps user data.
	p, _ := e.mustSync(adapter.ScopeProject, func(r *adapter.Request) { r.ForgeBin = "forge" })
	if p.Conflicts() != 0 || strings.Contains(e.read(".claude/settings.json"), "my tools") {
		t.Fatalf("--bin change: %+v", p.Items)
	}
}

func TestProjectRootResolution(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	sub := filepath.Join(e.proj, "a", "b")
	_ = os.MkdirAll(sub, 0o755)
	p := e.mustPlan(adapter.ScopeProject, func(r *adapter.Request) { r.ProjectDir = sub })
	if p.Root != e.proj {
		t.Fatalf("root %s want %s", p.Root, e.proj)
	}
	// no .git: the given directory is used and a note says so
	plain := t.TempDir()
	plain, _ = filepath.EvalSymlinks(plain)
	p = e.mustPlan(adapter.ScopeProject, func(r *adapter.Request) { r.ProjectDir = plain })
	if p.Root != plain || len(p.Notes) == 0 || !strings.Contains(p.Notes[0], ".git") {
		t.Fatalf("root %s notes %v", p.Root, p.Notes)
	}
	// a project root equal to HOME is refused
	if _, err := e.plan(adapter.ScopeProject, func(r *adapter.Request) { r.ProjectDir = e.home }); err == nil {
		t.Fatal("home as project root must be refused")
	}
}

func TestKeyOrderAndIndentPreserved(t *testing.T) {
	styles := map[string]string{
		"tabs": "{\n\t\"zeta\": true,\n\t\"mcpServers\": {\n\t\t\"user-srv\": {\n\t\t\t\"type\": \"stdio\",\n\t\t\t\"command\": \"u\"\n\t\t}\n\t},\n\t\"alpha\": [3, 2, 1]\n}\n",
		"crlf": "{\r\n  \"zeta\": true,\r\n  \"mcpServers\": {\r\n    \"user-srv\": {\"type\": \"stdio\", \"command\": \"u\"}\r\n  },\r\n  \"alpha\": [3, 2, 1]\r\n}\r\n",
		"four": "{\n    \"zeta\": true,\n    \"mcpServers\": {\n        \"user-srv\": {\"type\": \"stdio\", \"command\": \"u\"}\n    },\n    \"alpha\": [3, 2, 1]\n}",
		"min":  `{"zeta":true,"mcpServers":{"user-srv":{"type":"stdio","command":"u"}},"alpha":[3,2,1]}`,
	}
	for name, orig := range styles {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			standardRegistry(t, e)
			e.write(".mcp.json", orig, 0o644)
			e.mustSync(adapter.ScopeProject, func(r *adapter.Request) { r.NoHooks = true })
			got := e.read(".mcp.json")
			if !json.Valid([]byte(got)) {
				t.Fatalf("invalid JSON:\n%s", got)
			}
			// top-level key order and the user's member text are preserved verbatim
			zi, mi, ai := strings.Index(got, `"zeta"`), strings.Index(got, `"mcpServers"`), strings.Index(got, `"alpha"`)
			if !(zi >= 0 && zi < mi && mi < ai) {
				t.Fatalf("key order changed:\n%s", got)
			}
			userBlock := orig[strings.Index(orig, `"user-srv"`):strings.Index(orig, `"alpha"`)]
			userBlock = strings.TrimRight(userBlock, ", \t\r\n}")
			if !strings.Contains(got, strings.TrimRight(userBlock, " \t\r\n}")[:len(`"user-srv": {`)]) {
				t.Fatalf("user entry lost:\n%s", got)
			}
			if !strings.Contains(got, "[3, 2, 1]") && !strings.Contains(got, "[3,2,1]") {
				t.Fatalf("user array reformatted:\n%s", got)
			}
			switch name {
			case "tabs":
				if !strings.Contains(got, "\n\t\t\"forge\": {\n\t\t\t\"type\"") {
					t.Fatalf("tab indentation not used:\n%s", got)
				}
			case "crlf":
				if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
					t.Fatalf("mixed line endings:\n%q", got)
				}
			case "four":
				if !strings.Contains(got, "\n        \"forge\": {\n            \"type\"") || strings.HasSuffix(got, "\n") {
					t.Fatalf("4-space indentation / missing final newline not preserved:\n%s", got)
				}
			}
			// a second sync is a no-op and a restore gives back the exact original bytes
			if p := e.mustPlan(adapter.ScopeProject, func(r *adapter.Request) { r.NoHooks = true }); p.HasChanges() {
				t.Fatal("not idempotent")
			}
			ids, _ := adapter.ListBackups(e.backupDir())
			if _, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: ids[len(ids)-1].ID}); err != nil {
				t.Fatal(err)
			}
			if e.read(".mcp.json") != orig {
				t.Fatal("restore is not byte-exact")
			}
		})
	}
}

func TestTamperedRegistrySnapshotIsRefused(t *testing.T) {
	t.Run("symlink inside the snapshot", func(t *testing.T) {
		e := newEnv(t)
		e.addSkill("demo-skill", "1.0.0", nil)
		in, _ := adapter.LoadInputs(context.Background(), e.forgeHome, Name)
		dir := in.Skills[0].StoreDir
		_ = os.Chmod(dir, 0o755)
		if err := os.Symlink("/etc/hosts", filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
		p := e.mustPlan(adapter.ScopeProject)
		wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
	})
	t.Run("content differs from the registered hash", func(t *testing.T) {
		e := newEnv(t)
		e.addSkill("demo-skill", "1.0.0", nil)
		in, _ := adapter.LoadInputs(context.Background(), e.forgeHome, Name)
		dir := in.Skills[0].StoreDir
		_ = os.Chmod(dir, 0o755)
		f := filepath.Join(dir, "SKILL.md")
		_ = os.Chmod(f, 0o644)
		_ = os.WriteFile(f, []byte("tampered\n"), 0o644)
		p := e.mustPlan(adapter.ScopeProject)
		wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
		if e.exists(".claude/skills/demo-skill") {
			t.Fatal("tampered snapshot written")
		}
	})
}

func TestStateFileCorruptionAborts(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	for _, content := range []string{"not json", `{"version":99,"target":"claude-code","scope":"project","items":[]}`,
		`{"version":1,"target":"opencode","scope":"project","items":[]}`} {
		e.write(".forge/sync-state.json", content, 0o644)
		before := snapshot(t, e.proj)
		if _, err := e.plan(adapter.ScopeProject); err == nil {
			t.Fatalf("state %q must abort the plan", content)
		}
		if snapshot(t, e.proj) != before {
			t.Fatal("files changed")
		}
	}
}

func TestStateTamperedNamesAreIgnored(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	victim := filepath.Join(filepath.Dir(e.proj), "victim")
	_ = os.MkdirAll(victim, 0o755)
	_ = os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("x"), 0o644)
	e.write(".forge/sync-state.json", `{"version":1,"target":"claude-code","scope":"project","items":[`+
		`{"kind":"skill","name":"../../victim","hash":"x","applied_at":"t"},`+
		`{"kind":"mcp","name":"../x","hash":"x","applied_at":"t"},`+
		`{"kind":"hook","name":"garbage","hash":"x","applied_at":"t"}]}`+"\n", 0o644)
	e.mustSync(adapter.ScopeProject)
	if _, err := os.Stat(filepath.Join(victim, "keep.txt")); err != nil {
		t.Fatal("a tampered state entry removed files outside the project")
	}
	if strings.Contains(e.read(".forge/sync-state.json"), "victim") {
		t.Fatal("invalid state entries must be dropped")
	}
}
