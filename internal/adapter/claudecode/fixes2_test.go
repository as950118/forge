package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/mcp"
)

func plantBait(t *testing.T, path, content string) (string, time.Time) {
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
	return path, old
}

// A: hard links to OpenCode secret files (and to the Claude Code user file) are
// refused for every document the claude-code adapter reads.
func TestHardLinkedConfigsToSecretFilesAreRefused(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	xdg := filepath.Join(e.home, "xdgcfg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	baits := map[string]string{}
	baits["mcp-auth"], _ = plantBait(t, filepath.Join(e.home, ".config/opencode/mcp-auth.json"), `{"k":"FAKE-SECRET-1"}`)
	baits["auth-data"], _ = plantBait(t, filepath.Join(e.home, ".local/share/opencode/auth.json"), `{"k":"FAKE-SECRET-2"}`)
	baits["credentials"], _ = plantBait(t, filepath.Join(e.home, ".config/opencode/credentials.json"), `{"k":"FAKE-SECRET-3"}`)
	baits["xdg"], _ = plantBait(t, filepath.Join(xdg, "opencode", "service.json"), `{"k":"FAKE-SECRET-4"}`)
	for name, bait := range baits {
		for _, rel := range []string{".mcp.json", ".claude/settings.json"} {
			t.Run(name+"/"+rel, func(t *testing.T) {
				before, _ := os.Stat(bait)
				link := filepath.Join(e.proj, filepath.FromSlash(rel))
				_ = os.MkdirAll(filepath.Dir(link), 0o755)
				_ = os.Remove(link)
				if err := os.Link(bait, link); err != nil {
					t.Skipf("hard links unavailable: %v", err)
				}
				defer os.Remove(link)
				p, err := e.plan(adapter.ScopeProject)
				if err == nil {
					adapter.BuildDiffs(p)
					t.Fatalf("plan accepted the link:\n%s", renderPlan(p)+allDiffs(p))
				}
				if strings.Contains(err.Error(), "FAKE-SECRET") {
					t.Fatalf("error leaks: %v", err)
				}
				after, _ := os.Stat(bait)
				got, _ := os.ReadFile(bait)
				if !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() || !strings.Contains(string(got), "FAKE-SECRET") {
					t.Fatal("bait changed")
				}
			})
		}
	}
}

// A (general): any config file with more than one hard link is refused; a
// normal one works.
func TestHardLinkedConfigIsRefusedAndNormalFilesAreFine(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	e.write(".mcp.json", "{\n  \"mcpServers\": {}\n}\n", 0o644)
	e.mustSync(adapter.ScopeProject)
	cfg := filepath.Join(e.proj, ".mcp.json")
	alias := filepath.Join(e.proj, "alias.txt")
	if err := os.Link(cfg, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := e.plan(adapter.ScopeProject); err == nil || !strings.Contains(err.Error(), "하드링크된 설정 파일은 지원하지 않습니다") {
		t.Fatalf("plan: %v", err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	p := e.mustPlan(adapter.ScopeProject)
	if p.HasChanges() {
		t.Fatalf("unexpected changes: %+v", p.Items)
	}
}

// B: malicious keys in the database are refused right before rendering.
func TestRenderRefusesMaliciousValueKeysFromTheDatabase(t *testing.T) {
	keys := map[string]string{
		"file token": "{file:/etc/hosts}", "env token": "{env:OKVAR}", "dollar": "${HOME}",
		"control": "A\x01B", "newline": "A\nB", "quote": `A"B`, "backslash": `A\B`, "space": "A B",
		"long": strings.Repeat("A", 300), "unicode": "KEYÄ",
	}
	for slotName, spec := range map[string]mcp.Spec{
		"env":    {Name: "evil", Transport: "stdio", Command: "x"},
		"header": {Name: "evil", Transport: "http", URL: "https://example.com/mcp"},
	} {
		for _, literal := range []bool{false, true} {
			for kname, key := range keys {
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
				})
			}
		}
	}
}

// E: skills named like OpenCode secret files are ordinary skills here too.
func TestSkillsNamedLikeSecretFilesWork(t *testing.T) {
	for _, scope := range []adapter.Scope{adapter.ScopeProject, adapter.ScopeUser} {
		t.Run(string(scope), func(t *testing.T) {
			e := newEnv(t)
			names := []string{"credential-helper", "service", "auth", "credentials", "mcp-auth"}
			for _, n := range names {
				e.addSkill(n, "1.0.0", nil)
			}
			base := filepath.Join(e.proj, ".claude", "skills")
			if scope == adapter.ScopeUser {
				base = filepath.Join(e.home, ".claude", "skills")
			}
			p, _ := e.mustSync(scope)
			for _, n := range names {
				wantAction(t, p, adapter.KindSkill, n, adapter.ActionAdd)
				if _, err := os.Stat(filepath.Join(base, n, "SKILL.md")); err != nil {
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
				if _, err := os.Lstat(filepath.Join(base, n)); err == nil {
					t.Fatalf("%s not removed", n)
				}
			}
			for _, it := range p.Items {
				if strings.Contains(it.Reason, "변조") || strings.Contains(it.Reason, "보안 정책") {
					t.Fatalf("misleading reason: %+v", it)
				}
			}
		})
	}
}
