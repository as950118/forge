package cli

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const demoMD = "---\nname: demo-skill\ndescription: A demo skill.\n---\n# Body\n"

// forgeHome isolates FORGE_HOME in a temp dir and restores write permission
// on read-only snapshots before t.TempDir cleanup.
func forgeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	t.Setenv("FORGE_HOME", home)
	return home
}

func runErr(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd(BuildInfo{Version: "v", Commit: "c", Date: "d"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runErr(t, "", args...)
	if err != nil {
		t.Fatalf("forge %v: %v\n%s", args, err, out)
	}
	return out
}

func demoDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "demo-skill")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(demoMD), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestInitCreatesHomeAndDB(t *testing.T) {
	home := forgeHome(t)
	out := mustRun(t, "init")
	if !strings.Contains(out, "초기화") {
		t.Errorf("out=%q", out)
	}
	for _, p := range []string{"forge.db", "skills", "tmp"} {
		if _, err := os.Stat(filepath.Join(home, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := runErr(t, "", "init", "--project"); err == nil {
		t.Error("--project must be refused for now")
	}
}

func TestSkillLifecycleDir(t *testing.T) {
	forgeHome(t)
	d := demoDir(t)
	if out := mustRun(t, "skill", "add", d); !strings.Contains(out, "추가됨: demo-skill@h-") {
		t.Errorf("add: %q", out)
	}
	if out := mustRun(t, "skill", "add", d); !strings.Contains(out, "변경 없음") {
		t.Errorf("re-add: %q", out)
	}
	out := mustRun(t, "skill", "list")
	for _, w := range []string{"NAME", "demo-skill", "yes", "global", "A demo skill."} {
		if !strings.Contains(out, w) {
			t.Errorf("list missing %q: %s", w, out)
		}
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(mustRun(t, "skill", "list", "--json")), &items); err != nil || len(items) != 1 || items[0]["enabled"] != true {
		t.Errorf("list json: %v %v", err, items)
	}
	var one map[string]any
	if err := json.Unmarshal([]byte(mustRun(t, "skill", "show", "demo-skill", "--json")), &one); err != nil || one["name"] != "demo-skill" {
		t.Errorf("show json: %v %v", err, one)
	}
	if out := mustRun(t, "skill", "show", "demo-skill"); !strings.Contains(out, "VERSION") || !strings.Contains(out, "dir") {
		t.Errorf("show: %q", out)
	}
	mustRun(t, "skill", "disable", "demo-skill")
	if out := mustRun(t, "skill", "list"); !strings.Contains(out, "no") {
		t.Errorf("disabled list: %q", out)
	}
	mustRun(t, "skill", "enable", "demo-skill")

	// remove needs confirmation
	if _, err := runErr(t, "", "skill", "remove", "demo-skill"); err == nil {
		t.Error("remove without --yes/confirmation must fail")
	}
	if _, err := runErr(t, "n\n", "skill", "remove", "demo-skill"); err == nil {
		t.Error("remove answered n must fail")
	}
	if out := mustRun(t, "skill", "list", "--json"); !strings.Contains(out, "demo-skill") {
		t.Error("skill must survive refused removal")
	}
	if out, err := runErr(t, "y\n", "skill", "remove", "demo-skill"); err != nil || !strings.Contains(out, "삭제됨") {
		t.Errorf("interactive remove: %v %q", err, out)
	}
	if _, err := runErr(t, "", "skill", "show", "demo-skill"); err == nil {
		t.Error("show after remove must fail")
	}
}

func TestSkillAddZipAndRemoveYes(t *testing.T) {
	forgeHome(t)
	zp := filepath.Join(t.TempDir(), "s.zip")
	f, _ := os.Create(zp)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("demo-skill/SKILL.md")
	_, _ = w.Write([]byte(demoMD))
	_ = zw.Close()
	f.Close()
	mustRun(t, "skill", "add", "--yes", zp)
	if out := mustRun(t, "skill", "show", "demo-skill"); !strings.Contains(out, "zip") {
		t.Errorf("show: %q", out)
	}
	mustRun(t, "skill", "remove", "demo-skill", "--yes")
}

func TestSkillValidate(t *testing.T) {
	home := forgeHome(t)
	d := demoDir(t)
	if out := mustRun(t, "skill", "validate", d); !strings.Contains(out, "OK: demo-skill") {
		t.Errorf("validate: %q", out)
	}
	if _, err := os.Stat(filepath.Join(home, "forge.db")); err == nil {
		t.Error("validate must not create the registry")
	}
	bad := filepath.Join(t.TempDir(), "bad")
	_ = os.MkdirAll(bad, 0o755)
	_ = os.WriteFile(filepath.Join(bad, "SKILL.md"), []byte("---\nname: Bad\ndescription: d\n---\n"), 0o644)
	out, err := runErr(t, "", "skill", "validate", bad)
	if err == nil || !strings.Contains(err.Error(), "검증 실패") {
		t.Errorf("err=%v out=%q", err, out)
	}
}

func TestSkillProjectScopeUnsupported(t *testing.T) {
	forgeHome(t)
	_, err := runErr(t, "", "skill", "list", "--project")
	if err == nil || !strings.Contains(err.Error(), "프로젝트") {
		t.Errorf("err=%v", err)
	}
}

func TestSkillAddRejectsBadSources(t *testing.T) {
	forgeHome(t)
	for _, src := range []string{"file:///tmp/x", "http://example.com/r.git", filepath.Join(t.TempDir(), "missing")} {
		if _, err := runErr(t, "", "skill", "add", src); err == nil {
			t.Errorf("%s should fail", src)
		}
	}
}
