package profile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/skill"
	"github.com/heonjinjeong/forge/internal/store"
	"github.com/heonjinjeong/forge/internal/tool"
)

type fx struct {
	t    *testing.T
	home string
	db   *sql.DB
	ps   *Store
	ctx  context.Context
}

func newFx(t *testing.T) *fx {
	t.Helper()
	home := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
			if err == nil {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	if err := store.EnsureLayout(home); err != nil {
		t.Fatal(err)
	}
	conn, err := store.Open(context.Background(), store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &fx{t: t, home: home, db: conn, ps: &Store{DB: conn}, ctx: context.Background()}
}

func (f *fx) skill(name, body string) {
	f.t.Helper()
	d := filepath.Join(f.t.TempDir(), name)
	_ = os.MkdirAll(d, 0o755)
	_ = os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: d "+name+"\n---\n"+body), 0o644)
	p, err := skill.Prepare(f.ctx, d, skill.PrepareOptions{TmpDir: filepath.Join(f.home, "tmp")})
	if err != nil {
		f.t.Fatal(err)
	}
	defer p.Cleanup()
	if _, err := (&skill.Registry{DB: f.db, Home: f.home}).Add(f.ctx, p); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) mcp(name string) {
	f.t.Helper()
	if err := (&mcp.Registry{DB: f.db}).Add(f.ctx, mcp.Spec{Name: name, Transport: mcp.TransportStdio, Command: "/bin/echo"}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) tool(name string) {
	f.t.Helper()
	d := tool.Definition{Name: name, Kind: tool.KindCommand, Description: "t", Command: "/bin/echo", Policy: tool.Policy{Confirm: "none"}}
	if _, err := (&tool.Registry{DB: f.db, Home: f.home}).Add(f.ctx, d, false); err != nil {
		f.t.Fatal(err)
	}
}

func global(n string) Ref { return Ref{Name: n, Scope: ScopeGlobal} }

func TestNameValidation(t *testing.T) {
	f := newFx(t)
	for _, bad := range []string{"", " ", "a b", "a\nb", "-x", ".x", "a/b", "a\x1b[31m", "a\u202eb", strings.Repeat("a", 65), "한글", "a;rm"} {
		if _, err := f.ps.Create(f.ctx, global(bad), "", false); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	for _, good := range []string{"work", "a", "Team-1.2_x", strings.Repeat("a", 64)} {
		if _, err := f.ps.Create(f.ctx, global(good), "", false); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	if _, err := f.ps.Create(f.ctx, global("d"), "bad\x1b[2J description", false); err == nil {
		t.Error("control characters in the description must be refused")
	}
	if _, err := f.ps.Create(f.ctx, global("long"), strings.Repeat("x", 501), false); err == nil {
		t.Error("description limit")
	}
	if _, err := f.ps.Create(f.ctx, Ref{Name: "p", Scope: ScopeProject, Project: "relative/path"}, "", false); err == nil {
		t.Error("project path must be absolute")
	}
	if _, err := f.ps.Create(f.ctx, Ref{Name: "p", Scope: ScopeGlobal, Project: "/x"}, "", false); err == nil {
		t.Error("global profile with a project")
	}
}

func TestCRUDAndScopes(t *testing.T) {
	f := newFx(t)
	f.skill("s1", "b")
	f.tool("t1")
	f.mcp("m1")
	d, err := f.ps.Create(f.ctx, global("work"), "my work", false)
	if err != nil || d.Name != "work" || d.Scope != ScopeGlobal || d.Members != 0 {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := f.ps.Create(f.ctx, global("work"), "", false); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	// The same name may exist once per scope/project.
	proj := "/work/repo"
	if _, err := f.ps.Create(f.ctx, Ref{Name: "work", Scope: ScopeProject, Project: proj}, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ps.Create(f.ctx, Ref{Name: "work", Scope: ScopeProject, Project: "/work/other"}, "", false); err != nil {
		t.Fatal(err)
	}
	for _, a := range []AddSpec{{Kind: KindSkill, Name: "s1"}, {Kind: KindTool, Name: "t1"}, {Kind: KindMCP, Name: "m1", Exposure: "gateway", Clients: []string{"opencode"}}} {
		if err := f.ps.Add(f.ctx, global("work"), a); err != nil {
			t.Fatalf("%+v: %v", a, err)
		}
	}
	got, err := Get(f.ctx, f.db, global("work"))
	if err != nil || got.Members != 3 || got.Description != "my work" {
		t.Fatalf("%+v %v", got, err)
	}
	byName := map[string]Member{}
	for _, m := range got.MemberList {
		byName[m.Name] = m
	}
	if byName["m1"].ExposureFor("opencode") != ExposureGateway || byName["m1"].ExposureFor("claude-code") != ExposureDirect ||
		byName["t1"].ExposureFor("claude-code") != ExposureGateway || !byName["s1"].Enabled {
		t.Fatalf("%+v", byName)
	}
	// validation of members
	for _, bad := range []AddSpec{
		{Kind: KindSkill, Name: "missing"}, {Kind: "nope", Name: "x"}, {Kind: KindMCP, Name: "m1", Exposure: "sideways", Clients: []string{"opencode"}},
		{Kind: KindMCP, Name: "m1", Exposure: "gateway"}, {Kind: KindMCP, Name: "m1", Clients: []string{"opencode"}},
		{Kind: KindMCP, Name: "m1", Exposure: "gateway", Clients: []string{"emacs"}}, {Kind: KindTool, Name: "t1", Exposure: "direct", Clients: []string{"all"}},
		{Kind: KindMCP, Name: "m1", Pin: "1.0.0"},
	} {
		if err := f.ps.Add(f.ctx, global("work"), bad); err == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}
	if err := f.ps.Add(f.ctx, global("nope"), AddSpec{Kind: KindSkill, Name: "s1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// exposure=default removes the explicit row
	if err := f.ps.Add(f.ctx, global("work"), AddSpec{Kind: KindMCP, Name: "m1", Exposure: "direct", Clients: []string{"opencode"}}); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = f.db.QueryRow(`SELECT count(*) FROM profile_member_exposure`).Scan(&n)
	if n != 0 {
		t.Fatalf("default exposure must not keep a row, have %d", n)
	}
	if err := f.ps.Remove(f.ctx, global("work"), KindSkill, "s1"); err != nil {
		t.Fatal(err)
	}
	if err := f.ps.Remove(f.ctx, global("work"), KindSkill, "s1"); err == nil {
		t.Fatal("removing a missing member")
	}
	metas, _ := ListMeta(f.ctx, f.db)
	if len(metas) != 3 {
		t.Fatalf("%+v", metas)
	}
	if err := f.ps.Delete(f.ctx, global("work")); err != nil {
		t.Fatal(err)
	}
	if _, err := Get(f.ctx, f.db, global("work")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// the project profiles are untouched
	if _, err := Get(f.ctx, f.db, Ref{Name: "work", Scope: ScopeProject, Project: proj}); err != nil {
		t.Fatal(err)
	}
	// deleting a registry item removes it from profiles (FK cascade)
	_ = f.ps.Add(f.ctx, Ref{Name: "work", Scope: ScopeProject, Project: proj}, AddSpec{Kind: KindMCP, Name: "m1"})
	if err := (&mcp.Registry{DB: f.db}).Remove(f.ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	d2, _ := Get(f.ctx, f.db, Ref{Name: "work", Scope: ScopeProject, Project: proj})
	if d2.Members != 0 {
		t.Fatalf("%+v", d2)
	}
}

func TestPinMustBeARegisteredVersion(t *testing.T) {
	f := newFx(t)
	f.skill("s1", "v1")
	f.skill("s1", "v2")
	f.skill("s2", "other")
	info, _ := (&skill.Registry{DB: f.db, Home: f.home}).Get(f.ctx, "s1")
	var old, cur string
	for _, v := range info.Versions {
		if v.Current {
			cur = v.Version
		} else {
			old = v.Version
		}
	}
	_, _ = f.ps.Create(f.ctx, global("p"), "", false)
	s2info, _ := (&skill.Registry{DB: f.db, Home: f.home}).Get(f.ctx, "s2")
	for _, bad := range []string{"", "9.9.9", "../../x", old + " ", s2info.CurrentVersion} {
		if bad == "" {
			continue
		}
		if err := f.ps.Add(f.ctx, global("p"), AddSpec{Kind: KindSkill, Name: "s1", Pin: bad}); err == nil {
			t.Errorf("pin %q must be refused (another skill's version / unknown)", bad)
		}
	}
	if err := f.ps.Add(f.ctx, global("p"), AddSpec{Kind: KindSkill, Name: "s1", Pin: old}); err != nil {
		t.Fatal(err)
	}
	d, _ := Get(f.ctx, f.db, global("p"))
	if d.MemberList[0].PinnedVersion != old || cur == old {
		t.Fatalf("%+v", d.MemberList)
	}
	// unpinned members follow current
	f.skill("s3", "x")
	_ = f.ps.Add(f.ctx, global("p"), AddSpec{Kind: KindSkill, Name: "s3"})
	r, _ := resolveFor(f, "p")
	if r.Pins["s1"] != old || r.Pins["s3"] != "" {
		t.Fatalf("%+v", r.Pins)
	}
}

func resolveFor(f *fx, name string) (*Resolved, error) {
	if _, _, err := f.ps.Use(f.ctx, name, "", []string{"claude-code"}); err != nil {
		return nil, err
	}
	return Resolve(f.ctx, f.db, "claude-code", "")
}

func TestPrecedenceProjectGlobalLegacy(t *testing.T) {
	f := newFx(t)
	f.mcp("m1")
	f.mcp("m2")
	proj := "/work/repo"
	_, _ = f.ps.Create(f.ctx, global("g"), "", false)
	_, _ = f.ps.Create(f.ctx, Ref{Name: "pr", Scope: ScopeProject, Project: proj}, "", false)
	_ = f.ps.Add(f.ctx, global("g"), AddSpec{Kind: KindMCP, Name: "m1"})
	_ = f.ps.Add(f.ctx, Ref{Name: "pr", Scope: ScopeProject, Project: proj}, AddSpec{Kind: KindMCP, Name: "m2"})

	r, err := Resolve(f.ctx, f.db, "claude-code", proj)
	if err != nil || r.Rule != RuleLegacy || r.Active() {
		t.Fatalf("no activation = legacy: %+v %v", r, err)
	}
	if _, _, err := f.ps.Use(f.ctx, "g", "", []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	if r, _ = Resolve(f.ctx, f.db, "claude-code", proj); r.Rule != RuleGlobal || r.Profile.Name != "g" || len(r.MCP) != 1 {
		t.Fatalf("%+v", r)
	}
	if r, _ = Resolve(f.ctx, f.db, "opencode", proj); r.Rule != RuleLegacy {
		t.Fatalf("activation is per client: %+v", r)
	}
	if _, _, err := f.ps.Use(f.ctx, "pr", proj, []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	if r, _ = Resolve(f.ctx, f.db, "claude-code", proj); r.Rule != RuleProject || r.Profile.Name != "pr" || r.MCP["m2"] == "" || r.MCP["m1"] != "" {
		t.Fatalf("project beats global: %+v", r)
	}
	if r, _ = Resolve(f.ctx, f.db, "claude-code", "/work/elsewhere"); r.Rule != RuleGlobal {
		t.Fatalf("another project uses the global one: %+v", r)
	}
	if r, _ = Resolve(f.ctx, f.db, "claude-code", ""); r.Rule != RuleGlobal {
		t.Fatalf("no project: global only: %+v", r)
	}
	if !strings.Contains(r.Describe("claude-code"), "허용 목록") || !strings.Contains((&Resolved{Rule: RuleLegacy}).Describe("x"), "레거시") {
		t.Fatal("Describe must state the rule")
	}
	// unuse project -> back to global; unuse global -> legacy
	if got, err := f.ps.Unuse(f.ctx, proj, []string{"claude-code", "opencode"}); err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if r, _ = Resolve(f.ctx, f.db, "claude-code", proj); r.Rule != RuleGlobal {
		t.Fatalf("%+v", r)
	}
	if got, _ := f.ps.Unuse(f.ctx, "", []string{"all"}); len(got) != 1 {
		t.Fatalf("%v", got)
	}
	if r, _ = Resolve(f.ctx, f.db, "claude-code", proj); r.Rule != RuleLegacy {
		t.Fatalf("%+v", r)
	}
}

func TestUseRules(t *testing.T) {
	f := newFx(t)
	proj := "/work/repo"
	_, _ = f.ps.Create(f.ctx, global("g"), "", false)
	_, _ = f.ps.Create(f.ctx, Ref{Name: "pr", Scope: ScopeProject, Project: proj}, "", false)
	if _, _, err := f.ps.Use(f.ctx, "pr", "", []string{"claude-code"}); err == nil || !strings.Contains(err.Error(), "--project-dir") {
		t.Fatalf("a project profile cannot be activated globally: %v", err)
	}
	if _, _, err := f.ps.Use(f.ctx, "pr", "/work/other", []string{"claude-code"}); err == nil {
		t.Fatal("a project profile can only be used in its own project")
	}
	if _, _, err := f.ps.Use(f.ctx, "g", proj, []string{"emacs"}); err == nil {
		t.Fatal("unknown client")
	}
	if _, _, err := f.ps.Use(f.ctx, "g", proj, nil); err == nil {
		t.Fatal("client is required")
	}
	if _, _, err := f.ps.Use(f.ctx, "nope", proj, []string{"all"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// a global profile can be activated for one project; "all" covers every client
	m, cl, err := f.ps.Use(f.ctx, "g", proj, []string{"all"})
	if err != nil || m.Name != "g" || len(cl) != 2 {
		t.Fatalf("%+v %v %v", m, cl, err)
	}
	// re-activation replaces, it does not duplicate
	if _, _, err := f.ps.Use(f.ctx, "g", proj, []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = f.db.QueryRow(`SELECT count(*) FROM profile_activations`).Scan(&n)
	if n != 2 {
		t.Fatalf("activations: %d", n)
	}
	// delete is refused while active, allowed after unuse
	if err := f.ps.Delete(f.ctx, global("g")); err == nil || !strings.Contains(err.Error(), "unuse") {
		t.Fatalf("%v", err)
	}
	if _, err := f.ps.Unuse(f.ctx, proj, []string{"all"}); err != nil {
		t.Fatal(err)
	}
	if err := f.ps.Delete(f.ctx, global("g")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ps.Unuse(f.ctx, "", nil); err == nil {
		t.Fatal("client is required")
	}
	if got, err := f.ps.Unuse(f.ctx, "/never/seen", []string{"all"}); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestFromCurrentSnapshotsEnabledItemsAndLegacyExposure(t *testing.T) {
	f := newFx(t)
	f.skill("on-skill", "x")
	f.skill("off-skill", "y")
	f.tool("on-tool")
	f.tool("off-tool")
	f.mcp("on-mcp")
	f.mcp("gw-mcp")
	f.mcp("hid-mcp")
	f.mcp("off-mcp")
	sreg := &skill.Registry{DB: f.db, Home: f.home}
	treg := &tool.Registry{DB: f.db, Home: f.home}
	mreg := &mcp.Registry{DB: f.db}
	_ = sreg.SetEnabled(f.ctx, "off-skill", false)
	_ = treg.SetEnabled(f.ctx, "off-tool", false)
	_ = mreg.SetEnabled(f.ctx, "off-mcp", false)
	_ = mreg.SetExposure(f.ctx, "gw-mcp", "claude-code", mcp.ExposureGateway)
	_ = mreg.SetExposure(f.ctx, "hid-mcp", "opencode", mcp.ExposureHidden)
	d, err := f.ps.Create(f.ctx, global("snap"), "", true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Member{}
	for _, m := range d.MemberList {
		got[m.Kind+":"+m.Name] = m
	}
	for _, want := range []string{"skill:on-skill", "tool:on-tool", "mcp:on-mcp", "mcp:gw-mcp", "mcp:hid-mcp"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s: %v", want, got)
		}
	}
	for _, not := range []string{"skill:off-skill", "tool:off-tool", "mcp:off-mcp"} {
		if _, ok := got[not]; ok {
			t.Errorf("disabled item copied: %s", not)
		}
	}
	if got["mcp:gw-mcp"].ExposureFor("claude-code") != ExposureGateway || got["mcp:gw-mcp"].ExposureFor("opencode") != ExposureDirect ||
		got["mcp:hid-mcp"].ExposureFor("opencode") != ExposureHidden || got["mcp:on-mcp"].ExposureFor("claude-code") != ExposureDirect {
		t.Fatalf("legacy exposure not copied: %+v", got)
	}
	// the legacy settings are left alone
	info, _ := mreg.Get(f.ctx, "gw-mcp")
	if info.Exposure["claude-code"] != mcp.ExposureGateway {
		t.Fatal("legacy settings must stay")
	}
	// an empty registry gives an empty profile, no error
	g := newFx(t)
	if d, err := g.ps.Create(g.ctx, global("empty"), "", true); err != nil || d.Members != 0 {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestConcurrentWritersAreSafe(t *testing.T) {
	f := newFx(t)
	for i := 0; i < 6; i++ {
		f.mcp(fmt.Sprintf("m%d", i))
	}
	_, _ = f.ps.Create(f.ctx, global("p"), "", false)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 6; i++ {
		for j := 0; j < 3; j++ {
			wg.Add(1)
			go func(i, j int) {
				defer wg.Done()
				if err := f.ps.Add(f.ctx, global("p"), AddSpec{Kind: KindMCP, Name: fmt.Sprintf("m%d", i), Exposure: "gateway", Clients: []string{"claude-code"}}); err != nil {
					errs <- err
				}
			}(i, j)
		}
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if _, err := f.ps.Create(f.ctx, global(fmt.Sprintf("c%d", i%3)), "", false); err != nil && !errors.Is(err, ErrExists) {
				errs <- err
			}
		}(i)
		go func() {
			defer wg.Done()
			if _, _, err := f.ps.Use(f.ctx, "p", "", []string{"all"}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	d, err := Get(f.ctx, f.db, global("p"))
	if err != nil || d.Members != 6 {
		t.Fatalf("%+v %v", d, err)
	}
	var n int
	_ = f.db.QueryRow(`SELECT count(*) FROM profiles`).Scan(&n)
	if n != 4 {
		t.Fatalf("profiles: %d (unique names must hold under concurrency)", n)
	}
	_ = f.db.QueryRow(`SELECT count(*) FROM profile_activations`).Scan(&n)
	if n != 2 {
		t.Fatalf("activations: %d", n)
	}
}

func TestBrokenMembersAreFlaggedNotTrusted(t *testing.T) {
	f := newFx(t)
	f.skill("s1", "a")
	_, _ = f.ps.Create(f.ctx, global("p"), "", false)
	_ = f.ps.Add(f.ctx, global("p"), AddSpec{Kind: KindSkill, Name: "s1"})
	// Forge the database: a pinned version of another skill / a dangling reference
	// (foreign keys off on this handle).
	raw, err := sql.Open("sqlite", "file:"+store.DBPath(f.home)+"?_pragma=foreign_keys(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	f.skill("s2", "b")
	if _, err := raw.Exec(`UPDATE profile_members SET pinned_version_id=(SELECT v.id FROM skill_versions v JOIN skills s ON s.id=v.skill_id WHERE s.name='s2')`); err != nil {
		t.Fatal(err)
	}
	d, _ := Get(f.ctx, f.db, global("p"))
	if d.MemberList[0].Broken == "" {
		t.Fatalf("%+v", d.MemberList[0])
	}
	if _, err := raw.Exec(`UPDATE profile_members SET skill_id=9999, pinned_version_id=NULL`); err != nil {
		t.Fatal(err)
	}
	d, _ = Get(f.ctx, f.db, global("p"))
	if d.MemberList[0].Broken == "" {
		t.Fatalf("%+v", d.MemberList[0])
	}
	_, _, _ = f.ps.Use(f.ctx, "p", "", []string{"claude-code"})
	r, _ := Resolve(f.ctx, f.db, "claude-code", "")
	if len(r.Skills) != 0 {
		t.Fatalf("broken members must not expose anything: %+v", r.Skills)
	}
}

func TestProjectRoot(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(base, "repo")
	sub := filepath.Join(repo, "a", "b")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	_ = os.MkdirAll(sub, 0o755)
	if got, err := ProjectRoot(sub); err != nil || got != repo {
		t.Fatalf("%q %v", got, err)
	}
	plain := filepath.Join(base, "plain")
	_ = os.MkdirAll(plain, 0o755)
	if got, _ := ProjectRoot(plain); got != plain {
		t.Fatalf("%q", got)
	}
	link := filepath.Join(base, "link")
	_ = os.Symlink(sub, link)
	if got, _ := ProjectRoot(link); got != repo {
		t.Fatalf("symlinks are resolved: %q", got)
	}
	if _, err := ProjectRoot(filepath.Join(base, "missing")); err == nil {
		t.Fatal("missing directory")
	}
	file := filepath.Join(base, "f")
	_ = os.WriteFile(file, nil, 0o644)
	if _, err := ProjectRoot(file); err == nil {
		t.Fatal("a file is not a project directory")
	}
}

func TestJSONShape(t *testing.T) {
	f := newFx(t)
	f.mcp("m1")
	_, _ = f.ps.Create(f.ctx, global("p"), "", false)
	_ = f.ps.Add(f.ctx, global("p"), AddSpec{Kind: KindMCP, Name: "m1", Exposure: "hidden", Clients: []string{"all"}})
	d, _ := Get(f.ctx, f.db, global("p"))
	b, _ := json.Marshal(d)
	for _, k := range []string{`"name":"p"`, `"scope":"global"`, `"member_list"`, `"kind":"mcp"`, `"exposure":{"claude-code":"hidden","opencode":"hidden"}`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("JSON misses %s: %s", k, b)
		}
	}
}

// Removing a skill from the registry removes it from every profile (the schema
// cascades whole-skill deletes; ADR-F0001 D3). The profile then simply has fewer members.
func TestRemovingASkillRemovesItsMemberships(t *testing.T) {
	f := newFx(t)
	f.skill("s1", "a")
	_, _ = f.ps.Create(f.ctx, global("p"), "", false)
	info, _ := (&skill.Registry{DB: f.db, Home: f.home}).Get(f.ctx, "s1")
	if err := f.ps.Add(f.ctx, global("p"), AddSpec{Kind: KindSkill, Name: "s1", Pin: info.CurrentVersion}); err != nil {
		t.Fatal(err)
	}
	if err := (&skill.Registry{DB: f.db, Home: f.home}).Remove(f.ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if d, _ := Get(f.ctx, f.db, global("p")); d.Members != 0 {
		t.Fatalf("%+v", d.MemberList)
	}
}
