package skill

import (
	"archive/zip"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func extract(t *testing.T, entries []zentry, lim Limits) (string, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "out")
	return dest, ExtractZip(makeZip(t, entries), dest, lim)
}

func TestExtractZipOK(t *testing.T) {
	dest, err := extract(t, []zentry{{name: "SKILL.md", body: goodSkill}, {name: "scripts/run.sh", body: "#!/bin/sh", mode: 0o755}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "scripts", "run.sh")); err != nil {
		t.Error(err)
	}
}

func TestExtractZipRejects(t *testing.T) {
	small := Limits{MaxFiles: 3, MaxTotalSize: 1000, MaxFileSize: 500}
	cases := []struct {
		name    string
		entries []zentry
		lim     Limits
		want    string
	}{
		{"zip-slip", []zentry{{name: "../x", body: "x"}}, DefaultLimits, ".."},
		{"nested slip", []zentry{{name: "a/../../x", body: "x"}}, DefaultLimits, ".."},
		{"absolute", []zentry{{name: "/etc/passwd", body: "x"}}, DefaultLimits, "절대"},
		{"drive", []zentry{{name: "C:/x", body: "x"}}, DefaultLimits, "절대"},
		{"backslash", []zentry{{name: `a\..\x`, body: "x"}}, DefaultLimits, "역슬래시"},
		{"symlink", []zentry{{name: "link", body: "/etc/passwd", mode: os.ModeSymlink | 0o777}}, DefaultLimits, "심볼릭"},
		{"duplicate", []zentry{{name: "a", body: "x"}, {name: "a", body: "y"}}, DefaultLimits, "중복"},
		{"bomb single", []zentry{{name: "big", body: strings.Repeat("0", 501)}}, small, "단일 파일"},
		{"bomb total", []zentry{{name: "a", body: strings.Repeat("0", 400)}, {name: "b", body: strings.Repeat("0", 400)}, {name: "c", body: strings.Repeat("0", 400)}}, small, "총량"},
		{"too many files", []zentry{{name: "a", body: "1"}, {name: "b", body: "1"}, {name: "c", body: "1"}, {name: "d", body: "1"}}, small, "파일 수"},
	}
	for _, c := range cases {
		dest, err := extract(t, c.entries, c.lim)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v, want contains %q", c.name, err, c.want)
		}
		if _, e := os.Stat(filepath.Join(filepath.Dir(dest), "x")); e == nil {
			t.Errorf("%s: file escaped extraction dir", c.name)
		}
	}
}

func TestExtractZipEncryptedFlag(t *testing.T) {
	p := makeZip(t, []zentry{{name: "a", body: "x"}})
	b, _ := os.ReadFile(p)
	// set general purpose bit 0 in local header (offset 6) and central dir.
	b[6] |= 1
	idx := strings.LastIndex(string(b), "PK\x01\x02")
	b[idx+8] |= 1
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	err := ExtractZip(p, filepath.Join(t.TempDir(), "o"), DefaultLimits)
	if err == nil || !strings.Contains(err.Error(), "암호화") {
		t.Errorf("err=%v", err)
	}
}

func TestPrepareZipNestedRoot(t *testing.T) {
	z := makeZip(t, []zentry{{name: "wrapper/SKILL.md", body: goodSkill}, {name: "__MACOSX/._x", body: "j"}})
	p, err := Prepare(context_(), z, PrepareOptions{TmpDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cleanup()
	if p.Meta.Name != "demo-skill" || len(p.Warnings) != 1 {
		t.Errorf("%+v %v", p.Meta, p.Warnings)
	}
}

func TestExtractZipUnsupportedMethod(t *testing.T) {
	p := makeZip(t, []zentry{{name: "a", body: "x", method: zip.Store}})
	b, _ := os.ReadFile(p)
	b[8] = 12 // local header method (bzip2 = 12)
	idx := strings.LastIndex(string(b), "PK\x01\x02")
	b[idx+10] = 12
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	err := ExtractZip(p, filepath.Join(t.TempDir(), "o"), DefaultLimits)
	if err == nil || !strings.Contains(err.Error(), "압축 방식") {
		t.Errorf("err=%v", err)
	}
}

func TestDefaultLimitsValues(t *testing.T) {
	want := Limits{MaxFiles: 1000, MaxTotalSize: 50 << 20, MaxFileSize: 10 << 20}
	if DefaultLimits != want {
		t.Errorf("DefaultLimits=%+v, want %+v", DefaultLimits, want)
	}
}

// Layer 1: checkZipName rejects ".." on its own.
func TestCheckZipNameRejectsParent(t *testing.T) {
	for _, n := range []string{"../x", "a/../../x", "a/..", "..", "a/../b", "./../x"} {
		if _, err := checkZipName(n); err == nil || !strings.Contains(err.Error(), "..") {
			t.Errorf("%q: err=%v", n, err)
		}
	}
	for _, n := range []string{"a/b", "dir/", "a..b/c", "..a/b"} {
		if _, err := checkZipName(n); err != nil {
			t.Errorf("%q should pass: %v", n, err)
		}
	}
}

// Layer 2: withinDir (filepath.Rel) rejects escapes even if checkZipName were bypassed.
func TestWithinDir(t *testing.T) {
	dest := filepath.Join(string(filepath.Separator), "tmp", "dest")
	for target, want := range map[string]bool{
		filepath.Join(dest, "a", "b"):        true,
		dest:                                 true,
		filepath.Join(dest, "..", "x"):       false,
		filepath.Join(dest, "..", "dest2"):   false,
		filepath.Join(dest, "..", "..", "e"): false,
		filepath.Join(string(filepath.Separator), "etc", "passwd"): false,
	} {
		if got := withinDir(dest, target); got != want {
			t.Errorf("withinDir(%q,%q)=%v want %v", dest, target, got, want)
		}
	}
}

// Declared-size layer (pass 1): must reject before anything is written.
func TestExtractZipDeclaredSizeRejectsBeforeWriting(t *testing.T) {
	lim := Limits{MaxFiles: 10, MaxTotalSize: 1000, MaxFileSize: 500}
	cases := map[string][]zentry{
		"single": {{name: "ok", body: "x"}, {name: "big", body: strings.Repeat("0", 501)}},
		"total":  {{name: "a", body: strings.Repeat("0", 400)}, {name: "b", body: strings.Repeat("0", 400)}, {name: "c", body: strings.Repeat("0", 400)}},
	}
	for name, entries := range cases {
		dest, err := extract(t, entries, lim)
		if err == nil {
			t.Errorf("%s: want error", name)
		}
		if es, _ := os.ReadDir(dest); len(es) != 0 {
			t.Errorf("%s: %d entries written before declared-size check", name, len(es))
		}
	}
}

// Real-bytes layer: copyLimited enforces limits regardless of declared sizes.
func TestCopyLimitedEnforcesRealBytes(t *testing.T) {
	lim := Limits{MaxFiles: 10, MaxTotalSize: 150, MaxFileSize: 100}
	dir := t.TempDir()
	tl := &tally{lim: lim}
	if err := tl.copyLimited(filepath.Join(dir, "ok"), strings.NewReader(strings.Repeat("a", 100)), 0o644, "ok"); err != nil {
		t.Fatalf("exactly MaxFileSize should pass: %v", err)
	}
	err := tl.copyLimited(filepath.Join(dir, "big"), strings.NewReader(strings.Repeat("a", 101)), 0o644, "big")
	if err == nil || !strings.Contains(err.Error(), "단일 파일") {
		t.Errorf("per-file: %v", err)
	}
	tl2 := &tally{lim: lim}
	_ = tl2.copyLimited(filepath.Join(dir, "t1"), strings.NewReader(strings.Repeat("a", 100)), 0o644, "t1")
	err = tl2.copyLimited(filepath.Join(dir, "t2"), strings.NewReader(strings.Repeat("a", 60)), 0o644, "t2")
	if err == nil || !strings.Contains(err.Error(), "총 크기") {
		t.Errorf("total: %v", err)
	}
	// An endless reader must be cut off at MaxFileSize+1, not read forever.
	tl3 := &tally{lim: lim}
	err = tl3.copyLimited(filepath.Join(dir, "inf"), zeroReader{}, 0o644, "inf")
	if err == nil {
		t.Error("endless reader not rejected")
	}
	if st, e := os.Stat(filepath.Join(dir, "inf")); e == nil && st.Size() > lim.MaxFileSize+1 {
		t.Errorf("wrote %d bytes", st.Size())
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return len(p), nil }

func TestTallyAddFileLimit(t *testing.T) {
	tl := &tally{lim: Limits{MaxFiles: 2, MaxTotalSize: 1, MaxFileSize: 1}}
	if tl.addFile() != nil || tl.addFile() != nil {
		t.Fatal("within limit")
	}
	if err := tl.addFile(); err == nil || !strings.Contains(err.Error(), "파일 수") {
		t.Errorf("err=%v", err)
	}
}

// Layer 2 wiring: with layer 1 bypassed, ExtractZip itself must still refuse an
// escaping entry (fails if the withinDir/joinWithin call is dropped).
func TestExtractZipSecondLayerCatchesEscape(t *testing.T) {
	orig := zipNameCheck
	zipNameCheck = func(name string) (string, error) { return path.Clean(name), nil }
	t.Cleanup(func() { zipNameCheck = orig })
	base := t.TempDir()
	dest := filepath.Join(base, "out")
	zp := makeZip(t, []zentry{{name: "../evil.txt", body: "x", mode: 0o644, method: zip.Store}})
	err := ExtractZip(zp, dest, DefaultLimits)
	if err == nil || !strings.Contains(err.Error(), "벗어납니다") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(base, "evil.txt")); statErr == nil {
		t.Fatal("file escaped dest")
	}
	if _, err := joinWithin(dest, "../x"); err == nil {
		t.Error("joinWithin must reject ..")
	}
}
