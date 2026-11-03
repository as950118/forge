package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/as950118/forge/internal/mcp"
)

// fakeToken has the shape of a GitHub token (known prefix) and is not a real one.
func fakeToken() string { return "ghp_" + "1a2B3c4D5e6F7g8H9i0J1k2L3m4N5o6P7q8R" }

func writeDiffOp(path, before, after string) *Plan {
	p := &Plan{}
	op := Op{Kind: OpWriteFile, Path: path, Data: []byte(after), BeforeData: []byte(before), Before: FileState{Exists: before != ""}}
	p.Ops = append(p.Ops, op)
	BuildDiffs(p)
	return p
}

func allDiff(p *Plan) string {
	var sb strings.Builder
	for _, d := range p.Diffs {
		sb.WriteString(d.Diff)
	}
	return sb.String()
}

// AC-3: key names `pwd`, nested `credentials.pw` and the `-p VALUE` flag.
func TestDiffMasksPwdKeyNestedPwAndShortFlag(t *testing.T) {
	cases := map[string]string{
		"pwd key":        `{"db": {"pwd": "FAKE-VALUE-1"}}`,
		"db_pw key":      `{"db_pw": "FAKE-VALUE-1"}`,
		"camel rootPass": `{"rootPass": "FAKE-VALUE-1"}`,
		"nested pw":      `{"credentials": {"pw": "FAKE-VALUE-1"}}`,
		"-p array":       `{"args": ["mysql", "-p", "FAKE-VALUE-1"]}`,
		"-p array lines": "{\n  \"args\": [\n    \"-p\",\n    \"FAKE-VALUE-1\"\n  ]\n}\n",
		"--pwd array":    `{"args": ["--pwd", "FAKE-VALUE-1"]}`,
		"--pw=value":     `{"args": ["--pw=FAKE-VALUE-1"]}`,
		"--pwd=value":    "{\n  \"args\": [\n    \"--pwd=FAKE-VALUE-1\"\n  ]\n}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if out := MaskJSONDisplay(src); strings.Contains(out, "FAKE-VALUE-1") {
				t.Fatalf("leaked by MaskJSONDisplay: %s", out)
			}
			out := allDiff(writeDiffOp("/x/cfg.json", "{}\n", src+"\n"))
			if strings.Contains(out, "FAKE-VALUE-1") {
				t.Fatalf("leaked in the diff:\n%s", out)
			}
		})
	}
}

func TestDiffKeepsOrdinaryWordsReadable(t *testing.T) {
	src := `{"authors": ["Ada Lovelace"], "author": "Ada", "keywords": ["sync"], "bypass": "yes", "pass": true, "mcp": {"args": ["-y", "pkg"], "--port": "8080"}}`
	out := MaskJSONDisplay(src)
	for _, keep := range []string{"Ada Lovelace", `"Ada"`, `"sync"`, `"yes"`, `"pkg"`, "8080"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%s was masked: %s", keep, out)
		}
	}
	// registry-side rule is unchanged: "authors" still looks secret there
	if !mcp.LooksSecret("authors") {
		t.Skip("registry key rule changed")
	}
}

// AC-14: a number directly under a secret key is masked (not only inside a subtree).
func TestDiffMasksDirectNumberUnderSecretKey(t *testing.T) {
	for _, src := range []string{`{"password": 123456}`, "{\n  \"pin_token\": 987654,\n  \"port\": 8080\n}\n", `{"x": {"pwd": 424242}}`} {
		out := MaskJSONDisplay(src)
		for _, n := range []string{"123456", "987654", "424242"} {
			if strings.Contains(out, n) {
				t.Errorf("number %s leaked: %s", n, out)
			}
		}
		if strings.Contains(src, "8080") && !strings.Contains(out, "8080") {
			t.Errorf("non-secret number was masked: %s", out)
		}
	}
}

// AC-3: the line-level value-pattern masker (masker.line) is the only defense for
// text that is not JSON and for a token under a harmless key.
func TestDiffLineMaskerCatchesValuesWithoutKeyOrJSON(t *testing.T) {
	tok := fakeToken()
	text := "# notes\ncurl -H 'Authorization: Bearer abcdef0123456789abcdef' https://example.test\nexport TOKEN=" + tok + "\n"
	out := allDiff(writeDiffOp("/x/skill/SKILL.md", "# notes\n", text))
	if strings.Contains(out, tok) || strings.Contains(out, "abcdef0123456789abcdef") {
		t.Fatalf("a secret in non-JSON text leaked:\n%s", out)
	}
	if !strings.Contains(out, "# notes") || !strings.Contains(out, "***") {
		t.Fatalf("unexpected diff:\n%s", out)
	}
	js := `{"description": "` + tok + `", "note": "plain"}` + "\n"
	out = allDiff(writeDiffOp("/x/cfg.json", "{}\n", js))
	if strings.Contains(out, tok) {
		t.Fatalf("a token under a harmless key leaked:\n%s", out)
	}
	if !strings.Contains(out, "plain") {
		t.Fatalf("an ordinary value was masked:\n%s", out)
	}
}

// AC-4: a change that only differs in a secret value is announced, since both
// sides read "***".
func TestDiffAnnouncesSecretOnlyChange(t *testing.T) {
	before := "{\n  \"env\": {\n    \"API_TOKEN\": \"FAKE-OLD-1\",\n    \"MODE\": \"a\"\n  }\n}\n"
	after := "{\n  \"env\": {\n    \"API_TOKEN\": \"FAKE-NEW-2\",\n    \"MODE\": \"a\"\n  }\n}\n"
	p := writeDiffOp("/x/cfg.json", before, after)
	out := allDiff(p)
	if strings.Contains(out, "FAKE-") {
		t.Fatalf("leak:\n%s", out)
	}
	if len(p.Diffs) != 1 || p.Diffs[0].SecretChanges != 1 || !strings.Contains(out, "비밀 값 변경: 1줄") {
		t.Fatalf("secret-only change not announced (%+v):\n%s", p.Diffs, out)
	}
	// A visible change next to it is still shown, and the hidden one is still counted.
	p = writeDiffOp("/x/cfg.json", before, strings.Replace(after, `"a"`, `"b"`, 1))
	if p.Diffs[0].SecretChanges != 1 || !strings.Contains(allDiff(p), `+    "MODE": "b"`) {
		t.Fatalf("mixed change:\n%s", allDiff(p))
	}
	// No secret involved: nothing announced.
	if p := writeDiffOp("/x/cfg.json", `{"a":"1"}`+"\n", `{"a":"2"}`+"\n"); p.Diffs[0].SecretChanges != 0 || strings.Contains(allDiff(p), "비밀 값 변경") {
		t.Fatal("announced a change that is not secret")
	}
	// Text of a plain file shows both masked lines (identical). TASK-0027 AC-18
	// changed this on purpose: such a change is now announced as well, see
	// TestDiffAnnouncesSecretOnlyChangeInText.
	if p := writeDiffOp("/x/a.md", "token: FAKEVALUE1\n", "token: FAKEVALUE2\n"); !strings.Contains(allDiff(p), "token: ***") {
		t.Fatalf("the masked text lines disappeared: %+v", p.Diffs)
	}
}

// AC-9: a hard link to a file outside the tree is refused by TreeHash and copyTree
// and never read into a diff.
func TestSkillTreeHardLinkIsRefused(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("TOPSECRET-DATA-1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "SKILL.md"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(tree, "linked.txt")); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	if _, _, err := TreeHash(tree, false); !errors.Is(err, ErrHardLinked) {
		t.Errorf("TreeHash: want ErrHardLinked, got %v", err)
	}
	if err := copyTree(tree, filepath.Join(root, "copy"), 0o755, SnapshotMode); !errors.Is(err, ErrHardLinked) {
		t.Errorf("copyTree: want ErrHardLinked, got %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "copy", "linked.txt")); err == nil && strings.Contains(string(b), "TOPSECRET") {
		t.Error("copyTree copied the hard-linked content")
	}
	for name, op := range map[string]Op{
		"delete": {Kind: OpDeleteTree, Path: tree, Before: FileState{Exists: true}},
		"replace": {Kind: OpReplaceTree, Path: filepath.Join(root, "other"), Tree: &TreeSpec{SrcDir: tree},
			Before: FileState{}},
	} {
		p := &Plan{Ops: []Op{op}}
		BuildDiffs(p)
		out := allDiff(p)
		if strings.Contains(out, "TOPSECRET") {
			t.Errorf("%s: the hard-linked content is in the diff:\n%s", name, out)
		}
		if !strings.Contains(out, "하드링크") {
			t.Errorf("%s: the diff does not say the file was skipped:\n%s", name, out)
		}
	}
}

// AC-10: the remedy comes first, the path appears exactly once and a very long
// path does not cut the advice off.
func TestHardLinkMessageRemedyFirstPathOnce(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	long := root
	for len(long) < 300 {
		long = filepath.Join(long, strings.Repeat("d", 40))
	}
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Skip(err)
	}
	cfg := filepath.Join(long, "opencode.json")
	if err := os.WriteFile(cfg, []byte("{}\n"), 0o644); err != nil {
		t.Skip(err)
	}
	if err := os.Link(cfg, filepath.Join(root, "other-name.json")); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	_, err := LoadDoc(root, cfg, 0o644)
	if err == nil || !errors.Is(err, ErrHardLinked) {
		t.Fatalf("want ErrHardLinked, got %v", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "링크를 풀고 복사본으로 교체하세요") || !strings.Contains(msg, "하드링크된 설정 파일은 지원하지 않습니다") {
		t.Errorf("remedy is not first: %q", msg)
	}
	if n := strings.Count(msg, "dddd"); n == 0 || strings.Count(msg, "opencode.json") > 1 {
		t.Errorf("the path is missing or repeated (%d): %q", strings.Count(msg, "opencode.json"), msg)
	}
	if _, _, _, err := ReadFileSafe(cfg); err == nil || strings.Count(err.Error(), cfg) != 1 {
		t.Errorf("ReadFileSafe prints the path %d times: %v", strings.Count(err.Error(), cfg), err)
	}
}

// AC-11: incomplete manifest/journal temp files of a killed process in a backup
// directory are found and removed - exact name shape, dead pid, never through links.
func TestStaleBackupTempsAreCleaned(t *testing.T) {
	bdir, _ := filepath.EvalSymlinks(t.TempDir())
	id := "20260101T000000.000Z-abcdef"
	dir := filepath.Join(bdir, id)
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o700); err != nil {
		t.Fatal(err)
	}
	const dead = "9999999"
	stale := []string{".forge-tmp-" + dead + "-0badf00d-manifest.json", ".forge-tmp-" + dead + "-0badf00d-journal.json"}
	keep := []string{
		"manifest.json", "journal.json",
		".forge-tmp-" + dead + "-0badf00d-other.json",     // not a backup file name
		".forge-tmp-" + dead + "-nothex12-manifest.json",  // bad hex
		".forge-tmp-notapid-0badf00d-manifest.json",       // bad pid
		"x.forge-tmp-" + dead + "-0badf00d-manifest.json", // not a prefix match
		".forge-tmp-" + dead + "-0badf00d-manifest.json.bak",
	}
	live := ".forge-tmp-" + itoa(os.Getppid()) + "-0badf00d-journal.json" // a live process (our parent)
	keep = append(keep, live)
	for _, n := range append(append([]string{}, stale...), keep...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	victim := filepath.Join(bdir, "victim.txt")
	os.WriteFile(victim, []byte("precious"), 0o600)
	link := ".forge-tmp-" + dead + "-feedface-manifest.json"
	if err := os.Symlink(victim, filepath.Join(dir, link)); err != nil {
		t.Fatal(err)
	}
	// a symlinked backup directory is not entered
	elsewhere, _ := filepath.EvalSymlinks(t.TempDir())
	os.WriteFile(filepath.Join(elsewhere, stale[0]), []byte("x"), 0o600)
	if err := os.Symlink(elsewhere, filepath.Join(bdir, "20260102T000000.000Z-abcdef")); err != nil {
		t.Fatal(err)
	}
	// a directory named like a temp file is not removed
	os.MkdirAll(filepath.Join(dir, ".forge-tmp-"+dead+"-0badf00d-journal.json.d"), 0o700)

	got := FindStaleBackupTemps(bdir)
	if len(got) != len(stale) {
		t.Fatalf("want exactly %v, got %v", stale, got)
	}
	removed := CleanStaleBackupTemps(bdir)
	if len(removed) != len(stale) {
		t.Fatalf("cleaned %v", removed)
	}
	for _, n := range stale {
		if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
			t.Errorf("%s survived", n)
		}
	}
	for _, n := range append(keep, link) {
		if _, err := os.Lstat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s was removed: %v", n, err)
		}
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "precious" {
		t.Error("a file behind a link was touched")
	}
	if _, err := os.Stat(filepath.Join(elsewhere, stale[0])); err != nil {
		t.Error("a symlinked backup directory was entered")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

// AC-14: __proto__ is refused as an env/header name.
func TestCheckValueKeyRefusesProto(t *testing.T) {
	for _, slot := range []string{mcp.SlotEnv, mcp.SlotHeader} {
		if err := CheckValueKey(slot, "__proto__"); err == nil {
			t.Errorf("%s: __proto__ accepted", slot)
		}
		if err := CheckValueKey(slot, "X_proto"); err != nil {
			t.Errorf("%s: X_proto refused: %v", slot, err)
		}
	}
}
