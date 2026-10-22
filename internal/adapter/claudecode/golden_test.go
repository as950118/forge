package claudecode

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/mcp"
)

const fixtureDir = "testdata/" + fixtureVersion

// installFixture copies project_before into the test project ("dot-claude" -> ".claude").
func installFixture(t *testing.T, e *env) {
	t.Helper()
	root := filepath.Join(fixtureDir, "project_before")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		rel = strings.Replace(rel, "dot-claude/", ".claude/", 1)
		rel = strings.Replace(rel, "dot-mcp.json", ".mcp.json", 1)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		e.write(rel, string(b), 0o644)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join(fixtureDir, "golden", name)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (UPDATE_GOLDEN=1 to create)", err)
	}
	if string(want) != got {
		t.Fatalf("golden %s differs.\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

// treeListing lists skill files with mode and content hash (stable, no mtimes).
func treeListing(t *testing.T, root string) string {
	t.Helper()
	var sb strings.Builder
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		fi, _ := d.Info()
		b, _ := os.ReadFile(p)
		fmt.Fprintf(&sb, "%s %04o %s\n", filepath.ToSlash(rel), fi.Mode().Perm(), adapter.SHA256Hex(b)[:16])
		return nil
	})
	return sb.String()
}

func standardRegistry(t *testing.T, e *env) {
	t.Helper()
	e.addSkill("demo-skill", "1.0.0", map[string]string{"scripts/run.sh": "#!/bin/sh\necho hi\n", "ref/notes.txt": "notes\n"})
	e.addServer(mcp.Spec{Name: "fs-direct", Transport: "stdio", Command: "npx", Args: []string{"-y", "fs-server"},
		Values: []mcp.Value{envRef("env", "API_TOKEN", "FS_TOKEN")}})
	e.addServer(mcp.Spec{Name: "remote-direct", Transport: "http", URL: "https://example.com/mcp",
		Values: []mcp.Value{envRef("header", "Authorization", "REMOTE_AUTH")}})
	e.addServer(mcp.Spec{Name: "gw-proxied", Transport: "stdio", Command: "gw-server"})
	e.addServer(mcp.Spec{Name: "hidden-one", Transport: "stdio", Command: "hid"})
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.mreg.SetExposure(t.Context(), "gw-proxied", "claude-code", mcp.ExposureGateway))
	must(e.mreg.SetExposure(t.Context(), "hidden-one", "claude-code", mcp.ExposureHidden))
}

func render(p *adapter.Plan, e *env, res *adapter.ApplyResult) string {
	var buf bytes.Buffer
	adapter.WriteText(&buf, p, res, true)
	return strings.ReplaceAll(buf.String(), e.proj, "<PROJ>")
}

// TestGoldenFixture2_1_305 renders a project that mixes user-written and
// Forge-managed items and compares files, state and plan text with golden files.
func TestGoldenFixture2_1_305(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	userSkill := e.read(".claude/skills/user-skill/SKILL.md")
	beforeMCP, beforeSettings := e.read(".mcp.json"), e.read(".claude/settings.json")

	p := e.mustPlan(adapter.ScopeProject)
	adapter.BuildDiffs(p)
	golden(t, "plan1.txt", render(p, e, nil))
	if _, err := e.apply(p); err != nil {
		t.Fatal(err)
	}
	golden(t, "mcp1.json", e.read(".mcp.json"))
	golden(t, "settings1.json", e.read(".claude/settings.json"))
	golden(t, "state1.json", e.read(".forge/sync-state.json"))
	golden(t, "skills1.txt", treeListing(t, filepath.Join(e.proj, ".claude/skills")))
	if e.read(".claude/skills/user-skill/SKILL.md") != userSkill {
		t.Fatal("user skill changed")
	}

	// Registry changes: new skill version, remote-direct moves to the gateway,
	// a new direct server appears, fs-direct is disabled.
	e.addSkill("demo-skill", "1.1.0", map[string]string{"scripts/run.sh": "#!/bin/sh\necho hi v2\n"})
	if err := e.mreg.SetExposure(t.Context(), "remote-direct", "claude-code", mcp.ExposureGateway); err != nil {
		t.Fatal(err)
	}
	e.addServer(mcp.Spec{Name: "late-add", Transport: "sse", URL: "https://example.com/sse"})
	if err := e.mreg.SetEnabled(t.Context(), "fs-direct", false); err != nil {
		t.Fatal(err)
	}
	p2, _ := e.mustSync(adapter.ScopeProject)
	for kind, name := range map[string]string{adapter.KindSkill: "demo-skill"} {
		wantAction(t, p2, kind, name, adapter.ActionUpdate)
	}
	wantAction(t, p2, adapter.KindMCP, "remote-direct", adapter.ActionRemove)
	wantAction(t, p2, adapter.KindMCP, "fs-direct", adapter.ActionRemove)
	wantAction(t, p2, adapter.KindMCP, "late-add", adapter.ActionAdd)
	golden(t, "mcp2.json", e.read(".mcp.json"))
	golden(t, "settings2.json", e.read(".claude/settings.json"))
	golden(t, "state2.json", e.read(".forge/sync-state.json"))
	golden(t, "skills2.txt", treeListing(t, filepath.Join(e.proj, ".claude/skills")))

	// Third sync is a no-op.
	p3 := e.mustPlan(adapter.ScopeProject)
	if p3.HasChanges() {
		t.Fatalf("third plan must be empty, got %d ops", len(p3.Ops))
	}

	// Undo everything: byte-identical to the fixture (created files removed).
	ids, _ := adapter.ListBackups(e.backupDir())
	for i := len(ids) - 1; i >= 0; i-- {
		if _, err := adapter.Restore(t.Context(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: ids[i].ID}); err != nil {
			t.Fatalf("restore %s: %v", ids[i].ID, err)
		}
	}
	if e.read(".mcp.json") != beforeMCP || e.read(".claude/settings.json") != beforeSettings {
		t.Fatal("restoring all backups must give back the original user files")
	}
	if e.exists(".forge") || e.exists(".claude/skills/demo-skill") {
		t.Fatal("files created by sync must be gone after restore")
	}
}
