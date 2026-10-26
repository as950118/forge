package adapter_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/adapter/claudecode"
	"github.com/heonjinjeong/forge/internal/adapter/opencode"
	"github.com/heonjinjeong/forge/internal/mcp"
)

// The security rules live once in internal/adapter (planner.go, skills.go,
// mcpfile.go, docfile.go). These tests run the SAME scenarios against every
// target, so a rule that a target stops calling (or a common rule that is
// weakened) fails for all targets. A new target is added to targets and must pass
// unchanged (docs/adapter.md, new target checklist).
type conformTarget struct {
	name    string
	adapter func(forgeHome string) adapter.Adapter
	noHooks bool // claude-code would open settings.json in user scope; opencode rejects the flag
}

var conformTargets = []conformTarget{
	{"claude-code", func(h string) adapter.Adapter { return claudecode.New(h) }, true},
	{"opencode", func(h string) adapter.Adapter { return opencode.New(h) }, false},
}

func conformDir(t *testing.T, parts ...string) string {
	t.Helper()
	d := filepath.Join(parts...)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// conformBase returns an isolated, symlink-free base directory with HOME and the
// XDG variables pinned (nothing outside the base is touched).
func conformBase(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", conformDir(t, base, "home"))
	return base
}

// conformSkill writes a registry snapshot directory and returns its SkillInput.
func conformSkill(t *testing.T, forgeHome, name string) adapter.SkillInput {
	return conformSkillV(t, forgeHome, name, "1.0.0")
}

func conformSkillV(t *testing.T, forgeHome, name, version string) adapter.SkillInput {
	t.Helper()
	dir := conformDir(t, forgeHome, "skills", name+"@"+version)
	md := "---\nname: " + name + "\ndescription: " + name + " skill " + version + ".\n---\n# " + name + " " + version + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	// A guarded directory cannot be hashed: its input then carries no hash (the
	// guard tests expect the plan to refuse it before the hash matters).
	h, _, err := adapter.TreeHash(dir, false)
	if err != nil {
		h = ""
	}
	return adapter.SkillInput{Name: name, Version: version, StoreDir: dir, Hash: h}
}

// conformProject is an isolated project scope: forge home, project (a git root)
// and HOME below one base directory.
type conformProject struct {
	forge, proj, home string
	ad                adapter.Adapter
	tg                conformTarget
}

func newConformProject(t *testing.T, tg conformTarget) *conformProject {
	t.Helper()
	base := conformBase(t)
	c := &conformProject{forge: conformDir(t, base, "forge"), proj: conformDir(t, base, "proj"), home: filepath.Join(base, "home"), tg: tg}
	conformDir(t, c.proj, ".git")
	c.ad = tg.adapter(c.forge)
	return c
}

func (c *conformProject) req(mods ...func(*adapter.Request)) adapter.Request {
	r := adapter.Request{Scope: adapter.ScopeProject, ProjectDir: c.proj, Home: c.home, ForgeBin: "forge", BackupDir: filepath.Join(c.forge, "backups")}
	for _, m := range mods {
		m(&r)
	}
	return r
}

func (c *conformProject) plan(t *testing.T, skills []adapter.SkillInput, mods ...func(*adapter.Request)) (*adapter.Plan, error) {
	t.Helper()
	return c.ad.Plan(context.Background(), adapter.Inputs{Client: c.tg.name, Skills: skills}, c.req(mods...))
}

func (c *conformProject) apply(t *testing.T, p *adapter.Plan, crash func(string) bool) {
	t.Helper()
	if _, err := adapter.Apply(context.Background(), c.ad, p, adapter.ApplyOptions{BackupDir: filepath.Join(c.forge, "backups"), Crash: crash}); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func conformItem(t *testing.T, p *adapter.Plan, kind, name string) adapter.Item {
	t.Helper()
	for _, it := range p.Items {
		if it.Kind == kind && it.Name == name {
			return it
		}
	}
	t.Fatalf("no %s item %q in plan: %+v", kind, name, p.Items)
	return adapter.Item{}
}

// A skill directory that GuardPath refuses is a conflict that names the security
// policy, and AllowedPath refuses the same path (HOME is made an OpenCode data
// directory through XDG_DATA_HOME, so everything below it is guarded).
func TestSharedGuardedSkillTarget(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			base := conformBase(t)
			home := conformDir(t, base, "x", "opencode")
			t.Setenv("HOME", home)
			t.Setenv("XDG_DATA_HOME", filepath.Join(base, "x"))
			forge := conformDir(t, base, "forge")
			in := adapter.Inputs{Client: tg.name, Skills: []adapter.SkillInput{conformSkill(t, forge, "guarded-skill")}}
			ad := tg.adapter(forge)
			p, err := ad.Plan(context.Background(), in, adapter.Request{Scope: adapter.ScopeUser, Home: home, ForgeBin: "forge", NoHooks: tg.noHooks})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			it := conformItem(t, p, adapter.KindSkill, "guarded-skill")
			if it.Action != adapter.ActionConflict || !strings.Contains(it.Reason, "보안 정책: 대상 경로") {
				t.Fatalf("want a security-policy conflict, got %s %q", it.Action, it.Reason)
			}
			for _, op := range p.Ops {
				if op.Path == it.Path {
					t.Fatalf("guarded path is in the operations: %+v", op)
				}
			}
			if _, err := ad.AllowedPath(adapter.ScopeUser, home, it.Path); err == nil {
				t.Fatalf("AllowedPath accepted a guarded path: %s", it.Path)
			}
		})
	}
}

// A registry snapshot below a guarded directory is not read (reason names it).
func TestSharedGuardedStoreDir(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			base := conformBase(t)
			forge := conformDir(t, base, "x", "opencode")
			t.Setenv("XDG_DATA_HOME", filepath.Join(base, "x"))
			proj := conformDir(t, base, "proj")
			conformDir(t, proj, ".git")
			in := adapter.Inputs{Client: tg.name, Skills: []adapter.SkillInput{conformSkill(t, forge, "store-skill")}}
			p, err := tg.adapter(forge).Plan(context.Background(), in, adapter.Request{Scope: adapter.ScopeProject, ProjectDir: proj, Home: filepath.Join(base, "home"), ForgeBin: "forge"})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			it := conformItem(t, p, adapter.KindSkill, "store-skill")
			if it.Action != adapter.ActionConflict || !strings.Contains(it.Reason, "레지스트리 스냅샷 경로를 읽을 수 없습니다") {
				t.Fatalf("want a snapshot-path conflict, got %s %q", it.Action, it.Reason)
			}
		})
	}
}

// RenderServer re-validates every registry server right before it is written
// (the database can be edited directly): each refusal is a conflict, nothing is
// rendered.
func TestSharedRenderRefusals(t *testing.T) {
	envVal := func(key, ref string) mcp.Value {
		return mcp.Value{Slot: mcp.SlotEnv, Key: key, RefType: mcp.RefEnv, Ref: ref, Display: "${" + ref + "}"}
	}
	lit := func(key, v string) mcp.Value {
		return mcp.Value{Slot: mcp.SlotEnv, Key: key, RefType: mcp.RefLiteral, Literal: v, Display: v}
	}
	stdio := func(name, cmd string, vals ...mcp.Value) *mcp.Info {
		return &mcp.Info{Name: name, Transport: mcp.TransportStdio, Command: cmd, Values: vals, Enabled: true}
	}
	cases := []struct {
		srv  *mcp.Info
		want string // substring of the reason; "=..." means the whole reason
	}{
		{&mcp.Info{Name: "bad-url", Transport: mcp.TransportHTTP, URL: "ftp://example.com/x", Enabled: true}, "재검증 실패"},
		{stdio("bad name", "node"), "=서버 이름이 올바르지 않습니다"},
		{stdio("bad-ref", "node", envVal("TOKEN", "BAD NAME")), "환경변수 참조 이름이 올바르지 않습니다"},
		{stdio("bad-key", "node", lit("BAD NAME", "x")), "환경변수/헤더 이름이 올바르지 않거나"},
		{stdio("ctrl-char", "node\x07"), "제어/비가시 문자"},
		{stdio("subst-literal", "node", lit("REGION", "${HOME}")), "치환 문법"},
		{&mcp.Info{Name: "keychain-ref", Transport: mcp.TransportStdio, Command: "node", Enabled: true,
			Values: []mcp.Value{{Slot: mcp.SlotEnv, Key: "TOKEN", RefType: mcp.RefKeychain, Ref: "x", Display: "keychain:x"}}}, "keychain"},
	}
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			base := conformBase(t)
			forge := conformDir(t, base, "forge")
			proj := conformDir(t, base, "proj")
			conformDir(t, proj, ".git")
			in := adapter.Inputs{Client: tg.name}
			for _, c := range cases {
				in.Servers = append(in.Servers, c.srv)
			}
			p, err := tg.adapter(forge).Plan(context.Background(), in, adapter.Request{Scope: adapter.ScopeProject, ProjectDir: proj, Home: filepath.Join(base, "home"), ForgeBin: "forge"})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			for _, c := range cases {
				it := conformItem(t, p, adapter.KindMCP, c.srv.Name)
				ok := strings.Contains(it.Reason, c.want)
				if strings.HasPrefix(c.want, "=") {
					ok = it.Reason == c.want[1:]
				}
				if it.Action == adapter.ActionAdd || it.Action == adapter.ActionUpdate || !ok {
					t.Errorf("%s: want a refusal containing %q, got %s %q", c.srv.Name, c.want, it.Action, it.Reason)
				}
			}
			for _, op := range p.Ops {
				for _, c := range cases {
					if strings.Contains(string(op.Data), `"`+c.srv.Name+`"`) {
						t.Errorf("refused server %q was written to %s", c.srv.Name, op.Path)
					}
				}
			}
		})
	}
}

// A linked .claude/.opencode directory is refused: writing through it would
// change files elsewhere.
func TestSharedSkillBehindSymlinkIsRefused(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			sk := conformSkill(t, c.forge, "linked-skill")
			p, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			target := conformItem(t, p, adapter.KindSkill, "linked-skill").Path
			clientDir := filepath.Dir(filepath.Dir(target)) // <proj>/.claude or <proj>/.opencode
			elsewhere := conformDir(t, filepath.Dir(c.proj), "elsewhere")
			if err := os.Symlink(elsewhere, clientDir); err != nil {
				t.Fatal(err)
			}
			p, err = c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			it := conformItem(t, p, adapter.KindSkill, "linked-skill")
			if it.Action != adapter.ActionConflict || !strings.Contains(it.Reason, "심볼릭 링크") {
				t.Fatalf("want a symlink conflict, got %s %q", it.Action, it.Reason)
			}
		})
	}
}

// The state file is never read or written through a linked directory.
func TestSharedStateFileBehindSymlinkIsRefused(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			p, err := c.plan(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			forgeDir := filepath.Dir(p.StatePath) // <proj>/.forge
			elsewhere := conformDir(t, filepath.Dir(c.proj), "elsewhere")
			if err := os.Symlink(elsewhere, forgeDir); err != nil {
				t.Fatal(err)
			}
			if _, err := c.plan(t, nil); err == nil || !strings.Contains(err.Error(), "상태 파일 경로가 안전하지 않습니다") {
				t.Fatalf("want a refused state path, got %v", err)
			}
		})
	}
}

// A registry snapshot whose content differs from the registered hash is never
// copied.
func TestSharedTamperedSnapshotIsRefused(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			sk := conformSkill(t, c.forge, "tampered-skill")
			if err := os.WriteFile(filepath.Join(sk.StoreDir, "SKILL.md"), []byte("tampered\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			p, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			it := conformItem(t, p, adapter.KindSkill, "tampered-skill")
			if it.Action != adapter.ActionConflict || !strings.Contains(it.Reason, "변조") {
				t.Fatalf("want a tamper conflict, got %s %q", it.Action, it.Reason)
			}
			for _, op := range p.Ops {
				if op.Path == it.Path {
					t.Fatalf("a tampered snapshot produced an operation: %s", op.Path)
				}
			}
		})
	}
}

// A state entry whose directory lost its management marker is a user directory
// now: not removed, not even with --force-managed (only the state entry goes).
func TestSharedStaleSkillWithoutMarkerIsKept(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			sk := conformSkill(t, c.forge, "marker-skill")
			p, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			c.apply(t, p, nil)
			target := conformItem(t, p, adapter.KindSkill, "marker-skill").Path
			if err := os.Remove(filepath.Join(target, adapter.MarkerName)); err != nil {
				t.Fatal(err)
			}
			p, err = c.plan(t, nil, func(r *adapter.Request) { r.ForceManaged = true })
			if err != nil {
				t.Fatal(err)
			}
			if it := conformItem(t, p, adapter.KindSkill, "marker-skill"); it.Action != adapter.ActionSkip {
				t.Fatalf("want skip, got %s %q", it.Action, it.Reason)
			}
			for _, op := range p.Ops {
				if op.Path == target {
					t.Fatalf("the user directory is in the operations: %+v", op)
				}
			}
		})
	}
}

// AllowedPath is a whole-list check: the state file, the target's config files
// and skill directories one level below the skills directory - nothing else.
func TestSharedAllowedPathList(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			sk := conformSkill(t, c.forge, "allowed-skill")
			p, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			target := conformItem(t, p, adapter.KindSkill, "allowed-skill").Path
			skillsDir := filepath.Dir(target)
			for _, ok := range []string{target, p.StatePath} {
				if _, err := c.ad.AllowedPath(adapter.ScopeProject, p.Root, ok); err != nil {
					t.Errorf("AllowedPath(%s): %v", ok, err)
				}
			}
			for _, bad := range []string{filepath.Join(skillsDir, "Bad_Name"), filepath.Join(target, "nested"), filepath.Join(p.Root, "evil.txt")} {
				if _, err := c.ad.AllowedPath(adapter.ScopeProject, p.Root, bad); err == nil {
					t.Errorf("AllowedPath accepted %s", bad)
				}
			}
			if _, err := c.ad.AllowedPath(adapter.Scope("other"), p.Root, target); err == nil {
				t.Error("AllowedPath accepted an unknown scope")
			}
		})
	}
}

// A sync killed after it replaced a skill but before it wrote the state file
// leaves state=old hash, disk=new content, journal=new hash. The journal vouches
// for the new content, so disabling the skill afterwards plans a removal instead
// of a hash conflict (a forged journal cannot do this: see the journal-forgery
// tests of each target).
func TestSharedJournalVouchesForCrashedUpdate(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			run := func(crashAt int) (points []string, vouched bool) {
				c := newConformProject(t, tg)
				v1 := conformSkillV(t, c.forge, "crash-skill", "1.0.0")
				v2 := conformSkillV(t, c.forge, "crash-skill", "2.0.0")
				p1, err := c.plan(t, []adapter.SkillInput{v1})
				if err != nil {
					t.Fatal(err)
				}
				c.apply(t, p1, nil)
				target := conformItem(t, p1, adapter.KindSkill, "crash-skill").Path
				p2, err := c.plan(t, []adapter.SkillInput{v2})
				if err != nil {
					t.Fatal(err)
				}
				n := 0
				func() {
					defer func() {
						if r := recover(); r != nil {
							if _, ok := r.(adapter.SimulatedCrash); !ok {
								panic(r)
							}
						}
					}()
					adapter.Apply(context.Background(), c.ad, p2, adapter.ApplyOptions{BackupDir: filepath.Join(c.forge, "backups"),
						Crash: func(pt string) bool { points = append(points, pt); n++; return n-1 == crashAt }})
				}()
				want, _, err := adapter.TreeHash(v2.StoreDir, false)
				if err != nil {
					t.Fatal(err)
				}
				disk, _, derr := adapter.TreeHash(target, true)
				var st struct{ Items []adapter.StateItem }
				raw, rerr := os.ReadFile(p1.StatePath)
				if rerr != nil || json.Unmarshal(raw, &st) != nil {
					t.Fatalf("state file: %v", rerr)
				}
				stateHash := ""
				for _, it := range st.Items {
					if it.Kind == adapter.KindSkill && it.Name == "crash-skill" {
						stateHash = it.Hash
					}
				}
				p3, err := c.plan(t, nil)
				if err != nil {
					t.Fatal(err)
				}
				if derr != nil || disk != want || stateHash == want {
					return points, false
				}
				it := conformItem(t, p3, adapter.KindSkill, "crash-skill")
				if it.Action != adapter.ActionRemove {
					t.Errorf("crash at step %d: new content is journaled but the plan is %s %q", crashAt, it.Action, it.Reason)
				}
				return points, true
			}
			points, _ := run(-1)
			any := false
			for k := range points {
				t.Run(fmt.Sprintf("crash%02d", k), func(t *testing.T) {
					if _, v := run(k); v {
						any = true
					}
				})
			}
			if !any {
				t.Fatal("no crash point left the new skill on disk with the old state file: the scenario no longer tests the journal")
			}
		})
	}
}

// opencode reads the claude-code state file only through a safe path: a linked
// state file is not followed, the duplicate check is skipped with a note.
func TestOpenCodeForeignStateIsGuarded(t *testing.T) {
	base := conformBase(t)
	forge := conformDir(t, base, "forge")
	home := filepath.Join(base, "home")
	stateDir := conformDir(t, forge, "sync-state")
	other := filepath.Join(conformDir(t, base, "elsewhere"), "state.json")
	if err := os.WriteFile(other, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	foreign := claudecode.New(forge).StatePath(adapter.ScopeUser, home)
	if filepath.Dir(foreign) != stateDir {
		t.Fatalf("unexpected claude-code user state path %s", foreign)
	}
	if err := os.Symlink(other, foreign); err != nil {
		t.Fatal(err)
	}
	p, err := opencode.New(forge).Plan(context.Background(), adapter.Inputs{Client: "opencode"},
		adapter.Request{Scope: adapter.ScopeUser, Home: home, ForgeBin: "forge"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range p.Notes {
		if strings.Contains(n, "claude-code 상태 파일을 확인하지 못해") && strings.Contains(n, "경로가 안전하지 않음") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a note about the unsafe claude-code state path, got %q", p.Notes)
	}
}

func conformServer(name, command string) *mcp.Info {
	return &mcp.Info{Name: name, Transport: mcp.TransportStdio, Command: command, Enabled: true}
}

func (c *conformProject) planServers(t *testing.T, servers []*mcp.Info, mods ...func(*adapter.Request)) *adapter.Plan {
	t.Helper()
	p, err := c.ad.Plan(context.Background(), adapter.Inputs{Client: c.tg.name, Servers: servers}, c.req(mods...))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

func opFor(p *adapter.Plan, path string) bool {
	for _, op := range p.Ops {
		if op.Path == path {
			return true
		}
	}
	return false
}

// A managed server that was edited by hand and is no longer wanted is a
// conflict (overridable), not a removal; only --force-managed removes it.
func TestSharedStaleServerEditedByHandIsConflict(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			srv := []*mcp.Info{conformServer("stale-srv", "node-orig")}
			p := c.planServers(t, srv)
			c.apply(t, p, nil)
			path := conformItem(t, p, adapter.KindMCP, "stale-srv").Path
			b, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(b), "node-orig") {
				t.Fatalf("config not as expected: %v", err)
			}
			if err := os.WriteFile(path, []byte(strings.Replace(string(b), "node-orig", "node-hand", 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			p = c.planServers(t, nil)
			it := conformItem(t, p, adapter.KindMCP, "stale-srv")
			if it.Action != adapter.ActionConflict || !it.Overridable || opFor(p, path) {
				t.Fatalf("want an overridable conflict without an operation, got %s overridable=%v %q", it.Action, it.Overridable, it.Reason)
			}
			p = c.planServers(t, nil, func(r *adapter.Request) { r.ForceManaged = true })
			if it := conformItem(t, p, adapter.KindMCP, "stale-srv"); it.Action != adapter.ActionRemove || !opFor(p, path) {
				t.Fatalf("--force-managed must remove it, got %s %q", it.Action, it.Reason)
			}
		})
	}
}

// A same-named skill directory whose marker is missing or foreign is a user
// directory: conflict and no operation, not even with --force-managed.
func TestSharedSkillWithMissingOrForeignMarkerIsNeverOverwritten(t *testing.T) {
	for _, tg := range conformTargets {
		for _, variant := range []string{"missing", "foreign"} {
			t.Run(tg.name+"/"+variant, func(t *testing.T) {
				c := newConformProject(t, tg)
				sk := conformSkill(t, c.forge, "marked-skill")
				p, err := c.plan(t, []adapter.SkillInput{sk})
				if err != nil {
					t.Fatal(err)
				}
				c.apply(t, p, nil)
				target := conformItem(t, p, adapter.KindSkill, "marked-skill").Path
				marker := filepath.Join(target, adapter.MarkerName)
				if variant == "missing" {
					if err := os.Remove(marker); err != nil {
						t.Fatal(err)
					}
				} else {
					m := `{"managed_by":"forge","name":"someone-else","version":"1.0.0","tree_hash":"x"}`
					if err := os.WriteFile(marker, []byte(m), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				for _, force := range []bool{false, true} {
					p, err := c.plan(t, []adapter.SkillInput{sk}, func(r *adapter.Request) { r.ForceManaged = force })
					if err != nil {
						t.Fatal(err)
					}
					it := conformItem(t, p, adapter.KindSkill, "marked-skill")
					if it.Action != adapter.ActionConflict || it.Overridable || opFor(p, target) {
						t.Fatalf("force=%v: want a plain conflict without an operation, got %s overridable=%v %q", force, it.Action, it.Overridable, it.Reason)
					}
				}
			})
		}
	}
}

// Editing only the marker's tree_hash (content and state hash intact) is a
// modification of the managed directory, for the update and the removal path.
func TestSharedMarkerTreeHashEditIsConflict(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			sk := conformSkill(t, c.forge, "hash-skill")
			p, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			c.apply(t, p, nil)
			target := conformItem(t, p, adapter.KindSkill, "hash-skill").Path
			marker := filepath.Join(target, adapter.MarkerName)
			raw, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil || m["tree_hash"] == nil {
				t.Fatalf("marker: %v %s", err, raw)
			}
			m["tree_hash"] = "0000"
			out, _ := json.Marshal(m)
			if err := os.WriteFile(marker, out, 0o644); err != nil {
				t.Fatal(err)
			}
			for name, skills := range map[string][]adapter.SkillInput{"update": {sk}, "removal": nil} {
				p, err := c.plan(t, skills)
				if err != nil {
					t.Fatal(err)
				}
				it := conformItem(t, p, adapter.KindSkill, "hash-skill")
				if it.Action != adapter.ActionConflict || !it.Overridable || opFor(p, target) {
					t.Fatalf("%s: want an overridable conflict without an operation, got %s overridable=%v %q", name, it.Action, it.Overridable, it.Reason)
				}
			}
		})
	}
}

// ---- TASK-0022: interrupted-sync / restore handling, state and journal checks ----

// applyCrashAt applies p and simulates a kill at the named engine point; it
// returns the id of the backup the killed apply created.
func (c *conformProject) applyCrashAt(t *testing.T, p *adapter.Plan, point string) string {
	t.Helper()
	crashed := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(adapter.SimulatedCrash); !ok {
					panic(r)
				}
				crashed = true
			}
		}()
		_, _ = adapter.Apply(context.Background(), c.ad, p, adapter.ApplyOptions{BackupDir: filepath.Join(c.forge, "backups"),
			Crash: func(pt string) bool { return pt == point }})
	}()
	if !crashed {
		t.Fatalf("no crash at %q", point)
	}
	return c.lastBackup(t)
}

func (c *conformProject) lastBackup(t *testing.T) string {
	t.Helper()
	list, err := adapter.ListBackups(filepath.Join(c.forge, "backups"))
	if err != nil || len(list) == 0 {
		t.Fatalf("no backups: %v", err)
	}
	return list[len(list)-1].ID
}

// AC-1: a killed sync whose backup directory was deleted afterwards is reported
// with the safe resolution, and the items it wrote are not explained as "copied
// from elsewhere" / "user defined".
func TestSharedLostBackupOfInterruptedSyncIsExplained(t *testing.T) {
	for _, tg := range conformTargets {
		for _, kind := range []string{adapter.KindSkill, adapter.KindMCP} {
			t.Run(tg.name+"/"+kind, func(t *testing.T) {
				c := newConformProject(t, tg)
				var skills []adapter.SkillInput
				var servers []*mcp.Info
				name := "lost-skill"
				if kind == adapter.KindSkill {
					skills = []adapter.SkillInput{conformSkill(t, c.forge, name)}
				} else {
					name = "lost-srv"
					servers = []*mcp.Info{conformServer(name, "node-lost")}
				}
				plan := func(mods ...func(*adapter.Request)) *adapter.Plan {
					p, err := c.ad.Plan(context.Background(), adapter.Inputs{Client: tg.name, Skills: skills, Servers: servers}, c.req(mods...))
					if err != nil {
						t.Fatal(err)
					}
					return p
				}
				id := c.applyCrashAt(t, plan(), "op-0:done") // the item is written, the state file is not
				if p := plan(); len(p.Interrupted) != 1 || len(p.LostBackups) != 0 {
					t.Fatalf("control: want a normal interruption, got %+v lost=%v", p.Interrupted, p.LostBackups)
				}
				if err := os.RemoveAll(filepath.Join(c.forge, "backups", id)); err != nil {
					t.Fatal(err)
				}
				p := plan()
				if len(p.LostBackups) != 1 || p.LostBackups[0] != id || len(p.Interrupted) != 0 {
					t.Fatalf("want the lost backup %s reported, got lost=%v interrupted=%+v", id, p.LostBackups, p.Interrupted)
				}
				notes := strings.Join(p.Notes, "\n")
				for _, want := range []string{"삭제", "--skip-conflicts", "--force-managed"} {
					if !strings.Contains(notes, want) {
						t.Errorf("note lacks %q: %s", want, notes)
					}
				}
				it := conformItem(t, p, kind, name)
				if it.Action != adapter.ActionConflict || it.Overridable || !strings.Contains(it.Reason, "삭제되어") ||
					strings.Contains(it.Reason, "복사된") || strings.Contains(it.Reason, "직접 정의한") {
					t.Fatalf("want a lost-backup conflict, got %s overridable=%v %q", it.Action, it.Overridable, it.Reason)
				}
				var buf strings.Builder
				adapter.WriteText(&buf, p, nil, false)
				if !strings.Contains(buf.String(), "백업("+id+")이 삭제되어") {
					t.Errorf("the text output does not warn:\n%s", buf.String())
				}
				if r := adapter.BuildReport(p, nil, false); len(r.LostBackups) != 1 {
					t.Errorf("the JSON report lacks lost_backups: %+v", r.LostBackups)
				}
				// --force-managed does not turn it into an overwrite.
				pf := plan(func(r *adapter.Request) { r.ForceManaged = true })
				if it := conformItem(t, pf, kind, name); it.Action != adapter.ActionConflict {
					t.Fatalf("--force-managed must not adopt it, got %s", it.Action)
				}
			})
		}
	}
}

// AC-2: after a killed restore, "latest" must not select the safety backup (the
// state from before the restore: restoring it undoes the half-done restore), the
// warning names the backup to run again, and running it finishes the restore and
// clears the interruption.
func TestSharedInterruptedRestoreResumesWithTheRightBackup(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			v1 := conformSkillV(t, c.forge, "resume-skill", "1.0.0")
			v2 := conformSkillV(t, c.forge, "resume-skill", "2.0.0")
			p1, err := c.plan(t, []adapter.SkillInput{v1})
			if err != nil {
				t.Fatal(err)
			}
			c.apply(t, p1, nil)
			target := conformItem(t, p1, adapter.KindSkill, "resume-skill").Path
			p2, err := c.plan(t, []adapter.SkillInput{v2})
			if err != nil {
				t.Fatal(err)
			}
			c.apply(t, p2, nil)
			b2 := c.lastBackup(t) // v1 -> v2
			bdir := filepath.Join(c.forge, "backups")
			want1, _, _ := adapter.TreeHash(v1.StoreDir, false)

			// Restore b2 (back to v1) and kill it after the first file.
			crashed := false
			func() {
				defer func() {
					if r := recover(); r != nil {
						if _, ok := r.(adapter.SimulatedCrash); !ok {
							panic(r)
						}
						crashed = true
					}
				}()
				_, _ = adapter.Restore(context.Background(), c.ad, adapter.RestoreOptions{
					ApplyOptions: adapter.ApplyOptions{BackupDir: bdir, Crash: func(pt string) bool { return pt == "op-0:done" }}, ID: b2, Force: true})
			}()
			if !crashed {
				t.Fatal("the restore was not interrupted")
			}
			safety := c.lastBackup(t)
			if safety == b2 {
				t.Fatal("the interrupted restore left no safety backup")
			}
			if latest, err := adapter.ResolveBackup(bdir, "latest"); err != nil || latest != b2 {
				t.Fatalf("latest must skip the interrupted restore's safety backup %s and give %s, got %q (%v)", safety, b2, latest, err)
			}
			p, err := c.plan(t, []adapter.SkillInput{v2})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Interrupted) != 1 || p.Interrupted[0].Kind != "restore" || p.Interrupted[0].RestoreOf != b2 || p.Interrupted[0].ID != safety {
				t.Fatalf("want the interrupted restore of %s, got %+v", b2, p.Interrupted)
			}
			var buf strings.Builder
			adapter.WriteText(&buf, p, nil, false)
			if !strings.Contains(buf.String(), "forge sync --restore "+b2+" --force") {
				t.Errorf("the warning does not name the backup to run again (%s):\n%s", b2, buf.String())
			}
			if strings.Contains(buf.String(), "forge sync --restore "+safety) {
				t.Errorf("the warning tells to run the safety backup %s:\n%s", safety, buf.String())
			}
			// Running "latest --force" finishes the restore: the skill is v1 again.
			id, err := adapter.ResolveBackup(bdir, "latest")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.Restore(context.Background(), c.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: bdir}, ID: id, Force: true}); err != nil {
				t.Fatalf("restore latest --force: %v", err)
			}
			if got, _, err := adapter.TreeHash(target, true); err != nil || got != want1 {
				t.Fatalf("the restore was undone instead of finished (hash %s, want %s, %v)", got, want1, err)
			}
			p, err = c.plan(t, []adapter.SkillInput{v1})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Interrupted) != 0 {
				t.Fatalf("a finished resume still shows an interruption: %+v", p.Interrupted)
			}
		})
	}
}

// AC-15 (1): a state file of another target or scope is refused, not read as ours.
func TestSharedStateFileOfAnotherTargetOrScopeIsRejected(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			p, err := c.plan(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string]string{
				"target": `{"version":1,"target":"someone-else","scope":"project","items":[]}`,
				"scope":  `{"version":1,"target":"` + tg.name + `","scope":"user","items":[]}`,
			} {
				conformDir(t, filepath.Dir(p.StatePath))
				if err := os.WriteFile(p.StatePath, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := c.plan(t, nil); err == nil || !strings.Contains(err.Error(), "상태 파일의 대상") {
					t.Errorf("%s mismatch: want a refusal, got %v", name, err)
				}
			}
		})
	}
}

// AC-15 (2): an orphan skill (written by a killed sync, absent from the state
// file, no longer wanted) that was edited by hand is never removed, with or
// without --force-managed; an unedited one is removed (control).
func TestSharedHandEditedJournalOrphanIsNeverRemoved(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			setup := func() (*conformProject, string) {
				c := newConformProject(t, tg)
				sk := conformSkill(t, c.forge, "orphan-skill")
				p, err := c.plan(t, []adapter.SkillInput{sk})
				if err != nil {
					t.Fatal(err)
				}
				target := conformItem(t, p, adapter.KindSkill, "orphan-skill").Path
				c.applyCrashAt(t, p, "op-0:done")
				return c, target
			}
			c, target := setup()
			p, err := c.plan(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			if it := conformItem(t, p, adapter.KindSkill, "orphan-skill"); it.Action != adapter.ActionRemove || !opFor(p, target) {
				t.Fatalf("control: an unedited orphan must be removed, got %s %q", it.Action, it.Reason)
			}
			c, target = setup()
			f, err := os.OpenFile(filepath.Join(target, "SKILL.md"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString("my own notes\n")
			f.Close()
			for _, force := range []bool{false, true} {
				p, err := c.plan(t, nil, func(r *adapter.Request) { r.ForceManaged = force })
				if err != nil {
					t.Fatal(err)
				}
				if opFor(p, target) {
					t.Errorf("force=%v: the hand-edited orphan is in the operations", force)
				}
				for _, it := range p.Items {
					if it.Name == "orphan-skill" && it.Action == adapter.ActionRemove {
						t.Errorf("force=%v: planned to remove the hand-edited orphan: %q", force, it.Reason)
					}
				}
			}
		})
	}
}

// AC-15 (3): AllowedPath refuses an unknown scope even for a path that is valid in
// the user layout (a target must not fall back to it), and forged state items with
// invalid names (skill, MCP) neither plan an operation nor survive in the state.
func TestSharedAllowedPathUnknownScopeAndForgedStateNames(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name+"/unknown-scope", func(t *testing.T) {
			base := conformBase(t)
			forge := conformDir(t, base, "forge")
			home := filepath.Join(base, "home")
			ad := tg.adapter(forge)
			in := adapter.Inputs{Client: tg.name, Skills: []adapter.SkillInput{conformSkill(t, forge, "scope-skill")}}
			p, err := ad.Plan(context.Background(), in, adapter.Request{Scope: adapter.ScopeUser, Home: home, ForgeBin: "forge", NoHooks: tg.noHooks})
			if err != nil {
				t.Fatal(err)
			}
			skill := conformItem(t, p, adapter.KindSkill, "scope-skill").Path
			for _, path := range []string{skill, p.StatePath} {
				if _, err := ad.AllowedPath(adapter.ScopeUser, home, path); err != nil {
					t.Fatalf("control: user-scope path %s refused: %v", path, err)
				}
				if _, err := ad.AllowedPath(adapter.Scope("other"), home, path); err == nil {
					t.Errorf("AllowedPath accepted %s for an unknown scope", path)
				}
				if _, err := ad.AllowedPath(adapter.Scope(""), home, path); err == nil {
					t.Errorf("AllowedPath accepted %s for an empty scope", path)
				}
			}
		})
		t.Run(tg.name+"/forged-mcp-state-name", func(t *testing.T) {
			// A state item naming an invalid server name whose hash matches an entry
			// that is really in the file must not delete that entry.
			c := newConformProject(t, tg)
			srv := []*mcp.Info{conformServer("probe-srv", "node")}
			p0 := c.planServers(t, srv)
			cfg := conformItem(t, p0, adapter.KindMCP, "probe-srv").Path
			key := "mcpServers"
			if tg.name == "opencode" {
				key = "mcp"
			}
			entry := `{"command":"x"}`
			if err := os.WriteFile(cfg, []byte(`{"`+key+`":{"bad name":`+entry+`}}`+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			h, err := adapter.HashJSON([]byte(entry))
			if err != nil {
				t.Fatal(err)
			}
			state := fmt.Sprintf(`{"version":1,"target":%q,"scope":"project","items":[{"kind":"mcp","name":"bad name","hash":%q,"applied_at":"x"}]}`, tg.name, h)
			conformDir(t, filepath.Dir(p0.StatePath))
			if err := os.WriteFile(p0.StatePath, []byte(state), 0o644); err != nil {
				t.Fatal(err)
			}
			p := c.planServers(t, nil, func(r *adapter.Request) { r.ForceManaged = true })
			for _, op := range p.Ops {
				if op.Path == cfg && !strings.Contains(string(op.Data), `"bad name"`) {
					t.Fatalf("the entry named by a forged state item was deleted:\n%s", op.Data)
				}
			}
			for _, it := range p.Items {
				if it.Name == "bad name" && it.Action == adapter.ActionRemove {
					t.Fatalf("planned to remove an invalid-named server: %q", it.Reason)
				}
			}
		})
		t.Run(tg.name+"/forged-state-names", func(t *testing.T) {
			c := newConformProject(t, tg)
			p0, err := c.plan(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			// A directory outside the skills directory that looks managed under the name "../../evil".
			evil := conformDir(t, c.proj, "evil")
			if err := os.WriteFile(filepath.Join(evil, "SKILL.md"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			h, _, err := adapter.TreeHash(evil, true)
			if err != nil {
				t.Fatal(err)
			}
			mk := fmt.Sprintf(`{"managed_by":"forge","name":"../../evil","version":"1","tree_hash":%q}`, h)
			if err := os.WriteFile(filepath.Join(evil, adapter.MarkerName), []byte(mk), 0o644); err != nil {
				t.Fatal(err)
			}
			state := fmt.Sprintf(`{"version":1,"target":%q,"scope":"project","items":[`+
				`{"kind":"skill","name":"../../evil","hash":%q,"applied_at":"x"},`+
				`{"kind":"skill","name":"Bad_Name","hash":%q,"applied_at":"x"},`+
				`{"kind":"mcp","name":"bad name","hash":%q,"applied_at":"x"},`+
				`{"kind":"mcp","name":"../x","hash":%q,"applied_at":"x"}]}`, tg.name, h, h, h, h)
			conformDir(t, filepath.Dir(p0.StatePath))
			if err := os.WriteFile(p0.StatePath, []byte(state), 0o644); err != nil {
				t.Fatal(err)
			}
			p, err := c.plan(t, nil, func(r *adapter.Request) { r.ForceManaged = true })
			if err != nil {
				t.Fatal(err)
			}
			if opFor(p, evil) {
				t.Fatalf("a forged skill name planned an operation on %s", evil)
			}
			if _, err := os.Stat(filepath.Join(evil, "SKILL.md")); err != nil {
				t.Fatalf("the outside directory was touched: %v", err)
			}
			if _, err := c.ad.AllowedPath(adapter.ScopeProject, p.Root, evil); err == nil {
				t.Errorf("AllowedPath accepted %s", evil)
			}
			for _, it := range p.NewState.Items {
				if strings.Contains(it.Name, "evil") || it.Name == "Bad_Name" || it.Name == "bad name" || it.Name == "../x" {
					t.Errorf("a forged state item survived: %+v", it)
				}
			}
		})
	}
}

// AC-15 (4a): a config file above MaxJSONFileBytes is refused (nothing is read whole).
func TestSharedOversizedConfigFileIsRefused(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			srv := []*mcp.Info{conformServer("big-srv", "node")}
			path := conformItem(t, c.planServers(t, srv), adapter.KindMCP, "big-srv").Path
			big := make([]byte, adapter.MaxJSONFileBytes+1)
			for i := range big {
				big[i] = ' '
			}
			copy(big, "{}")
			if err := os.WriteFile(path, big, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := c.ad.Plan(context.Background(), adapter.Inputs{Client: tg.name, Servers: srv}, c.req())
			if err == nil || !strings.Contains(err.Error(), "너무 큽니다") {
				t.Fatalf("want a size refusal, got %v", err)
			}
		})
	}
}

// AC-15 (4b): leftover temp files below a linked directory are neither listed nor
// removed (Preflight drops temp directories that are reached through a symlink).
func TestSharedPreflightIgnoresTempDirsBehindSymlinks(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			sk := conformSkill(t, c.forge, "temp-skill")
			p, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			skillsDir := filepath.Dir(conformItem(t, p, adapter.KindSkill, "temp-skill").Path)
			clientDir := filepath.Dir(skillsDir) // <proj>/.claude or <proj>/.opencode
			outside := conformDir(t, filepath.Dir(c.proj), "outside", filepath.Base(skillsDir))
			leftover := filepath.Join(outside, ".forge-tmp-9999999-0badf00d-temp-skill")
			if err := os.WriteFile(leftover, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Dir(outside), clientDir); err != nil {
				t.Fatal(err)
			}
			p, err = c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.StaleTemps) != 0 {
				t.Errorf("listed leftovers behind a symlink: %v", p.StaleTemps)
			}
			for _, d := range p.TempDirs {
				if strings.HasPrefix(d, skillsDir) {
					t.Errorf("a temp directory below the link is searched: %s", d)
				}
			}
			p.SkipConflicts = true
			c.apply(t, p, nil)
			if _, err := os.Stat(leftover); err != nil {
				t.Errorf("a file behind a symlink was removed: %v", err)
			}
		})
	}
}

// AC-15 (4c): a read-only managed skill directory is not removed when the skill is
// no longer wanted (a conflict, no operation).
func TestSharedStaleSkillReadOnlyIsNotRemoved(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			sk := conformSkill(t, c.forge, "ro-skill")
			p, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			c.apply(t, p, nil)
			target := conformItem(t, p, adapter.KindSkill, "ro-skill").Path
			if err := os.Chmod(target, 0o555); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(target, 0o755)
			p, err = c.plan(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			it := conformItem(t, p, adapter.KindSkill, "ro-skill")
			if it.Action != adapter.ActionConflict || !strings.Contains(it.Reason, "읽기 전용") || opFor(p, target) {
				t.Fatalf("want a read-only conflict without an operation, got %s %q", it.Action, it.Reason)
			}
		})
	}
}

// AC-14: a skills directory without search permission is reported as what it is,
// not as a symbolic link.
func TestSharedUnreadableSkillsDirIsNotCalledASymlink(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			sk := conformSkill(t, c.forge, "perm-skill")
			p, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			c.apply(t, p, nil)
			skillsDir := filepath.Dir(conformItem(t, p, adapter.KindSkill, "perm-skill").Path)
			if err := os.Chmod(skillsDir, 0); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(skillsDir, 0o755)
			if os.Geteuid() == 0 {
				t.Skip("root ignores directory permissions")
			}
			p, err = c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Skipf("plan refused wholesale: %v", err)
			}
			it := conformItem(t, p, adapter.KindSkill, "perm-skill")
			if it.Action != adapter.ActionConflict || strings.Contains(it.Reason, "심볼릭") {
				t.Fatalf("want a non-symlink conflict, got %s %q", it.Action, it.Reason)
			}
		})
	}
}

// AC-9: a skill-tree file with another hard link (to a file outside the tree) is
// refused: not read, not diffed, not copied into a backup.
func TestSharedHardLinkedFileInsideSkillTreeIsRefused(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			sk := conformSkill(t, c.forge, "link-skill")
			p, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			c.apply(t, p, nil)
			target := conformItem(t, p, adapter.KindSkill, "link-skill").Path
			outside := filepath.Join(conformDir(t, filepath.Dir(c.proj), "secretdir"), "data.txt")
			if err := os.WriteFile(outside, []byte("TOPSECRET-DATA-1234\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(outside, filepath.Join(target, "notes.txt")); err != nil {
				t.Skipf("hard links unsupported: %v", err)
			}
			sk2 := conformSkillV(t, c.forge, "link-skill", "2.0.0")
			for _, force := range []bool{false, true} {
				p, err := c.plan(t, []adapter.SkillInput{sk2}, func(r *adapter.Request) { r.ForceManaged = force })
				if err != nil {
					t.Fatal(err)
				}
				it := conformItem(t, p, adapter.KindSkill, "link-skill")
				if it.Action != adapter.ActionConflict || it.Overridable || opFor(p, target) || !strings.Contains(it.Reason, "하드링크") {
					t.Fatalf("force=%v: want a hard-link refusal without operation, got %s overridable=%v %q", force, it.Action, it.Overridable, it.Reason)
				}
				adapter.BuildDiffs(p)
				for _, d := range p.Diffs {
					if strings.Contains(d.Diff, "TOPSECRET") {
						t.Fatalf("hard-linked content in the diff: %s", d.Diff)
					}
				}
			}
			// The removal path refuses it as well.
			p, err = c.plan(t, nil, func(r *adapter.Request) { r.ForceManaged = true })
			if err != nil {
				t.Fatal(err)
			}
			if opFor(p, target) {
				t.Fatal("a skill tree with a hard-linked file is in the removal operations")
			}
		})
	}
}

// AC-14: a claude-code sidecar claim (project scope: a file of the repository, so
// untrusted) only makes opencode skip a skill while .claude/skills/<name> really
// exists with Forge's marker; a forged claim cannot keep a skill from being synced.
func TestOpenCodeForgedClaimDoesNotSkipSkill(t *testing.T) {
	tg := conformTargets[1]
	c := newConformProject(t, tg)
	sk := conformSkill(t, c.forge, "claimed-skill")
	root, _, err := opencode.New(c.forge).ResolveRoot(adapter.Request{Scope: adapter.ScopeProject, ProjectDir: c.proj, Home: c.home})
	if err != nil {
		t.Fatal(err)
	}
	statePath := claudecode.New(c.forge).StatePath(adapter.ScopeProject, root)
	conformDir(t, filepath.Dir(statePath))
	forged := `{"version":1,"target":"claude-code","scope":"project","items":[{"kind":"skill","name":"claimed-skill","hash":"` + strings.Repeat("a", 64) + `","applied_at":"x"}]}`
	if err := os.WriteFile(statePath, []byte(forged), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := c.plan(t, []adapter.SkillInput{sk})
	if err != nil {
		t.Fatal(err)
	}
	if it := conformItem(t, p, adapter.KindSkill, "claimed-skill"); it.Action != adapter.ActionAdd {
		t.Fatalf("a forged claim made opencode skip the skill: %s %q", it.Action, it.Reason)
	}
	// With the managed directory in place the claim is honoured (control).
	dir := conformDir(t, root, ".claude", "skills", "claimed-skill")
	mk := `{"managed_by":"forge","name":"claimed-skill","version":"1.0.0","tree_hash":"x"}`
	if err := os.WriteFile(filepath.Join(dir, adapter.MarkerName), []byte(mk), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err = c.plan(t, []adapter.SkillInput{sk})
	if err != nil {
		t.Fatal(err)
	}
	if it := conformItem(t, p, adapter.KindSkill, "claimed-skill"); it.Action != adapter.ActionSkip {
		t.Fatalf("control: a real claim must skip, got %s %q", it.Action, it.Reason)
	}
}

// AC-11 end to end: temp files of a killed process inside a backup directory are
// listed by the plan and removed by the next apply.
func TestSharedBackupDirTempsAreListedAndCleaned(t *testing.T) {
	for _, tg := range conformTargets {
		t.Run(tg.name, func(t *testing.T) {
			c := newConformProject(t, tg)
			sk := conformSkill(t, c.forge, "temps-skill")
			p, err := c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			id := c.applyCrashAt(t, p, "journal-written") // killed while the manifest/journal were being written
			dir := filepath.Join(c.forge, "backups", id)
			stale := []string{filepath.Join(dir, ".forge-tmp-9999999-0badf00d-manifest.json"), filepath.Join(dir, ".forge-tmp-9999999-0badf00d-journal.json")}
			for _, f := range stale {
				if err := os.WriteFile(f, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			p, err = c.plan(t, []adapter.SkillInput{sk})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.StaleTemps) != 2 {
				t.Fatalf("want the two leftovers in the plan, got %v", p.StaleTemps)
			}
			res, err := adapter.Apply(context.Background(), c.ad, p, adapter.ApplyOptions{BackupDir: filepath.Join(c.forge, "backups")})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.CleanedTemps) != 2 {
				t.Fatalf("cleaned %v", res.CleanedTemps)
			}
			for _, f := range stale {
				if _, err := os.Lstat(f); err == nil {
					t.Errorf("%s survived the apply", f)
				}
			}
		})
	}
}
