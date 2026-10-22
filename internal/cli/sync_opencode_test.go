package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncTargetValidationMessages(t *testing.T) {
	e := newSyncEnv(t)
	out, err := e.sync(t, "--target", "nope")
	if err == nil || !strings.Contains(err.Error(), "claude-code, opencode") {
		t.Fatalf("unknown target must list the supported ones: %v %s", err, out)
	}
	if _, err := e.sync(t); err == nil || !strings.Contains(err.Error(), "opencode") {
		t.Fatalf("missing target must list the supported ones: %v", err)
	}
	for _, flag := range []string{"--no-hooks", "--remove-hooks"} {
		_, err := e.sync(t, "--target", "opencode", flag)
		if err == nil || !strings.Contains(err.Error(), "지원하지 않습니다") {
			t.Fatalf("%s on opencode: %v", flag, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.proj, "opencode.json")); err == nil {
		t.Fatal("rejected invocations wrote files")
	}
}

func TestSyncOpenCodePlanApplyRestore(t *testing.T) {
	e := newSyncEnv(t)
	before := treeSig(t, e.proj) + treeSig(t, e.home)
	for _, extra := range [][]string{{}, {"--dry-run", "--diff"}, {"--json"}, {"--scope", "user", "--diff"}} {
		if out, err := e.sync(t, append([]string{"--target", "opencode"}, extra...)...); err != nil {
			t.Fatalf("%v: %v\n%s", extra, err, out)
		}
		if treeSig(t, e.proj)+treeSig(t, e.home) != before {
			t.Fatalf("%v: plan wrote files", extra)
		}
	}
	out, _ := e.sync(t, "--target", "opencode", "--diff")
	for _, want := range []string{"계획만 출력", "demo-skill", "direct-srv", "gw-srv", "{env:MY_TOKEN}", "{env:GW_AUTH}", "opencode reload"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan misses %q:\n%s", want, out)
		}
	}
	e.sync(t, "--target", "opencode", "--apply")
	for _, f := range []string{"opencode.json", ".opencode/skills/demo-skill/SKILL.md", ".forge/sync-state.opencode.json"} {
		if _, err := os.Stat(filepath.Join(e.proj, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	var cfg struct {
		MCP map[string]map[string]any `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(e.read(t, "opencode.json")), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.MCP["forge"]["type"] != "local" || cfg.MCP["gw-srv"]["type"] != "remote" || cfg.MCP["direct-srv"]["type"] != "local" {
		t.Fatalf("%v", cfg.MCP)
	}
	for _, f := range []string{".mcp.json", ".claude"} {
		if _, err := os.Stat(filepath.Join(e.proj, f)); err == nil {
			t.Fatalf("opencode sync created %s", f)
		}
	}
	out, err := e.sync(t, "--target", "opencode", "--apply")
	if err != nil || !strings.Contains(out, "변경 없음") || strings.Contains(out, "백업:") {
		t.Fatalf("second apply: %v\n%s", err, out)
	}
	if !strings.Contains(mustRun(t, "sync", "--list-backups"), "opencode") {
		t.Fatal("list-backups")
	}
	mustRun(t, "sync", "--restore", "latest")
	for _, f := range []string{"opencode.json", ".opencode", ".forge"} {
		if _, err := os.Stat(filepath.Join(e.proj, f)); err == nil {
			t.Errorf("%s must be gone after restore", f)
		}
	}
}

func TestSyncBothTargetsCoexist(t *testing.T) {
	e := newSyncEnv(t)
	e.sync(t, "--target", "claude-code", "--apply")
	out, err := e.sync(t, "--target", "opencode", "--apply")
	if err != nil || !strings.Contains(out, ".claude/skills 로 이미 노출됨") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(e.proj, ".opencode/skills/demo-skill")); err == nil {
		t.Fatal("skill duplicated")
	}
	// restoring the opencode backup leaves the claude-code files alone
	mustRun(t, "sync", "--restore", "latest")
	if _, err := os.Stat(filepath.Join(e.proj, ".claude/skills/demo-skill/SKILL.md")); err != nil {
		t.Fatal("claude-code files damaged by an opencode restore")
	}
	if _, err := os.Stat(filepath.Join(e.proj, "opencode.json")); err == nil {
		t.Fatal("opencode.json must be gone")
	}
}

func TestSyncOpenCodeUserScopePrintsCommandsOnly(t *testing.T) {
	e := newSyncEnv(t)
	gdir := filepath.Join(e.home, ".config", "opencode")
	if err := os.MkdirAll(gdir, 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(gdir, "opencode.json")
	if err := os.WriteFile(global, []byte(`{"provider":{"x":{"options":{"apiKey":"DECOY"}}}}`), 0o000); err != nil {
		t.Fatal(err)
	}
	out, err := e.sync(t, "--target", "opencode", "--scope", "user", "--apply")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"opencode mcp add forge --global -- forge mcp serve --client opencode", "'API_TOKEN={env:MY_TOKEN}'", "직접 실행할 명령"} {
		if !strings.Contains(out, want) {
			t.Errorf("user-scope output misses %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "opencode mcp add") {
			c := strings.TrimSpace(line)
			if o, err := exec.Command("sh", "-n", "-c", c).CombinedOutput(); err != nil {
				t.Fatalf("sh -n: %v %s", err, o)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(gdir, "skills/demo-skill/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(global); err != nil || fi.Mode().Perm() != 0 {
		t.Fatalf("global config touched: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(e.proj, "opencode.json")); err == nil {
		t.Fatal("user scope wrote into the project")
	}
}

func TestSyncOpenCodeJSONCRefusal(t *testing.T) {
	e := newSyncEnv(t)
	if err := os.WriteFile(filepath.Join(e.proj, "opencode.jsonc"), []byte("{\n// c\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeSig(t, e.proj)
	out, err := e.sync(t, "--target", "opencode", "--apply")
	if err == nil || !strings.Contains(err.Error(), "JSONC") {
		t.Fatalf("%v %s", err, out)
	}
	if treeSig(t, e.proj) != before {
		t.Fatal("wrote files")
	}
}
