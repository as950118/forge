package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/as950118/forge/internal/textsafe"
)

// claudeUserFile is the Claude Code user-level state file. It mixes secrets with
// settings and its structure is unverified (docs/clients/claude-code.md), so
// Forge neither reads nor writes it. Every file primitive in this package goes
// through GuardPath, which refuses it by name wherever it lives. This is the
// only place in the sync code that spells the file name; a test scans the other
// sources to keep it that way.
const claudeUserFile = ".claude.json"

// OpenCode files Forge never opens (docs/clients/opencode.md): the background
// service access file (password), the OAuth token stores, the session database
// (it holds a credential table) and anything named credential*. They are refused
// by (DIRECTORY, NAME) and by IDENTITY (os.SameFile, so a hard link or symlink
// from anywhere is refused too), never by the bare name: a project file or skill
// directory called credential-helper or service is ordinary. The OpenCode
// directories are the config directory (~/.config/opencode and
// $XDG_CONFIG_HOME/opencode) and the data/state directories
// (~/.local/share|state/opencode, $XDG_DATA_HOME|$XDG_STATE_HOME/opencode).
// The user-level config files (opencode.json, opencode.jsonc, config.json in
// the config directory) may hold provider API keys, so they are refused too; a
// project's own opencode.json is an ordinary sync target. Everything below the
// data and state directories is refused.
var (
	openCodeSecretNames = []string{"service.json", "mcp-auth.json", "auth.json", "opencode.db", "opencode.db-wal", "opencode.db-shm"}
	openCodeGlobalNames = []string{"opencode.json", "opencode.jsonc", "config.json"}
)

const openCodeCredentialPrefix = "credential"

// OpenCodeDirs returns the OpenCode config directories and the data/state
// directories (home based defaults plus the XDG overrides when they are absolute
// paths; a relative XDG value is invalid per the XDG spec and ignored). It is
// deliberately inclusive: a wrongly guarded directory only costs a refusal.
func OpenCodeDirs() (config, data []string) {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		config = append(config, filepath.Join(home, ".config", "opencode"))
		data = append(data, filepath.Join(home, ".local", "share", "opencode"), filepath.Join(home, ".local", "state", "opencode"))
	}
	add := func(list *[]string, env string) {
		if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
			*list = append(*list, filepath.Join(filepath.Clean(v), "opencode"))
		}
	}
	add(&config, "XDG_CONFIG_HOME")
	add(&data, "XDG_DATA_HOME")
	add(&data, "XDG_STATE_HOME")
	return config, data
}

// fileGuard holds the stat info of the files that must never be reached under
// another name: the Claude Code user file and the OpenCode secret files. Only
// stat is used; nothing is opened.
type fileGuard struct {
	claude *os.FileInfo
	cands  []os.FileInfo
}

func newFileGuard() *fileGuard {
	g := &fileGuard{}
	add := func(p string) {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			g.cands = append(g.cands, fi)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if fi, err := os.Stat(filepath.Join(home, claudeUserFile)); err == nil {
			g.claude = &fi
			g.cands = append(g.cands, fi)
		}
	}
	config, data := OpenCodeDirs()
	for _, d := range config {
		for _, n := range append(append([]string{}, openCodeSecretNames...), openCodeGlobalNames...) {
			add(filepath.Join(d, n))
		}
		if entries, err := os.ReadDir(d); err == nil {
			for _, e := range entries {
				if strings.HasPrefix(foldName(e.Name()), openCodeCredentialPrefix) {
					add(filepath.Join(d, e.Name()))
				}
			}
		}
	}
	for _, d := range data { // every file directly in the data/state directories
		if entries, err := os.ReadDir(d); err == nil {
			for _, e := range entries {
				add(filepath.Join(d, e.Name()))
			}
		}
	}
	return g
}

// check returns an error when fi is one of the guarded files (any spelling).
func (g *fileGuard) check(fi os.FileInfo) error {
	if g.claude != nil && os.SameFile(*g.claude, fi) {
		return fmt.Errorf("보안 정책: %s 는 읽거나 쓰지 않습니다 (비밀이 섞여 있음)", claudeUserFile)
	}
	for _, c := range g.cands {
		if os.SameFile(c, fi) {
			return fmt.Errorf("보안 정책: 이 경로는 OpenCode 비밀/설정 파일과 같은 파일입니다(하드링크·링크). 읽거나 쓰지 않습니다")
		}
	}
	return nil
}

// GuardPath is called before any read, write, rename or delete of a path. It
// refuses the Claude Code user file by NAME (case-insensitive and ignoring
// invisible/format characters and trailing dots or spaces, because APFS and NTFS
// are case-insensitive and normalization-insensitive) and by IDENTITY: a path
// that exists and is the same file as $HOME/<that file> (hard link, symlink,
// bind-style alias, any spelling) is refused too. The OpenCode secret files are
// refused by directory+name and by identity (see the comment above).
func GuardPath(p string) error {
	if matchesClaudeUserName(filepath.Base(p)) {
		return fmt.Errorf("보안 정책: %s 는 읽거나 쓰지 않습니다 (비밀이 섞여 있음)", claudeUserFile)
	}
	if what := openCodeForbidden(p); what != "" {
		return fmt.Errorf("보안 정책: OpenCode %s 는 읽거나 쓰지 않습니다 (비밀이 들어 있을 수 있음)", what)
	}
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return newFileGuard().check(fi)
	}
	return nil
}

// foldName normalizes a file name for comparison: invisible/format characters and
// combining marks are dropped, trailing dots and spaces are trimmed, case folded.
func foldName(name string) string {
	folded := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, name)
	return strings.ToLower(strings.TrimRight(folded, ". "))
}

func matchesClaudeUserName(name string) bool {
	return foldName(name) == claudeUserFile
}

// openCodeForbidden returns what is being refused by (directory, name) ("" when
// p is fine). Identity is checked separately by GuardPath.
func openCodeForbidden(p string) string {
	base := foldName(filepath.Base(p))
	dir := filepath.Dir(p)
	config, data := OpenCodeDirs()
	for _, d := range data {
		if hasAncestor(dir, d) {
			return "데이터/상태 디렉터리"
		}
	}
	for _, d := range config {
		if !sameDirPath(dir, d) {
			continue
		}
		for _, n := range openCodeSecretNames {
			if base == n {
				return "파일 " + textsafe.Display(n, 40)
			}
		}
		if strings.HasPrefix(base, openCodeCredentialPrefix) {
			return "credential 파일"
		}
		for _, n := range openCodeGlobalNames {
			if base == n {
				return "전역 설정 파일 (provider 키가 들어 있을 수 있음)"
			}
		}
	}
	return ""
}

// sameDirPath reports whether a and b are the same directory: by identity when
// both exist (case-insensitive file systems, links), by case-folded spelling
// otherwise.
func sameDirPath(a, b string) bool {
	if ai, err := os.Stat(a); err == nil {
		if bi, err := os.Stat(b); err == nil {
			return os.SameFile(ai, bi)
		}
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// hasAncestor reports whether dir is anc or lies below it.
func hasAncestor(dir, anc string) bool {
	for i, d := 0, dir; i < 64; i++ {
		if sameDirPath(d, anc) {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
	return false
}
