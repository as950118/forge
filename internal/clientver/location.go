package clientver

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Unsafe locations (docs/doctor.md "PATH 쓰기 가능 정책").
//
// Running "<binary> --version" is still running a program. A binary that another
// user can replace (it, or any directory on the way to it, is writable by group
// or others, or is owned by someone who is neither the current user nor root) is
// not run: another account could have planted it. The policy, applied to the
// symlink-resolved path and to the directory the PATH entry names:
//
//   - "group/other writable" below means: the other-write bit, or the group-write
//     bit of a group that is not trusted (gid 0, macOS admin gid 80, or the
//     owner's user-private group; staff/users/everyone are not trusted, and a
//     failed lookup is "not trusted"). Group MEMBERS are not enumerated.
//   - the binary file: group/other write bit set, or owner not you/root -> unsafe
//   - the directory that holds the binary (and the one that holds the link):
//     group/other write bit set -> unsafe, EVEN with the sticky bit (/tmp is
//     sticky and world-writable: nothing is run from it); owner not you/root -> unsafe
//   - every ancestor directory up to "/": group/other write bit set and NOT sticky
//     -> unsafe (a sticky directory only lets a file's owner rename it, so a
//     sticky ancestor such as /tmp above your own 0700 directory is fine);
//     owner not you/root -> unsafe
//
// A root-owned system directory or a Homebrew prefix owned by you with mode 0755
// is therefore accepted. Only POSIX mode bits and owners are judged (no ACLs).
// Non-unix systems judge the mode bits only.

// groupResolver decides whether the group of a file or directory is a trusted
// one (tests inject fakes; production uses systemGroups).
type groupResolver interface {
	Trusted(fi os.FileInfo) bool
}

var groups groupResolver = systemGroups{}

// writableByOthers: the other-write bit is always unsafe; the group-write bit is
// unsafe unless the group is trusted (gid 0, macOS admin gid 80, or the owner's
// user-private group). Broad groups such as staff are not trusted.
func writableByOthers(fi os.FileInfo) bool {
	m := fi.Mode().Perm()
	if m&0o002 != 0 {
		return true
	}
	return m&0o020 != 0 && !groups.Trusted(fi)
}

// maxLinkHops bounds the symlink chain that is followed hop by hop.
const maxLinkHops = 40

// unsafeLocation returns "" when running cand is acceptable, else the offending
// path and a short reason. (resolved, the kernel's own resolution of cand, is not
// used: the policy follows the path itself so that it judges exactly what is
// traversed.)
//
// The path is WALKED component by component from "/", never normalized:
//   - a ".." is applied to the physical directory reached so far, after the
//     directories passed on the way have been judged (so "<0777 dir>/sub/../x"
//     is judged through "<0777 dir>", where an attacker could turn "sub" into a
//     symlink to change what ".." means; filepath.Clean would hide that);
//   - every directory entered is judged (other-writable, untrusted-group-writable,
//     owner) together with all its ancestors up to "/"; sticky directories are
//     exempt when only passed through (their entries cannot be renamed by others,
//     and the entries' owners are judged), not when they hold a link or the binary;
//   - a symlink component is a hop: the directory holding it is judged strictly,
//     the link by owner (and by mode where the system honours it, i.e. not on
//     Linux), then its target is walked from the directory that holds the link
//     (relative target) or from "/" (absolute), at most maxLinkHops in total;
//   - the final file is judged by mode and owner and the directory holding it
//     strictly.
//
// Ordinary "..": Homebrew links are relative ("../Cellar/..."), so ".." is
// walked, not refused. A name that merely contains dots ("a..b", "...") is an
// ordinary name.
func unsafeLocation(cand, _ string) (offender, reason string) {
	w := &walker{seen: map[string]bool{}}
	final, off, why := w.walk("/", cand, true)
	if off != "" {
		return off, why
	}
	if fi, err := os.Stat(final); err == nil && !fi.IsDir() {
		if writableByOthers(fi) {
			return final, fmt.Sprintf("파일 모드 %04o: 그룹/다른 사용자가 쓸 수 있음", fi.Mode().Perm())
		}
		if t, ok := trustedOwner(fi); ok && !t {
			return final, "파일 소유자가 현재 사용자도 root 도 아님"
		}
	}
	return "", ""
}

type walker struct {
	hops int
	seen map[string]bool
}

// judge checks directory d and all its ancestors; strict applies the "holds a
// link or the binary" rule (no sticky exemption) to d itself.
func (w *walker) judge(d string, strict bool) (string, string) {
	for first := true; ; first = false {
		key := d
		if first && strict {
			key += "\x00strict"
		}
		if !w.seen[key] {
			w.seen[key] = true
			if why := dirProblem(d, first && strict); why != "" {
				return d, why
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", ""
		}
		d = parent
	}
}

// walk follows target starting at the physical directory pos and returns the
// physical path it ends at. last says that target's last component is the
// path's last one (so a non-directory there is the binary).
func (w *walker) walk(pos, target string, last bool) (string, string, string) {
	if filepath.IsAbs(target) {
		pos = "/"
	}
	var comps []string
	for _, c := range strings.Split(target, "/") {
		if c != "" && c != "." {
			comps = append(comps, c)
		}
	}
	for i, c := range comps {
		isLast := last && i == len(comps)-1
		if c == ".." {
			pos = filepath.Dir(pos)
			if off, why := w.judge(pos, false); off != "" {
				return "", off, why
			}
			continue
		}
		next := filepath.Join(pos, c) // c has no separator and is neither "." nor ".."
		fi, err := os.Lstat(next)
		switch {
		case err != nil:
			return next, "", "" // does not exist: nothing more to follow
		case fi.Mode()&os.ModeSymlink != 0:
			if off, why := w.judge(pos, true); off != "" {
				return "", off, why
			}
			w.hops++
			if w.hops > maxLinkHops {
				return "", next, fmt.Sprintf("심볼릭 링크 체인이 %d 홉을 넘거나 순환함", maxLinkHops)
			}
			if runtime.GOOS != "linux" && writableByOthers(fi) {
				return "", next, fmt.Sprintf("링크 모드 %04o: 그룹/다른 사용자가 쓸 수 있음", fi.Mode().Perm())
			}
			if t, ok := trustedOwner(fi); ok && !t {
				return "", next, "링크 소유자가 현재 사용자도 root 도 아님"
			}
			t, err := os.Readlink(next)
			if err != nil {
				return "", next, "링크를 읽을 수 없음"
			}
			end, off, why := w.walk(pos, t, false)
			if off != "" {
				return "", off, why
			}
			if end == "" {
				return "", next, "링크를 따라갈 수 없음"
			}
			pos = end
			if isLast {
				if st, err := os.Stat(pos); err == nil && !st.IsDir() {
					if off, why := w.judge(filepath.Dir(pos), true); off != "" {
						return "", off, why
					}
				}
			}
		case fi.IsDir():
			if off, why := w.judge(next, false); off != "" {
				return "", off, why
			}
			pos = next
		default: // a file: the binary (or something not followed further)
			if isLast {
				if off, why := w.judge(pos, true); off != "" {
					return "", off, why
				}
			}
			pos = next
		}
	}
	return pos, "", ""
}

// dirOfResolved is the directory that holds cand (the PATH entry), symlinks resolved.
func dirOfResolved(cand string) string {
	d := filepath.Dir(cand)
	if r, err := filepath.EvalSymlinks(d); err == nil {
		return r
	}
	return d
}

func dirProblem(d string, immediate bool) string {
	fi, err := os.Stat(d)
	if err != nil || !fi.IsDir() {
		return ""
	}
	mode := fi.Mode()
	sticky := mode&os.ModeSticky != 0
	if writableByOthers(fi) && (immediate || !sticky) {
		extra := ""
		if sticky {
			extra = ", sticky 여도 실행하지 않음"
		}
		return fmt.Sprintf("디렉터리 모드 %04o: 그룹/다른 사용자가 쓸 수 있음%s", mode.Perm(), extra)
	}
	if t, ok := trustedOwner(fi); ok && !t {
		return "디렉터리 소유자가 현재 사용자도 root 도 아님"
	}
	return ""
}

// unsafeMessage is the warning text of Result.Message.
func unsafeMessage(binary, cand, offender, reason string) string {
	fix := "신뢰할 수 있는 위치(본인 소유, 다른 사용자가 쓸 수 없음)에 설치하세요"
	if strings.Contains(reason, "모드") {
		fix = "chmod go-w " + quoteForHint(offender)
	}
	return fmt.Sprintf("%s 는 다른 사용자가 수정할 수 있는 위치라 실행하지 않았습니다 (%s: %s): %s", quoteForHint(cand), quoteForHint(offender), reason, fix)
}
