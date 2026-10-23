package skill

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Cloner fetches a git repository into dest (shallow) and returns the commit.
// It is an interface so tests can inject a fake.
type Cloner interface {
	Clone(ctx context.Context, repoURL, dest string) (commit string, err error)
}

// ValidateGitURL accepts only https:// and ssh:// URLs without embedded
// passwords. file://, git://, http://, scp-style and option-like values are
// rejected.
func ValidateGitURL(raw string) error {
	if raw == "" || strings.HasPrefix(raw, "-") {
		return fmt.Errorf("git URL 이 올바르지 않습니다: %q", raw)
	}
	if strings.ContainsAny(raw, " \t\r\n\x00") {
		return fmt.Errorf("git URL 에 공백/제어 문자가 있습니다")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("git URL 을 해석할 수 없습니다: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "ssh" {
		return fmt.Errorf("git URL 스킴 %q 은 허용되지 않습니다 (https, ssh 만 허용)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("git URL 에 호스트가 없습니다")
	}
	if strings.HasPrefix(u.Hostname(), "-") || strings.HasPrefix(u.Host, "-") {
		return fmt.Errorf("git URL 호스트가 '-' 로 시작할 수 없습니다 (옵션 주입 방지): %q", u.Host)
	}
	if u.User != nil {
		if _, hasPw := u.User.Password(); hasPw || (u.Scheme == "https" && u.User.Username() != "") {
			return fmt.Errorf("자격증명이 포함된 URL 은 허용되지 않습니다 (source_uri 에 저장되면 안 됨)")
		}
	}
	if u.RawQuery != "" {
		return fmt.Errorf("git URL 에 쿼리스트링을 사용할 수 없습니다")
	}
	return nil
}

// RefCloner is an optional Cloner extension: fetch one ref (branch, tag or
// commit) instead of the default branch. Tests inject fakes the same way.
type RefCloner interface {
	CloneRef(ctx context.Context, repoURL, ref, dest string) (commit string, err error)
}

var (
	commitRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	refRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+-]{0,199}$`)
)

// IsCommitHash reports whether ref is a full (40 or 64 hex) commit hash, the
// only kind of ref that cannot move.
func IsCommitHash(ref string) bool { return commitRe.MatchString(ref) }

// ValidateRef accepts commit hashes, branch names and tag names that cannot be
// mistaken for a git option or revision expression: no leading '-', no "..",
// no "@", no spaces/control characters, no trailing ".lock" or "/".
func ValidateRef(ref string) error {
	switch {
	case ref == "":
		return fmt.Errorf("ref 가 비어 있습니다")
	case !refRe.MatchString(ref):
		return fmt.Errorf("ref %q 이 올바르지 않습니다 (영문/숫자로 시작, 영문/숫자/./_/+/-// 만, 최대 200자; 공백·제어 문자·'@'·'~'·'^'·':' 불가)", ref)
	case strings.Contains(ref, "..") || strings.HasSuffix(ref, ".lock") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") || strings.HasSuffix(ref, "."):
		return fmt.Errorf("ref %q 이 git ref 규칙에 맞지 않습니다", ref)
	}
	return nil
}

// ExecGit runs the external git binary.
type ExecGit struct {
	// allowFile permits file:// for tests only (unexported: product code
	// cannot enable it).
	allowFile bool
}

func (g ExecGit) runner(ctx context.Context) func(args ...string) ([]byte, error) {
	proto := "https:ssh"
	if g.allowFile {
		proto = "https:ssh:file"
	}
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL="+proto,
	)
	return func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = env
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git %s 실패: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
		}
		return out.Bytes(), nil
	}
}

// CloneRef fetches exactly one ref (`git init` + `git fetch --depth 1 -- <url>
// <ref>` + detached checkout) and returns the checked-out commit. When ref is a
// full commit hash the result must be that very commit (a server that returns
// something else is an error), which is what makes the pin meaningful.
func (g ExecGit) CloneRef(ctx context.Context, repoURL, ref, dest string) (string, error) {
	if !g.allowFile {
		if err := ValidateGitURL(repoURL); err != nil {
			return "", err
		}
	}
	if err := ValidateRef(ref); err != nil {
		return "", err
	}
	run := g.runner(ctx)
	if _, err := run("init", "-q", "--", dest); err != nil {
		return "", err
	}
	if _, err := run("-C", dest, "fetch", "-q", "--depth", "1", "--no-tags", "--", repoURL, ref); err != nil {
		return "", err
	}
	if _, err := run("-C", dest, "checkout", "-q", "--detach", "FETCH_HEAD"); err != nil {
		return "", err
	}
	out, err := run("-C", dest, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(out))
	if IsCommitHash(ref) && commit != ref {
		return "", fmt.Errorf("요청한 커밋 %s 과 다른 커밋(%s)이 받아졌습니다", ref, commit)
	}
	return commit, nil
}

// Clone runs `git clone --depth 1 -- <url> <dest>` and returns HEAD.
func (g ExecGit) Clone(ctx context.Context, repoURL, dest string) (string, error) {
	if !g.allowFile {
		if err := ValidateGitURL(repoURL); err != nil {
			return "", err
		}
	}
	run := g.runner(ctx)
	if _, err := run("clone", "--depth", "1", "--no-tags", "--", repoURL, dest); err != nil {
		return "", err
	}
	out, err := run("-C", dest, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
