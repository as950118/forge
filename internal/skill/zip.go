package skill

import (
	"archive/zip"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var driveRe = regexp.MustCompile(`^[A-Za-z]:`)

// checkZipName validates an archive entry name and returns it cleaned
// (slash separated, no trailing slash).
func checkZipName(name string) (string, error) {
	switch {
	case name == "":
		return "", fmt.Errorf("빈 경로 항목")
	case strings.ContainsRune(name, 0):
		return "", fmt.Errorf("경로에 NUL 문자가 있습니다: %q", name)
	case strings.Contains(name, `\`):
		return "", fmt.Errorf("경로에 역슬래시가 있습니다: %q", name)
	case strings.HasPrefix(name, "/") || driveRe.MatchString(name):
		return "", fmt.Errorf("절대 경로는 허용되지 않습니다: %q", name)
	}
	for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if part == ".." {
			return "", fmt.Errorf("상위 경로(..)는 허용되지 않습니다: %q", name)
		}
	}
	c := path.Clean(name)
	if c == "." {
		return "", fmt.Errorf("잘못된 경로 항목: %q", name)
	}
	return c, nil
}

// withinDir reports whether target is dest itself or lies below it (second,
// path-based zip-slip layer independent of checkZipName).
func withinDir(dest, target string) bool {
	rel, err := filepath.Rel(dest, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// joinWithin joins a cleaned entry name onto dest and enforces withinDir.
// ExtractZip must use its result as the write target.
func joinWithin(dest, clean string) (string, error) {
	target := filepath.Join(dest, filepath.FromSlash(clean))
	if !withinDir(dest, target) {
		return "", fmt.Errorf("zip 항목이 대상 디렉터리를 벗어납니다: %q", clean)
	}
	return target, nil
}

// zipNameCheck is the first defense layer; a variable only so tests can bypass
// it and prove the second layer (joinWithin) is really wired into ExtractZip.
var zipNameCheck = checkZipName

// ExtractZip safely extracts a zip archive into dest (created if needed).
// Defenses: zip-slip (absolute, "..", backslash), symlinks and special files,
// encrypted entries, unsupported compression methods, duplicate entries,
// and file count / per-file / total size limits (checked against both the
// declared sizes and the bytes actually written).
func ExtractZip(zipPath, dest string, lim Limits) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("zip 파일을 열 수 없습니다: %w", err)
	}
	defer zr.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	t := &tally{lim: lim}
	seen := map[string]bool{}

	// Pass 1: validate everything before writing anything.
	var declared int64
	files := 0
	for _, f := range zr.File {
		clean, err := zipNameCheck(f.Name)
		if err != nil {
			return fmt.Errorf("zip 항목 거부: %w", err)
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("zip 항목 거부: 심볼릭 링크는 허용되지 않습니다: %q", f.Name)
		}
		if !mode.IsDir() && !mode.IsRegular() {
			return fmt.Errorf("zip 항목 거부: 일반 파일이 아닌 항목입니다: %q", f.Name)
		}
		if f.Flags&0x1 != 0 {
			return fmt.Errorf("zip 항목 거부: 암호화된 항목은 지원하지 않습니다: %q (암호 없이 다시 압축하세요)", f.Name)
		}
		if f.Method != zip.Store && f.Method != zip.Deflate {
			return fmt.Errorf("zip 항목 거부: 지원하지 않는 압축 방식(method=%d)입니다: %q (Store/Deflate 만 지원)", f.Method, f.Name)
		}
		if seen[clean] {
			return fmt.Errorf("zip 항목 거부: 중복 경로입니다: %q", f.Name)
		}
		seen[clean] = true
		if mode.IsDir() {
			continue
		}
		files++
		if files > lim.MaxFiles {
			return fmt.Errorf("zip 파일 수가 제한(%d개)을 초과했습니다", lim.MaxFiles)
		}
		if f.UncompressedSize64 > uint64(lim.MaxFileSize) {
			return fmt.Errorf("zip 항목 %q 이 단일 파일 크기 제한(%d바이트)을 초과했습니다", f.Name, lim.MaxFileSize)
		}
		declared += int64(f.UncompressedSize64)
		if declared > lim.MaxTotalSize {
			return fmt.Errorf("zip 압축 해제 총량이 제한(%d바이트)을 초과합니다", lim.MaxTotalSize)
		}
	}

	// Pass 2: write, still enforcing limits on real bytes.
	for _, f := range zr.File {
		clean, _ := zipNameCheck(f.Name)
		target, err := joinWithin(dest, clean)
		if err != nil {
			return fmt.Errorf("%w (항목 %q)", err, f.Name)
		}
		if f.Mode().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := t.addFile(); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("zip 항목 %q 을 읽을 수 없습니다: %w", f.Name, err)
		}
		mode := os.FileMode(0o644)
		if f.Mode()&0o111 != 0 {
			mode = 0o755
		}
		err = t.copyLimited(target, rc, mode, f.Name)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
