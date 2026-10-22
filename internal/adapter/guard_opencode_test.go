package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func guardEnv(t *testing.T) (home, proj string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, proj = filepath.Join(base, "home"), filepath.Join(base, "proj")
	for _, d := range []string{filepath.Join(home, ".config", "opencode"), filepath.Join(home, ".local", "share", "opencode"), proj} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	return home, proj
}

func TestGuardRefusesOpenCodeSecretNamesInOpenCodeDirs(t *testing.T) {
	home, proj := guardEnv(t)
	xdgC, xdgD := filepath.Join(filepath.Dir(proj), "xdgc"), filepath.Join(filepath.Dir(proj), "xdgd")
	t.Setenv("XDG_CONFIG_HOME", xdgC)
	t.Setenv("XDG_DATA_HOME", xdgD)
	dirs := []string{filepath.Join(home, ".config", "opencode"), filepath.Join(home, ".local", "share", "opencode"), filepath.Join(xdgC, "opencode"), filepath.Join(xdgD, "opencode")}
	names := []string{"service.json", "SERVICE.JSON", "Service.Json", "service.json ", "service.json.", "mcp-auth.json", "opencode.db", "OpenCode.DB",
		"opencode.db-wal", "opencode.db-shm", "credential", "credentials.json", "Credential-store", "credential.d",
		"\u200bservice.json", "serv\u200bice.json", "opencode.d\u00adb", "mcp-auth.json\u0301"}
	for _, d := range dirs {
		for _, n := range append(names, "auth.json") {
			if err := GuardPath(filepath.Join(d, n)); err == nil {
				t.Errorf("GuardPath accepted %q in %s", n, d)
			}
		}
	}
	// the same names elsewhere (project, skills directories) are ordinary: only the
	// (directory, name) pair or the identity of the file is decisive
	for _, n := range append(names, "auth.json") {
		for _, d := range []string{proj, filepath.Join(proj, ".opencode", "skills"), filepath.Join(proj, ".claude", "skills"), filepath.Join(home, ".config", "opencode", "skills")} {
			if err := GuardPath(filepath.Join(d, n)); err != nil {
				t.Errorf("GuardPath refused %q in %s: %v", n, d, err)
			}
		}
	}
	for _, n := range []string{"credential-helper", "service", "auth", "opencode.json", "opencode.jsonc", "config.json", "service.jsonx", "xservice.json", "opencode.db.bak", "opencode.dbx", "my-credential", "mcp-auth.jsonl", "SKILL.md"} {
		if err := GuardPath(filepath.Join(proj, n)); err != nil {
			t.Errorf("GuardPath refused innocent project file %q: %v", n, err)
		}
	}
}

func TestGuardRefusesTheGlobalOpenCodeConfigByLocationAndIdentity(t *testing.T) {
	home, proj := guardEnv(t)
	gdir := filepath.Join(home, ".config", "opencode")
	for _, n := range []string{"opencode.json", "opencode.jsonc", "config.json", "OpenCode.JSON", "CONFIG.JSON"} {
		if err := GuardPath(filepath.Join(gdir, n)); err == nil {
			t.Errorf("global %q accepted (file need not exist)", n)
		}
	}
	// the skills below it are ordinary targets
	if err := GuardPath(filepath.Join(gdir, "skills", "x")); err != nil {
		t.Errorf("global skills dir refused: %v", err)
	}
	// anything in the data directory
	for _, n := range []string{"anything.txt", "log/opencode.log", "sub/deep/file"} {
		if err := GuardPath(filepath.Join(home, ".local", "share", "opencode", filepath.FromSlash(n))); err == nil {
			t.Errorf("data dir path %q accepted", n)
		}
	}
	// identity: hard link and symlink from the project to the global config
	global := filepath.Join(gdir, "opencode.json")
	if err := os.WriteFile(global, []byte(`{"x":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(proj, "opencode.json")
	if err := os.Link(global, hard); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	soft := filepath.Join(proj, "alias.txt")
	if err := os.Symlink(global, soft); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{hard, soft} {
		if err := GuardPath(p); err == nil {
			t.Errorf("alias %s accepted", p)
		}
		if _, _, _, err := ReadFileSafe(p); err == nil {
			t.Errorf("ReadFileSafe read alias %s", p)
		}
		if err := WriteFileAtomic(p, []byte("x"), 0o600); err == nil {
			t.Errorf("WriteFileAtomic wrote alias %s", p)
		}
	}
	if b, _ := os.ReadFile(global); string(b) != `{"x":1}` {
		t.Fatal("global config modified")
	}
	// a project's own opencode.json (not linked) is fine, also under a directory named like the global one
	other := filepath.Join(proj, "sub", ".config", "opencode", "opencode.json")
	if err := GuardPath(other); err != nil {
		t.Errorf("a project path with a similar layout was refused: %v", err)
	}
}

func TestGuardPrimitivesNeverTouchOpenCodeSecretFiles(t *testing.T) {
	home, _ := guardEnv(t)
	for _, p := range []string{filepath.Join(home, ".config", "opencode", "service.json"), filepath.Join(home, ".local", "share", "opencode", "opencode.db"),
		filepath.Join(home, ".local", "share", "opencode", "mcp-auth.json")} {
		if _, _, _, err := ReadFileSafe(p); err == nil {
			t.Errorf("read %s", filepath.Base(p))
		}
		if _, err := Snapshot(p); err == nil {
			t.Errorf("snapshot %s", filepath.Base(p))
		}
		if err := WriteFileAtomic(p, []byte("x"), 0o600); err == nil {
			t.Errorf("write %s", filepath.Base(p))
		}
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was created", filepath.Base(p))
		}
	}
	if !strings.Contains(GuardPath(filepath.Join(home, ".config", "opencode", "service.json")).Error(), "OpenCode") {
		t.Error("error text")
	}
}

type bait struct {
	rel     string
	content string
}

func plantBaits(t *testing.T, home string, xdgC, xdgD string) map[string]bait {
	t.Helper()
	baits := map[string]bait{
		"service":    {filepath.Join(home, ".config", "opencode", "mcp-auth.json"), `{"k":"FAKE-SECRET-1"}`},
		"data":       {filepath.Join(home, ".local", "share", "opencode", "auth.json"), `{"k":"FAKE-SECRET-2"}`},
		"credential": {filepath.Join(home, ".config", "opencode", "credentials.json"), `{"k":"FAKE-SECRET-3"}`},
		"xdg":        {filepath.Join(xdgC, "opencode", "service.json"), `{"k":"FAKE-SECRET-4"}`},
		"xdgdata":    {filepath.Join(xdgD, "opencode", "whatever.bin"), `{"k":"FAKE-SECRET-5"}`},
	}
	for _, b := range baits {
		if err := os.MkdirAll(filepath.Dir(b.rel), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(b.rel, []byte(b.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return baits
}

func TestGuardRefusesHardLinksToOpenCodeSecretFiles(t *testing.T) {
	home, proj := guardEnv(t)
	xdgC, xdgD := filepath.Join(filepath.Dir(proj), "xdgc"), filepath.Join(filepath.Dir(proj), "xdgd")
	t.Setenv("XDG_CONFIG_HOME", xdgC)
	t.Setenv("XDG_DATA_HOME", xdgD)
	for name, b := range plantBaits(t, home, xdgC, xdgD) {
		t.Run(name, func(t *testing.T) {
			before, _ := os.Stat(b.rel)
			link := filepath.Join(proj, "opencode.json")
			_ = os.Remove(link)
			if err := os.Link(b.rel, link); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			defer os.Remove(link)
			if err := GuardPath(link); err == nil {
				t.Error("GuardPath accepted a hard link to a secret file")
			}
			data, _, _, err := ReadFileSafe(link)
			if err == nil || len(data) != 0 || strings.Contains(err.Error(), "FAKE-SECRET") {
				t.Errorf("ReadFileSafe: data=%q err=%v", data, err)
			}
			if _, err := Snapshot(link); err == nil {
				t.Error("Snapshot accepted the link")
			}
			if err := WriteFileAtomic(link, []byte("x"), 0o600); err == nil {
				t.Error("WriteFileAtomic accepted the link")
			}
			after, _ := os.Stat(b.rel)
			got, _ := os.ReadFile(b.rel)
			if string(got) != b.content || !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
				t.Error("bait changed")
			}
		})
	}
}

func TestSkillTreeWithHardLinkToSecretIsRefused(t *testing.T) {
	home, proj := guardEnv(t)
	secret := filepath.Join(home, ".config", "opencode", "mcp-auth.json")
	if err := os.WriteFile(secret, []byte("FAKE-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(proj, "skill")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(dir, "notes.txt")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, _, err := TreeHash(dir, false); err == nil {
		t.Error("TreeHash walked a hard link to a secret file")
	}
	if err := copyTree(dir, filepath.Join(proj, "copy"), 0o755, SnapshotMode); err == nil {
		t.Error("copyTree copied a hard link to a secret file")
	}
	if b, err := os.ReadFile(filepath.Join(proj, "copy", "notes.txt")); err == nil && strings.Contains(string(b), "FAKE") {
		t.Error("secret copied")
	}
}

func TestReadFileSafeRefusesAnyHardLinkedFile(t *testing.T) {
	_, proj := guardEnv(t)
	a, b := filepath.Join(proj, "opencode.json"), filepath.Join(proj, "other.txt")
	if err := os.WriteFile(a, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, _, ok, err := ReadFileSafe(a); err != nil || !ok || string(data) != `{"a":1}` {
		t.Fatalf("a normal file must read fine: %v", err)
	}
	if err := os.Link(a, b); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	for _, p := range []string{a, b} {
		_, _, _, err := ReadFileSafe(p)
		if err == nil || !strings.Contains(err.Error(), "하드링크된 설정 파일은 지원하지 않습니다") {
			t.Errorf("%s: %v", p, err)
		}
		if _, err := Snapshot(p); err == nil {
			t.Errorf("%s snapshot", p)
		}
	}
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := ReadFileSafe(a); err != nil || !ok {
		t.Fatalf("after the link is gone the file reads again: %v", err)
	}
}
