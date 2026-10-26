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
