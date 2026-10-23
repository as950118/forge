package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Source types stored in skill_versions.source_type.
const (
	SourceDir = "dir"
	SourceZip = "zip"
	SourceGit = "git"
)

// PrepareOptions configures Prepare.
type PrepareOptions struct {
	// TmpDir is where staging directories are created ("" = system temp).
	TmpDir string
	Limits Limits
	// Git performs clones; nil uses ExecGit{}.
	Git Cloner
	// Ref pins a git source to a branch, tag or commit ("" = default branch
	// HEAD). Only valid for git URLs; the Cloner must implement RefCloner.
	Ref string
}

// Prepared is a validated, staged skill ready to be stored.
type Prepared struct {
	Root       string // staged skill directory (contains SKILL.md)
	Meta       *Meta
	SourceType string
	SourceURI  string
	// SourceRef is the resolved commit for git sources; when the user asked for a
	// ref it is stored as "<ref>@<commit>" (see SplitSourceRef) so that
	// `forge skill update` can fetch the same ref again.
	SourceRef string
	// RequestedRef is the --ref the user gave ("" = none); Commit the resolved commit.
	RequestedRef string
	Commit       string
	Warnings     []string
	cleanup      func()
}

// Cleanup removes the staging area. Safe to call more than once.
func (p *Prepared) Cleanup() {
	if p != nil && p.cleanup != nil {
		p.cleanup()
		p.cleanup = nil
	}
}

// Prepare stages src (local dir, .zip file, or https/ssh git URL), validates
// SKILL.md and returns the result. On error nothing is left behind.
func Prepare(ctx context.Context, src string, opts PrepareOptions) (_ *Prepared, err error) {
	if opts.Limits == (Limits{}) {
		opts.Limits = DefaultLimits
	}
	if opts.Git == nil {
		opts.Git = ExecGit{}
	}
	if opts.TmpDir != "" {
		if err := os.MkdirAll(opts.TmpDir, 0o700); err != nil {
			return nil, err
		}
	}
	stage, err := os.MkdirTemp(opts.TmpDir, "import-*")
	if err != nil {
		return nil, err
	}
	p := &Prepared{cleanup: func() { _ = RemoveAllForce(stage) }}
	defer func() {
		if err != nil {
			p.Cleanup()
		}
	}()

	if opts.Ref != "" && !(strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "ssh://")) {
		return nil, fmt.Errorf("--ref 는 git URL(https://, ssh://) 소스에만 사용할 수 있습니다")
	}
	var origName string // original directory name, for the name-mismatch warning
	switch {
	case strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "ssh://"):
		if err := ValidateGitURL(src); err != nil {
			return nil, err
		}
		clone := filepath.Join(stage, "clone")
		var commit string
		var err error
		if opts.Ref != "" {
			if err := ValidateRef(opts.Ref); err != nil {
				return nil, err
			}
			rc, ok := opts.Git.(RefCloner)
			if !ok {
				return nil, fmt.Errorf("이 git 구현은 --ref 를 지원하지 않습니다")
			}
			commit, err = rc.CloneRef(ctx, src, opts.Ref, clone)
		} else {
			commit, err = opts.Git.Clone(ctx, src, clone)
		}
		if err != nil {
			return nil, fmt.Errorf("git 가져오기 실패: %w", err)
		}
		work := filepath.Join(stage, "work")
		if err := CopyDir(clone, work, opts.Limits); err != nil { // drops .git, enforces limits
			return nil, fmt.Errorf("git 저장소 검사 실패: %w", err)
		}
		_ = RemoveAllForce(clone)
		p.Root, p.SourceType, p.SourceURI, p.SourceRef = work, SourceGit, src, commit
		p.RequestedRef, p.Commit = opts.Ref, commit
		if opts.Ref != "" {
			p.SourceRef = opts.Ref + "@" + commit
		}
	case strings.Contains(src, "://"):
		return nil, fmt.Errorf("지원하지 않는 소스 스킴입니다: %q (https://, ssh:// git URL, 로컬 디렉터리, .zip 만 가능)", src)
	default:
		abs, err := filepath.Abs(src)
		if err != nil {
			return nil, err
		}
		fi, err := os.Lstat(abs)
		if err != nil {
			return nil, fmt.Errorf("소스를 찾을 수 없습니다: %w", err)
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			// resolve only the top-level argument; contents are still checked.
			rp, err := filepath.EvalSymlinks(abs)
			if err != nil {
				return nil, err
			}
			abs = rp
			if fi, err = os.Stat(abs); err != nil {
				return nil, err
			}
		}
		switch {
		case fi.IsDir():
			work := filepath.Join(stage, "work")
			if err := CopyDir(abs, work, opts.Limits); err != nil {
				return nil, fmt.Errorf("디렉터리 가져오기 실패: %w", err)
			}
			origName = filepath.Base(abs)
			p.Root, p.SourceType, p.SourceURI = work, SourceDir, abs
		case fi.Mode().IsRegular() && strings.EqualFold(filepath.Ext(abs), ".zip"):
			work := filepath.Join(stage, "work")
			if err := ExtractZip(abs, work, opts.Limits); err != nil {
				return nil, err
			}
			p.Root, p.SourceType, p.SourceURI = work, SourceZip, abs
		default:
			return nil, fmt.Errorf("소스는 디렉터리 또는 .zip 파일이어야 합니다: %s", abs)
		}
	}

	root, nested, err := findSkillRoot(p.Root)
	if err != nil {
		return nil, err
	}
	p.Root = root
	if nested != "" {
		origName = nested
	}
	data, err := os.ReadFile(filepath.Join(root, "SKILL.md"))
	if err != nil {
		return nil, fmt.Errorf("SKILL.md 를 읽을 수 없습니다: %w", err)
	}
	meta, err := ParseSkillMD(data)
	if err != nil {
		return nil, err
	}
	p.Meta = meta
	if origName != "" && origName != meta.Name {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"디렉터리명 %q 과 name %q 이 다릅니다 (OpenCode 규칙은 일치를 요구하지만 Forge 는 name 으로 렌더링하므로 등록은 가능)", origName, meta.Name))
	}
	return p, nil
}

// findSkillRoot returns dir if it holds SKILL.md, else the single top-level
// directory (ignoring __MACOSX) that does. nested is that directory's name.
func findSkillRoot(dir string) (root, nested string, err error) {
	if st, e := os.Stat(filepath.Join(dir, "SKILL.md")); e == nil && st.Mode().IsRegular() {
		return dir, "", nil
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		return "", "", e
	}
	var cand []string
	for _, en := range entries {
		if en.IsDir() && en.Name() != "__MACOSX" {
			cand = append(cand, en.Name())
		}
	}
	if len(cand) == 1 {
		sub := filepath.Join(dir, cand[0])
		if st, e := os.Stat(filepath.Join(sub, "SKILL.md")); e == nil && st.Mode().IsRegular() {
			return sub, cand[0], nil
		}
	}
	return "", "", fmt.Errorf("SKILL.md 를 찾을 수 없습니다 (소스 루트 또는 단일 최상위 디렉터리에 있어야 합니다)")
}

// SplitSourceRef splits a stored skill_versions.source_ref into the ref the
// user requested ("" if none or if it was a bare commit) and the commit.
func SplitSourceRef(stored string) (requested, commit string) {
	if i := strings.LastIndexByte(stored, '@'); i > 0 {
		return stored[:i], stored[i+1:]
	}
	return "", stored
}
