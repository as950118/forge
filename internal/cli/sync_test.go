package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type syncEnv struct {
	home, proj, decoy string
}

// newSyncEnv isolates HOME, FORGE_HOME and a project, registers a skill and two
// MCP servers (direct, gateway) and plants a decoy for the Claude Code user file.
func newSyncEnv(t *testing.T) *syncEnv {
	t.Helper()
	forgeHome(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &syncEnv{home: filepath.Join(base, "home"), proj: filepath.Join(base, "proj")}
	for _, d := range []string{e.home, filepath.Join(e.proj, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", e.home)
	e.decoy = filepath.Join(e.home, ".claude"+".json")
	if err := os.WriteFile(e.decoy, []byte(`{"decoy":true}`), 0o000); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "init")
	mustRun(t, "skill", "add", demoDir(t))
	mustRun(t, "mcp", "add", "direct-srv", "--env", "API_TOKEN=${MY_TOKEN}", "--", "npx", "-y", "srv")
	mustRun(t, "mcp", "add", "gw-srv", "--url", "https://example.com/mcp", "--transport", "http", "--header", "Authorization=${GW_AUTH}")
	mustRun(t, "mcp", "expose", "gw-srv", "--client", "claude-code", "--mode", "gateway")
	return e
}

func (e *syncEnv) sync(t *testing.T, args ...string) (string, error) {
	t.Helper()
	all := append([]string{"sync", "--project-dir", e.proj}, args...)
	return runErr(t, "", all...)
}

func (e *syncEnv) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.proj, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func treeSig(t *testing.T, root string) string {
	t.Helper()
	var sb strings.Builder
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, _ := d.Info()
		sb.WriteString(p + "|" + fi.Mode().String() + "|")
		if fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			sb.Write(b)
		}
		sb.WriteString("\n")
		return nil
	})
	return sb.String()
}

func TestSyncFlagValidation(t *testing.T) {
	e := newSyncEnv(t)
	cases := map[string][]string{
		"apply+dry-run":      {"--target", "claude-code", "--apply", "--dry-run"},
		"no target":          {},
		"opencode":           {"--target", "opencode"},
		"unknown target":     {"--target", "nope"},
		"bad scope":          {"--target", "claude-code", "--scope", "global"},
		"restore+list":       {"--restore", "latest", "--list-backups"},
		"restore with apply": {"--restore", "latest", "--apply"},
		"force alone":        {"--target", "claude-code", "--force"},
		"keep alone":         {"--target", "claude-code", "--keep", "3"},
		"relative bin":       {"--target", "claude-code", "--bin", "./forge"},
		"no+remove hooks":    {"--target", "claude-code", "--no-hooks", "--remove-hooks"},
		"restore+remove":     {"--restore", "latest", "--remove-hooks"},
	}
	for name, args := range cases {
		if out, err := e.sync(t, args...); err == nil {
			t.Errorf("%s: want an error, got output %q", name, out)
		}
	}
	if _, err := os.Stat(filepath.Join(e.proj, ".mcp.json")); err == nil {
		t.Fatal("rejected invocations wrote files")
	}
}

func TestSyncDefaultIsPlanOnly(t *testing.T) {
	e := newSyncEnv(t)
	before := treeSig(t, e.proj) + treeSig(t, e.home)
	for _, extra := range [][]string{{}, {"--dry-run"}, {"--dry-run", "--diff"}, {"--json"}, {"--scope", "user", "--diff"}} {
		out, err := e.sync(t, append([]string{"--target", "claude-code"}, extra...)...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", extra, err, out)
		}
		if treeSig(t, e.proj)+treeSig(t, e.home) != before {
			t.Fatalf("%v: plan wrote files", extra)
		}
	}
	out, _ := e.sync(t, "--target", "claude-code", "--diff")
	for _, want := range []string{"계획만 출력", "demo-skill", "direct-srv", "exposure=gateway", "forge", "PreToolUse", "+++ ", "${MY_TOKEN}"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output misses %q:\n%s", want, out)
		}
	}
	var rep map[string]any
	js, _ := e.sync(t, "--target", "claude-code", "--json")
	if err := json.Unmarshal([]byte(js), &rep); err != nil || rep["mode"] != "plan" || rep["applied"] != false {
		t.Fatalf("json: %v %s", err, js)
	}
}

func TestSyncApplyRestoreFlow(t *testing.T) {
	e := newSyncEnv(t)
	e.sync(t, "--target", "claude-code", "--apply")
	for _, f := range []string{".mcp.json", ".claude/settings.json", ".claude/skills/demo-skill/SKILL.md", ".forge/sync-state.json"} {
		if _, err := os.Stat(filepath.Join(e.proj, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if !strings.Contains(e.read(t, ".mcp.json"), `"direct-srv"`) || strings.Contains(e.read(t, ".mcp.json"), "gw-srv") {
		t.Fatalf(".mcp.json:\n%s", e.read(t, ".mcp.json"))
	}
	// second run: nothing to do
	out, err := e.sync(t, "--target", "claude-code", "--apply")
	if err != nil || !strings.Contains(out, "변경 없음") || strings.Contains(out, "백업:") {
		t.Fatalf("second apply: %v\n%s", err, out)
	}
	// backups
	var lb struct {
		Backups []map[string]any `json:"backups"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "sync", "--list-backups", "--json")), &lb); err != nil || len(lb.Backups) != 1 {
		t.Fatalf("list-backups: %v %+v", err, lb)
	}
	if !strings.Contains(mustRun(t, "sync", "--list-backups"), "claude-code") {
		t.Fatal("list-backups text")
	}
	snap := treeSig(t, e.proj)
	out = mustRun(t, "sync", "--restore", "latest", "--dry-run")
	if !strings.Contains(out, "복원 계획") || treeSig(t, e.proj) != snap {
		t.Fatalf("restore dry run:\n%s", out)
	}
	out = mustRun(t, "sync", "--restore", "latest")
	if !strings.Contains(out, "복원 직전 상태를 백업") {
		t.Fatalf("restore:\n%s", out)
	}
	for _, f := range []string{".mcp.json", ".claude", ".forge"} {
		if _, err := os.Stat(filepath.Join(e.proj, f)); err == nil {
			t.Errorf("%s must be gone after restoring the pre-sync state", f)
		}
	}
	// the restore itself is undoable
	mustRun(t, "sync", "--restore", "latest")
	if _, err := os.Stat(filepath.Join(e.proj, ".mcp.json")); err != nil {
		t.Fatal("undoing the restore must bring the synced files back")
	}
	// prune: dry run keeps, real prune keeps the newest N
	out = mustRun(t, "sync", "--prune-backups", "--keep", "1", "--dry-run")
	if !strings.Contains(out, "삭제 예정") {
		t.Fatalf("prune dry run: %s", out)
	}
	if err := json.Unmarshal([]byte(mustRun(t, "sync", "--list-backups", "--json")), &lb); err != nil || len(lb.Backups) != 3 {
		t.Fatalf("dry-run prune deleted: %d", len(lb.Backups))
	}
	mustRun(t, "sync", "--prune-backups", "--keep", "1")
	if err := json.Unmarshal([]byte(mustRun(t, "sync", "--list-backups", "--json")), &lb); err != nil || len(lb.Backups) != 1 {
		t.Fatalf("prune: %d", len(lb.Backups))
	}
}

func TestSyncConflictsExitNonZeroAndSkip(t *testing.T) {
	e := newSyncEnv(t)
	if err := os.MkdirAll(filepath.Join(e.proj, ".claude/skills/demo-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(e.proj, ".claude/skills/demo-skill/SKILL.md")
	if err := os.WriteFile(userFile, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeSig(t, e.proj)
	out, err := e.sync(t, "--target", "claude-code")
	if err != nil || !strings.Contains(out, "충돌") {
		t.Fatalf("plan with conflict must exit 0 and list it: %v\n%s", err, out)
	}
	out, err = e.sync(t, "--target", "claude-code", "--apply")
	if err == nil || !strings.Contains(out, "충돌") {
		t.Fatalf("apply with conflict must fail: %v\n%s", err, out)
	}
	if treeSig(t, e.proj) != before {
		t.Fatal("apply with conflicts wrote files")
	}
	if _, err := e.sync(t, "--target", "claude-code", "--apply", "--skip-conflicts"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(userFile); string(b) != "mine\n" {
		t.Fatal("user skill modified")
	}
	if !strings.Contains(e.read(t, ".mcp.json"), "direct-srv") {
		t.Fatal("non-conflicting items must be applied")
	}
}

func TestSyncUserScopePrintsCommandsOnly(t *testing.T) {
	e := newSyncEnv(t)
	out, err := e.sync(t, "--target", "claude-code", "--scope", "user", "--apply")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"claude mcp add --scope user --transport stdio forge -- forge mcp serve --client claude-code", "'API_TOKEN=${MY_TOKEN}'", "직접 실행할 명령"} {
		if !strings.Contains(out, want) {
			t.Errorf("user-scope output misses %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(e.home, ".claude/skills/demo-skill/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".mcp.json")); err == nil {
		t.Fatal("user scope must not write .mcp.json")
	}
	if fi, err := os.Stat(e.decoy); err != nil || fi.Mode().Perm() != 0 || fi.Size() != int64(len(`{"decoy":true}`)) {
		t.Fatalf("decoy touched: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(e.proj, ".mcp.json")); err == nil {
		t.Fatal("user scope wrote into the project")
	}
}

func TestSyncConcurrentApplies(t *testing.T) {
	e := newSyncEnv(t)
	oldTimeout := syncLockTimeout
	syncLockTimeout = 20 * time.Second
	defer func() { syncLockTimeout = oldTimeout }()
	done := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			_, err := e.sync(t, "--target", "claude-code", "--apply")
			done <- err
		}()
	}
	for i := 0; i < 3; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var lb struct {
		Backups []map[string]any `json:"backups"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "sync", "--list-backups", "--json")), &lb); err != nil || len(lb.Backups) != 1 {
		t.Fatalf("exactly one concurrent sync may write; backups=%d err=%v", len(lb.Backups), err)
	}
}

func TestSyncNoHooksAndBin(t *testing.T) {
	e := newSyncEnv(t)
	e.sync(t, "--target", "claude-code", "--apply", "--no-hooks", "--bin", "/opt/forge/bin/forge")
	if _, err := os.Stat(filepath.Join(e.proj, ".claude/settings.json")); err == nil {
		t.Fatal("--no-hooks must not create settings.json")
	}
	if !strings.Contains(e.read(t, ".mcp.json"), `"/opt/forge/bin/forge"`) {
		t.Fatal("--bin not used")
	}
}

func TestSyncNoHooksKeepsInstalledHooksAndRemoveHooksRemovesThem(t *testing.T) {
	e := newSyncEnv(t)
	e.sync(t, "--target", "claude-code", "--apply")
	installed := e.read(t, ".claude/settings.json")
	if !strings.Contains(installed, "forge hook ingest") {
		t.Fatal("hooks were not installed")
	}
	// --no-hooks: neither install, update nor remove.
	out, err := e.sync(t, "--target", "claude-code", "--apply", "--no-hooks")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if e.read(t, ".claude/settings.json") != installed {
		t.Fatal("--no-hooks modified settings.json")
	}
	plan, _ := e.sync(t, "--target", "claude-code", "--no-hooks")
	if !strings.Contains(plan, "그대로 둡니다") || !strings.Contains(plan, "삭제 0") {
		t.Fatalf("plan must say installed hooks are left alone:\n%s", plan)
	}
	// --remove-hooks removes exactly the managed handlers.
	plan, _ = e.sync(t, "--target", "claude-code", "--remove-hooks")
	if strings.Contains(plan, "삭제 0") || !strings.Contains(plan, "remove-hooks") {
		t.Fatalf("plan must show the removal:\n%s", plan)
	}
	if e.read(t, ".claude/settings.json") != installed {
		t.Fatal("a plan changed settings.json")
	}
	if out, err := e.sync(t, "--target", "claude-code", "--apply", "--remove-hooks"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(e.read(t, ".claude/settings.json"), "forge hook ingest") {
		t.Fatal("--remove-hooks left managed hooks behind")
	}
	// A later plain sync installs them again.
	e.sync(t, "--target", "claude-code", "--apply")
	if !strings.Contains(e.read(t, ".claude/settings.json"), "forge hook ingest") {
		t.Fatal("hooks not reinstalled")
	}
}

func TestSyncProjectDirCaseVariantOfHomeIsRefused(t *testing.T) {
	e := newSyncEnv(t)
	variant := filepath.Join(filepath.Dir(e.home), strings.ToUpper(filepath.Base(e.home)))
	if _, err := os.Lstat(variant); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	out, err := runErr(t, "", "sync", "--target", "claude-code", "--project-dir", variant)
	if err == nil {
		t.Fatalf("a case variant of HOME was accepted as the project root:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(e.home, ".mcp.json")); serr == nil {
		t.Fatal("wrote into HOME")
	}
}
