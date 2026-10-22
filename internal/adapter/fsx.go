package adapter

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MarkerName is the management marker file inside a managed skill directory.
// It is excluded from tree hashes so that a marker never changes a skill's hash.
const MarkerName = ".forge-managed"

// Limits for reading user files (a settings file is small; refuse anything absurd).
const (
	MaxJSONFileBytes = 16 << 20
	maxTreeFiles     = 5000
)

// SHA256Hex returns the hex sha256 of b.
func SHA256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ErrSymlink is returned for paths that are symbolic links (never followed).
var ErrSymlink = errors.New("심볼릭 링크는 따라가지 않습니다")

// CheckNoSymlinks verifies that every existing component of path below root is
// not a symbolic link. root itself is taken as given (callers resolve it once
// with filepath.EvalSymlinks): a repository that lives under a linked parent
// (/tmp, a linked workspace) is fine, but a linked .claude, skills directory or
// config file is refused because writing through it would modify files
// elsewhere. path must be below root.
func CheckNoSymlinks(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("경로가 루트 밖입니다: %s", path)
	}
	cur := root
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlink, cur)
		}
	}
	return nil
}

// ErrHardLinked is returned for a regular file that has more than one hard link
// (st_nlink > 1). A hard link to a secret file under another name cannot be told
// apart from the file by its name, and the guard only knows a fixed set of
// secret files, so every file read through ReadFileSafe (settings and config
// documents, state files, markers) must be the only name of its content. This
// holds for plan, apply and restore alike because all of them read through here.
var ErrHardLinked = errors.New("하드링크된 설정 파일은 지원하지 않습니다(다른 파일과 같은 내용을 공유함)")

// ReadFileSafe reads a regular file without following a symlink at the final
// component. A missing file returns (nil, false, nil).
func ReadFileSafe(path string) ([]byte, os.FileInfo, bool, error) {
	if err := GuardPath(path); err != nil {
		return nil, nil, false, err
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, nil, true, fmt.Errorf("%w: %s", ErrSymlink, path)
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, true, fmt.Errorf("일반 파일이 아닙니다: %s", path)
	}
	if linkCount(fi) > 1 {
		return nil, nil, true, fmt.Errorf("%w: %s", ErrHardLinked, path)
	}
	if fi.Size() > MaxJSONFileBytes {
		return nil, nil, true, fmt.Errorf("파일이 너무 큽니다(%d바이트): %s", fi.Size(), path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, true, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxJSONFileBytes+1))
	if err != nil {
		return nil, nil, true, err
	}
	return b, fi, true, nil
}

// TreeHash hashes a directory tree with the same scheme as skill.HashTree
// (regular files sorted by slash path: path, exec bit, size, content), so a
// copied skill hashes like its registry snapshot. skipMarker leaves out the
// root-level management marker. Symlinks and special files are errors.
func TreeHash(root string, skipMarker bool) (string, []TreeFile, error) {
	if err := GuardPath(root); err != nil {
		return "", nil, err
	}
	var rels []string
	guard := newFileGuard()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if fi, e := d.Info(); e == nil && d.Type().IsRegular() {
			if e := guard.check(fi); e != nil {
				return e
			}
		}
		if d.Type()&fs.ModeSymlink != 0 {
			rel, _ := filepath.Rel(root, p)
			return fmt.Errorf("%w: %s", ErrSymlink, filepath.ToSlash(rel))
		}
		if !d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, p)
			return fmt.Errorf("일반 파일이 아닌 항목: %s", filepath.ToSlash(rel))
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if skipMarker && rel == MarkerName {
			return nil
		}
		rels = append(rels, rel)
		if len(rels) > maxTreeFiles {
			return fmt.Errorf("파일이 너무 많습니다(>%d)", maxTreeFiles)
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	sort.Strings(rels)
	h := sha256.New()
	files := make([]TreeFile, 0, len(rels))
	for _, rel := range rels {
		full := filepath.Join(root, filepath.FromSlash(rel))
		fi, err := os.Lstat(full)
		if err != nil {
			return "", nil, err
		}
		x := "-"
		if fi.Mode()&0o111 != 0 {
			x = "x"
		}
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", rel, x, fi.Size())
		f, err := os.Open(full)
		if err != nil {
			return "", nil, err
		}
		fh := sha256.New()
		_, err = io.Copy(io.MultiWriter(h, fh), f)
		f.Close()
		if err != nil {
			return "", nil, err
		}
		files = append(files, TreeFile{Rel: rel, Mode: uint32(fi.Mode().Perm()), Size: fi.Size(), SHA256: hex.EncodeToString(fh.Sum(nil))})
	}
	return hex.EncodeToString(h.Sum(nil)), files, nil
}

// Snapshot observes path without following symlinks. A symlink is an error.
func Snapshot(path string) (FileState, error) {
	if err := GuardPath(path); err != nil {
		return FileState{}, err
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return FileState{}, nil
	}
	if err != nil {
		return FileState{}, err
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return FileState{}, fmt.Errorf("%w: %s", ErrSymlink, path)
	case fi.Mode().IsRegular():
		b, _, _, err := ReadFileSafe(path)
		if err != nil {
			return FileState{}, err
		}
		return FileState{Exists: true, Kind: "file", Hash: SHA256Hex(b), Mode: uint32(fi.Mode().Perm())}, nil
	case fi.IsDir():
		h, files, err := TreeHash(path, false)
		if err != nil {
			return FileState{}, err
		}
		return FileState{Exists: true, Kind: "dir", Hash: h, Mode: uint32(fi.Mode().Perm()), Files: files}, nil
	}
	return FileState{}, fmt.Errorf("지원하지 않는 파일 형식: %s", path)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// WriteFileAtomic writes data to a temp file in the same directory, fsyncs,
// sets mode and renames over path. The parent directory must exist.
func WriteFileAtomic(path string, data []byte, mode fs.FileMode) error {
	return writeFileAtomic(path, data, mode, nil)
}

// crashFn marks a point where a test may simulate a SIGKILL (see ApplyOptions.Crash).
type crashFn func(point string)

func (c crashFn) at(point string) {
	if c != nil {
		c(point)
	}
}

func writeFileAtomic(path string, data []byte, mode fs.FileMode, crash crashFn) error {
	if err := GuardPath(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, tempName(tmpPrefix, filepath.Base(path)))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	crash.at("file-tmp-written")
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	syncDir(dir)
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// copyTree copies src into dst (which must not exist). Symlinks are rejected.
// fileMode decides the mode of each file; directories get dirMode.
func copyTree(src, dst string, dirMode fs.FileMode, fileMode func(rel string, srcMode fs.FileMode) fs.FileMode) error {
	if err := GuardPath(src); err != nil {
		return err
	}
	if err := os.Mkdir(dst, dirMode); err != nil {
		return err
	}
	guard := newFileGuard()
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlink, relSlash)
		}
		if d.IsDir() {
			return os.Mkdir(filepath.Join(dst, rel), dirMode)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("일반 파일이 아닌 항목: %s", relSlash)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if err := guard.check(fi); err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(filepath.Join(dst, rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		if err := out.Chmod(fileMode(relSlash, fi.Mode())); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// SnapshotMode is the mode of copied skill files: 0644, or 0755 when the source
// is executable. The read-only mode of registry snapshots is not inherited.
func SnapshotMode(_ string, src fs.FileMode) fs.FileMode {
	if src&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// removeTree deletes a directory tree we own, making it writable first.
func removeTree(path string) error {
	if err := GuardPath(path); err != nil {
		return err
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// WriteBlocker returns a reason when path cannot (or must not) be replaced: an
// existing read-only file or directory is a deliberate "do not touch" signal, and
// the parent directory (or its nearest existing ancestor) must be writable.
// It returns "" when writing is fine.
func WriteBlocker(path string) string {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Perm()&0o200 == 0 {
			return "읽기 전용이라 변경하지 않습니다"
		}
	}
	dir := filepath.Dir(path)
	for {
		fi, err := os.Lstat(dir)
		if err == nil {
			if !fi.IsDir() {
				return "상위 경로가 디렉터리가 아닙니다"
			}
			if fi.Mode().Perm()&0o200 == 0 {
				return "상위 디렉터리가 읽기 전용이라 쓸 수 없습니다"
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ShellQuote quotes s for a POSIX shell (single quotes unless it is plainly safe).
func ShellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+:@%=,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
