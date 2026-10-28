package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TASK-0027 AC-16 / AC-18 / AC-19: tests for rules that mutation runs showed to be
// unguarded, and for the display-masking gaps. Every secret here is made up.

// ---- AC-16: secretFlag ----

func TestSecretFlagCaseEqualsAndWords(t *testing.T) {
	yes := []string{"-p", "-P", "--token", "--TOKEN", "--Api-Key", "--pwd", "--db-pw", "--passphrase", "--pin"}
	no := []string{"-x", "-pp", "--port", "--pin=1", "--token=abc", "-p=x", "--pass=abc", "token", "", "--bypass", "--pinned"}
	for _, s := range yes {
		if !secretFlag(s) {
			t.Errorf("secretFlag(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if secretFlag(s) {
			t.Errorf("secretFlag(%q) = true, want false", s)
		}
	}
}

func TestMaskJSONDisplayFlagArrayElements(t *testing.T) {
	// "-P" (upper case) takes a secret value like "-p".
	if out := MaskJSONDisplay(`{"args": ["mysql", "-P", "FAKE-VALUE-1"]}`); strings.Contains(out, "FAKE-VALUE-1") {
		t.Errorf("-P value leaked: %s", out)
	}
	// "--flag=value" is one element: the value is masked in place and the NEXT
	// element is an ordinary argument (the "=" skip of secretFlag).
	out := MaskJSONDisplay(`{"args": ["--pass=FAKE-VALUE-2", "visible-one"]}`)
	if strings.Contains(out, "FAKE-VALUE-2") || !strings.Contains(out, "visible-one") {
		t.Errorf("--pass=value handling wrong: %s", out)
	}
	// A reference is not a secret: it stays, and it does not mask the next element either.
	out = MaskJSONDisplay(`{"args": ["--token=${API_TOK}", "visible-two", "--pass=${DB_PASS}", "visible-three"]}`)
	for _, keep := range []string{"${API_TOK}", "visible-two", "${DB_PASS}", "visible-three"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%s was masked: %s", keep, out)
		}
	}
	// The line masker (one element per line) behaves the same.
	lines := "{\n  \"args\": [\n    \"--token=${API_TOK}\",\n    \"visible-four\",\n    \"--pass=FAKE-VALUE-3\"\n  ]\n}\n"
	d := allDiff(writeDiffOp("/x/cfg.json", "{}\n", lines))
	if strings.Contains(d, "FAKE-VALUE-3") || !strings.Contains(d, "visible-four") || !strings.Contains(d, "${API_TOK}") {
		t.Errorf("line masker:\n%s", d)
	}
}

// ---- AC-16: hiddenSecretChanges counts added lines too ----

func TestHiddenSecretChangesWhenLinesAreAdded(t *testing.T) {
	before := "{\n  \"a_token\": \"FAKE-OLD-1\",\n  \"mode\": \"x\"\n}\n"
	after := "{\n  \"a_token\": \"FAKE-NEW-1\",\n  \"b_token\": \"FAKE-NEW-2\",\n  \"c_token\": \"FAKE-NEW-3\",\n  \"mode\": \"x\"\n}\n"
	p := writeDiffOp("/x/cfg.json", before, after)
	out := allDiff(p)
	if strings.Contains(out, "FAKE-") {
		t.Fatalf("leak:\n%s", out)
	}
	// raw diff: -1 +3; masked diff: +2 (one pair is identical once masked) => 1 hidden.
	if p.Diffs[0].SecretChanges != 1 || !strings.Contains(out, "비밀 값 변경: 1줄") {
		t.Fatalf("want 1 hidden change, got %d:\n%s", p.Diffs[0].SecretChanges, out)
	}
	// Only removed lines: three secret lines turn into one that reads the same.
	p = writeDiffOp("/x/cfg.json", after, before)
	if p.Diffs[0].SecretChanges != 1 {
		t.Fatalf("reverse: want 1 hidden change, got %d:\n%s", p.Diffs[0].SecretChanges, allDiff(p))
	}
}

// ---- AC-16: IsManagedSkillDir ----

func TestIsManagedSkillDir(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(dir, name string, managedBy string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(marker{ManagedBy: managedBy, Name: name, Version: "1.0.0", TreeHash: strings.Repeat("a", 64)})
		if err := os.WriteFile(filepath.Join(dir, MarkerName), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ok := filepath.Join(base, ".claude", "skills", "demo-skill")
	mk(ok, "demo-skill", "forge")
	if !IsManagedSkillDir(ok, "demo-skill") {
		t.Fatal("a managed directory is not recognized")
	}
	if IsManagedSkillDir(ok, "other-skill") {
		t.Error("the marker names another skill: must not count")
	}
	other := filepath.Join(base, "x", "skills", "demo-skill")
	mk(other, "someone-else", "forge")
	if IsManagedSkillDir(other, "demo-skill") {
		t.Error("marker name mismatch accepted")
	}
	notForge := filepath.Join(base, "y", "skills", "demo-skill")
	mk(notForge, "demo-skill", "someone")
	if IsManagedSkillDir(notForge, "demo-skill") {
		t.Error("a marker of another manager accepted")
	}
	noMarker := filepath.Join(base, "z", "skills", "demo-skill")
	if err := os.MkdirAll(noMarker, 0o755); err != nil {
		t.Fatal(err)
	}
	if IsManagedSkillDir(noMarker, "demo-skill") {
		t.Error("a directory without a marker accepted")
	}
	if IsManagedSkillDir(ok, "Bad Name") || IsManagedSkillDir(filepath.Join(base, "missing"), "demo-skill") {
		t.Error("an invalid name or a missing directory accepted")
	}
	// The directory itself is a link to a managed one.
	link := filepath.Join(base, "link", "skills", "demo-skill")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ok, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if IsManagedSkillDir(link, "demo-skill") {
		t.Error("a symbolic link to a managed directory accepted")
	}
	// A parent component is a link: Lstat of the leaf alone would say "real directory".
	parentLink := filepath.Join(base, "viaparent")
	if err := os.Symlink(filepath.Join(base, ".claude"), parentLink); err != nil {
		t.Fatal(err)
	}
	if IsManagedSkillDir(filepath.Join(parentLink, "skills", "demo-skill"), "demo-skill") {
		t.Error("a directory below a symbolic link accepted")
	}
}

// ---- AC-16: LoadDoc, a path that cannot be checked (not a link) ----

func TestLoadDocUncheckablePathIsBlockedNotMislabelledAsLink(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)
	d, err := LoadDoc(root, filepath.Join(locked, "sub", "cfg.json"), 0o644)
	if err != nil {
		t.Fatalf("an uncheckable path must block the item, not fail the whole sync: %v", err)
	}
	if !strings.Contains(d.Blocked, "경로를 확인할 수 없어") || strings.Contains(d.Blocked, "심볼릭") {
		t.Errorf("wrong reason: %q", d.Blocked)
	}
	if _, ok := d.Op(); ok {
		t.Error("a blocked document produced an operation")
	}
	// A real link still says so.
	_ = os.Chmod(locked, 0o755)
	target := filepath.Join(root, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "lnk")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	d, err = LoadDoc(root, filepath.Join(root, "lnk", "cfg.json"), 0o644)
	if err != nil || !strings.Contains(d.Blocked, "심볼릭 링크") {
		t.Errorf("symlink: %v %q", err, d.Blocked)
	}
}

// ---- AC-16 / AC-17: resumeInterruptedRestores ----

func writeTestJournal(t *testing.T, bdir string, j *Journal) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(bdir, j.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(filepath.Join(bdir, j.ID), j); err != nil {
		t.Fatal(err)
	}
}

func TestResumeInterruptedRestoresMatchesOnlyThisRestore(t *testing.T) {
	bdir := t.TempDir()
	root := "/r/proj"
	const (
		mID     = "20260101T000000.000Z-aaaaaa"
		otherID = "20260101T000001.000Z-bbbbbb"
		by      = "20260101T000009.000Z-cccccc"
	)
	hashA := strings.Repeat("a", 64)
	m := &Manifest{Version: 1, ID: mID, Kind: "sync", Target: "claude-code", Scope: ScopeProject, Root: root,
		Entries: []Entry{
			{Path: "/r/proj/.mcp.json", Before: FileState{Exists: true, Kind: "file", Hash: hashA}},
			{Path: "/r/proj/new.json", Before: FileState{Exists: false}},
		}}
	mkj := func(id string, mod func(*Journal)) *Journal {
		j := &Journal{Version: JournalVersion, ID: id, Kind: "restore", Target: "claude-code", Scope: ScopeProject, Root: root, Status: JournalPending, RestoreOf: mID}
		if mod != nil {
			mod(j)
		}
		return j
	}
	ids := map[string]*Journal{}
	add := func(suffix string, mod func(*Journal)) string {
		id := "20260102T0000" + suffix + ".000Z-" + strings.Repeat(suffix[:1], 6)
		ids[id] = mkj(id, mod)
		writeTestJournal(t, bdir, ids[id])
		return id
	}
	match := add("01", nil)
	otherBackup := add("02", func(j *Journal) { j.RestoreOf = otherID })
	otherTarget := add("03", func(j *Journal) { j.Target = "opencode" })
	otherScope := add("04", func(j *Journal) { j.Scope = ScopeUser })
	otherRoot := add("05", func(j *Journal) { j.Root = "/r/other" })
	asSync := add("06", func(j *Journal) { j.Kind = "sync" }) // a sync journal that (forged) names restore_of
	legacyOK := add("07", func(j *Journal) {
		j.RestoreOf = ""
		j.Ops = []JournalOp{{Path: "/r/proj/.mcp.json", Op: OpWriteFile, AfterExists: true, AfterHash: hashA}, {Path: "/r/proj/new.json", Op: OpDeleteFile, BeforeExists: true, BeforeHash: hashA}}
	})
	legacyWrongHash := add("08", func(j *Journal) {
		j.RestoreOf = ""
		j.Ops = []JournalOp{{Path: "/r/proj/.mcp.json", Op: OpWriteFile, AfterExists: true, AfterHash: strings.Repeat("b", 64)}}
	})
	legacyForeignPath := add("09", func(j *Journal) {
		j.RestoreOf = ""
		j.Ops = []JournalOp{{Path: "/r/proj/other.json", Op: OpDeleteFile, BeforeExists: true, BeforeHash: hashA}}
	})
	legacyNoOps := add("10", func(j *Journal) { j.RestoreOf = "" })
	legacyExistsMismatch := add("11", func(j *Journal) {
		j.RestoreOf = ""
		j.Ops = []JournalOp{{Path: "/r/proj/new.json", Op: OpWriteFile, AfterExists: true, AfterHash: hashA}}
	})

	resumeInterruptedRestores(bdir, m, by)

	status := func(id string) string {
		j, err := loadJournal(bdir, id)
		if err != nil {
			t.Fatal(err)
		}
		return j.Status
	}
	for _, id := range []string{match, legacyOK} {
		if s := status(id); s != JournalRecovered {
			t.Errorf("%s should be recovered, is %s", id, s)
		}
	}
	for name, id := range map[string]string{"other backup": otherBackup, "other target": otherTarget, "other scope": otherScope,
		"other root": otherRoot, "sync journal": asSync, "legacy wrong hash": legacyWrongHash,
		"legacy foreign path": legacyForeignPath, "legacy no ops": legacyNoOps, "legacy exists mismatch": legacyExistsMismatch} {
		if s := status(id); s != JournalPending {
			t.Errorf("%s must stay pending, is %s", name, s)
		}
	}
}

// ---- AC-18: display masking gaps ----

func TestDisplayMaskPassphrasePinGluedFlagCommandAndURL(t *testing.T) {
	cases := map[string]string{
		"passphrase key":      `{"ssh": {"passphrase": "FAKESEC-1"}}`,
		"camel passphrase":    `{"keyFile": "x", "sshPassphrase": "FAKESEC-1"}`,
		"pin string":          `{"pin": "FAKESEC-1"}`,
		"pin number":          `{"pin": 991122}`,
		"pin_code":            `{"PIN_CODE": "FAKESEC-1"}`,
		"pin array":           `{"pin": ["FAKESEC-1"]}`,
		"--pin flag":          `{"args": ["--pin", "FAKESEC-1"]}`,
		"glued -p":            `{"args": ["mysql", "-pFAKESEC-1"]}`,
		"glued -p lines":      "{\n  \"args\": [\n    \"-pFAKESEC-1\"\n  ]\n}\n",
		"command -p":          `{"command": "mysql -u root -p FAKESEC-1 db"}`,
		"command glued":       `{"command": "mysql -pFAKESEC-1 db"}`,
		"command --password":  `{"command": "tool --password FAKESEC-1 run"}`,
		"command password=":   `{"command": "tool password=FAKESEC-1 run"}`,
		"sh -c":               `{"args": ["sh", "-c", "mysql --password=FAKESEC-1 db"]}`,
		"url credentials":     `{"url": "https://bob:FAKESEC-1@example.com/x"}`,
		"url in note":         `{"note": "see https://bob:FAKESEC-1@example.com/x"}`,
		"url multi-line":      "{\n  \"url\": \"https://bob:FAKESEC-1@example.com/x\"\n}\n",
		"url one line object": `{"mcp": {"srv": {"url": "https://bob:FAKESEC-1@example.com/x"}}}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeDiffOp("/x/cfg.json", "{}\n", src)
			out := allDiff(p)
			for _, leak := range []string{"FAKESEC-1", "991122"} {
				if strings.Contains(out, leak) {
					t.Fatalf("leaked %s in the diff:\n%s", leak, out)
				}
			}
			if !strings.Contains(out, "***") {
				t.Fatalf("nothing masked:\n%s", out)
			}
		})
	}
}

// The new rules must not hide ordinary text: a corpus of values that are not secrets.
func TestDisplayMaskKeepsOrdinaryValuesReadable(t *testing.T) {
	src := "{\n" +
		"  \"pinned\": \"v1.2.3\",\n" +
		"  \"mapping\": \"a-b\",\n" +
		"  \"spinner\": \"dots\",\n" +
		"  \"passthrough\": \"yes\",\n" +
		"  \"args\": [\"-y\", \"pkg@1.0.0\", \"-pthread\", \"-print\", \"-perm\", \"--port\", \"8080\", \"--pinned\", \"ok-pinned\", \"--passthrough\", \"ok-pass\"],\n" +
		"  \"homepage\": \"https://example.com/docs/page\",\n" +
		"  \"url\": \"https://example.com:8443/mcp\",\n" +
		"  \"command\": \"node server.js --port 8080 --verbose\",\n" +
		"  \"commit\": \"" + strings.Repeat("ab", 20) + "\"\n" +
		"}\n"
	out := MaskJSONDisplay(src)
	d := allDiff(writeDiffOp("/x/cfg.json", "{}\n", src))
	for _, keep := range []string{"v1.2.3", "a-b", "dots", "yes", "pkg@1.0.0", "-pthread", "-print", "-perm", "8080", "ok-pinned", "ok-pass",
		"https://example.com/docs/page", "https://example.com:8443/mcp", "node server.js --port 8080 --verbose", strings.Repeat("ab", 20)} {
		if !strings.Contains(out, keep) {
			t.Errorf("%q masked by MaskJSONDisplay: %s", keep, out)
		}
		if !strings.Contains(d, keep) {
			t.Errorf("%q masked in the diff:\n%s", keep, d)
		}
	}
}

// A secret edited in a file that is not JSON: both masked lines read the same, so
// the count is announced (JSON files already did that).
func TestDiffAnnouncesSecretOnlyChangeInText(t *testing.T) {
	p := writeDiffOp("/x/a.md", "intro\ntoken: FAKEVALUE1\nend\n", "intro\ntoken: FAKEVALUE2\nend\n")
	out := allDiff(p)
	if strings.Contains(out, "FAKEVALUE") {
		t.Fatalf("leak:\n%s", out)
	}
	if p.Diffs[0].SecretChanges != 1 || !strings.Contains(out, "비밀 값 변경: 1줄") {
		t.Fatalf("not announced (%d):\n%s", p.Diffs[0].SecretChanges, out)
	}
	// A visible change next to it: the secret one is still counted, only once.
	p = writeDiffOp("/x/a.md", "token: FAKEVALUE1\nplain: one\n", "token: FAKEVALUE2\nplain: two\n")
	if p.Diffs[0].SecretChanges != 1 || !strings.Contains(allDiff(p), "+plain: two") {
		t.Fatalf("mixed:\n%s", allDiff(p))
	}
	// No secret, nothing announced; a secret that did not change is not announced.
	if p := writeDiffOp("/x/a.md", "a: 1\n", "a: 2\n"); p.Diffs[0].SecretChanges != 0 || strings.Contains(allDiff(p), "비밀 값 변경") {
		t.Fatal("announced a change that is not secret")
	}
	if p := writeDiffOp("/x/a.md", "token: FAKEVALUE1\nx: 1\n", "token: FAKEVALUE1\nx: 2\n"); p.Diffs[0].SecretChanges != 0 {
		t.Fatalf("an unchanged secret was announced: %+v", p.Diffs)
	}
}

// ---- AC-19 ----

func TestPlanTableShowsLongReasonsInFull(t *testing.T) {
	long := strings.Repeat("긴 설명 ", 40) + "해소: --skip-conflicts 로 제외"
	p := &Plan{Target: "claude-code", Scope: ScopeProject, Root: "/r",
		Items: []Item{{Kind: KindSkill, Name: "demo", Action: ActionConflict, Reason: long}}}
	var sb strings.Builder
	WriteText(&sb, p, nil, false)
	out := sb.String()
	if !strings.Contains(out, "표에서 줄였습니다") || !strings.Contains(out, "해소: --skip-conflicts 로 제외") {
		t.Fatalf("the cut advice is not shown in full:\n%s", out)
	}
	// A short reason adds nothing.
	p.Items[0].Reason = "짧음"
	sb.Reset()
	WriteText(&sb, p, nil, false)
	if strings.Contains(sb.String(), "표에서 줄였습니다") {
		t.Fatalf("unexpected truncation note:\n%s", sb.String())
	}
}

func TestInterruptedRestoreDoesNotPromiseApplyResumesIt(t *testing.T) {
	restore := InterruptedInfo{ID: "20260101T000002.000Z-bbbbbb", Kind: "restore", RestoreOf: "20260101T000001.000Z-aaaaaa", Ops: 2, Done: 1, Todo: 1}
	sync := InterruptedInfo{ID: "20260101T000003.000Z-cccccc", Kind: "sync", Ops: 2, Todo: 2}
	render := func(in ...InterruptedInfo) string {
		var sb strings.Builder
		WriteText(&sb, &Plan{Target: "claude-code", Scope: ScopeProject, Root: "/r", Interrupted: in}, nil, false)
		return sb.String()
	}
	out := render(restore)
	if !strings.Contains(out, "자동으로 이어 하지 않습니다") || !strings.Contains(out, "--apply 는 중단된 restore 를 이어 하지 않습니다") {
		t.Errorf("restore wording:\n%s", out)
	}
	if strings.Contains(out, "이어서 완료합니다") {
		t.Errorf("a restore interruption claims that --apply continues it:\n%s", out)
	}
	out = render(sync)
	if !strings.Contains(out, "--apply 는 중단된 sync 를") || !strings.Contains(out, "이어서 완료합니다") || strings.Contains(out, "자동으로 이어 하지 않습니다") {
		t.Errorf("sync wording:\n%s", out)
	}
	out = render(restore, sync)
	if !strings.Contains(out, "이어서 완료합니다") || !strings.Contains(out, "이어 하지 않습니다") {
		t.Errorf("both:\n%s", out)
	}
}

// AC-15: a forged restore_of with control characters never reaches the output raw.
func TestBuildReportSanitizesInterruptedRestoreOf(t *testing.T) {
	evil := "x\x1b[31m\u202ey\n"
	p := &Plan{Target: "claude-code", Scope: ScopeProject, Root: "/r",
		Interrupted: []InterruptedInfo{{ID: "a\x1b[1m", Kind: "restore", CreatedAt: "t\x07", RestoreOf: evil}}}
	r := BuildReport(p, nil, false)
	b, _ := json.Marshal(r.Interrupted)
	var sb strings.Builder
	WriteText(&sb, p, nil, false)
	for _, s := range []string{r.Interrupted[0].RestoreOf, r.Interrupted[0].ID, r.Interrupted[0].CreatedAt, sb.String()} {
		if strings.ContainsAny(s, "\x1b\x07\u202e") {
			t.Fatalf("a control character reached the output: %q", s)
		}
	}
	if len(b) == 0 || strings.Contains(string(b), "\\u001b") && strings.Contains(r.Interrupted[0].RestoreOf, "\x1b") {
		t.Fatalf("unsanitized restore_of: %q", r.Interrupted[0].RestoreOf)
	}
}

func TestHardLinkedRegistrySnapshotHasItsOwnReason(t *testing.T) {
	if r := HardLinkedSnapshotReason("demo"); !strings.Contains(r, "하드링크") || strings.Contains(r, "손상") && !strings.Contains(r, "해시 불일치가 아님") {
		t.Fatalf("reason: %q", r)
	}
}

// Very large inputs fall back to "remove everything, add everything": the lines
// that did not change then appear on both sides. They must not be counted as
// secret-only edits (the real text is identical).
func TestDiffSecretCountIgnoresUnchangedLinesInFallbackMode(t *testing.T) {
	var before, after strings.Builder
	before.WriteString("first-before\n")
	after.WriteString("first-after\n")
	for i := 0; i < 2100; i++ {
		before.WriteString("same line number " + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + "\n")
		after.WriteString("same line number " + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + "\n")
	}
	before.WriteString("last-before\n")
	after.WriteString("last-after\n")
	p := writeDiffOp("/x/big.md", before.String(), after.String())
	if len(p.Diffs) != 1 {
		t.Fatalf("diffs: %d", len(p.Diffs))
	}
	if p.Diffs[0].SecretChanges != 0 || strings.Contains(p.Diffs[0].Diff, "비밀 값 변경") {
		t.Fatalf("unchanged lines were counted as secret edits: %d", p.Diffs[0].SecretChanges)
	}
}

// The glued short flag is handled in both masking layers.
func TestGluedShortFlagMaskedInArrayElementAndInText(t *testing.T) {
	if out := MaskJSONDisplay(`{"args": ["mysql", "-pQwerty7zz", "db"]}`); strings.Contains(out, "Qwerty7zz") || !strings.Contains(out, `"db"`) {
		t.Errorf("array element: %s", out)
	}
	// An element that has spaces is a command string, not a flag: left to the line masker.
	if out := MaskJSONDisplay(`{"args": ["echo -pQwerty7zz"]}`); !strings.Contains(out, "Qwerty7zz") {
		t.Errorf("a command string must not be treated as a flag element: %s", out)
	}
	d := allDiff(writeDiffOp("/x/notes.md", "intro\n", "intro\nrun: mysql -pQwerty7zz mydb\ncc -pthread x.c\n"))
	if strings.Contains(d, "Qwerty7zz") || !strings.Contains(d, "-pthread") || !strings.Contains(d, "mydb") {
		t.Errorf("text:\n%s", d)
	}
}

// The last layer of the line masker: a string with no key or flag before it (first
// literal of a document) still gets the inline credential patterns.
func TestLineMaskerMasksInlineSecretInFirstLiteral(t *testing.T) {
	d := allDiff(writeDiffOp("/x/list.json", "[]\n", "[\n  \"run --password Zq9xk2mm now\"\n]\n"))
	if strings.Contains(d, "Zq9xk2mm") || !strings.Contains(d, "--password ***") {
		t.Fatalf("leak:\n%s", d)
	}
}

// "-p" of well-known harmless commands stays readable; every other "-p" is masked.
func TestHarmlessDashPCommandsStayReadable(t *testing.T) {
	keep := []string{
		`{"command": "mkdir -p /tmp/a/b"}`,
		`{"command": "cp -p src dst"}`,
		`{"command": "sh -c 'mkdir -p out && ls -p out'"}`,
		`{"command": "install -m 0644 -p a b"}`,
	}
	for _, src := range keep {
		d := allDiff(writeDiffOp("/x/cfg.json", "{}\n", src+"\n"))
		if strings.Contains(d, "***") {
			t.Errorf("harmless -p was masked: %s\n%s", src, d)
		}
	}
	text := allDiff(writeDiffOp("/x/a.md", "x\n", "x\nrun mkdir -p build/out\n"))
	if !strings.Contains(text, "mkdir -p build/out") {
		t.Errorf("text: %s", text)
	}
	mask := []string{
		`{"command": "mysql -p Qwerty7zz db"}`,
		`{"command": "mysql -pQwerty7zz db"}`,
		`{"command": "sometool -p Qwerty7zz"}`,
		`{"command": "mkdir -p out && mysql -p Qwerty7zz"}`,
		`{"command": "mkdirx -p Qwerty7zz"}`,
		`{"command": "cp -p a b; mysql -pQwerty7zz"}`,
	}
	for _, src := range mask {
		d := allDiff(writeDiffOp("/x/cfg.json", "{}\n", src+"\n"))
		if strings.Contains(d, "Qwerty7zz") {
			t.Errorf("leak: %s\n%s", src, d)
		}
	}
	if d := allDiff(writeDiffOp("/x/cfg.json", "{}\n", `{"command": "mkdir -p out && mysql -p Qwerty7zz"}`+"\n")); !strings.Contains(d, "mkdir -p out") {
		t.Errorf("the harmless part of a mixed command was masked:\n%s", d)
	}
}

// A secret that changes inside a JSON string the key-based masking does not cover
// reads the same on both sides after the line masker: announced and counted.
func TestDiffAnnouncesSecretOnlyChangeInsideJSONString(t *testing.T) {
	for name, pair := range map[string][2]string{
		"command": {`{"command": "mysql -pQwerty7aa db"}`, `{"command": "mysql -pQwerty7bb db"}`},
		"url":     {`{"url": "https://u:Qwerty7aa@h.example/x"}`, `{"url": "https://u:Qwerty7bb@h.example/x"}`},
		"lines":   {"{\n  \"url\": \"https://u:Qwerty7aa@h.example/x\"\n}\n", "{\n  \"url\": \"https://u:Qwerty7bb@h.example/x\"\n}\n"},
	} {
		t.Run(name, func(t *testing.T) {
			p := writeDiffOp("/x/cfg.json", pair[0]+"\n", pair[1]+"\n")
			out := allDiff(p)
			if strings.Contains(out, "Qwerty7") {
				t.Fatalf("leak:\n%s", out)
			}
			if p.Diffs[0].SecretChanges != 1 || !strings.Contains(out, "변경 전후가 같게 보입니다") {
				t.Fatalf("not announced (%d):\n%s", p.Diffs[0].SecretChanges, out)
			}
		})
	}
	// an unrelated visible change is not counted; a vanished secret change is not counted twice
	p := writeDiffOp("/x/cfg.json", `{"a": "1"}`+"\n", `{"a": "2"}`+"\n")
	if p.Diffs[0].SecretChanges != 0 {
		t.Fatalf("%+v", p.Diffs)
	}
	before := "{\n  \"API_TOKEN\": \"FAKE-OLD-1\",\n  \"url\": \"https://u:Qwerty7aa@h.example\"\n}\n"
	after := "{\n  \"API_TOKEN\": \"FAKE-NEW-1\",\n  \"url\": \"https://u:Qwerty7bb@h.example\"\n}\n"
	p = writeDiffOp("/x/cfg.json", before, after)
	if p.Diffs[0].SecretChanges != 2 {
		t.Fatalf("want 2 (one vanished, one same-looking), got %d:\n%s", p.Diffs[0].SecretChanges, allDiff(p))
	}
}
