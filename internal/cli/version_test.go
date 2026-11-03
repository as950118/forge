package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/as950118/forge/internal/clientver"
)

// fakeClients installs shell scripts called claude/opencode in a temp PATH
// directory and makes sync and doctor use exactly that directory (never the real
// PATH). Each script appends its argv to a log and prints the given version
// line. Returns the log path and a function that reads it.
func fakeClients(t *testing.T, versions map[string]string) (dir string, argv func() string) {
	t.Helper()
	dir, _ = filepath.EvalSymlinks(t.TempDir())
	log := filepath.Join(t.TempDir(), "argv.log")
	for name, out := range versions {
		body := "#!/bin/sh\nprintf 'n=%s' \"$#\" >> " + log + "\nfor a in \"$@\"; do printf ' [%s]' \"$a\" >> " + log + "; done\nprintf '\\n' >> " + log + "\necho '" + out + "'\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := versionChecker
	versionChecker = func(dirs []string) *clientver.Checker {
		return &clientver.Checker{PathEnv: dir, ProjectDirs: dirs}
	}
	t.Cleanup(func() { versionChecker = old })
	return dir, func() string { b, _ := os.ReadFile(log); return string(b) }
}

func TestSyncApplyStopsOnUntestedVersion(t *testing.T) {
	e := newSyncEnv(t)
	_, argv := fakeClients(t, map[string]string{"claude": "2.2.0 (Claude Code)"})
	out, err := e.sync(t, "--target", "claude-code", "--apply")
	if err == nil || !strings.Contains(err.Error(), "--allow-untested-version") || !strings.Contains(err.Error(), "미검증 버전: 생성한 설정이 올바르게 로드되지 않을 수 있습니다") {
		t.Fatalf("%v\n%s", err, out)
	}
	if exists(t, e.proj, ".mcp.json") || exists(t, e.proj, ".claude") || exists(t, e.proj, ".forge") {
		t.Fatal("nothing may be written when the gate stops the apply")
	}
	if l, _ := os.ReadDir(filepath.Join(e.homeForge(), "backups")); len(l) != 0 {
		t.Fatal("no backup either")
	}
	if got := argv(); got != "n=1 [--version]\n" {
		t.Fatalf("the client must be run with exactly --version: %q", got)
	}
	out = e.syncOK(t, "--target", "claude-code", "--apply", "--allow-untested-version")
	if !exists(t, e.proj, ".mcp.json") || !strings.Contains(out, "클라이언트 버전 claude-code: 2.2.0 [warn]") {
		t.Fatalf("the override applies and records the version in the report:\n%s", out)
	}
	if got := argv(); strings.Count(got, "\n") != 2 || strings.Contains(got, "n=0") || strings.Replace(got, "n=1 [--version]\n", "", -1) != "" {
		t.Fatalf("never anything but --version: %q", got)
	}
}

func TestSyncApplyVersionOutcomes(t *testing.T) {
	cases := []struct {
		name, claude string
		allowed      bool
		note         string
	}{
		{"verified", "2.1.305", true, "[ok] 검증된 버전"},
		{"patch differs", "2.1.311", true, "[info] 미검증 패치"},
		{"minor differs", "2.3.0", false, ""},
		{"major differs", "3.0.0", false, ""},
		{"garbage", "hello", false, ""},
		{"prerelease", "2.1.305-beta.1", false, ""},
	}
	for _, c := range cases {
		e := newSyncEnv(t)
		fakeClients(t, map[string]string{"claude": c.claude})
		out, err := e.sync(t, "--target", "claude-code", "--apply")
		if c.allowed {
			if err != nil || !strings.Contains(out, c.note) || !exists(t, e.proj, ".mcp.json") {
				t.Errorf("%s: %v\n%s", c.name, err, out)
			}
		} else if err == nil || exists(t, e.proj, ".mcp.json") {
			t.Errorf("%s: must stop: %v", c.name, err)
		}
	}
}

func TestSyncApplyProceedsWhenClientIsNotInstalled(t *testing.T) {
	e := newSyncEnv(t)
	fakeClients(t, map[string]string{}) // empty PATH directory
	out := e.syncOK(t, "--target", "all", "--apply")
	if !strings.Contains(out, "[info]") || !exists(t, e.proj, ".mcp.json") || !exists(t, e.proj, "opencode.json") {
		t.Fatalf("an uninstalled client is normal:\n%s", out)
	}
}

func TestPlanDoesNotRunTheClientUnlessAsked(t *testing.T) {
	e := newSyncEnv(t)
	_, argv := fakeClients(t, map[string]string{"claude": "9.9.9", "opencode": "2.0.24"})
	for _, args := range [][]string{{"--target", "claude-code"}, {"--target", "all", "--diff"}, {"--target", "opencode", "--json"}} {
		e.syncOK(t, args...)
	}
	if got := argv(); got != "" {
		t.Fatalf("a plan must not execute a client by default: %q", got)
	}
	out := e.syncOK(t, "--target", "all", "--check-version")
	if !strings.Contains(out, "클라이언트 버전 claude-code: 9.9.9 [warn]") || !strings.Contains(out, "클라이언트 버전 opencode: 2.0.24 [ok]") {
		t.Fatalf("--check-version shows the versions (a warning does not fail a plan):\n%s", out)
	}
	if got := argv(); strings.Count(got, "n=1 [--version]\n") != 2 || strings.Count(got, "\n") != 2 {
		t.Fatalf("exactly one --version per client: %q", got)
	}
	for _, args := range [][]string{{"--target", "all", "--allow-untested-version"}, {"--target", "all", "--check-version", "--apply"}} {
		if _, err := e.sync(t, args...); err == nil {
			t.Errorf("%v must be refused", args)
		}
	}
	if _, err := runErr(t, "", "sync", "--list-backups", "--check-version"); err == nil {
		t.Error("backup commands take no version flags")
	}
}

func TestSyncAllGateCoversBothAndBlocksBoth(t *testing.T) {
	e := newSyncEnv(t)
	fakeClients(t, map[string]string{"claude": "2.1.305", "opencode": "3.0.0"})
	out, err := e.sync(t, "--target", "all", "--apply")
	if err == nil || !strings.Contains(err.Error(), "opencode:") {
		t.Fatalf("%v\n%s", err, out)
	}
	if exists(t, e.proj, ".mcp.json") || exists(t, e.proj, "opencode.json") {
		t.Fatal("the gate stops BOTH targets before anything is written")
	}
	e.syncOK(t, "--target", "all", "--apply", "--allow-untested-version")
	if !exists(t, e.proj, ".mcp.json") || !exists(t, e.proj, "opencode.json") {
		t.Fatal("override")
	}
}

func TestVersionGateRefusesABinaryInsideTheProject(t *testing.T) {
	e := newSyncEnv(t)
	bin := filepath.Join(e.proj, "node_modules", ".bin")
	_ = os.MkdirAll(bin, 0o755)
	marker := filepath.Join(t.TempDir(), "ran")
	_ = os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\ntouch "+marker+"\necho 2.1.305\n"), 0o755)
	old := versionChecker
	versionChecker = func(dirs []string) *clientver.Checker {
		return &clientver.Checker{PathEnv: bin, ProjectDirs: dirs}
	}
	defer func() { versionChecker = old }()
	if _, err := e.sync(t, "--target", "claude-code", "--apply"); err == nil || !strings.Contains(err.Error(), "프로젝트 디렉터리 안") {
		t.Fatalf("%v", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("the project's binary was executed")
	}
	if exists(t, e.proj, ".mcp.json") {
		t.Fatal("nothing written")
	}
}

func TestUserScopeApplyDoesNotTreatHomeAsAProjectDir(t *testing.T) {
	e := newSyncEnv(t)
	// ~/.local/bin/claude is a normal install location: for --scope user the HOME
	// directory is not a "project directory".
	bin := filepath.Join(e.home, ".local", "bin")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho 2.1.305\n"), 0o755)
	old := versionChecker
	versionChecker = func(dirs []string) *clientver.Checker {
		return &clientver.Checker{PathEnv: bin, ProjectDirs: dirs}
	}
	defer func() { versionChecker = old }()
	out := e.syncOK(t, "--target", "claude-code", "--scope", "user", "--apply", "--no-hooks")
	if !strings.Contains(out, "[ok] 검증된 버전") {
		t.Fatalf("%s", out)
	}
}
