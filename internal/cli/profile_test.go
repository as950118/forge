package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func addSkillDir(t *testing.T, name, body string) {
	t.Helper()
	d := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: skill " + name + "\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "skill", "add", d)
}

func (e *syncEnv) forge(t *testing.T, args ...string) string {
	t.Helper()
	return mustRun(t, args...)
}

func TestProfileCRUDFlow(t *testing.T) {
	e := newSyncEnv(t)
	if out := mustRun(t, "profile", "list"); !strings.Contains(out, "프로필이 없습니다") || !strings.Contains(out, "기존 동작") {
		t.Fatalf("empty list must explain the legacy behaviour:\n%s", out)
	}
	out := mustRun(t, "profile", "create", "work", "--description", "daily work")
	if !strings.Contains(out, "프로필 생성: work") || !strings.Contains(out, "forge profile use work --client all") {
		t.Fatalf("%s", out)
	}
	if _, err := runErr(t, "", "profile", "create", "work"); err == nil || !strings.Contains(err.Error(), "이미 있습니다") {
		t.Fatalf("duplicate: %v", err)
	}
	mustRun(t, "profile", "add", "work", "--skill", "demo-skill", "--pin", pinOf(t, "demo-skill"))
	mustRun(t, "profile", "add", "work", "--mcp", "gw-srv", "--exposure", "gateway", "--client", "claude-code", "--client", "opencode")
	mustRun(t, "profile", "add", "work", "--mcp", "direct-srv")
	show := mustRun(t, "profile", "show", "work")
	for _, want := range []string{"work", "허용 목록", "demo-skill", "gw-srv", "gateway", "direct-srv", "claude-code", "opencode"} {
		if !strings.Contains(show, want) {
			t.Errorf("show misses %q:\n%s", want, show)
		}
	}
	var js struct {
		Schema  int `json:"schema"`
		Profile struct {
			Name    string `json:"name"`
			Scope   string `json:"scope"`
			Members []struct {
				Kind, Name    string
				PinnedVersion string `json:"pinned_version"`
			} `json:"member_list"`
		} `json:"profile"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "profile", "show", "work", "--json")), &js); err != nil || js.Schema != 1 || js.Profile.Name != "work" || len(js.Profile.Members) != 3 {
		t.Fatalf("%v %+v", err, js)
	}
	for _, m := range js.Profile.Members {
		if m.Name == "demo-skill" && m.PinnedVersion == "" {
			t.Fatal("pin lost")
		}
	}
	// add/remove validation
	for _, args := range [][]string{
		{"profile", "add", "work"},
		{"profile", "add", "work", "--skill", "demo-skill", "--mcp", "direct-srv"},
		{"profile", "add", "work", "--skill", "nope"},
		{"profile", "add", "work", "--skill", "demo-skill", "--pin", "9.9.9"},
		{"profile", "add", "work", "--mcp", "direct-srv", "--exposure", "gateway"},
		{"profile", "add", "work", "--mcp", "direct-srv", "--client", "claude-code"},
		{"profile", "add", "work", "--mcp", "direct-srv", "--pin", "1"},
		{"profile", "add", "work", "--mcp", "direct-srv", "--exposure", "gateway", "--client", "emacs"},
		{"profile", "remove", "work", "--tool", "ghost"},
		{"profile", "create", "bad name"},
		{"profile", "create", "x", "--global", "--project-dir", e.proj},
		{"profile", "use", "work"},
		{"profile", "use", "work", "--client", "emacs"},
		{"profile", "use", "ghost", "--client", "all"},
		{"profile", "show", "ghost"},
		{"profile", "delete", "ghost"},
	} {
		if out, err := runErr(t, "", args...); err == nil {
			t.Errorf("%v must fail, got %q", args, out)
		}
	}
	mustRun(t, "profile", "remove", "work", "--mcp", "direct-srv")
	if out := mustRun(t, "profile", "use", "work", "--client", "all"); !strings.Contains(out, "설정 파일은 아직 바뀌지 않았습니다") {
		t.Fatalf("use must say that nothing was written:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.proj, ".mcp.json")); err == nil {
		t.Fatal("profile use wrote client files")
	}
	if out := mustRun(t, "profile", "list"); !strings.Contains(out, "claude-code@전역") || !strings.Contains(out, "opencode@전역") {
		t.Fatalf("%s", out)
	}
	if _, err := runErr(t, "", "profile", "delete", "work"); err == nil || !strings.Contains(err.Error(), "unuse") {
		t.Fatalf("active profile must not be deleted: %v", err)
	}
	cur := mustRun(t, "profile", "current", "--project-dir", e.proj)
	if !strings.Contains(cur, "프로필 work (전역 활성화") || !strings.Contains(cur, "우선순위") {
		t.Fatalf("%s", cur)
	}
	mustRun(t, "profile", "unuse", "--client", "opencode")
	var cj struct {
		Clients []struct{ Client, Rule, Profile string } `json:"clients"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "profile", "current", "--project-dir", e.proj, "--json")), &cj); err != nil {
		t.Fatal(err)
	}
	rules := map[string]string{}
	for _, c := range cj.Clients {
		rules[c.Client] = c.Rule + ":" + c.Profile
	}
	if rules["claude-code"] != "global:work" || rules["opencode"] != "legacy:" {
		t.Fatalf("%v", rules)
	}
	if out := mustRun(t, "profile", "unuse", "--client", "opencode"); !strings.Contains(out, "해제할 활성화가 없습니다") {
		t.Fatal(out)
	}
	mustRun(t, "profile", "unuse", "--client", "all")
	mustRun(t, "profile", "delete", "work")
	if out := mustRun(t, "profile", "list"); !strings.Contains(out, "프로필이 없습니다") {
		t.Fatal(out)
	}
}

func pinOf(t *testing.T, skill string) string {
	t.Helper()
	var info struct {
		Skill struct {
			Current string `json:"current_version"`
		} `json:"skill"`
		Versions []struct {
			Version string
			Current bool
		}
		CurrentVersion string `json:"current_version"`
	}
	out := mustRun(t, "skill", "show", skill, "--json")
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if info.CurrentVersion != "" {
		return info.CurrentVersion
	}
	if info.Skill.Current != "" {
		return info.Skill.Current
	}
	t.Fatalf("no current version in %s", out)
	return ""
}

func TestProjectScopedProfileAndPrecedence(t *testing.T) {
	e := newSyncEnv(t)
	addSkillDir(t, "other-skill", "o")
	mustRun(t, "profile", "create", "g")
	mustRun(t, "profile", "add", "g", "--skill", "demo-skill")
	mustRun(t, "profile", "create", "pr", "--project-dir", e.proj)
	mustRun(t, "profile", "add", "pr", "--project-dir", e.proj, "--skill", "other-skill")
	if out := mustRun(t, "profile", "list"); !strings.Contains(out, "프로젝트 /") {
		t.Fatalf("%s", out)
	}
	// project profiles are not visible as global ones
	if _, err := runErr(t, "", "profile", "show", "pr"); err == nil {
		t.Fatal("a project profile is not global")
	}
	if _, err := runErr(t, "", "profile", "use", "pr", "--client", "all"); err == nil || !strings.Contains(err.Error(), "--project-dir") {
		t.Fatalf("%v", err)
	}
	mustRun(t, "profile", "use", "g", "--client", "all")
	mustRun(t, "profile", "use", "pr", "--client", "claude-code", "--project-dir", e.proj)

	cc := e.syncOK(t, "--target", "claude-code")
	if !strings.Contains(cc, "프로필 \"pr\" 적용 (이 프로젝트에 활성화") || !strings.Contains(cc, "other-skill") || strings.Contains(cc, "demo-skill") {
		t.Fatalf("claude-code must follow the project profile:\n%s", cc)
	}
	oc := e.syncOK(t, "--target", "opencode")
	if !strings.Contains(oc, "프로필 \"g\" 적용 (전역 활성화") || !strings.Contains(oc, "demo-skill") || strings.Contains(oc, "other-skill") {
		t.Fatalf("opencode falls back to the global profile:\n%s", oc)
	}
	// user scope ignores project activations
	us := e.syncOK(t, "--target", "claude-code", "--scope", "user")
	if !strings.Contains(us, "프로필 \"g\"") || !strings.Contains(us, "--scope user") {
		t.Fatalf("user scope follows the global profile only:\n%s", us)
	}
	cur := mustRun(t, "profile", "current", "--project-dir", e.proj)
	if !strings.Contains(cur, "프로필 pr (이 프로젝트에 활성화") || !strings.Contains(cur, "프로필 g (전역 활성화") {
		t.Fatalf("%s", cur)
	}
	mustRun(t, "profile", "unuse", "--client", "claude-code", "--project-dir", e.proj)
	if cc, _ = e.sync(t, "--target", "claude-code"); !strings.Contains(cc, "프로필 \"g\" 적용 (전역") {
		t.Fatalf("unuse of the project activation falls back to global:\n%s", cc)
	}
}

// The profile is an allow list: only members are rendered.
func TestProfileAllowListInSync(t *testing.T) {
	e := newSyncEnv(t)
	addSkillDir(t, "other-skill", "o")
	mustRun(t, "profile", "create", "only-demo")
	mustRun(t, "profile", "add", "only-demo", "--skill", "demo-skill")
	mustRun(t, "profile", "add", "only-demo", "--mcp", "direct-srv", "--exposure", "hidden", "--client", "opencode")
	mustRun(t, "profile", "use", "only-demo", "--client", "all")
	e.syncOK(t, "--target", "claude-code", "--apply")
	mcp := e.read(t, ".mcp.json")
	if !strings.Contains(mcp, `"forge"`) || strings.Contains(mcp, "gw-srv") {
		t.Fatalf("gateway entry + member only (gw-srv is not a member):\n%s", mcp)
	}
	if !strings.Contains(mcp, "direct-srv") {
		t.Fatalf("direct-srv is a member (default exposure direct for claude-code):\n%s", mcp)
	}
	if _, err := os.Stat(filepath.Join(e.proj, ".claude", "skills", "other-skill")); err == nil {
		t.Fatal("non-member skill rendered")
	}
	if _, err := os.Stat(filepath.Join(e.proj, ".claude", "skills", "demo-skill", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	// Adding the member makes it appear; hidden for opencode keeps it away from opencode.json.
	mustRun(t, "profile", "add", "only-demo", "--mcp", "gw-srv")
	e.syncOK(t, "--target", "all", "--apply")
	if m := e.read(t, ".mcp.json"); !strings.Contains(m, "gw-srv") || !strings.Contains(m, "direct-srv") {
		t.Fatalf("members rendered for claude-code:\n%s", m)
	}
	if o := e.read(t, "opencode.json"); strings.Contains(o, "direct-srv") || !strings.Contains(o, "gw-srv") {
		t.Fatalf("direct-srv is hidden for opencode:\n%s", o)
	}
	// Removing the member removes the managed entry again.
	mustRun(t, "profile", "remove", "only-demo", "--mcp", "gw-srv")
	e.syncOK(t, "--target", "all", "--apply")
	if m := e.read(t, ".mcp.json"); strings.Contains(m, "gw-srv") {
		t.Fatalf("%s", m)
	}
	// A disabled member is not exposed (enabled wins over membership).
	mustRun(t, "mcp", "disable", "direct-srv")
	e.syncOK(t, "--target", "claude-code", "--apply")
	if m := e.read(t, ".mcp.json"); strings.Contains(m, "direct-srv") {
		t.Fatalf("%s", m)
	}
}

// --from-current + legacy settings: the profile reproduces today's sync result.
func TestFromCurrentReproducesLegacyPlanAndLegacyCLIPolicy(t *testing.T) {
	e := newSyncEnv(t) // gw-srv is exposed=gateway for claude-code through the legacy setting
	mustRun(t, "mcp", "expose", "direct-srv", "--client", "opencode", "--mode", "hidden")
	items := func(client string) string {
		out, err := e.sync(t, "--target", client, "--json")
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Items []struct{ Kind, Name, Action, Reason string } `json:"items"`
			Files []struct{ Path, Change string }               `json:"files"`
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(r)
		return strings.ReplaceAll(string(b), e.proj, "<proj>")
	}
	legacyCC, legacyOC := items("claude-code"), items("opencode")
	mustRun(t, "profile", "create", "snap", "--from-current")
	mustRun(t, "profile", "use", "snap", "--client", "all")
	if got := items("claude-code"); got != legacyCC {
		t.Fatalf("claude-code plan changed after --from-current:\n%s\n%s", legacyCC, got)
	}
	if got := items("opencode"); got != legacyOC {
		t.Fatalf("opencode plan changed after --from-current:\n%s\n%s", legacyOC, got)
	}

	// Policy of the legacy CLI while a profile applies: refuse and print the exact profile command.
	cases := [][]string{
		{"mcp", "expose", "gw-srv", "--client", "claude-code", "--mode", "hidden"},
		{"mcp", "disable", "direct-srv", "--client", "opencode"},
		{"mcp", "enable", "direct-srv", "--client", "opencode"},
	}
	for _, args := range cases {
		_, err := runErr(t, "", args...)
		if err == nil || !strings.Contains(err.Error(), "프로필 \"snap\"") || !strings.Contains(err.Error(), "forge profile add snap --mcp") {
			t.Errorf("%v: %v", args, err)
		}
	}
	// the legacy settings were not changed by the refused calls
	info := mustRun(t, "mcp", "show", "gw-srv")
	if !strings.Contains(info, "claude-code=gateway") {
		t.Fatalf("%s", info)
	}
	// A global disable still works and says how it interacts with profiles.
	if out := mustRun(t, "mcp", "disable", "direct-srv"); !strings.Contains(out, "프로필이 활성화된 클라이언트가 있습니다") {
		t.Fatalf("%s", out)
	}
	mustRun(t, "mcp", "enable", "direct-srv")
	// After unuse the legacy command works again.
	mustRun(t, "profile", "unuse", "--client", "all")
	mustRun(t, "mcp", "expose", "gw-srv", "--client", "claude-code", "--mode", "direct")
	// profile commands never touch the legacy settings
	if out := mustRun(t, "mcp", "show", "direct-srv"); !strings.Contains(out, "opencode=hidden") {
		t.Fatalf("legacy setting must survive: %s", out)
	}
}

func TestProfileCommandsDoNotCreateOrWriteOutsideTheRegistry(t *testing.T) {
	e := newSyncEnv(t)
	before := treeSig(t, e.proj) + treeSig(t, e.home)
	mustRun(t, "profile", "create", "x", "--from-current")
	mustRun(t, "profile", "use", "x", "--client", "all", "--project-dir", e.proj)
	mustRun(t, "profile", "current")
	mustRun(t, "profile", "list", "--json")
	if treeSig(t, e.proj)+treeSig(t, e.home) != before {
		t.Fatal("profile commands changed the project or HOME")
	}
	if b, _ := os.ReadFile(e.decoy); string(b) != "" && !strings.Contains(string(b), "decoy") {
		t.Fatal("decoy changed")
	}
}

func TestProfileReadCommandsNeedAnInitializedHome(t *testing.T) {
	forgeHome(t)
	for _, a := range [][]string{{"profile", "list"}, {"profile", "show", "x"}, {"profile", "current"}} {
		if _, err := runErr(t, "", a...); err == nil || !strings.Contains(err.Error(), "forge init") {
			t.Errorf("%v: %v", a, err)
		}
	}
}
