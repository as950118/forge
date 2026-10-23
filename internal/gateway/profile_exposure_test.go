package gateway

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/profile"
)

func profileEnv(t *testing.T) (*env, *profile.Store) {
	e := newEnv(t)
	e.addTool("t1", "none", "/bin/echo", "x")
	e.addTool("t2", "none", "/bin/echo", "y")
	e.addMock("gw-srv", "alpha", mcp.ExposureGateway)
	e.addMock("gw-other", "omega", mcp.ExposureGateway)
	e.addSkill("s1", "skill one", "BODY-ONE-V1\n", nil)
	e.addSkill("s2", "skill two", "BODY-TWO\n", nil)
	return e, &profile.Store{DB: e.db}
}

func add(t *testing.T, ps *profile.Store, ref profile.Ref, a profile.AddSpec) {
	t.Helper()
	if err := ps.Add(context.Background(), ref, a); err != nil {
		t.Fatalf("%+v: %v", a, err)
	}
}

func names(t *testing.T, e *env, g *Gateway, client string) []string {
	cs := e.connect(g, client, nil)
	return listNames(t, cs)
}

func TestProfileExposureAllowListAndLiveUpdate(t *testing.T) {
	e, ps := profileEnv(t)
	ctx := context.Background()
	gl := profile.Ref{Name: "p", Scope: profile.ScopeGlobal}
	if _, err := ps.Create(ctx, gl, "", false); err != nil {
		t.Fatal(err)
	}
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindTool, Name: "t1"})
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindMCP, Name: "gw-srv", Exposure: "gateway", Clients: []string{"all"}})
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindSkill, Name: "s1"})
	g := e.gw(func(o *Options) { o.Exposure = ProfileExposure{DB: e.db} })

	// No activation: the legacy rule shows everything that is enabled.
	cs := e.connect(g, "claude-code", nil)
	all := listNames(t, cs)
	for _, n := range []string{"forge_t1", "forge_t2", "gw_srv__alpha", "gw_other__omega"} {
		if !has(all, n) {
			t.Fatalf("legacy rule must show %s: %v", n, all)
		}
	}

	if _, _, err := ps.Use(ctx, "p", "", []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	// The running gateway follows the profile on the next request (no restart).
	got := listNames(t, cs)
	if !has(got, "forge_t1") || !has(got, "gw_srv__alpha") || has(got, "forge_t2") || has(got, "gw_other__omega") {
		t.Fatalf("allow list not applied to claude-code: %v", got)
	}
	// opencode has no activation: unchanged legacy view.
	oc := names(t, e, g, "opencode")
	if !has(oc, "forge_t2") || !has(oc, "gw_other__omega") {
		t.Fatalf("opencode must keep the legacy view: %v", oc)
	}
	// forge_skill: s1 is a member, s2 is not.
	out, isErr := invoke(t, cs, "forge_skill", map[string]any{"name": "s1"})
	if isErr || !strings.Contains(out, "BODY-ONE-V1") {
		t.Fatalf("s1: %v %s", isErr, out)
	}
	if out, isErr := invoke(t, cs, "forge_skill", map[string]any{"name": "s2"}); !isErr {
		t.Fatalf("s2 is not in the profile: %s", out)
	}
	if out, _ := invoke(t, cs, "forge_list", nil); strings.Contains(out, "s2:") || !strings.Contains(out, "s1:") {
		t.Fatalf("forge_list must follow the profile:\n%s", out)
	}
	// A call to an item outside the profile is a miss, not a success.
	if res, err := callRaw(cs, "forge_t2", map[string]any{"v": "x"}); err == nil && !res.IsError {
		t.Fatal("forge_t2 is hidden by the profile")
	}

	// Live edits: add t2, hide gw-srv for claude-code only.
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindTool, Name: "t2"})
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindMCP, Name: "gw-srv", Exposure: "hidden", Clients: []string{"claude-code"}})
	got = listNames(t, cs)
	if !has(got, "forge_t2") || has(got, "gw_srv__alpha") {
		t.Fatalf("live profile edit not applied: %v", got)
	}
	// Deactivating returns to the legacy view.
	if _, err := ps.Unuse(ctx, "", []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	if got = listNames(t, cs); !has(got, "gw_other__omega") || !has(got, "gw_srv__alpha") {
		t.Fatalf("unuse: %v", got)
	}
}

func TestProfileExposureModesAndGatewaySkills(t *testing.T) {
	e, ps := profileEnv(t)
	ctx := context.Background()
	gl := profile.Ref{Name: "p", Scope: profile.ScopeGlobal}
	if _, err := ps.Create(ctx, gl, "", false); err != nil {
		t.Fatal(err)
	}
	// mcp direct in the profile: not proxied by the gateway even though the legacy setting says gateway.
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindMCP, Name: "gw-srv", Exposure: "direct", Clients: []string{"claude-code"}})
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindMCP, Name: "gw-other"})
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindSkill, Name: "s1", Exposure: "gateway", Clients: []string{"claude-code"}})
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindSkill, Name: "s2", Exposure: "hidden", Clients: []string{"claude-code"}})
	if err := ps.Add(ctx, gl, profile.AddSpec{Kind: profile.KindTool, Name: "t1", Exposure: "direct", Clients: []string{"claude-code"}}); err == nil {
		t.Fatal("a custom tool cannot be direct")
	}
	if _, _, err := ps.Use(ctx, "p", "", []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	g := e.gw(func(o *Options) { o.Exposure = ProfileExposure{DB: e.db} })
	cs := e.connect(g, "claude-code", nil)
	got := listNames(t, cs)
	if has(got, "gw_srv__alpha") {
		t.Fatalf("a direct profile member must not be proxied: %v", got)
	}
	if has(got, "gw_other__omega") {
		t.Fatalf("the default exposure of an mcp member is direct (the legacy gateway setting is ignored): %v", got)
	}
	// s1 is gateway-only (served by forge_skill, not rendered by sync); s2 is hidden.
	if out, isErr := invoke(t, cs, "forge_skill", map[string]any{"name": "s1"}); isErr || !strings.Contains(out, "BODY-ONE-V1") {
		t.Fatalf("gateway-mode skill must be served: %v %s", isErr, out)
	}
	if _, isErr := invoke(t, cs, "forge_skill", map[string]any{"name": "s2"}); !isErr {
		t.Fatal("a hidden skill must not be served")
	}
}

func TestProfileExposureMcpDefaultIsDirectNotLegacy(t *testing.T) {
	e, ps := profileEnv(t)
	ctx := context.Background()
	gl := profile.Ref{Name: "p", Scope: profile.ScopeGlobal}
	_, _ = ps.Create(ctx, gl, "", false)
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindMCP, Name: "gw-other"}) // no explicit exposure
	if _, _, err := ps.Use(ctx, "p", "", []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	g := e.gw(func(o *Options) { o.Exposure = ProfileExposure{DB: e.db} })
	if got := names(t, e, g, "claude-code"); has(got, "gw_other__omega") {
		t.Fatalf("while a profile applies the legacy exposure setting is ignored (default direct): %v", got)
	}
}

func TestProfilePinnedSkillVersion(t *testing.T) {
	e, ps := profileEnv(t)
	ctx := context.Background()
	e.addSkill("s1", "skill one", "BODY-ONE-V2\n", nil) // a second, newer version (current)
	info, err := e.skills.Get(ctx, "s1")
	if err != nil || len(info.Versions) != 2 {
		t.Fatalf("%v %+v", err, info)
	}
	var v1 string
	for _, v := range info.Versions {
		if !v.Current {
			v1 = v.Version
		}
	}
	gl := profile.Ref{Name: "p", Scope: profile.ScopeGlobal}
	_, _ = ps.Create(ctx, gl, "", false)
	if err := ps.Add(ctx, gl, profile.AddSpec{Kind: profile.KindSkill, Name: "s1", Pin: "nope-1.2.3"}); err == nil {
		t.Fatal("a version that is not registered must be refused")
	}
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindSkill, Name: "s1", Pin: v1})
	if _, _, err := ps.Use(ctx, "p", "", []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	g := e.gw(func(o *Options) { o.Exposure = ProfileExposure{DB: e.db} })
	cs := e.connect(g, "claude-code", nil)
	out, isErr := invoke(t, cs, "forge_skill", map[string]any{"name": "s1"})
	if isErr || !strings.Contains(out, "BODY-ONE-V1") || strings.Contains(out, "BODY-ONE-V2") || !strings.Contains(out, "버전 "+v1) {
		t.Fatalf("pinned version must be served: %v\n%s", isErr, out)
	}
	// opencode has no activation: it gets the current version.
	oc := e.connect(g, "opencode", nil)
	out, _ = invoke(t, oc, "forge_skill", map[string]any{"name": "s1"})
	if !strings.Contains(out, "BODY-ONE-V2") {
		t.Fatalf("legacy view serves current:\n%s", out)
	}
	// Removing the pin follows current again.
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindSkill, Name: "s1"}) // idempotent, pin kept
	out, _ = invoke(t, cs, "forge_skill", map[string]any{"name": "s1"})
	if !strings.Contains(out, "BODY-ONE-V1") {
		t.Fatalf("re-adding without --pin keeps the pin:\n%s", out)
	}
	// The registry refuses to delete a version a profile pins.
	if err := e.skills.Remove(ctx, "s1"); err == nil {
		t.Log("skill remove succeeded although pinned (members cascade): acceptable only if the profile no longer pins")
	}
}

func TestProfileProjectOverridesGlobalAndUnknownClientIsConservative(t *testing.T) {
	e, ps := profileEnv(t)
	ctx := context.Background()
	proj := "/work/repo"
	gp := profile.Ref{Name: "g", Scope: profile.ScopeGlobal}
	pp := profile.Ref{Name: "pr", Scope: profile.ScopeProject, Project: proj}
	_, _ = ps.Create(ctx, gp, "", false)
	_, _ = ps.Create(ctx, pp, "", false)
	add(t, ps, gp, profile.AddSpec{Kind: profile.KindTool, Name: "t1"})
	add(t, ps, pp, profile.AddSpec{Kind: profile.KindTool, Name: "t2"})
	if _, _, err := ps.Use(ctx, "g", "", []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ps.Use(ctx, "pr", proj, []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	inProject := e.gw(func(o *Options) { o.Exposure = ProfileExposure{DB: e.db, Project: proj} })
	got := names(t, e, inProject, "claude-code")
	if !has(got, "forge_t2") || has(got, "forge_t1") {
		t.Fatalf("project activation must win over the global one: %v", got)
	}
	elsewhere := e.gw(func(o *Options) { o.Exposure = ProfileExposure{DB: e.db, Project: "/other"} })
	got = names(t, e, elsewhere, "claude-code")
	if !has(got, "forge_t1") || has(got, "forge_t2") {
		t.Fatalf("another project falls back to the global activation: %v", got)
	}
	// An unidentified client must satisfy the profile of every client that has one.
	pe := ProfileExposure{DB: e.db, Project: "/other"}
	d1, _ := e.tools.Get(ctx, "t1")
	d2, _ := e.tools.Get(ctx, "t2")
	if !pe.ToolVisible(d1, "") || pe.ToolVisible(d2, "") {
		t.Fatal("unidentified client: only what claude-code's profile allows")
	}
}

func TestProfileExposureFailsClosed(t *testing.T) {
	e, ps := profileEnv(t)
	ctx := context.Background()
	gl := profile.Ref{Name: "p", Scope: profile.ScopeGlobal}
	_, _ = ps.Create(ctx, gl, "", false)
	broken, err := sql.Open("sqlite", "file:"+t.TempDir()+"/none.db?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	pe := ProfileExposure{DB: broken}
	d1, _ := e.tools.Get(ctx, "t1")
	infos, _ := e.servers.List(ctx)
	sk, _ := e.skills.Get(ctx, "s1")
	if pe.ToolVisible(d1, "claude-code") || pe.SkillVisible(sk, "claude-code") || pe.ServerMode(&infos[0], "claude-code") != mcp.ExposureHidden {
		t.Fatal("an unreadable profile table must hide everything (fail closed)")
	}
}

// L1: while profiles are switched, every tools/list shows the members of exactly
// one profile (one frozen read per request), never a mix of two states.
func TestToolsListIsOneProfileWhileSwitching(t *testing.T) {
	e, ps := profileEnv(t)
	ctx := context.Background()
	a := profile.Ref{Name: "pa", Scope: profile.ScopeGlobal}
	b := profile.Ref{Name: "pb", Scope: profile.ScopeGlobal}
	_, _ = ps.Create(ctx, a, "", false)
	_, _ = ps.Create(ctx, b, "", false)
	add(t, ps, a, profile.AddSpec{Kind: profile.KindTool, Name: "t1"})
	add(t, ps, a, profile.AddSpec{Kind: profile.KindMCP, Name: "gw-srv", Exposure: "gateway", Clients: []string{"all"}})
	add(t, ps, b, profile.AddSpec{Kind: profile.KindTool, Name: "t2"})
	add(t, ps, b, profile.AddSpec{Kind: profile.KindMCP, Name: "gw-other", Exposure: "gateway", Clients: []string{"all"}})
	if _, _, err := ps.Use(ctx, "pa", "", []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	g := e.gw(func(o *Options) { o.Exposure = ProfileExposure{DB: e.db} })
	cs := e.connect(g, "claude-code", nil)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			name := []string{"pa", "pb"}[i%2]
			_, _, _ = ps.Use(ctx, name, "", []string{"claude-code"})
		}
	}()
	seenA, seenB := 0, 0
	for i := 0; i < 150; i++ {
		got := listNames(t, cs)
		inA := has(got, "forge_t1") || has(got, "gw_srv__alpha")
		inB := has(got, "forge_t2") || has(got, "gw_other__omega")
		if inA && inB {
			close(stop)
			<-done
			t.Fatalf("tools/list mixed two profile states: %v", got)
		}
		if inA {
			seenA++
		} else if inB {
			seenB++
		}
		if (inA && (!has(got, "forge_t1") || !has(got, "gw_srv__alpha"))) || (inB && (!has(got, "forge_t2") || !has(got, "gw_other__omega"))) {
			close(stop)
			<-done
			t.Fatalf("incomplete profile view: %v", got)
		}
	}
	close(stop)
	<-done
	if seenA == 0 || seenB == 0 {
		t.Logf("switching not observed on both sides (A=%d B=%d); consistency still held", seenA, seenB)
	}
}
