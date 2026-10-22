package opencode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/mcp"
)

type baitSig struct {
	hash  string
	mtime time.Time
	mode  os.FileMode
}

func sigOf(t *testing.T, p string) baitSig {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	return baitSig{adapter.SHA256Hex(b), fi.ModTime(), fi.Mode()}
}

// plantBait writes a fake secret file (mode 0600, old mtime) and returns its path.
func plantBait(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

func (e *env) setXDG(t *testing.T) (cfg, data string) {
	t.Helper()
	cfg, data = filepath.Join(e.home, "xdgcfg"), filepath.Join(e.home, "xdgdata")
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_DATA_HOME", data)
	return cfg, data
}

// A: a project opencode.json that is a hard link to a secret file (4+ kinds).
func TestHardLinkedProjectConfigToSecretFilesIsRefused(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	xdgC, xdgD := e.setXDG(t)
	baits := map[string]string{
		"other-secret-name": plantBait(t, filepath.Join(e.home, ".config/opencode/mcp-auth.json"), `{"k":"FAKE-SECRET-1"}`),
		"credentials":       plantBait(t, filepath.Join(e.home, ".config/opencode/credentials.json"), `{"k":"FAKE-SECRET-2"}`),
		"data-dir":          plantBait(t, filepath.Join(e.home, ".local/share/opencode/auth.json"), `{"k":"FAKE-SECRET-3"}`),
		"xdg-config":        plantBait(t, filepath.Join(xdgC, "opencode", "service.json"), `{"k":"FAKE-SECRET-4"}`),
		"xdg-data":          plantBait(t, filepath.Join(xdgD, "opencode", "state.bin"), `{"k":"FAKE-SECRET-5"}`),
	}
	link := filepath.Join(e.proj, "opencode.json")
	for name, bait := range baits {
		t.Run(name, func(t *testing.T) {
			before := sigOf(t, bait)
			_ = os.Remove(link)
			if err := os.Link(bait, link); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			defer os.Remove(link)
			p, err := e.plan(adapter.ScopeProject)
			if err == nil {
				adapter.BuildDiffs(p)
				t.Fatalf("plan accepted a hard link to a secret file:\n%s", renderPlan(p)+allDiffs(p))
			}
			if strings.Contains(err.Error(), "FAKE-SECRET") {
				t.Fatalf("error leaks: %v", err)
			}
			if sigOf(t, bait) != before {
				t.Fatal("bait changed")
			}
			if b, _ := os.ReadFile(bait); !strings.Contains(string(b), "FAKE-SECRET") {
				t.Fatal("bait content changed")
			}
		})
	}
	// no backup was created from the linked file
	if ents, _ := os.ReadDir(e.backupDir()); len(ents) != 0 {
		t.Fatalf("backups created: %d", len(ents))
	}
}

// A (general defence): any config file with st_nlink > 1 is refused in plan,
// apply (stale check) and restore; a normal file is unaffected.
func TestHardLinkedConfigIsRefusedEverywhereAndNormalFilesAreFine(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.write("opencode.json", "{\n  \"model\": \"x\"\n}\n", 0o644)
	e.mustSync(adapter.ScopeProject) // nlink == 1: fine
	if err := e.mreg.SetExposure(t.Context(), "fs-direct", "opencode", mcp.ExposureGateway); err != nil {
		t.Fatal(err)
	}
	_, res := e.mustSync(adapter.ScopeProject) // removes a server: a backup of opencode.json
	cfg := filepath.Join(e.proj, "opencode.json")
	other := filepath.Join(e.proj, "unrelated.txt")
	if err := os.Link(cfg, other); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := e.plan(adapter.ScopeProject); err == nil || !strings.Contains(err.Error(), "하드링크된 설정 파일은 지원하지 않습니다") {
		t.Fatalf("plan: %v", err)
	}
	if _, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: res.BackupID, Force: true}); err == nil {
		t.Fatal("restore touched a hard-linked config file")
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	if _, err := e.plan(adapter.ScopeProject); err != nil {
		t.Fatalf("after the link is gone: %v", err)
	}
}

// A plan built before a link appeared is refused at apply time.
func TestApplyRefusesConfigThatBecameHardLinked(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.write("opencode.json", "{}\n", 0o644)
	p := e.mustPlan(adapter.ScopeProject)
	if err := os.Link(filepath.Join(e.proj, "opencode.json"), filepath.Join(e.proj, "alias.txt")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := e.apply(p); err == nil {
		t.Fatal("apply wrote through a config file that became hard linked")
	}
	if e.read("opencode.json") != "{}\n" {
		t.Fatal("config modified")
	}
}

// B: malicious keys edited into the database are refused right before rendering.
func TestRenderRefusesMaliciousValueKeysFromTheDatabase(t *testing.T) {
	keys := map[string]string{
		"file token": "{file:/etc/hosts}",
		"env token":  "{env:OKVAR}",
		"dollar":     "${HOME}",
		"control":    "A\x01B",
		"newline":    "A\nB",
		"quote":      `A"B`,
		"backslash":  `A\B`,
		"space":      "A B",
		"long":       strings.Repeat("A", 300),
		"unicode":    "KEYÄ",
	}
	for slotName, spec := range map[string]mcp.Spec{
		"env":    {Name: "evil", Transport: "stdio", Command: "x"},
		"header": {Name: "evil", Transport: "http", URL: "https://example.com/mcp"},
	} {
		for _, literal := range []bool{false, true} {
			all := map[string]string{}
			for k, v := range keys {
				all[k] = v
			}
			if slotName == "env" {
				all["leading digit"] = "1KEY" // fine for a header token, not for an env name
			}
			for kname, key := range all {
				t.Run(fmt.Sprintf("%s/literal=%v/%s", slotName, literal, kname), func(t *testing.T) {
					e := newEnv(t)
					s := spec
					if literal {
						v, err := mcp.ParseValue(slotName, "MODE=plain")
						if err != nil {
							t.Fatal(err)
						}
						s.Values = []mcp.Value{v}
					} else {
						s.Values = []mcp.Value{envRef(slotName, "MODE_X", "OKVAR")}
					}
					e.addServer(s)
					if _, err := e.conn.Exec(`UPDATE mcp_server_values SET key=?`, key); err != nil {
						t.Fatal(err)
					}
					p := e.mustPlan(adapter.ScopeProject)
					it := itemFor(p, adapter.KindMCP, "evil")
					if it == nil || it.Action != adapter.ActionConflict {
						t.Fatalf("not refused: %+v", it)
					}
					for _, op := range p.Ops {
						if strings.Contains(string(op.Data), "file:/etc/hosts") || strings.Contains(string(op.Data), "OKVAR") {
							t.Fatalf("rendered: %s", op.Data)
						}
					}
					u := e.mustPlan(adapter.ScopeUser)
					if it := itemFor(u, adapter.KindMCP, "evil"); it == nil || it.Action != adapter.ActionConflict {
						t.Fatalf("user scope not refused: %+v", it)
					}
					for _, c := range u.Commands {
						if strings.Contains(c, "evil") {
							t.Fatalf("command printed: %s", c)
						}
					}
				})
			}
		}
	}
}

func TestRenderAcceptsNormalKeys(t *testing.T) {
	e := newEnv(t)
	e.addServer(mcp.Spec{Name: "ok", Transport: "http", URL: "https://example.com/mcp",
		Values: []mcp.Value{envRef("header", "X-Api-Key", "OKVAR")}})
	p := e.mustPlan(adapter.ScopeProject)
	wantAction(t, p, adapter.KindMCP, "ok", adapter.ActionAdd)
}

// D: XDG_CONFIG_HOME.
func TestXDGConfigHomeSkillsDirectory(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	xdgC, _ := e.setXDG(t)
	sig := e.decoySig()
	p, _ := e.mustSync(adapter.ScopeUser)
	want := filepath.Join(xdgC, "opencode", "skills", "demo-skill")
	it := itemFor(p, adapter.KindSkill, "demo-skill")
	if it == nil || it.Path != want {
		t.Fatalf("item path: %+v want %s", it, want)
	}
	if !e.existsAbs(filepath.Join(want, "SKILL.md")) || e.existsHome(".config/opencode/skills") {
		t.Fatal("skill must be written below XDG_CONFIG_HOME only")
	}
	if !strings.Contains(renderPlan(p), "XDG_CONFIG_HOME 사용") || !strings.Contains(renderPlan(p), want) {
		t.Fatalf("plan must show the real target:\n%s", renderPlan(p))
	}
	e.checkDecoy(sig)
	// idempotent
	p2 := e.mustPlan(adapter.ScopeUser)
	if p2.HasChanges() {
		t.Fatalf("second plan has changes: %+v", p2.Items)
	}
}

func TestXDGInvalidValuesFallBackToDefaultWithWarning(t *testing.T) {
	cases := map[string]func(e *env) string{
		"relative": func(e *env) string { return "rel/cfg" },
		"outside":  func(e *env) string { return filepath.Join(filepath.Dir(e.home), "elsewhere") },
		"control":  func(e *env) string { return filepath.Join(e.home, "x\ty") },
		"symlinked": func(e *env) string {
			real, link := filepath.Join(e.home, "realcfg"), filepath.Join(e.home, "linkcfg")
			_ = os.MkdirAll(real, 0o755)
			if err := os.Symlink(real, link); err != nil {
				t.Skip("symlinks unavailable")
			}
			return link
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			standardRegistry(t, e)
			t.Setenv("XDG_CONFIG_HOME", mk(e))
			p, _ := e.mustSync(adapter.ScopeUser)
			if !e.existsHome(".config/opencode/skills/demo-skill/SKILL.md") {
				t.Fatal("default location not used")
			}
			if !strings.Contains(renderPlan(p), "경고: XDG_CONFIG_HOME=") {
				t.Fatalf("no warning:\n%s", renderPlan(p))
			}
			if strings.Contains(renderPlan(p), "\t") {
				t.Fatal("raw control character in the plan")
			}
		})
	}
	t.Run("unset and empty", func(t *testing.T) {
		e := newEnv(t)
		standardRegistry(t, e)
		t.Setenv("XDG_CONFIG_HOME", "")
		p, _ := e.mustSync(adapter.ScopeUser)
		if !e.existsHome(".config/opencode/skills/demo-skill/SKILL.md") || strings.Contains(renderPlan(p), "XDG_CONFIG_HOME") {
			t.Fatalf("empty value must behave like unset:\n%s", renderPlan(p))
		}
	})
}

func TestXDGDirectoriesAreForbiddenProjectRootsAndGuarded(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	xdgC, xdgD := e.setXDG(t)
	outside := filepath.Join(filepath.Dir(e.home), "xdgout")
	for _, d := range []string{filepath.Join(xdgC, "opencode"), filepath.Join(xdgD, "opencode")} {
		_ = os.MkdirAll(d, 0o755)
		if _, err := e.plan(adapter.ScopeProject, func(r *adapter.Request) { r.ProjectDir = d }); err == nil {
			t.Errorf("%s accepted as a project root", d)
		}
	}
	t.Setenv("XDG_STATE_HOME", outside)
	_ = os.MkdirAll(filepath.Join(outside, "opencode"), 0o755)
	if _, err := e.plan(adapter.ScopeProject, func(r *adapter.Request) { r.ProjectDir = filepath.Join(outside, "opencode") }); err == nil {
		t.Error("an XDG directory outside HOME was accepted as a project root")
	}
	for _, p := range []string{filepath.Join(xdgC, "opencode", "service.json"), filepath.Join(xdgC, "opencode", "opencode.json"),
		filepath.Join(xdgD, "opencode", "anything"), filepath.Join(outside, "opencode", "x")} {
		if err := adapter.GuardPath(p); err == nil {
			t.Errorf("%s not guarded", p)
		}
	}
	// but the skills below the XDG config directory are ordinary
	if err := adapter.GuardPath(filepath.Join(xdgC, "opencode", "skills", "demo-skill")); err != nil {
		t.Errorf("xdg skills refused: %v", err)
	}
}

// E: skills whose names look like secret file names are ordinary skills.
func TestSkillsNamedLikeSecretFilesWork(t *testing.T) {
	for _, scope := range []adapter.Scope{adapter.ScopeProject, adapter.ScopeUser} {
		t.Run(string(scope), func(t *testing.T) {
			e := newEnv(t)
			names := []string{"credential-helper", "service", "auth", "credentials", "mcp-auth"}
			for _, n := range names {
				e.addSkill(n, "1.0.0", nil)
			}
			sig := e.decoySig()
			p, _ := e.mustSync(scope)
			for _, n := range names {
				wantAction(t, p, adapter.KindSkill, n, adapter.ActionAdd)
			}
			base := ".opencode/skills/"
			if scope == adapter.ScopeUser {
				base = ".config/opencode/skills/"
			}
			exists := func(rel string) bool {
				if scope == adapter.ScopeUser {
					return e.existsHome(rel)
				}
				return e.exists(rel)
			}
			for _, n := range names {
				if !exists(base + n + "/SKILL.md") {
					t.Fatalf("%s not written", n)
				}
			}
			for _, n := range names {
				e.addSkill(n, "1.1.0", map[string]string{"extra.txt": "v2\n"})
			}
			p, _ = e.mustSync(scope)
			for _, n := range names {
				wantAction(t, p, adapter.KindSkill, n, adapter.ActionUpdate)
			}
			for _, n := range names {
				if err := e.sreg.SetEnabled(t.Context(), n, false); err != nil {
					t.Fatal(err)
				}
			}
			p, _ = e.mustSync(scope)
			for _, n := range names {
				wantAction(t, p, adapter.KindSkill, n, adapter.ActionRemove)
				if exists(base + n) {
					t.Fatalf("%s not removed", n)
				}
			}
			for _, it := range p.Items {
				if strings.Contains(it.Reason, "변조") || strings.Contains(it.Reason, "보안 정책") {
					t.Fatalf("misleading reason: %+v", it)
				}
			}
			e.checkDecoy(sig)
		})
	}
}
