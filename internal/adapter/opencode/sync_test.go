package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/as950118/forge/internal/adapter"
	"github.com/as950118/forge/internal/mcp"
)

func (e *env) openJSON() map[string]any {
	e.t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(e.read("opencode.json")), &m); err != nil {
		e.t.Fatalf("opencode.json invalid: %v", err)
	}
	return m
}

func (e *env) mcpEntry(name string) map[string]any {
	e.t.Helper()
	m, _ := e.openJSON()["mcp"].(map[string]any)
	v, _ := m[name].(map[string]any)
	return v
}

func TestPlanWritesNothing(t *testing.T) {
	for _, scope := range []adapter.Scope{adapter.ScopeProject, adapter.ScopeUser} {
		e := newEnv(t)
		installFixture(t, e)
		standardRegistry(t, e)
		sig := e.decoySig()
		before := snapshot(t, e.proj, e.home, filepath.Join(e.forgeHome, "sync-state"), filepath.Join(e.forgeHome, "backups"), filepath.Join(e.forgeHome, "locks"))
		p := e.mustPlan(scope)
		adapter.BuildDiffs(p)
		if !p.HasChanges() {
			t.Fatalf("%s: expected changes", scope)
		}
		if after := snapshot(t, e.proj, e.home, filepath.Join(e.forgeHome, "sync-state"), filepath.Join(e.forgeHome, "backups"), filepath.Join(e.forgeHome, "locks")); after != before {
			t.Fatalf("%s: plan wrote files", scope)
		}
		e.checkDecoy(sig)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	_, res := e.mustSync(adapter.ScopeProject)
	if res.BackupID == "" {
		t.Fatal("first apply must make a backup")
	}
	before := snapshot(t, e.proj)
	ids1, _ := adapter.ListBackups(e.backupDir())
	p := e.mustPlan(adapter.ScopeProject)
	if p.HasChanges() {
		t.Fatalf("second plan must be empty:\n%s", renderPlan(p))
	}
	r2, err := e.apply(p)
	if err != nil || r2.BackupID != "" {
		t.Fatalf("second apply: %+v %v", r2, err)
	}
	ids2, _ := adapter.ListBackups(e.backupDir())
	if len(ids1) != len(ids2) || snapshot(t, e.proj) != before {
		t.Fatal("second apply changed something")
	}
}

func TestConversionIsExact(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	weird := []string{"--msg", "a b", "it's", "$(touch x)", `"q"`, "x;y|z", "back\\slash", "tab-here", "`id`", "uni\u00e9"}
	e.addServer(mcp.Spec{Name: "weird", Transport: "stdio", Command: "/opt/my tool/bin", Args: weird,
		Values: []mcp.Value{envRef("env", "REGION_TOKEN", "REG_TOK")}})
	e.addServer(mcp.Spec{Name: "sse-one", Transport: "sse", URL: "https://example.com/sse"})
	e.mustSync(adapter.ScopeProject, func(r *adapter.Request) { r.ForgeBin = "/opt/forge bin/forge" })
	want := map[string]any{
		"forge":         map[string]any{"type": "local", "command": []any{"/opt/forge bin/forge", "mcp", "serve", "--client", "opencode"}, "enabled": true},
		"fs-direct":     map[string]any{"type": "local", "command": []any{"npx", "-y", "fs-server"}, "environment": map[string]any{"API_TOKEN": "{env:FS_TOKEN}"}, "enabled": true},
		"remote-direct": map[string]any{"type": "remote", "url": "https://example.com/mcp", "headers": map[string]any{"Authorization": "{env:REMOTE_AUTH}"}, "enabled": true},
		"sse-one":       map[string]any{"type": "remote", "url": "https://example.com/sse", "enabled": true},
	}
	for name, w := range want {
		if got := e.mcpEntry(name); !reflect.DeepEqual(got, w) {
			t.Errorf("%s: got %v want %v", name, got, w)
		}
	}
	w := e.mcpEntry("weird")
	cmd, _ := w["command"].([]any)
	wantCmd := []any{"/opt/my tool/bin"}
	for _, a := range weird {
		wantCmd = append(wantCmd, a)
	}
	if !reflect.DeepEqual(cmd, wantCmd) {
		t.Errorf("weird command not preserved as JSON strings: %v", cmd)
	}
	if w["environment"].(map[string]any)["REGION_TOKEN"] != "{env:REG_TOK}" {
		t.Errorf("weird env: %v", w)
	}
	for _, absent := range []string{"gw-proxied", "hidden-one"} {
		if e.mcpEntry(absent) != nil {
			t.Errorf("%s must not be rendered", absent)
		}
	}
	// No ${...} and no keys outside the documented format.
	raw := e.read("opencode.json")
	if strings.Contains(raw, "${") || strings.Contains(raw, `"cwd"`) || strings.Contains(raw, `"timeout"`) || strings.Contains(raw, `"oauth"`) {
		t.Fatalf("unexpected content:\n%s", raw)
	}
	// Sync state holds no secret names/values and uses its own file.
	if !e.exists(".forge/sync-state.opencode.json") || e.exists(".forge/sync-state.json") {
		t.Fatal("state file names")
	}
}

func TestSseNoteAndUnrenderedFieldsAreReported(t *testing.T) {
	e := newEnv(t)
	e.addServer(mcp.Spec{Name: "sse-one", Transport: "sse", URL: "https://example.com/sse", TimeoutMS: 9000})
	e.addServer(mcp.Spec{Name: "with-cwd", Transport: "stdio", Command: "x", Cwd: "/tmp"})
	p := e.mustPlan(adapter.ScopeProject)
	text := renderPlan(p)
	if !strings.Contains(text, "sse 서버는 type=remote") || !strings.Contains(text, "timeout_ms") {
		t.Fatalf("notes missing:\n%s", text)
	}
	wantAction(t, p, adapter.KindMCP, "with-cwd", adapter.ActionSkip)
}

func TestUserItemsPreservedAcrossStyles(t *testing.T) {
	styles := map[string]string{
		"tabs":  "{\n\t\"zeta\": true,\n\t\"provider\": {\"x\": {\"options\": {\"apiKey\": \"FIXTURE-k\"}}},\n\t\"mcp\": {\n\t\t\"user-srv\": {\n\t\t\t\"type\": \"local\",\n\t\t\t\"command\": [\"u\"]\n\t\t}\n\t},\n\t\"alpha\": [3, 2, 1]\n}\n",
		"crlf":  "{\r\n  \"zeta\": true,\r\n  \"mcp\": {\r\n    \"user-srv\": {\"type\": \"local\", \"command\": [\"u\"]}\r\n  },\r\n  \"alpha\": [3, 2, 1]\r\n}\r\n",
		"four":  "{\n    \"zeta\": true,\n    \"mcp\": {\n        \"user-srv\": {\"type\": \"local\", \"command\": [\"u\"]}\n    },\n    \"alpha\": [3, 2, 1]\n}",
		"two":   "{\n  \"zeta\": true,\n  \"mcp\": {\n    \"user-srv\": {\"type\": \"local\", \"command\": [\"u\"]}\n  },\n  \"alpha\": [3, 2, 1]\n}\n",
		"min":   `{"zeta":true,"mcp":{"user-srv":{"type":"local","command":["u"]}},"alpha":[3,2,1]}`,
		"nomcp": "{\n  \"zeta\": true,\n  \"alpha\": [3, 2, 1]\n}\n",
	}
	for name, orig := range styles {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			standardRegistry(t, e)
			e.write("opencode.json", orig, 0o644)
			e.mustSync(adapter.ScopeProject)
			got := e.read("opencode.json")
			if !json.Valid([]byte(got)) {
				t.Fatalf("invalid JSON:\n%s", got)
			}
			zi, ai := strings.Index(got, `"zeta"`), strings.Index(got, `"alpha"`)
			if !(zi >= 0 && zi < ai) {
				t.Fatalf("key order changed:\n%s", got)
			}
			if !strings.Contains(got, "[3, 2, 1]") && !strings.Contains(got, "[3,2,1]") {
				t.Fatalf("user array reformatted:\n%s", got)
			}
			if name != "nomcp" && e.mcpEntry("user-srv") == nil {
				t.Fatalf("user server lost:\n%s", got)
			}
			if name == "tabs" && !strings.Contains(got, "\"provider\": {\"x\": {\"options\": {\"apiKey\": \"FIXTURE-k\"}}},") {
				t.Fatalf("user provider block reformatted:\n%s", got)
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
					t.Fatalf("4-space indent / missing final newline not preserved:\n%s", got)
				}
			case "two":
				if !strings.Contains(got, "\n    \"forge\": {\n      \"type\"") {
					t.Fatalf("2-space indent not used:\n%s", got)
				}
			}
			if p := e.mustPlan(adapter.ScopeProject); p.HasChanges() {
				t.Fatal("not idempotent")
			}
			ids, _ := adapter.ListBackups(e.backupDir())
			if _, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: ids[len(ids)-1].ID}); err != nil {
				t.Fatal(err)
			}
			if e.read("opencode.json") != orig {
				t.Fatal("restore is not byte-exact")
			}
		})
	}
}

func TestConflicts(t *testing.T) {
	t.Run("user skill with the same name", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		e.write(".opencode/skills/demo-skill/SKILL.md", "mine\n", 0o644)
		p := e.mustPlan(adapter.ScopeProject)
		wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
		before := snapshot(t, e.proj, e.home)
		if _, err := e.apply(p); !errors.Is(err, adapter.ErrConflicts) {
			t.Fatalf("want ErrConflicts, got %v", err)
		}
		if snapshot(t, e.proj, e.home) != before {
			t.Fatal("apply with conflicts must write nothing")
		}
		if _, err := os.Stat(e.backupDir()); err == nil {
			t.Fatal("no backup when refused")
		}
		p2 := e.mustPlan(adapter.ScopeProject)
		p2.SkipConflicts = true
		if _, err := e.apply(p2); err != nil {
			t.Fatal(err)
		}
		if e.read(".opencode/skills/demo-skill/SKILL.md") != "mine\n" || e.exists(".opencode/skills/demo-skill/.forge-managed") {
			t.Fatal("user skill must be untouched")
		}
		if e.mcpEntry("forge") == nil || strings.Contains(e.read(".forge/sync-state.opencode.json"), "demo-skill") {
			t.Fatal("rest must be applied and the conflicting skill must not be managed")
		}
	})
	t.Run("user server with the same name and the gateway name", func(t *testing.T) {
		e := newEnv(t)
		e.write("opencode.json", `{"mcp":{"fs-direct":{"type":"local","command":["mine"]},"forge":{"type":"local","command":["my-forge"]}}}`+"\n", 0o644)
		standardRegistry(t, e)
		p := e.mustPlan(adapter.ScopeProject)
		wantAction(t, p, adapter.KindMCP, "fs-direct", adapter.ActionConflict)
		wantAction(t, p, adapter.KindMCP, "forge", adapter.ActionConflict)
		wantAction(t, p, adapter.KindMCP, "remote-direct", adapter.ActionAdd)
		p.SkipConflicts = true
		if _, err := e.apply(p); err != nil {
			t.Fatal(err)
		}
		s := e.read("opencode.json")
		if !strings.Contains(s, `"command":["mine"]`) || !strings.Contains(s, `"command":["my-forge"]`) || !strings.Contains(s, "remote-direct") {
			t.Fatalf("user servers must be byte-preserved:\n%s", s)
		}
		if strings.Contains(e.read(".forge/sync-state.opencode.json"), `"fs-direct"`) {
			t.Fatal("user server recorded as managed")
		}
	})
	t.Run("registry server named forge", func(t *testing.T) {
		e := newEnv(t)
		e.addServer(mcp.Spec{Name: "forge", Transport: "stdio", Command: "x"})
		if it := itemFor(e.mustPlan(adapter.ScopeProject), adapter.KindMCP, "forge"); it == nil || it.Action != adapter.ActionConflict {
			t.Fatalf("must conflict: %+v", it)
		}
	})
	t.Run("managed items edited by hand need --force-managed", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		e.mustSync(adapter.ScopeProject)
		raw := e.read("opencode.json")
		e.write("opencode.json", strings.Replace(raw, `"fs-server"`, `"evil-server"`, 1), 0o644)
		e.write(".opencode/skills/demo-skill/extra.txt", "hand edit\n", 0o644)
		p := e.mustPlan(adapter.ScopeProject)
		for _, w := range [][2]string{{adapter.KindMCP, "fs-direct"}, {adapter.KindSkill, "demo-skill"}} {
			it := itemFor(p, w[0], w[1])
			if it == nil || it.Action != adapter.ActionConflict || !it.Overridable {
				t.Fatalf("%s: want overridable conflict, got %+v", w[1], it)
			}
		}
		p2 := e.mustPlan(adapter.ScopeProject, func(r *adapter.Request) { r.ForceManaged = true })
		if p2.Conflicts() != 0 {
			t.Fatal("force-managed must clear managed conflicts")
		}
		if _, err := e.apply(p2); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(e.read("opencode.json"), "evil-server") || e.exists(".opencode/skills/demo-skill/extra.txt") {
			t.Fatal("managed items must be overwritten")
		}
	})
	t.Run("force-managed never overwrites user items", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		e.write("opencode.json", `{"mcp":{"fs-direct":{"type":"local","command":["mine"]}}}`+"\n", 0o644)
		e.write(".opencode/skills/demo-skill/SKILL.md", "mine\n", 0o644)
		p := e.mustPlan(adapter.ScopeProject, func(r *adapter.Request) { r.ForceManaged = true })
		for _, w := range [][2]string{{adapter.KindMCP, "fs-direct"}, {adapter.KindSkill, "demo-skill"}} {
			it := itemFor(p, w[0], w[1])
			if it == nil || it.Action != adapter.ActionConflict || it.Overridable {
				t.Fatalf("%s: want non-overridable conflict, got %+v", w[1], it)
			}
		}
	})
	t.Run("marker without state and state without marker", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		e.mustSync(adapter.ScopeProject)
		_ = os.Remove(filepath.Join(e.proj, ".forge", "sync-state.opencode.json"))
		p := e.mustPlan(adapter.ScopeProject)
		wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
		wantAction(t, p, adapter.KindMCP, "fs-direct", adapter.ActionConflict)
	})
}

func TestUnparsableConfigAbortsWithoutWriting(t *testing.T) {
	cases := map[string]string{
		"comment":        "{\n  // note\n  \"mcp\": {}\n}\n",
		"trailing comma": `{"mcp": {},}` + "\n",
		"bom":            "\ufeff{\"mcp\":{}}\n",
		"empty":          "",
		"array":          "[]\n",
		"mcp not object": `{"mcp": []}` + "\n",
		"duplicate mcp":  `{"mcp": {}, "mcp": {}}` + "\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			standardRegistry(t, e)
			e.write("opencode.json", content, 0o644)
			before := snapshot(t, e.proj, e.home)
			if _, err := e.plan(adapter.ScopeProject); err == nil {
				t.Fatal("plan must fail")
			}
			if snapshot(t, e.proj, e.home) != before {
				t.Fatal("files changed")
			}
		})
	}
}

func TestJSONCHandling(t *testing.T) {
	t.Run("only opencode.jsonc aborts", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		e.write("opencode.jsonc", "{\n  // comment\n}\n", 0o644)
		before := snapshot(t, e.proj, e.home)
		_, err := e.plan(adapter.ScopeProject)
		if err == nil || !strings.Contains(err.Error(), "JSONC 는 아직 지원하지 않습니다") {
			t.Fatalf("want a JSONC refusal, got %v", err)
		}
		if snapshot(t, e.proj, e.home) != before {
			t.Fatal("files changed")
		}
	})
	t.Run("both: json is edited, jsonc is untouched, warning shown", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		const jc = "{\n  // keep me\n  \"mcp\": {}\n}\n"
		e.write("opencode.jsonc", jc, 0o644)
		e.write("opencode.json", "{}\n", 0o644)
		p := e.mustPlan(adapter.ScopeProject)
		if !strings.Contains(renderPlan(p), "opencode.jsonc 도 존재합니다") {
			t.Fatal("no warning about opencode.jsonc")
		}
		if _, err := e.apply(p); err != nil {
			t.Fatal(err)
		}
		if e.read("opencode.jsonc") != jc || e.mcpEntry("forge") == nil {
			t.Fatal("jsonc must stay byte-identical and json must be edited")
		}
	})
	t.Run(".opencode/opencode.json is not a target", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		e.write(".opencode/opencode.json", `{"mcp":{}}`+"\n", 0o644)
		e.mustSync(adapter.ScopeProject)
		if e.read(".opencode/opencode.json") != `{"mcp":{}}`+"\n" || !e.exists("opencode.json") {
			t.Fatal("wrong file edited")
		}
	})
}

func TestSecretsAreMaskedAndRefused(t *testing.T) {
	e := newEnv(t)
	e.write("opencode.json", `{
  "mcp": {
    "user": {"type": "remote", "url": "https://u.example.com", "headers": {"Authorization": "Bearer FIXTURE-literal-key-444"}}
  },
  "provider": {"openai": {"options": {"apiKey": "FIXTURE-literal-key-111", "api_key": "FIXTURE-literal-key-222"}, "headers": {"Authorization": "Bearer FIXTURE-literal-key-333"}}}
}
`, 0o644)
	standardRegistry(t, e)
	p := e.mustPlan(adapter.ScopeProject)
	adapter.BuildDiffs(p)
	diff := allDiffs(p)
	if !strings.Contains(diff, `"apiKey": "***"`) || !strings.Contains(diff, `"provider"`) {
		t.Fatalf("provider apiKey context line not masked:\n%s", diff)
	}
	for _, s := range []string{"FIXTURE-literal-key-111", "FIXTURE-literal-key-222", "FIXTURE-literal-key-333", "FIXTURE-literal-key-444"} {
		if strings.Contains(diff, s) || strings.Contains(renderPlan(p), s) {
			t.Fatalf("secret %s leaked", s)
		}
	}
	// {env:NAME} references stay readable.
	if !strings.Contains(diff, "{env:FS_TOKEN}") {
		t.Fatalf("env references should stay visible:\n%s", diff)
	}
	// Literal secrets are refused at render time (registry bypassed on purpose).
	for name, s := range map[string]*mcp.Info{
		"secret literal": {Name: "bad1", Transport: "stdio", Command: "x", Values: []mcp.Value{{Slot: "env", Key: "API_TOKEN", RefType: mcp.RefLiteral, Literal: "plain"}}},
		"keychain":       {Name: "bad2", Transport: "stdio", Command: "x", Values: []mcp.Value{{Slot: "env", Key: "OK", RefType: mcp.RefKeychain, Ref: "s/a"}}},
		"subst literal":  {Name: "bad3", Transport: "stdio", Command: "x", Values: []mcp.Value{{Slot: "env", Key: "MODE", RefType: mcp.RefLiteral, Literal: "{env:HOME}"}}},
		"file subst arg": {Name: "bad4", Transport: "stdio", Command: "x", Args: []string{"--cfg={file:/etc/passwd}"}},
		"dollar arg":     {Name: "bad5", Transport: "stdio", Command: "x", Args: []string{"${HOME}"}},
		"control char":   {Name: "bad7", Transport: "stdio", Command: "x", Args: []string{"a\tb"}},
		"cwd":            {Name: "bad6", Transport: "stdio", Command: "x", Cwd: "/tmp"},
	} {
		if _, bad, _ := renderServer(s); bad == "" {
			t.Errorf("%s: render must be refused", name)
		}
	}
}

func allDiffs(p *adapter.Plan) string {
	var sb strings.Builder
	for _, d := range p.Diffs {
		sb.WriteString(d.Diff)
	}
	return sb.String()
}

func TestSymlinksAreRefused(t *testing.T) {
	t.Run("linked .opencode directory", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		other := filepath.Join(filepath.Dir(e.proj), "elsewhere")
		_ = os.MkdirAll(other, 0o755)
		if err := os.Symlink(other, filepath.Join(e.proj, ".opencode")); err != nil {
			t.Fatal(err)
		}
		p := e.mustPlan(adapter.ScopeProject)
		wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionConflict)
		p.SkipConflicts = true
		if _, err := e.apply(p); err != nil {
			t.Fatal(err)
		}
		if ents, _ := os.ReadDir(other); len(ents) != 0 {
			t.Fatalf("wrote through the symlink: %v", ents)
		}
	})
	t.Run("linked opencode.json", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		target := filepath.Join(filepath.Dir(e.proj), "shared.json")
		if err := os.WriteFile(target, []byte(`{"mcp":{}}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(e.proj, "opencode.json")); err != nil {
			t.Fatal(err)
		}
		p := e.mustPlan(adapter.ScopeProject)
		wantAction(t, p, adapter.KindMCP, "forge", adapter.ActionConflict)
		p.SkipConflicts = true
		if _, err := e.apply(p); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(target); string(b) != `{"mcp":{}}`+"\n" {
			t.Fatal("linked target modified")
		}
	})
	t.Run("linked .forge directory", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		other := filepath.Join(filepath.Dir(e.proj), "forge-elsewhere")
		_ = os.MkdirAll(other, 0o755)
		if err := os.Symlink(other, filepath.Join(e.proj, ".forge")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.plan(adapter.ScopeProject); err == nil {
			t.Fatal("a linked .forge must stop the plan")
		}
	})
	t.Run("late link is caught at apply", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		p := e.mustPlan(adapter.ScopeProject)
		other := filepath.Join(filepath.Dir(e.proj), "late")
		_ = os.MkdirAll(other, 0o755)
		if err := os.Symlink(other, filepath.Join(e.proj, ".opencode")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.apply(p); err == nil {
			t.Fatal("apply must refuse")
		}
		if ents, _ := os.ReadDir(other); len(ents) != 0 {
			t.Fatal("wrote through a late symlink")
		}
	})
}

func TestRollbackOnMidwayFailure(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	before := snapshot(t, e.proj)
	p := e.mustPlan(adapter.ScopeProject)
	for step := 0; step < len(p.Ops); step++ {
		_, err := adapter.Apply(context.Background(), e.ad, p, adapter.ApplyOptions{BackupDir: e.backupDir(),
			Inject: func(i int, _ adapter.Op) error {
				if i == step {
					return errors.New("injected failure")
				}
				return nil
			}})
		if err == nil {
			t.Fatalf("step %d: want failure", step)
		}
		if snapshot(t, e.proj) != before {
			t.Fatalf("step %d: not rolled back", step)
		}
	}
	if det := adapter.DetectInterrupted(e.ad, e.backupDir(), adapter.ScopeProject, p.Root); len(det.Journals) != 0 {
		t.Fatal("rolled back sync must not look interrupted")
	}
}

func TestBackupRestoreAndTargetMismatch(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	before := snapshot(t, e.proj)
	_, res := e.mustSync(adapter.ScopeProject)
	m, _, err := adapter.LoadManifest(e.backupDir(), res.BackupID)
	if err != nil || m.Target != Name {
		t.Fatalf("manifest target: %+v %v", m, err)
	}
	rr, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: res.BackupID})
	if err != nil || rr == nil {
		t.Fatalf("restore: %v", err)
	}
	if snapshot(t, e.proj) != before {
		t.Fatal("restore did not give back the original tree")
	}
	// A backup of another target is refused by this adapter.
	data, _ := os.ReadFile(filepath.Join(e.backupDir(), res.BackupID, "manifest.json"))
	forged := strings.Replace(string(data), `"target": "opencode"`, `"target": "claude-code"`, 1)
	if forged == string(data) {
		t.Fatal("manifest has no target field")
	}
	if err := os.WriteFile(filepath.Join(e.backupDir(), res.BackupID, "manifest.json"), []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: res.BackupID, Force: true}); err == nil {
		t.Fatal("restore of another target's backup must be refused")
	}
}

func TestRestoreRejectsTamperedManifests(t *testing.T) {
	for name, edit := range map[string]func(path, orig string) string{
		"global config": func(_, orig string) string {
			return strings.Replace(orig, `"path": "`, `"path": "`+filepath.ToSlash(filepath.Join(os.Getenv("HOME"), ".config/opencode/opencode.json"))+`", "x": "`, 1)
		},
		"service.json": func(_, orig string) string {
			return strings.Replace(orig, `"path": "`, `"path": "`+filepath.ToSlash(filepath.Join(os.Getenv("HOME"), ".config/opencode/service.json"))+`", "x": "`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			standardRegistry(t, e)
			sig := e.decoySig()
			_, res := e.mustSync(adapter.ScopeProject)
			mp := filepath.Join(e.backupDir(), res.BackupID, "manifest.json")
			data, _ := os.ReadFile(mp)
			if err := os.WriteFile(mp, []byte(edit(mp, string(data))), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: res.BackupID, Force: true}); err == nil {
				t.Fatal("tampered manifest must be refused")
			}
			e.checkDecoy(sig)
		})
	}
}

func TestUserScopeSkillsAndCommands(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.addServer(mcp.Spec{Name: "weird", Transport: "stdio", Command: "/opt/my tool/bin", Args: []string{"a b", "it's", "$(x)", "q;r"},
		Values: []mcp.Value{envRef("env", "K_TOKEN", "KT")}})
	sig := e.decoySig()
	p, _ := e.mustSync(adapter.ScopeUser)
	if !e.existsHome(".config/opencode/skills/demo-skill/SKILL.md") || !e.existsHome(".config/opencode/skills/demo-skill/.forge-managed") {
		t.Fatal("user skill not written")
	}
	if !e.existsAbs(filepath.Join(e.forgeHome, "sync-state", "opencode-user.json")) {
		t.Fatal("user state file name")
	}
	if e.exists("opencode.json") || e.exists(".opencode") {
		t.Fatal("user scope wrote into the project")
	}
	e.checkDecoy(sig)         // the global opencode.json and friends were not read or modified
	if len(p.Commands) != 5 { // forge, fs-direct, remote-direct, weird (gw/hidden skipped) + ...
		// forge + fs-direct + remote-direct + weird
		if len(p.Commands) != 4 {
			t.Fatalf("commands: %v", p.Commands)
		}
	}
	for _, c := range p.Commands {
		if strings.Contains(c, "FS_TOKEN=") || strings.Contains(c, "${") {
			t.Fatalf("command leaks or uses wrong syntax: %s", c)
		}
		if out, err := exec.Command("sh", "-n", "-c", c).CombinedOutput(); err != nil {
			t.Fatalf("sh -n: %v\n%s\n%s", err, out, c)
		}
		// argv round trip through the shell, with `opencode` replaced by printf.
		probe := "printf '%s\\n' " + strings.TrimPrefix(c, "opencode ")
		out, err := exec.Command("sh", "-c", probe).Output()
		if err != nil {
			t.Fatalf("probe: %v\n%s", err, c)
		}
		argv := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		if argv[0] != "mcp" || argv[1] != "add" || (argv[3] != "--global") {
			t.Fatalf("unexpected argv: %q", argv)
		}
		if strings.HasPrefix(c, "opencode mcp add weird ") {
			want := []string{"mcp", "add", "weird", "--global", "--env", "K_TOKEN={env:KT}", "--", "/opt/my tool/bin", "a b", "it's", "$(x)", "q;r"}
			if !reflect.DeepEqual(argv, want) {
				t.Fatalf("weird argv: %q", argv)
			}
		}
	}
	text := renderPlan(p)
	if !strings.Contains(text, "[미확인]") || !strings.Contains(text, "mcp remove") {
		t.Fatalf("user-scope notes missing:\n%s", text)
	}
}

func (e *env) existsHome(rel string) bool {
	return e.existsAbs(filepath.Join(e.home, filepath.FromSlash(rel)))
}
func (e *env) existsAbs(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestForbiddenProjectRoots(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	try := func(dir string) error {
		_, err := e.plan(adapter.ScopeProject, func(r *adapter.Request) { r.ProjectDir = dir })
		return err
	}
	for _, rel := range []string{"", ".config/opencode", ".local/share/opencode", ".local/state/opencode", ".claude"} {
		d := filepath.Join(e.home, filepath.FromSlash(rel))
		_ = os.MkdirAll(d, 0o755)
		if err := try(d); err == nil {
			t.Errorf("root %q accepted", rel)
		}
	}
	if err := try(e.forgeHome); err == nil {
		t.Error("forge home accepted as project root")
	}
	sub := filepath.Join(e.home, "work")
	_ = os.MkdirAll(sub, 0o755)
	if err := try(sub); err != nil {
		t.Errorf("a project below home must work: %v", err)
	}
}

func TestSyncRefusesAliasesOfSecretFilesInTheProject(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	sig := e.decoySig()
	global := filepath.Join(e.home, ".config/opencode/opencode.json")
	if err := os.Link(global, filepath.Join(e.proj, "opencode.json")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := e.plan(adapter.ScopeProject); err == nil {
		t.Fatal("a project opencode.json that is a hard link to the global config must be refused")
	}
	e.checkDecoy(sig)
}

func TestExposureTransitionsRemoveManagedServers(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.mustSync(adapter.ScopeProject)
	if e.mcpEntry("fs-direct") == nil {
		t.Fatal("setup")
	}
	if err := e.mreg.SetExposure(t.Context(), "fs-direct", "opencode", mcp.ExposureGateway); err != nil {
		t.Fatal(err)
	}
	p, _ := e.mustSync(adapter.ScopeProject)
	wantAction(t, p, adapter.KindMCP, "fs-direct", adapter.ActionRemove)
	if e.mcpEntry("fs-direct") != nil {
		t.Fatal("not removed")
	}
	// A different client's exposure does not matter.
	if err := e.mreg.SetExposure(t.Context(), "remote-direct", "claude-code", mcp.ExposureHidden); err != nil {
		t.Fatal(err)
	}
	if e.mustPlan(adapter.ScopeProject).HasChanges() {
		t.Fatal("claude-code exposure must not change the opencode plan")
	}
}

func TestHooksAreRejected(t *testing.T) {
	e := newEnv(t)
	for _, mod := range []func(*adapter.Request){func(r *adapter.Request) { r.NoHooks = true }, func(r *adapter.Request) { r.RemoveHooks = true }} {
		if _, err := e.plan(adapter.ScopeProject, mod); err == nil || !strings.Contains(err.Error(), "지원하지 않습니다") {
			t.Fatalf("want an unsupported error, got %v", err)
		}
	}
}

func TestBinValidation(t *testing.T) {
	e := newEnv(t)
	for _, bin := range []string{"./forge", "a\nb", "x{env:HOME}", "${HOME}/forge"} {
		if _, err := e.plan(adapter.ScopeProject, func(r *adapter.Request) { r.ForgeBin = bin }); err == nil {
			t.Errorf("bin %q accepted", bin)
		}
	}
}

func TestPermissionsPreserved(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.write("opencode.json", "{}\n", 0o600)
	e.mustSync(adapter.ScopeProject)
	fi, _ := os.Stat(filepath.Join(e.proj, "opencode.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func TestReadOnlyConfigIsAConflict(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.write("opencode.json", "{}\n", 0o444)
	p := e.mustPlan(adapter.ScopeProject)
	wantAction(t, p, adapter.KindMCP, "forge", adapter.ActionConflict)
}

func TestDisabledSkillIsRemovedWithBackup(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.mustSync(adapter.ScopeProject)
	if err := e.sreg.SetEnabled(t.Context(), "demo-skill", false); err != nil {
		t.Fatal(err)
	}
	p, res := e.mustSync(adapter.ScopeProject)
	wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionRemove)
	if e.exists(".opencode/skills/demo-skill") || res.BackupID == "" {
		t.Fatal("not removed with backup")
	}
}

func TestSourcesDoNotSpellSecretFileNames(t *testing.T) {
	ents, _ := os.ReadDir(".")
	for _, en := range ents {
		if !strings.HasSuffix(en.Name(), ".go") || strings.HasSuffix(en.Name(), "_test.go") {
			continue
		}
		b, _ := os.ReadFile(en.Name())
		for _, n := range []string{"service.json", "mcp-auth.json", "opencode.db", ".config/opencode/opencode.json"} {
			for _, line := range strings.Split(string(b), "\n") {
				code := line
				if i := strings.Index(code, "//"); i >= 0 {
					code = code[:i]
				}
				if strings.Contains(code, `"`+n) {
					t.Errorf("%s spells %q outside the guard", en.Name(), n)
				}
			}
		}
	}
}
