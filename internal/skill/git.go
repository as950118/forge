package skill

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
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

// ExecGit runs the external git binary.
type ExecGit struct {
	// allowFile permits file:// for tests only (unexported: product code
	// cannot enable it).
	allowFile bool
}

// Clone runs `git clone --depth 1 -- <url> <dest>` and returns HEAD.
func (g ExecGit) Clone(ctx context.Context, repoURL, dest string) (string, error) {
	if !g.allowFile {
		if err := ValidateGitURL(repoURL); err != nil {
			return "", err
		}
	}
	proto := "https:ssh"
	if g.allowFile {
		proto = "https:ssh:file"
	}
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL="+proto,
	)
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = env
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git %s 실패: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
		}
		return out.Bytes(), nil
	}
	if _, err := run("clone", "--depth", "1", "--no-tags", "--", repoURL, dest); err != nil {
		return "", err
	}
	out, err := run("-C", dest, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
