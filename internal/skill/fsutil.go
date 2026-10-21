package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Limits bound what an import may materialize on disk (decompression bombs).
type Limits struct {
	MaxFiles     int
	MaxTotalSize int64
	MaxFileSize  int64
}

// DefaultLimits: 1000 files, 50 MiB total, 10 MiB per file.
var DefaultLimits = Limits{MaxFiles: 1000, MaxTotalSize: 50 << 20, MaxFileSize: 10 << 20}

type tally struct {
	lim   Limits
	files int
	total int64
}

func (t *tally) addFile() error {
	t.files++
	if t.files > t.lim.MaxFiles {
		return fmt.Errorf("파일 수가 제한(%d개)을 초과했습니다", t.lim.MaxFiles)
	}
	return nil
}

func (t *tally) addBytes(n int64) error {
	t.total += n
	if t.total > t.lim.MaxTotalSize {
		return fmt.Errorf("총 크기가 제한(%d바이트)을 초과했습니다", t.lim.MaxTotalSize)
	}
	return nil
}

// copyLimited copies up to MaxFileSize bytes from r into a new file at dst.
func (t *tally) copyLimited(dst string, r io.Reader, mode os.FileMode, name string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, t.lim.MaxFileSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > t.lim.MaxFileSize {
		return fmt.Errorf("파일 %q 이 단일 파일 크기 제한(%d바이트)을 초과했습니다", name, t.lim.MaxFileSize)
	}
	return t.addBytes(n)
}

// CopyDir copies a local directory tree into dst, rejecting symlinks and
// special files, skipping .git directories, and enforcing lim.
func CopyDir(src, dst string, lim Limits) error {
	t := &tally{lim: lim}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("심볼릭 링크는 허용되지 않습니다: %s", filepath.ToSlash(rel))
		case info.IsDir():
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		case !info.Mode().IsRegular():
			return fmt.Errorf("일반 파일이 아닌 항목은 허용되지 않습니다: %s", filepath.ToSlash(rel))
		}
		if err := t.addFile(); err != nil {
			return err
		}
		if info.Size() > lim.MaxFileSize {
			return fmt.Errorf("파일 %q 이 단일 파일 크기 제한(%d바이트)을 초과했습니다", filepath.ToSlash(rel), lim.MaxFileSize)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		return t.copyLimited(filepath.Join(dst, rel), in, mode, filepath.ToSlash(rel))
	})
}

// HashTree returns the content hash (sha256 hex) of a directory tree: regular
// files only, sorted by slash path, covering path, exec bit and content.
func HashTree(root string) (string, error) {
	var rels []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("일반 파일이 아닌 항목: %s", p)
		}
		rel, _ := filepath.Rel(root, p)
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(rels)
	h := sha256.New()
	for _, rel := range rels {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(full)
		if err != nil {
			return "", err
		}
		x := "-"
		if info.Mode()&0o111 != 0 {
			x = "x"
		}
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", rel, x, info.Size())
		f, err := os.Open(full)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// makeWritable restores write permission under root so it can be deleted.
func makeWritable(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o755)
		} else if d.Type().IsRegular() {
			_ = os.Chmod(p, 0o644)
		}
		return nil
	})
}

// makeReadOnly strips write permission from a stored snapshot.
func makeReadOnly(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.Chmod(p, info.Mode().Perm()&^0o222)
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- { // children first
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			return err
		}
	}
	return nil
}

// RemoveAllForce deletes a (possibly read-only) snapshot directory.
func RemoveAllForce(path string) error {
	makeWritable(path)
	return os.RemoveAll(path)
}
