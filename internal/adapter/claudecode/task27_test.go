package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heonjinjeong/forge/internal/adapter"
)

// TASK-0027 AC-16: the settings.json hook item explains a lost backup like MCP
// servers and skills do. Crash points are scanned until the hook has been written
// while the state file has not.
func TestLostBackupExplainsTheHookConflict(t *testing.T) {
	found := false
	for k := 0; k < 40 && !found; k++ {
		e := newEnv(t)
		standardRegistry(t, e)
		p := e.mustPlan(adapter.ScopeProject)
		if e.applyCrashAt(p, k) == "" {
			break // past the last crash point
		}
		list, err := adapter.ListBackups(e.backupDir())
		if err != nil || len(list) == 0 {
			continue
		}
		id := list[len(list)-1].ID
		if err := os.RemoveAll(filepath.Join(e.backupDir(), id)); err != nil {
			t.Fatal(err)
		}
		p2, err := e.plan(adapter.ScopeProject)
		if err != nil {
			continue
		}
		for _, it := range p2.Items {
			if it.Kind != adapter.KindHook || it.Action != adapter.ActionConflict {
				continue
			}
			if !strings.Contains(it.Reason, "삭제되어") || !strings.Contains(it.Reason, id) || strings.Contains(it.Reason, "사용자가 만든") {
				t.Fatalf("crash point %d: the hook conflict does not explain the lost backup: %q", k, it.Reason)
			}
			if it.Overridable {
				t.Errorf("a lost-backup hook conflict must not be overridable")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no crash point produced a hook conflict from a lost backup")
	}
}

// TASK-0027 AC-7: a valid, readable JSON file that is the user-level state file
// (same inode, any name) is refused wherever the sync would read or write a file,
// and neither its content nor a change of it shows up anywhere.
func TestValidJSONUserFileAliasesAreRefused(t *testing.T) {
	const secret = "FAKE-OAUTH-SECRET-1"
	for name, rel := range map[string]string{
		"project .mcp.json":           ".mcp.json",
		"project settings.json":       ".claude/settings.json",
		"project settings.local.json": ".claude/settings.local.json",
		"project user-file name":      ".claude" + ".json",
		"project innocent json":       "innocent.json",
		"project skill file":          ".claude/skills/demo-skill/SKILL.md",
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			standardRegistry(t, e)
			// Make the planted user file valid JSON and readable (the other tests use an unreadable decoy).
			if err := os.Chmod(e.decoy, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(e.decoy, []byte(`{"mcpServers":{},"oauthAccount":"`+secret+`"}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(e.decoy)
			link := filepath.Join(e.proj, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(e.decoy, link); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			p, err := e.plan(adapter.ScopeProject)
			var shown strings.Builder
			if err != nil {
				shown.WriteString(err.Error())
			} else {
				adapter.BuildDiffs(p)
				adapter.WriteText(&shown, p, nil, true)
				for _, op := range p.Ops {
					if op.Path == link {
						t.Fatalf("the alias of the user file is in the operations: %s", op.Path)
					}
				}
				p.SkipConflicts = true
				if _, aerr := e.apply(p); aerr != nil {
					shown.WriteString(aerr.Error())
				}
			}
			if strings.Contains(shown.String(), secret) {
				t.Fatalf("the content of the user file reached the output:\n%s", shown.String())
			}
			after, rerr := os.ReadFile(e.decoy)
			if rerr != nil || string(after) != string(before) {
				t.Fatalf("the user file changed: %v %q", rerr, after)
			}
		})
	}
}

// User scope: ~/.claude/settings.json being an alias of the user file.
func TestValidJSONUserFileAliasInHomeSettingsIsRefused(t *testing.T) {
	const secret = "FAKE-OAUTH-SECRET-2"
	e := newEnv(t)
	standardRegistry(t, e)
	if err := os.Chmod(e.decoy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.decoy, []byte(`{"oauthAccount":"`+secret+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(e.decoy)
	link := filepath.Join(e.home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(e.decoy, link); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	var shown strings.Builder
	p, err := e.plan(adapter.ScopeUser)
	if err != nil {
		shown.WriteString(err.Error())
	} else {
		adapter.BuildDiffs(p)
		adapter.WriteText(&shown, p, nil, true)
		for _, op := range p.Ops {
			if op.Path == link {
				t.Fatalf("the alias is in the operations")
			}
		}
	}
	if strings.Contains(shown.String(), secret) {
		t.Fatalf("leak:\n%s", shown.String())
	}
	if after, _ := os.ReadFile(e.decoy); string(after) != string(before) {
		t.Fatal("the user file changed")
	}
}
