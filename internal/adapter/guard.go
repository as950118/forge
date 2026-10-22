package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// claudeUserFile is the Claude Code user-level state file. It mixes secrets with
// settings and its structure is unverified (docs/clients/claude-code.md), so
// Forge neither reads nor writes it. Every file primitive in this package goes
// through GuardPath, which refuses it by name wherever it lives. This is the
// only place in the sync code that spells the file name; a test scans the other
// sources to keep it that way.
const claudeUserFile = ".claude.json"

// GuardPath is called before any read, write, rename or delete of a path. It
// refuses the Claude Code user file by NAME (case-insensitive and ignoring
// invisible/format characters and trailing dots or spaces, because APFS and NTFS
// are case-insensitive and normalization-insensitive) and by IDENTITY: a path
// that exists and is the same file as $HOME/<that file> (hard link, symlink,
// bind-style alias, any spelling) is refused too.
func GuardPath(p string) error {
	if matchesClaudeUserName(filepath.Base(p)) || sameAsUserFile(p) {
		return fmt.Errorf("보안 정책: %s 는 읽거나 쓰지 않습니다 (비밀이 섞여 있음)", claudeUserFile)
	}
	return nil
}

func matchesClaudeUserName(name string) bool {
	folded := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, name)
	folded = strings.TrimRight(folded, ". ")
	return strings.EqualFold(folded, claudeUserFile)
}

// sameAsUserFile reports whether p is (a link to) the user's Claude Code file.
// It only stats: neither file is opened, so nothing is read or touched.
func sameAsUserFile(p string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	target, err := os.Stat(filepath.Join(home, claudeUserFile))
	if err != nil {
		return false
	}
	pi, err := os.Stat(p)
	if err != nil {
		return false
	}
	return os.SameFile(target, pi)
}
