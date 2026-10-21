package skill

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fakeCloner struct {
	gotURL string
	calls  int
}

func (f *fakeCloner) Clone(_ context.Context, url, dest string) (string, error) {
	f.calls++
	f.gotURL = url
	if err := os.MkdirAll(filepath.Join(dest, ".git"), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dest, ".git", "HEAD"), []byte("x"), 0o644); err != nil {
		return "", err
	}
	return "abc123", os.WriteFile(filepath.Join(dest, "SKILL.md"), []byte(goodSkill), 0o644)
}

func TestPrepareGitFake(t *testing.T) {
	f := &fakeCloner{}
	p, err := Prepare(context.Background(), "https://example.com/org/repo.git", PrepareOptions{TmpDir: t.TempDir(), Git: f})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cleanup()
	if p.SourceType != SourceGit || p.SourceRef != "abc123" || p.Meta.Name != "demo-skill" {
		t.Errorf("%+v", p)
	}
	if _, err := os.Stat(filepath.Join(p.Root, ".git")); !os.IsNotExist(err) {
		t.Error(".git must be dropped")
	}
}

func TestPrepareGitRejectsBadURLs(t *testing.T) {
	bad := []string{
		"file:///tmp/repo", "http://example.com/r.git", "git://example.com/r.git",
		"https://user:pw@example.com/r.git", "https://token@example.com/r.git",
		"ext::sh -c id", "ssh://example.com/r?x=1", "-uhttps://x", "https:///nohost",
		"ssh://-oProxyCommand=x/repo", "https://-evil.example.com/r.git", "ssh://git@-oProxyCommand=x/repo",
	}
	for _, u := range bad {
		f := &fakeCloner{}
		_, err := Prepare(context.Background(), u, PrepareOptions{TmpDir: t.TempDir(), Git: f})
		if err == nil || f.calls != 0 {
			t.Errorf("%q: err=%v calls=%d", u, err, f.calls)
		}
	}
	if err := ValidateGitURL("ssh://git@github.com/org/repo.git"); err != nil {
		t.Errorf("ssh user without password should pass: %v", err)
	}
}

func TestExecGitRefusesFileScheme(t *testing.T) {
	_, err := ExecGit{}.Clone(context.Background(), "file:///tmp/x", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "스킴") {
		t.Errorf("err=%v", err)
	}
}

// Integration: real git against a local temp repo. Uses the test-only
// allowFile switch; product code (ExecGit{}) rejects file://.
func TestExecGitLocalIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "SKILL.md"), goodSkill)
	for _, args := range [][]string{
		{"init", "-q"}, {"add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-qm", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	dest := filepath.Join(t.TempDir(), "clone")
	commit, err := ExecGit{allowFile: true}.Clone(context.Background(), "file://"+repo, dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(commit) != 40 {
		t.Errorf("commit=%q", commit)
	}
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		t.Error(err)
	}
}

func TestValidateGitURLRejectsDashHost(t *testing.T) {
	for _, u := range []string{"ssh://-oProxyCommand=x/repo", "https://-x.example.com/r", "ssh://git@-oProxyCommand=x/repo"} {
		err := ValidateGitURL(u)
		if err == nil || !strings.Contains(err.Error(), "'-'") {
			t.Errorf("%q: err=%v", u, err)
		}
	}
}
