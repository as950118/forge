package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/heonjinjeong/forge/internal/textsafe"
)

// runSplit runs forge with separate stdout/stderr (the preview goes to stderr).
func runSplit(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := NewRootCmd(BuildInfo{Version: "v", Commit: "c", Date: "d"})
	var o, e bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err = cmd.Execute()
	return o.String(), e.String(), err
}

// asTerminal makes confirmInstall believe stdin is an interactive terminal.
func asTerminal(t *testing.T) {
	t.Helper()
	old := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { stdinIsTerminal = old })
}

// fakeGit is the only "git" tests ever use: it writes a skill into dest and
// records what it was asked for. No process, no network.
type fakeGit struct {
	skillMD string
	extra   map[string]string
	commit  string
	calls   int
	refs    []string
	dests   []string
}

func (f *fakeGit) write(dest string) error {
	f.dests = append(f.dests, dest)
	if err := os.MkdirAll(filepath.Join(dest, ".git"), 0o755); err != nil {
		return err
	}
	md := f.skillMD
	if md == "" {
		md = demoMD
	}
	if err := os.WriteFile(filepath.Join(dest, "SKILL.md"), []byte(md), 0o644); err != nil {
		return err
	}
	for k, v := range f.extra {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dest, k)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dest, k), []byte(v), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeGit) Clone(_ context.Context, _, dest string) (string, error) {
	f.calls++
	f.refs = append(f.refs, "")
	return f.c(), f.write(dest)
}

func (f *fakeGit) CloneRef(_ context.Context, _, ref, dest string) (string, error) {
	f.calls++
	f.refs = append(f.refs, ref)
	if len(ref) == 40 {
		return ref, f.write(dest)
	}
	return f.c(), f.write(dest)
}

func (f *fakeGit) c() string {
	if f.commit == "" {
		return strings.Repeat("c", 40)
	}
	return f.commit
}

func useGit(t *testing.T, f *fakeGit) {
	t.Helper()
	skillGit = f
	t.Cleanup(func() { skillGit = nil })
}

const gitURL = "https://example.com/org/skills.git"

// snapshotTree returns relative path -> sha256 (dirs as "dir") of everything under root.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			out[rel] = "dir"
			return nil
		}
		b, _ := os.ReadFile(p)
		h := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(h[:])
		return nil
	})
	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func skillCount(t *testing.T) int {
	t.Helper()
	out := mustRun(t, "skill", "list", "--json")
	var items []map[string]any
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatal(err)
	}
	return len(items)
}

func dirEntries(t *testing.T, d string) []string {
	t.Helper()
	ents, err := os.ReadDir(d)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func mkZip(t *testing.T, md string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("demo-skill/SKILL.md")
	_, _ = w.Write([]byte(md))
	_ = zw.Close()
	_ = f.Close()
	return p
}

// ---- approval policy -------------------------------------------------------

func TestGitAddWithoutApprovalIsRefusedAndInstallsNothing(t *testing.T) {
	home := forgeHome(t)
	mustRun(t, "init")
	g := &fakeGit{}
	useGit(t, g)
	out, errOut, err := runSplit(t, "", "skill", "add", gitURL)
	if err == nil {
		t.Fatalf("a git source without --yes and without a terminal must fail\n%s%s", out, errOut)
	}
	if !strings.Contains(err.Error(), "--yes") || !strings.Contains(err.Error(), "--preview") {
		t.Errorf("guidance missing: %v", err)
	}
	if !strings.Contains(errOut, "스킬 설치 미리보기") || !strings.Contains(errOut, "트리 sha256") {
		t.Errorf("the preview must still be shown so a person can decide: %s", errOut)
	}
	if skillCount(t) != 0 {
		t.Error("nothing may be installed")
	}
	if e := dirEntries(t, filepath.Join(home, "skills")); len(e) != 0 {
		t.Errorf("skills dir not empty: %v", e)
	}
	if e := dirEntries(t, filepath.Join(home, "tmp")); len(e) != 0 {
		t.Errorf("staging left behind: %v", e)
	}
}

func TestGitAddWithYesInstallsAndRecordsSource(t *testing.T) {
	forgeHome(t)
	g := &fakeGit{}
	useGit(t, g)
	out, errOut, err := runSplit(t, "", "skill", "add", "--yes", gitURL)
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if !strings.Contains(out, "추가됨: demo-skill@") {
		t.Errorf("out=%s", out)
	}
	if !strings.Contains(errOut, g.c()) || !strings.Contains(errOut, "고정되지 않음") {
		t.Errorf("preview must show the resolved commit and warn that HEAD is not pinned: %s", errOut)
	}
	var info struct {
		Versions []struct {
			SourceType string `json:"source_type"`
			SourceURI  string `json:"source_uri"`
			SourceRef  string `json:"source_ref"`
			Hash       string `json:"content_hash"`
		}
	}
	if err := json.Unmarshal([]byte(mustRun(t, "skill", "show", "demo-skill", "--json")), &info); err != nil {
		t.Fatal(err)
	}
	v := info.Versions[0]
	if v.SourceType != "git" || v.SourceURI != gitURL || v.SourceRef != g.c() || len(v.Hash) != 64 {
		t.Errorf("recorded source: %+v", v)
	}
}

func TestZipAddNeedsApproval(t *testing.T) {
	forgeHome(t)
	zp := mkZip(t, demoMD)
	_, errOut, err := runSplit(t, "", "skill", "add", zp)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("zip without approval: err=%v", err)
	}
	if !strings.Contains(errOut, "zip") {
		t.Errorf("preview should name the source type: %s", errOut)
	}
	if skillCount(t) != 0 {
		t.Error("nothing may be installed")
	}
	if _, _, err := runSplit(t, "", "skill", "add", "--yes", zp); err != nil {
		t.Fatal(err)
	}
	if skillCount(t) != 1 {
		t.Error("--yes installs")
	}
}

func TestLocalDirNeedsNoPromptButShowsPreview(t *testing.T) {
	forgeHome(t)
	d := demoDir(t)
	out, errOut, err := runSplit(t, "", "skill", "add", d) // stdin is not a terminal, no --yes
	if err != nil {
		t.Fatalf("a local directory must install without a prompt: %v", err)
	}
	if !strings.Contains(out, "추가됨") || !strings.Contains(errOut, "스킬 설치 미리보기") {
		t.Errorf("out=%q err=%q", out, errOut)
	}
}

func TestLocalDirWarnsOnHooksButDoesNotBlock(t *testing.T) {
	forgeHome(t)
	md := "---\nname: demo-skill\ndescription: d\nhooks:\n  Stop:\n    - command: echo done\n---\nbody\n"
	d := filepath.Join(t.TempDir(), "demo-skill")
	_ = os.MkdirAll(filepath.Join(d, "scripts"), 0o755)
	_ = os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(md), 0o644)
	_ = os.WriteFile(filepath.Join(d, "scripts", "x.sh"), []byte("#!/bin/sh\n"), 0o755)
	out, errOut, err := runSplit(t, "", "skill", "add", d)
	if err != nil {
		t.Fatalf("%v", err)
	}
	for _, w := range []string{"hooks 가 있습니다", "! hooks:", "실행 가능한 파일 1개", "scripts/x.sh", "sha256=", "인터프리터=/bin/sh"} {
		if !strings.Contains(errOut, w) {
			t.Errorf("preview lacks %q:\n%s", w, errOut)
		}
	}
	if !strings.Contains(out, "추가됨") {
		t.Errorf("out=%s", out)
	}
}

func TestStrictLocalRequiresApproval(t *testing.T) {
	forgeHome(t)
	d := demoDir(t)
	if _, _, err := runSplit(t, "", "skill", "add", "--strict-local", d); err == nil {
		t.Fatal("--strict-local without --yes/terminal must be refused")
	}
	if skillCount(t) != 0 {
		t.Error("nothing may be installed")
	}
	if _, _, err := runSplit(t, "", "skill", "add", "--strict-local", "--yes", d); err != nil {
		t.Fatal(err)
	}
	if skillCount(t) != 1 {
		t.Error("installed with --yes")
	}
}

func TestApprovalPromptInputs(t *testing.T) {
	cases := []struct {
		input   string
		install bool
	}{
		{"y\n", true}, {"Y\n", true}, {"yes\n", true}, {" YES \n", true}, {"y", true}, // last line without newline
		{"n\n", false}, {"\n", false}, {"", false}, {"yy\n", false}, {"ye\n", false}, {"ok\n", false}, {"yes please\n", false}, {"y n\n", false}, {"\x1b[A\n", false},
	}
	for _, c := range cases {
		forgeHome(t)
		asTerminal(t)
		useGit(t, &fakeGit{})
		_, errOut, err := runSplit(t, c.input, "skill", "add", gitURL)
		if c.install != (err == nil) {
			t.Errorf("input %q: err=%v (install want %v)", c.input, err, c.install)
		}
		if !strings.Contains(errOut, "[y/N]") {
			t.Errorf("input %q: no prompt shown: %s", c.input, errOut)
		}
		if got := skillCount(t); (got == 1) != c.install {
			t.Errorf("input %q: installed=%d", c.input, got)
		}
	}
}

func TestYesIsRejectedWithPreview(t *testing.T) {
	forgeHome(t)
	if _, _, err := runSplit(t, "", "skill", "add", "--preview", "--yes", demoDir(t)); err == nil {
		t.Error("--preview --yes is contradictory")
	}
}

// ---- --preview changes nothing ---------------------------------------------

func TestPreviewOnlyChangesNothing(t *testing.T) {
	home := forgeHome(t)
	mustRun(t, "init")
	mustRun(t, "skill", "add", demoDir(t)) // an existing skill so the comparison path runs too
	dbBefore, _ := os.ReadFile(filepath.Join(home, "forge.db"))
	treeBefore := snapshotTree(t, home)

	g := &fakeGit{extra: map[string]string{"new.txt": "x"}}
	useGit(t, g)
	for _, src := range []string{gitURL, mkZip(t, demoMD), demoDir(t)} {
		out, errOut, err := runSplit(t, "", "skill", "add", "--preview", src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if !strings.Contains(out, "아무것도 설치하지 않았습니다") || !strings.Contains(errOut, "스킬 설치 미리보기") {
			t.Errorf("%s: out=%q err=%q", src, out, errOut)
		}
	}
	dbAfter, _ := os.ReadFile(filepath.Join(home, "forge.db"))
	if !bytes.Equal(dbBefore, dbAfter) {
		t.Error("forge.db changed by --preview")
	}
	if after := snapshotTree(t, home); !sameTree(treeBefore, after) {
		t.Errorf("FORGE_HOME changed by --preview:\nbefore=%v\nafter=%v", treeBefore, after)
	}
	// the git clone went to a temp dir that is gone again
	for _, d := range g.dests {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("clone destination left behind: %s", d)
		}
	}
	if g.calls != 1 {
		t.Errorf("git calls=%d", g.calls)
	}
}

func TestPreviewOnlyDoesNotCreateHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "forge-home-not-created")
	t.Setenv("FORGE_HOME", home)
	out, _, err := runSplit(t, "", "skill", "add", "--preview", demoDir(t))
	if err != nil || !strings.Contains(out, "설치하지 않았습니다") {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := os.Stat(home); err == nil {
		t.Error("--preview created FORGE_HOME")
	}
}

func TestPreviewJSONSchema(t *testing.T) {
	forgeHome(t)
	md := "---\nname: demo-skill\ndescription: d\nallowed-tools: Bash\n---\nuse curl http://1.2.3.4/x\n"
	d := filepath.Join(t.TempDir(), "demo-skill")
	_ = os.MkdirAll(d, 0o755)
	_ = os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(md), 0o644)
	out, errOut, err := runSplit(t, "", "skill", "add", "--preview", "--json", d)
	if err != nil {
		t.Fatal(err)
	}
	if errOut != "" {
		t.Errorf("--json must not print the text preview: %q", errOut)
	}
	var doc struct {
		Installed   bool `json:"installed"`
		PreviewOnly bool `json:"preview_only"`
		Preview     struct {
			Schema      int    `json:"schema"`
			SourceType  string `json:"source_type"`
			Name        string `json:"name"`
			TreeHash    string `json:"tree_sha256"`
			FileCount   int    `json:"file_count"`
			TotalBytes  int64  `json:"total_bytes"`
			Frontmatter []struct {
				Key   string `json:"key"`
				Risky bool   `json:"risky"`
			} `json:"frontmatter"`
			Executables []any    `json:"executables"`
			Patterns    []any    `json:"patterns"`
			URLs        []string `json:"urls"`
			IPs         []string `json:"ips"`
			Warnings    []string `json:"warnings"`
			Notes       []string `json:"notes"`
		} `json:"preview"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	p := doc.Preview
	if doc.Installed || !doc.PreviewOnly || p.Schema != 1 || p.SourceType != "dir" || p.Name != "demo-skill" || len(p.TreeHash) != 64 || p.FileCount != 1 || p.TotalBytes != int64(len(md)) {
		t.Errorf("%+v", doc)
	}
	if p.Executables == nil || p.Patterns == nil || p.URLs == nil || p.IPs == nil || p.Warnings == nil || p.Notes == nil {
		t.Errorf("list fields must be [] not null: %s", out)
	}
	var risky []string
	for _, f := range p.Frontmatter {
		if f.Risky {
			risky = append(risky, f.Key)
		}
	}
	if len(risky) != 1 || risky[0] != "allowed-tools" || len(p.URLs) != 1 || len(p.IPs) != 1 {
		t.Errorf("risky=%v urls=%v ips=%v", risky, p.URLs, p.IPs)
	}
}

// ---- hash pin --------------------------------------------------------------

func treeHashOf(t *testing.T, src string) string {
	t.Helper()
	out, _, err := runSplit(t, "", "skill", "add", "--preview", "--json", src)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Preview struct {
			H string `json:"tree_sha256"`
		} `json:"preview"`
	}
	_ = json.Unmarshal([]byte(out), &d)
	return d.Preview.H
}

func TestSHA256Pin(t *testing.T) {
	forgeHome(t)
	d := demoDir(t)
	h := treeHashOf(t, d)
	wrong := strings.Repeat("0", 64)

	_, errOut, err := runSplit(t, "", "skill", "add", "--sha256", wrong, d)
	if err == nil || !strings.Contains(err.Error(), "다릅니다") || !strings.Contains(err.Error(), h) || !strings.Contains(err.Error(), wrong) {
		t.Fatalf("mismatch must fail showing both hashes: %v", err)
	}
	if !strings.Contains(errOut, h) {
		t.Error("the preview with the actual hash is still shown")
	}
	if skillCount(t) != 0 {
		t.Fatal("a mismatching pin must install nothing")
	}
	for _, bad := range []string{"abc", h[:63], h + "0", "zz" + h[2:], ""} {
		if bad == "" {
			continue
		}
		if _, _, err := runSplit(t, "", "skill", "add", "--sha256", bad, d); err == nil || skillCount(t) != 0 {
			t.Errorf("malformed pin %q must be refused", bad)
		}
	}
	// whitespace and case are normalized
	if _, _, err := runSplit(t, "", "skill", "add", "--sha256", " "+strings.ToUpper(h)+" ", d); err != nil {
		t.Fatalf("matching pin (upper case, padded): %v", err)
	}
	if skillCount(t) != 1 {
		t.Error("matching pin installs")
	}
}

func TestSHA256PinStillNeedsApprovalForGit(t *testing.T) {
	forgeHome(t)
	g := &fakeGit{}
	useGit(t, g)
	out, _, _ := runSplit(t, "", "skill", "add", "--preview", "--json", gitURL)
	var d struct {
		Preview struct {
			H string `json:"tree_sha256"`
		} `json:"preview"`
	}
	_ = json.Unmarshal([]byte(out), &d)
	if _, _, err := runSplit(t, "", "skill", "add", "--sha256", d.Preview.H, gitURL); err == nil {
		t.Error("a pin is not an approval: git still needs --yes")
	}
	if _, _, err := runSplit(t, "", "skill", "add", "--sha256", d.Preview.H, "--yes", gitURL); err != nil {
		t.Fatal(err)
	}
}

func TestGitRefPinning(t *testing.T) {
	forgeHome(t)
	commit := strings.Repeat("d", 40)

	g := &fakeGit{}
	useGit(t, g)
	_, errOut, err := runSplit(t, "", "skill", "add", "--preview", "--ref", "main", gitURL)
	if err != nil || !strings.Contains(errOut, "고정되지 않음: 브랜치/태그") || g.refs[0] != "main" {
		t.Errorf("branch ref: err=%v refs=%v\n%s", err, g.refs, errOut)
	}
	_, errOut, err = runSplit(t, "", "skill", "add", "--preview", "--ref", commit, gitURL)
	if err != nil || !strings.Contains(errOut, "(고정됨: 커밋 해시)") || strings.Contains(errOut, "고정되지 않음") {
		t.Errorf("commit ref must be shown as pinned: err=%v\n%s", err, errOut)
	}
	_, errOut, _ = runSplit(t, "", "skill", "add", "--preview", gitURL)
	if !strings.Contains(errOut, "(지정 안 함)") || !strings.Contains(errOut, "고정되지 않음: 기본 브랜치 HEAD") {
		t.Errorf("no ref: %s", errOut)
	}
	if _, _, err := runSplit(t, "", "skill", "add", "--preview", "--ref", "--upload-pack=x", gitURL); err == nil {
		t.Error("option-like ref must be refused")
	}
	if _, _, err := runSplit(t, "", "skill", "add", "--preview", "--ref", "main", demoDir(t)); err == nil {
		t.Error("--ref only applies to git URLs")
	}
	// recorded as <ref>@<commit>
	if _, _, err := runSplit(t, "", "skill", "add", "--yes", "--ref", commit, gitURL); err != nil {
		t.Fatal(err)
	}
	if o := mustRun(t, "skill", "show", "demo-skill", "--json"); !strings.Contains(o, commit+"@"+commit) {
		t.Errorf("source_ref not recorded with the requested ref: %s", o)
	}
}

// ---- update / check --------------------------------------------------------

const v1MD = "---\nname: demo-skill\ndescription: A demo skill.\nversion: 1.0.0\n---\n# v1\n"
const v2MD = "---\nname: demo-skill\ndescription: A demo skill.\nversion: 1.1.0\n---\n# v2\n"

func TestSkillUpdateShowsChangesAndNeedsApproval(t *testing.T) {
	home := forgeHome(t)
	src := filepath.Join(t.TempDir(), "demo-skill")
	_ = os.MkdirAll(src, 0o755)
	_ = os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(v1MD), 0o644)
	mustRun(t, "skill", "add", src)

	// unchanged source
	if out := mustRun(t, "skill", "update", "demo-skill"); !strings.Contains(out, "변경 없음") {
		t.Errorf("unchanged: %s", out)
	}

	// the source changes: SKILL.md edited, a script appears
	_ = os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(v2MD), 0o644)
	_ = os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	_, errOut, err := runSplit(t, "", "skill", "update", "demo-skill")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("update without approval must be refused: %v", err)
	}
	for _, w := range []string{"스킬 업데이트 미리보기", "[기존 설치본과의 변경]", "현재 버전 1.0.0", "추가된 파일", "run.sh", "변경된 파일", "SKILL.md", "새로 생긴 실행 파일"} {
		if !strings.Contains(errOut, w) {
			t.Errorf("change summary lacks %q:\n%s", w, errOut)
		}
	}
	if o := mustRun(t, "skill", "show", "demo-skill", "--json"); strings.Count(o, `"version": "`) != 1 {
		t.Errorf("a refused update must not add a version: %s", o)
	}

	out, _, err := runSplit(t, "", "skill", "update", "--yes", "demo-skill")
	if err != nil || !strings.Contains(out, "새 버전 추가: demo-skill@1.1.0") {
		t.Fatalf("%v %s", err, out)
	}
	var info struct {
		CurrentVersion string `json:"current_version"`
		Versions       []struct {
			Version string `json:"version"`
			Current bool   `json:"current"`
		}
	}
	_ = json.Unmarshal([]byte(mustRun(t, "skill", "show", "demo-skill", "--json")), &info)
	if info.CurrentVersion != "1.1.0" || len(info.Versions) != 2 {
		t.Errorf("%+v", info)
	}
	for _, v := range []string{"demo-skill@1.0.0", "demo-skill@1.1.0"} {
		if _, err := os.Stat(filepath.Join(home, "skills", v, "SKILL.md")); err != nil {
			t.Errorf("version snapshot %s must be kept: %v", v, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(home, "skills", "demo-skill@1.0.0", "SKILL.md"))
	if !strings.Contains(string(b), "# v1") {
		t.Error("old immutable version was modified")
	}
}

func TestSkillUpdateGitFollowsRecordedRef(t *testing.T) {
	forgeHome(t)
	g := &fakeGit{skillMD: v1MD, commit: strings.Repeat("1", 40)}
	useGit(t, g)
	mustRun(t, "skill", "add", "--yes", "--ref", "release", gitURL)
	if g.refs[len(g.refs)-1] != "release" {
		t.Fatal("setup")
	}
	g.skillMD, g.commit = v2MD, strings.Repeat("2", 40)
	_, errOut, err := runSplit(t, "", "skill", "update", "--yes", "demo-skill")
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if g.refs[len(g.refs)-1] != "release" {
		t.Errorf("update must fetch the recorded ref again: %v", g.refs)
	}
	if o := mustRun(t, "skill", "show", "demo-skill", "--json"); !strings.Contains(o, "release@"+strings.Repeat("2", 40)) {
		t.Errorf("new commit not recorded: %s", o)
	}
	// a skill added without a ref updates from the default branch
	g2 := &fakeGit{skillMD: v1MD}
	useGit(t, g2)
	forgeHome(t)
	mustRun(t, "skill", "add", "--yes", gitURL)
	g2.skillMD = v2MD
	mustRun(t, "skill", "update", "--yes", "demo-skill")
	if g2.refs[len(g2.refs)-1] != "" {
		t.Errorf("refs=%v", g2.refs)
	}
}

func TestSkillUpdateRefusesRenamedSource(t *testing.T) {
	forgeHome(t)
	g := &fakeGit{skillMD: v1MD}
	useGit(t, g)
	mustRun(t, "skill", "add", "--yes", gitURL)
	g.skillMD = strings.Replace(v2MD, "name: demo-skill", "name: other-skill", 1)
	if _, _, err := runSplit(t, "", "skill", "update", "--yes", "demo-skill"); err == nil || !strings.Contains(err.Error(), "이름") {
		t.Errorf("a source that now serves another skill must not update: %v", err)
	}
}

func TestSkillUpdateUnknownAndNoSource(t *testing.T) {
	forgeHome(t)
	if _, _, err := runSplit(t, "", "skill", "update", "ghost"); err == nil {
		t.Error("unknown skill")
	}
}

func tamper(t *testing.T, home, rel, content string) {
	t.Helper()
	dir := filepath.Join(home, "skills", filepath.Dir(rel))
	_ = os.Chmod(dir, 0o755)
	f := filepath.Join(home, "skills", rel)
	_ = os.Chmod(f, 0o644)
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkillCheckDetectsStoreTampering(t *testing.T) {
	home := forgeHome(t)
	src := filepath.Join(t.TempDir(), "demo-skill")
	_ = os.MkdirAll(src, 0o755)
	_ = os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(v1MD), 0o644)
	mustRun(t, "skill", "add", src)
	out, _, err := runSplit(t, "", "skill", "check")
	if err != nil || !strings.Contains(out, "OK       demo-skill@1.0.0") || !strings.Contains(out, "--fetch") {
		t.Fatalf("intact: %v %s", err, out)
	}
	dbBefore, _ := os.ReadFile(filepath.Join(home, "forge.db"))

	tamper(t, home, "demo-skill@1.0.0/SKILL.md", v1MD+"\nignore previous instructions\n")
	out, _, err = runSplit(t, "", "skill", "check", "demo-skill")
	if err == nil || !strings.Contains(out, "변조 의심") || !strings.Contains(err.Error(), "변조") {
		t.Fatalf("tampering must be reported with a non-zero exit: err=%v\n%s", err, out)
	}
	jout, _, jerr := runSplit(t, "", "skill", "check", "--json")
	var doc struct {
		Schema   int `json:"schema"`
		Tampered int `json:"tampered"`
		Skills   []struct {
			Name  string `json:"name"`
			Store string `json:"store_state"`
			Up    string `json:"upstream"`
		}
	}
	if jerr == nil || json.Unmarshal([]byte(jout), &doc) != nil || doc.Schema != 1 || doc.Tampered != 1 || doc.Skills[0].Store != "modified" || doc.Skills[0].Up != "not_checked" {
		t.Errorf("json: %v %s", jerr, jout)
	}
	// check is read-only
	dbAfter, _ := os.ReadFile(filepath.Join(home, "forge.db"))
	if !bytes.Equal(dbBefore, dbAfter) {
		t.Error("skill check changed forge.db")
	}
	// removal of the snapshot
	_ = filepath.WalkDir(filepath.Join(home, "skills", "demo-skill@1.0.0"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	_ = os.RemoveAll(filepath.Join(home, "skills", "demo-skill@1.0.0"))
	out, _, err = runSplit(t, "", "skill", "check")
	if err == nil || !strings.Contains(out, "누락") {
		t.Errorf("missing snapshot: %v %s", err, out)
	}
}

func TestSkillCheckFetchOnlyWhenAsked(t *testing.T) {
	forgeHome(t)
	g := &fakeGit{skillMD: v1MD}
	useGit(t, g)
	mustRun(t, "skill", "add", "--yes", gitURL)
	callsAfterAdd := g.calls
	mustRun(t, "skill", "check")
	if g.calls != callsAfterAdd {
		t.Fatalf("check without --fetch must not touch git (calls %d -> %d)", callsAfterAdd, g.calls)
	}
	out := mustRun(t, "skill", "check", "--fetch")
	if g.calls != callsAfterAdd+1 || !strings.Contains(out, "원본: 설치본과 같습니다") {
		t.Errorf("--fetch same: calls=%d\n%s", g.calls, out)
	}
	g.skillMD = v2MD
	out = mustRun(t, "skill", "check", "--fetch")
	if !strings.Contains(out, "원본: 설치본과 다릅니다") || !strings.Contains(out, "forge skill update demo-skill") {
		t.Errorf("--fetch changed: %s", out)
	}
	// an unreachable source is reported, not fatal
	skillGit = &failingGit{}
	out = mustRun(t, "skill", "check", "--fetch")
	if !strings.Contains(out, "확인 실패") {
		t.Errorf("fetch failure: %s", out)
	}
}

type failingGit struct{}

func (failingGit) Clone(context.Context, string, string) (string, error) {
	return "", io.ErrUnexpectedEOF
}

func TestSkillCheckNoStoreAndEmpty(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FORGE_HOME", filepath.Join(root, "none"))
	if _, _, err := runSplit(t, "", "skill", "check"); err == nil {
		t.Error("no store")
	}
	if _, err := os.Stat(filepath.Join(root, "none")); err == nil {
		t.Error("check must not create the store")
	}
	forgeHome(t)
	mustRun(t, "init")
	if out := mustRun(t, "skill", "check"); !strings.Contains(out, "등록된 스킬이 없습니다") {
		t.Errorf("%s", out)
	}
}

// ---- hostile input end to end ----------------------------------------------

func TestPreviewOutputIsTerminalSafe(t *testing.T) {
	forgeHome(t)
	const esc = "\x1b[31m"
	const bidi = "\u202e"
	md := "---\nname: demo-skill\ndescription: \"red\\u001b[31m text\\u202edcba\"\nhooks: \"\\u001b[2J\"\n---\nvisit https://x.example.com/a" + bidi + "b and 10.1.2.3\n"
	d := filepath.Join(t.TempDir(), "demo-skill")
	_ = os.MkdirAll(filepath.Join(d, "scripts"), 0o755)
	_ = os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(md), 0o644)
	_ = os.WriteFile(filepath.Join(d, "scripts", esc+"run"+bidi+".sh"), []byte("#!/bin/sh\n"), 0o755)
	for _, args := range [][]string{{"skill", "add", "--preview", d}, {"skill", "add", d}} {
		out, errOut, err := runSplit(t, "", args...)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{out, errOut} {
			for _, r := range strings.ReplaceAll(s, "\n", "") {
				if textsafe.NeedsEscape(r) {
					t.Fatalf("%v: output contains the raw rune %U: %q", args, r, s)
				}
			}
		}
		if !strings.Contains(errOut, `\x1b[31m`) || !strings.Contains(errOut, `\u202e`) {
			t.Errorf("hostile characters must be visible escapes:\n%s", errOut)
		}
		if !strings.Contains(errOut, "hooks 가 있습니다") {
			t.Errorf("hooks warning missing")
		}
	}
}

func TestExistingSkillChangeSummaryOnAdd(t *testing.T) {
	forgeHome(t)
	d := demoDir(t)
	mustRun(t, "skill", "add", d)
	_ = os.WriteFile(filepath.Join(d, "extra.sh"), []byte("#!/bin/sh\n"), 0o755)
	_, errOut, err := runSplit(t, "", "skill", "add", d)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"[기존 설치본과의 변경]", "추가된 파일", "extra.sh", "새로 생긴 실행 파일"} {
		if !strings.Contains(errOut, w) {
			t.Errorf("lacks %q:\n%s", w, errOut)
		}
	}
}

// /dev/null is a character device but not a terminal: `forge skill add <git> </dev/null`
// (what scripts and the demo do) must be refused with the --yes guidance, not
// answered by an imaginary prompt.
func TestDevNullAndPipesAreNotTerminals(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip("no /dev/null")
	}
	defer f.Close()
	if stdinIsTerminal(f) {
		t.Error("/dev/null must not count as a terminal")
	}
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	if stdinIsTerminal(r) {
		t.Error("a pipe is not a terminal")
	}
	if stdinIsTerminal(strings.NewReader("y\n")) {
		t.Error("a reader is not a terminal")
	}
}
