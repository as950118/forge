package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type allJSON struct {
	Schema    int      `json:"schema"`
	Target    string   `json:"target"`
	Mode      string   `json:"mode"`
	Conflicts int      `json:"conflicts"`
	Applied   []string `json:"applied"`
	Partial   bool     `json:"partial"`
	Restore   []string `json:"restore"`
	Targets   []struct {
		Target string `json:"target"`
		Status string `json:"status"`
		Error  string `json:"error"`
		Report struct {
			Conflicts int `json:"conflicts"`
			Items     []struct{ Kind, Name, Action string }
		} `json:"report"`
	} `json:"targets"`
}

func parseAll(t *testing.T, out string) allJSON {
	t.Helper()
	var r allJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return r
}

func exists(t *testing.T, p ...string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(p...))
	return err == nil
}

func TestSyncAllPlanIsUnifiedAndWritesNothing(t *testing.T) {
	e := newSyncEnv(t)
	before := treeSig(t, e.proj) + treeSig(t, e.home)
	out, err := e.sync(t, "--target", "all", "--diff")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"=== claude-code ===", "=== opencode ===", "=== 통합 요약 ===", "충돌 0건", "demo-skill", "direct-srv", "프로필 없음"} {
		if !strings.Contains(out, want) {
			t.Errorf("unified plan misses %q:\n%s", want, out)
		}
	}
	// The skill OpenCode would also read from .claude/skills is claimed by claude-code:
	// the unified plan already shows it as skipped for opencode (no double exposure).
	ocPart := out[strings.Index(out, "=== opencode ==="):]
	if !strings.Contains(ocPart, ".claude/skills 로 이미 노출됨") {
		t.Fatalf("the opencode part must not render the skill claude-code renders:\n%s", ocPart)
	}
	if treeSig(t, e.proj)+treeSig(t, e.home) != before {
		t.Fatal("the plan wrote files")
	}
	r := parseAll(t, mustJSON(t, e, "--target", "all", "--json"))
	if r.Schema != 1 || r.Target != "all" || r.Mode != "plan" || len(r.Targets) != 2 || r.Targets[0].Target != "claude-code" || r.Targets[1].Target != "opencode" || r.Partial {
		t.Fatalf("%+v", r)
	}
	for _, tg := range r.Targets {
		if tg.Status != "planned" {
			t.Fatalf("%+v", tg)
		}
	}
	// the same flag validation as a single target
	for _, args := range [][]string{{"--target", "all", "--apply", "--dry-run"}, {"--target", "all", "--force"}, {"--target", "all", "--scope", "x"}} {
		if _, err := e.sync(t, args...); err == nil {
			t.Errorf("%v must fail", args)
		}
	}
}

func mustJSON(t *testing.T, e *syncEnv, args ...string) string {
	t.Helper()
	out, err := e.sync(t, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return out
}

// AC-3: one profile switch makes both clients consistent, including the rule
// that a skill is not exposed twice.
func TestProfileSwitchMakesBothClientsConsistent(t *testing.T) {
	e := newSyncEnv(t)
	addSkillDir(t, "other-skill", "o")
	mustRun(t, "profile", "create", "A")
	mustRun(t, "profile", "add", "A", "--skill", "demo-skill")
	mustRun(t, "profile", "add", "A", "--mcp", "direct-srv")
	mustRun(t, "profile", "add", "A", "--mcp", "gw-srv", "--exposure", "gateway", "--client", "all")
	mustRun(t, "profile", "create", "B")
	mustRun(t, "profile", "add", "B", "--skill", "other-skill")
	mustRun(t, "profile", "add", "B", "--mcp", "gw-srv", "--exposure", "hidden", "--client", "opencode")

	mustRun(t, "profile", "use", "A", "--client", "all")
	e.syncOK(t, "--target", "all", "--apply")
	cc, oc := e.read(t, ".mcp.json"), e.read(t, "opencode.json")
	for name, doc := range map[string]string{"claude-code": cc, "opencode": oc} {
		if !strings.Contains(doc, "forge") || !strings.Contains(doc, "direct-srv") {
			t.Errorf("%s: profile A must render the gateway entry and direct-srv:\n%s", name, doc)
		}
		if strings.Contains(doc, "gw-srv") {
			t.Errorf("%s: gw-srv is gateway-only under profile A, not written directly:\n%s", name, doc)
		}
	}
	if !exists(t, e.proj, ".claude", "skills", "demo-skill", "SKILL.md") {
		t.Fatal("demo-skill must be rendered for claude-code")
	}
	if exists(t, e.proj, ".opencode", "skills", "demo-skill") {
		t.Fatal("a skill must not be exposed twice (claude-code already renders it under .claude/skills, which opencode also reads)")
	}
	if exists(t, e.proj, ".claude", "skills", "other-skill") || exists(t, e.proj, ".opencode", "skills", "other-skill") {
		t.Fatal("other-skill is not in profile A")
	}

	// Second run: nothing to do for either client.
	out := e.syncOK(t, "--target", "all")
	if strings.Count(out, "변경 없음: 쓸 파일이 없습니다") != 2 {
		t.Fatalf("both clients must be in sync:\n%s", out)
	}

	// Switch to B with one command, then one sync.
	mustRun(t, "profile", "use", "B", "--client", "all")
	plan := e.syncOK(t, "--target", "all")
	if !strings.Contains(plan, "프로필 \"B\" 적용 (전역 활성화, 클라이언트 claude-code)") || !strings.Contains(plan, "프로필 \"B\" 적용 (전역 활성화, 클라이언트 opencode)") {
		t.Fatalf("the plan must state the rule per client:\n%s", plan)
	}
	e.syncOK(t, "--target", "all", "--apply")
	cc, oc = e.read(t, ".mcp.json"), e.read(t, "opencode.json")
	for name, doc := range map[string]string{"claude-code": cc, "opencode": oc} {
		if strings.Contains(doc, "direct-srv") {
			t.Errorf("%s: profile B has no direct-srv:\n%s", name, doc)
		}
		if !strings.Contains(doc, "forge") {
			t.Errorf("%s: the gateway entry stays:\n%s", name, doc)
		}
	}
	// gw-srv is hidden for opencode only; claude-code keeps the default (direct).
	if !strings.Contains(cc, "gw-srv") || strings.Contains(oc, "gw-srv") {
		t.Fatalf("per-client exposure of one member:\ncc=%s\noc=%s", cc, oc)
	}
	if exists(t, e.proj, ".claude", "skills", "demo-skill") {
		t.Fatal("demo-skill was removed by the switch (managed skill, no longer wanted)")
	}
	if !exists(t, e.proj, ".claude", "skills", "other-skill", "SKILL.md") || exists(t, e.proj, ".opencode", "skills", "other-skill") {
		t.Fatal("other-skill exactly once, under .claude/skills")
	}
	if out := e.syncOK(t, "--target", "all"); strings.Count(out, "변경 없음: 쓸 파일이 없습니다") != 2 {
		t.Fatalf("both in sync after the switch:\n%s", out)
	}

	// Per-client profiles: the clients may differ on purpose, each consistent with its own profile.
	mustRun(t, "profile", "use", "A", "--client", "opencode")
	e.syncOK(t, "--target", "all", "--apply")
	oc = e.read(t, "opencode.json")
	if !strings.Contains(oc, "direct-srv") || strings.Contains(e.read(t, ".mcp.json"), "direct-srv") {
		t.Fatalf("opencode follows A, claude-code follows B:\n%s", oc)
	}
	if !exists(t, e.proj, ".opencode", "skills", "demo-skill", "SKILL.md") || exists(t, e.proj, ".claude", "skills", "demo-skill") {
		t.Fatal("demo-skill belongs to A (opencode) and is then rendered by opencode itself")
	}
}

func TestSyncAllConflictAnywhereWritesNothing(t *testing.T) {
	e := newSyncEnv(t)
	// A user-owned claude-code entry with the same name as a registry server.
	if err := os.WriteFile(filepath.Join(e.proj, ".mcp.json"), []byte(`{"mcpServers":{"direct-srv":{"type":"stdio","command":"mine"}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mcpBefore := e.read(t, ".mcp.json")
	out, err := e.sync(t, "--target", "all", "--apply")
	if err == nil || !strings.Contains(err.Error(), "충돌") {
		t.Fatalf("a conflict must stop everything: %v\n%s", err, out)
	}
	if !strings.Contains(out, "=== 통합 요약 ===") || !strings.Contains(out, "적용하지 않음") {
		t.Fatalf("the unified plan and the per-target state must be shown:\n%s", out)
	}
	if e.read(t, ".mcp.json") != mcpBefore || exists(t, e.proj, "opencode.json") || exists(t, e.proj, ".claude") || exists(t, e.proj, ".opencode") || exists(t, e.proj, ".forge") {
		t.Fatal("something was written although there was a conflict (the opencode part has none)")
	}
	if l, _ := os.ReadDir(filepath.Join(e.homeForge(), "backups")); len(l) != 0 {
		t.Fatalf("no backup may be made: %v", l)
	}
	js, jerr := e.sync(t, "--target", "all", "--apply", "--json")
	if jerr == nil {
		t.Fatal("must fail")
	}
	r := parseAll(t, js)
	if r.Conflicts != 1 || r.Mode != "apply" || len(r.Applied) != 0 || r.Targets[0].Status != "not-applied" || r.Targets[1].Status != "not-applied" {
		t.Fatalf("%+v", r)
	}
	// --skip-conflicts applies both without the conflicting item.
	e.syncOK(t, "--target", "all", "--apply", "--skip-conflicts")
	if !strings.Contains(e.read(t, ".mcp.json"), `"mine"`) || !exists(t, e.proj, "opencode.json") {
		t.Fatal("skip-conflicts must apply the rest and keep the user's entry")
	}
}

func (e *syncEnv) homeForge() string { return os.Getenv("FORGE_HOME") }

func TestSyncAllPartialFailureIsReportedAndRestorable(t *testing.T) {
	e := newSyncEnv(t)
	defer func() { beforeApply = nil }()
	// The opencode.json changes between planning and applying: only the second
	// target fails (stale plan), claude-code has been applied by then.
	beforeApply = func(target string) {
		if target == "opencode" {
			_ = os.WriteFile(filepath.Join(e.proj, "opencode.json"), []byte(`{"someone":"else"}`+"\n"), 0o644)
		}
	}
	out, err := e.sync(t, "--target", "all", "--apply")
	if err == nil || !strings.Contains(err.Error(), "일부만 적용") {
		t.Fatalf("partial success must be an error: %v\n%s", err, out)
	}
	for _, want := range []string{"claude-code: 적용됨", "opencode: 실패", "부분 성공: claude-code 만 적용", "forge sync --restore ", "forge sync --target all --apply"} {
		if !strings.Contains(out, want) {
			t.Errorf("report misses %q:\n%s", want, out)
		}
	}
	// L2: the failed section must not claim "plan only".
	oc := out[strings.Index(out, "=== opencode ==="):]
	if strings.Contains(oc, "계획만 출력") || !strings.Contains(oc, "적용을 시도했으나 실패") {
		t.Errorf("failed adapter section has the wrong mode line:\n%s", oc)
	}
	if cc := out[strings.Index(out, "=== claude-code ==="):strings.Index(out, "=== opencode ===")]; !strings.Contains(cc, "모드: 적용\n") {
		t.Errorf("applied adapter section must say 모드: 적용:\n%s", cc)
	}
	if !exists(t, e.proj, ".mcp.json") {
		t.Fatal("claude-code part must be applied")
	}
	if got := e.read(t, "opencode.json"); got != `{"someone":"else"}`+"\n" {
		t.Fatalf("the failed target must not have touched the file: %q", got)
	}
	// JSON form
	e2 := newSyncEnv(t)
	beforeApply = func(target string) {
		if target == "opencode" {
			_ = os.WriteFile(filepath.Join(e2.proj, "opencode.json"), []byte(`{"someone":"else"}`+"\n"), 0o644)
		}
	}
	js, jerr := e2.sync(t, "--target", "all", "--apply", "--json")
	if jerr == nil {
		t.Fatal("must fail")
	}
	r := parseAll(t, js)
	if !r.Partial || len(r.Applied) != 1 || r.Applied[0] != "claude-code" || r.Targets[0].Status != "applied" || r.Targets[1].Status != "failed" || r.Targets[1].Error == "" || len(r.Restore) != 1 {
		t.Fatalf("%+v", r)
	}
	// Undo the applied part with the printed command, then converge with a retry.
	beforeApply = nil
	restore := strings.TrimPrefix(r.Restore[0], "forge ")
	mustRun(t, strings.Fields(restore)...)
	if exists(t, e2.proj, ".mcp.json") {
		t.Fatal("restore must remove the file claude-code created")
	}
	_ = os.Remove(filepath.Join(e2.proj, "opencode.json"))
	e2.syncOK(t, "--target", "all", "--apply")
	if !exists(t, e2.proj, ".mcp.json") || !exists(t, e2.proj, "opencode.json") {
		t.Fatal("retry must converge")
	}

	// A failure of the first target means the second is not applied at all.
	e3 := newSyncEnv(t)
	beforeApply = func(target string) {
		if target == "claude-code" {
			_ = os.WriteFile(filepath.Join(e3.proj, ".mcp.json"), []byte(`{"mcpServers":{}}`+"\n"), 0o644)
		}
	}
	out, err = e3.sync(t, "--target", "all", "--apply")
	if err == nil || strings.Contains(err.Error(), "일부만") {
		t.Fatalf("no partial state when the first fails: %v", err)
	}
	if exists(t, e3.proj, "opencode.json") || !strings.Contains(out, "opencode: 적용하지 않음") {
		t.Fatalf("opencode must not be applied after claude-code failed:\n%s", out)
	}
	if oc := out[strings.Index(out, "=== opencode ==="):]; strings.Contains(oc, "계획만 출력") || !strings.Contains(oc, "적용하지 않았습니다") {
		t.Errorf("not-applied section has the wrong mode line:\n%s", oc)
	}
	if cc := out[strings.Index(out, "=== claude-code ==="):strings.Index(out, "=== opencode ===")]; !strings.Contains(cc, "적용을 시도했으나 실패") {
		t.Errorf("failed claude-code section:\n%s", cc)
	}
}

func TestSyncAllNoHooksOnlyAppliesToClaudeCode(t *testing.T) {
	e := newSyncEnv(t)
	e.syncOK(t, "--target", "all", "--apply", "--no-hooks")
	if exists(t, e.proj, ".claude", "settings.json") {
		t.Fatal("--no-hooks must leave hooks alone")
	}
	if !exists(t, e.proj, "opencode.json") {
		t.Fatal("opencode part is written")
	}
}

// syncOK runs forge sync for the project and fails the test on an error.
func (e *syncEnv) syncOK(t *testing.T, args ...string) string {
	t.Helper()
	out, err := e.sync(t, args...)
	if err != nil {
		t.Fatalf("forge sync %v: %v\n%s", args, err, out)
	}
	return out
}
