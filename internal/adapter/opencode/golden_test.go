package opencode

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/as950118/forge/internal/adapter"
	"github.com/as950118/forge/internal/mcp"
)

const fixtureDir = "testdata/" + fixtureVersion

// installFixture copies project_before into the test project ("dot-opencode" -> ".opencode").
func installFixture(t *testing.T, e *env) {
	t.Helper()
	root := filepath.Join(fixtureDir, "project_before")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = strings.Replace(filepath.ToSlash(rel), "dot-opencode/", ".opencode/", 1)
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
	must(e.mreg.SetExposure(t.Context(), "gw-proxied", "opencode", mcp.ExposureGateway))
	must(e.mreg.SetExposure(t.Context(), "hidden-one", "opencode", mcp.ExposureHidden))
}

func render(p *adapter.Plan, e *env, res *adapter.ApplyResult) string {
	var buf bytes.Buffer
	adapter.WriteText(&buf, p, res, true)
	return strings.ReplaceAll(buf.String(), e.proj, "<PROJ>")
}

// TestGoldenFixtureV2_0_24 renders a project that mixes user-written and
// Forge-managed items and compares files, state and plan text with golden files.
func TestGoldenFixtureV2_0_24(t *testing.T) {
	e := newEnv(t)
	installFixture(t, e)
	standardRegistry(t, e)
	userSkill := e.read(".opencode/skills/user-skill/SKILL.md")
	before := e.read("opencode.json")

	p := e.mustPlan(adapter.ScopeProject)
	adapter.BuildDiffs(p)
	text := render(p, e, nil)
	if strings.Contains(text, "FIXTURE-not-a-real-key") {
		t.Fatal("provider apiKey leaked into the plan diff")
	}
	golden(t, "plan1.txt", text)
	if _, err := e.apply(p); err != nil {
		t.Fatal(err)
	}
	golden(t, "opencode1.json", e.read("opencode.json"))
	golden(t, "state1.json", e.read(".forge/sync-state.opencode.json"))
	golden(t, "skills1.txt", treeListing(t, filepath.Join(e.proj, ".opencode/skills")))
	if e.read(".opencode/skills/user-skill/SKILL.md") != userSkill {
		t.Fatal("user skill changed")
	}

	// Registry changes: new skill version, remote-direct moves to the gateway,
	// a new direct server appears, fs-direct is disabled.
	e.addSkill("demo-skill", "1.1.0", map[string]string{"scripts/run.sh": "#!/bin/sh\necho hi v2\n"})
	if err := e.mreg.SetExposure(t.Context(), "remote-direct", "opencode", mcp.ExposureGateway); err != nil {
		t.Fatal(err)
	}
	e.addServer(mcp.Spec{Name: "late-add", Transport: "sse", URL: "https://example.com/sse"})
	if err := e.mreg.SetEnabled(t.Context(), "fs-direct", false); err != nil {
		t.Fatal(err)
	}
	p2, _ := e.mustSync(adapter.ScopeProject)
	wantAction(t, p2, adapter.KindSkill, "demo-skill", adapter.ActionUpdate)
	wantAction(t, p2, adapter.KindMCP, "remote-direct", adapter.ActionRemove)
	wantAction(t, p2, adapter.KindMCP, "fs-direct", adapter.ActionRemove)
	wantAction(t, p2, adapter.KindMCP, "late-add", adapter.ActionAdd)
	golden(t, "opencode2.json", e.read("opencode.json"))
	golden(t, "state2.json", e.read(".forge/sync-state.opencode.json"))
	golden(t, "skills2.txt", treeListing(t, filepath.Join(e.proj, ".opencode/skills")))

	if p3 := e.mustPlan(adapter.ScopeProject); p3.HasChanges() {
		t.Fatalf("third plan must be empty, got %d ops", len(p3.Ops))
	}

	// Undo everything: byte-identical to the fixture (created files removed).
	ids, _ := adapter.ListBackups(e.backupDir())
	for i := len(ids) - 1; i >= 0; i-- {
		if _, err := adapter.Restore(t.Context(), e.ad, adapter.RestoreOptions{ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: ids[i].ID}); err != nil {
			t.Fatalf("restore %s: %v", ids[i].ID, err)
		}
	}
	if e.read("opencode.json") != before {
		t.Fatal("restoring all backups must give back the original opencode.json")
	}
	if e.exists(".forge") || e.exists(".opencode/skills/demo-skill") {
		t.Fatal("files created by sync must be gone after restore")
	}
}
