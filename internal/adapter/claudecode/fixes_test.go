package claudecode

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/heonjinjeong/forge/internal/adapter"
)

// ---- item 2: forbidden roots and the Claude Code user file by identity ----

// swapCase flips the case of the last path element ("home" -> "HOME").
func swapCase(p string) string {
	dir, base := filepath.Split(p)
	return dir + strings.ToUpper(base)
}

func caseInsensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Lstat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

func TestForbiddenProjectRootsAreJudgedByDirectoryIdentity(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	if err := os.MkdirAll(filepath.Join(e.home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	try := func(dir string) error {
		_, err := e.plan(adapter.ScopeProject, func(r *adapter.Request) { r.ProjectDir = dir })
		return err
	}
	// Plain spellings are refused (home, Forge home, ~/.claude).
	for name, dir := range map[string]string{"home": e.home, "forge home": e.forgeHome, "~/.claude": filepath.Join(e.home, ".claude")} {
		if err := try(dir); err == nil {
			t.Errorf("%s: project root accepted", name)
		}
	}
	// A symlink to the home directory too.
	link := filepath.Join(t.TempDir(), "homelink")
	if err := os.Symlink(e.home, link); err != nil {
		t.Fatal(err)
	}
	if err := try(link); err == nil {
		t.Error("symlink to home: project root accepted")
	}
	// A subdirectory of home is a normal project.
	sub := filepath.Join(e.home, "work")
	if err := os.MkdirAll(filepath.Join(sub, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := try(sub); err != nil {
		t.Errorf("a project below home must be fine: %v", err)
	}
	// Case variants (APFS, NTFS): only meaningful on a case-insensitive filesystem.
	if !caseInsensitiveFS(t, filepath.Dir(e.home)) {
		t.Log("case-sensitive filesystem: case-variant check skipped (identity is covered by the os.SameFile cases above)")
		return
	}
	for name, dir := range map[string]string{
		"home":       swapCase(e.home),
		"forge home": swapCase(e.forgeHome),
		"~/.claude":  filepath.Join(e.home, ".CLAUDE"),
	} {
		if err := try(dir); err == nil {
			t.Errorf("%s: a case variant bypassed the forbidden-root check", name)
		}
		// With a .git inside, the repository root search lands on the same directory.
	}
	if err := os.MkdirAll(filepath.Join(e.home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := try(swapCase(e.home)); err == nil {
		t.Error("home with a .git directory (case variant) accepted")
	}
	if err := try(filepath.Join(swapCase(e.home), "work")); err != nil {
		t.Errorf("a project below a case variant of home must still work: %v", err)
	}
}

func TestGuardPathByNameAndIdentity(t *testing.T) {
	e := newEnv(t)
	userFile := filepath.Base(e.decoy)
	names := []string{
		userFile,
		strings.ToUpper(userFile),
		".Claude.Json",
		userFile + " ",
		userFile + ".",
		"\u200b" + userFile,
		".claude.js\u200bon",
		".claude.json\u0301", // trailing combining mark (normalization variants)
		".claude\u00ad.json", // soft hyphen
	}
	for _, n := range names {
		if err := adapter.GuardPath(filepath.Join(e.proj, n)); err == nil {
			t.Errorf("GuardPath accepted %q", n)
		}
	}
	for _, n := range []string{".claude", "claude.json", ".claude.json.bak", ".mcp.json", "settings.json", ".claude.jsonx"} {
		if err := adapter.GuardPath(filepath.Join(e.proj, n)); err != nil {
			t.Errorf("GuardPath refused innocent %q: %v", n, err)
		}
	}
	sig := e.decoySig()
	// Hard link (any name) and symlink to the decoy are the same file: refused.
	hard := filepath.Join(e.proj, "innocent-name.txt")
	if err := os.Link(e.decoy, hard); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	soft := filepath.Join(e.proj, "softlink.txt")
	if err := os.Symlink(e.decoy, soft); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{hard, soft} {
		if err := adapter.GuardPath(p); err == nil {
			t.Errorf("GuardPath accepted an alias of the user file: %s", p)
		}
		if _, _, _, err := adapter.ReadFileSafe(p); err == nil {
			t.Errorf("ReadFileSafe read an alias of the user file: %s", p)
		}
		if _, err := adapter.Snapshot(p); err == nil {
			t.Errorf("Snapshot accepted an alias of the user file: %s", p)
		}
		if err := adapter.WriteFileAtomic(p, []byte("x"), 0o600); err == nil {
			t.Errorf("WriteFileAtomic wrote an alias of the user file: %s", p)
		}
	}
	// Case-insensitive filesystem: another spelling of the same file.
	if caseInsensitiveFS(t, e.home) {
		if err := adapter.GuardPath(filepath.Join(e.home, strings.ToUpper(userFile))); err == nil {
			t.Error("upper-case spelling accepted")
		}
	}
	e.checkDecoy(sig)
	// An unrelated file is fine.
	other := filepath.Join(e.proj, "plain.txt")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := adapter.GuardPath(other); err != nil {
		t.Errorf("unrelated file refused: %v", err)
	}
}

// A sync whose project links the decoy under an innocent name must fail instead
// of reading it.
func TestSyncRefusesAliasOfTheUserFileInTheProject(t *testing.T) {
	e := newEnv(t)
	standardRegistry(t, e)
	sig := e.decoySig()
	if err := os.Link(e.decoy, filepath.Join(e.proj, ".mcp.json")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := e.plan(adapter.ScopeProject); err == nil {
		t.Fatal("a .mcp.json that is a hard link to the user file must be refused")
	}
	e.checkDecoy(sig)
}

// ---- item 3: diff masking by JSON value ----

func planWithUserSettings(t *testing.T, settings string) (*env, *adapter.Plan) {
	t.Helper()
	e := newEnv(t)
	standardRegistry(t, e)
	e.write(".claude/settings.json", settings, 0o644)
	p := e.mustPlan(adapter.ScopeProject)
	adapter.BuildDiffs(p)
	return e, p
}

func allDiffs(p *adapter.Plan) string {
	var sb strings.Builder
	for _, d := range p.Diffs {
		sb.WriteString(d.Diff)
	}
	return sb.String()
}

func TestDiffMasksSecretValuesByJSONValue(t *testing.T) {
	cases := map[string]struct {
		settings string
		secrets  []string // must never appear in any diff line
		visible  []string // plain values that must stay readable
	}{
		"one-line minified": {
			`{"env":{"API_TOKEN":"abc123supersecretvalue","REGION":"eu-west-1"},"theme":"dark"}` + "\n",
			[]string{"abc123supersecretvalue"}, []string{"eu-west-1", "dark"},
		},
		"pretty printed": {
			"{\n  \"env\": {\n    \"API_TOKEN\": \"abc123supersecretvalue\",\n    \"REGION\": \"eu-west-1\"\n  },\n  \"theme\": \"dark\"\n}\n",
			[]string{"abc123supersecretvalue"}, []string{"eu-west-1", "dark"},
		},
		"nested and arrays of objects": {
			`{"servers":[{"name":"one","apiKey":"zzzKEYvalue11","nested":{"Password":"hunter2hunter2"}},{"name":"two","cfg":{"deep":{"client_secret":"s3cr3tValue99"}}}],"theme":"dark"}` + "\n",
			[]string{"zzzKEYvalue11", "hunter2hunter2", "s3cr3tValue99"}, []string{"one", "two", "dark"},
		},
		"camelCase upper and mixed names": {
			`{"accessToken":"tok-AAA-1111","CLIENT_SECRET":"cs-BBB-2222","Authorization":"Basic QUJDREVGRw==","sessionCookie":"ck-CCC-3333","dbPassword":"pw-DDD-4444","private_key":"pk-EEE-5555","note":"hello"}` + "\n",
			[]string{"tok-AAA-1111", "cs-BBB-2222", "Basic QUJDREVGRw==", "ck-CCC-3333", "pw-DDD-4444", "pk-EEE-5555"}, []string{"hello"},
		},
		"array below a secret key and flag value": {
			`{"tokens":["tok-aaa-111","tok-bbb-222"],"args":["--token","plainsecret99","--verbose"],"theme":"dark"}` + "\n",
			[]string{"tok-aaa-111", "tok-bbb-222", "plainsecret99"}, []string{"--verbose", "dark"},
		},
		"escaped key name": {
			`{"API_TOKEN":"escapedsecret77","x":"y"}` + "\n",
			[]string{"escapedsecret77"}, []string{"\"x\""},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, p := planWithUserSettings(t, c.settings)
			d := allDiffs(p)
			if !strings.Contains(d, "settings.json") {
				t.Fatalf("no settings.json diff:\n%s", d)
			}
			for _, s := range c.secrets {
				if strings.Contains(d, s) {
					t.Errorf("secret %q leaked in the diff:\n%s", s, d)
				}
			}
			if !strings.Contains(d, "***") {
				t.Errorf("nothing masked:\n%s", d)
			}
			for _, v := range c.visible {
				if !strings.Contains(d, v) {
					t.Errorf("plain value %q was masked away:\n%s", v, d)
				}
			}
		})
	}
}

func TestMaskingIsDisplayOnlyAndKeepsLineStructure(t *testing.T) {
	settings := `{"env":{"API_TOKEN":"abc123supersecretvalue"},"theme":"dark"}` + "\n"
	e, p := planWithUserSettings(t, settings)
	var data []byte
	for _, op := range p.Ops {
		if strings.HasSuffix(op.Path, "settings.json") {
			data = append([]byte(nil), op.Data...)
		}
	}
	if !bytes.Contains(data, []byte("abc123supersecretvalue")) {
		t.Fatal("the planned write must carry the user's value verbatim")
	}
	// BuildDiffs again must not alter operation data, and the applied file keeps the secret.
	adapter.BuildDiffs(p)
	for _, op := range p.Ops {
		if strings.HasSuffix(op.Path, "settings.json") && !bytes.Equal(op.Data, data) {
			t.Fatal("BuildDiffs changed the data to be written")
		}
	}
	if _, err := e.apply(p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.read(".claude/settings.json"), `"API_TOKEN":"abc123supersecretvalue"`) {
		t.Fatal("masking leaked into the written file")
	}
}

func TestMaskJSONTextKeepsLinesOneToOne(t *testing.T) {
	inputs := []string{
		"{\n  \"a\": \"x\",\n  \"token\": \"sekret-value-1\",\n  \"list\": [\n    \"--password\",\n    \"hunter2\"\n  ]\n}\n",
		`{"a":{"b":[{"secret":"s1"},{"secret":"s2"}]}}`,
		"{\"token\": \"line1\nline2\"}\n", // raw newline inside a string (invalid JSON): left alone
		`{"token": "unterminated`,
		"",
	}
	for _, in := range inputs {
		out := adapter.MaskJSONDisplay(in)
		if strings.Count(out, "\n") != strings.Count(in, "\n") {
			t.Errorf("line count changed:\n%q\n%q", in, out)
		}
		if !utf8.ValidString(out) {
			t.Errorf("invalid utf-8 produced for %q", in)
		}
	}
	out := adapter.MaskJSONDisplay(inputs[0])
	if strings.Contains(out, "sekret-value-1") || strings.Contains(out, "hunter2") {
		t.Errorf("not masked: %q", out)
	}
	if strings.Contains(adapter.MaskJSONDisplay(`{"token":"${MY_TOKEN}"}`), "***") {
		t.Error("an environment reference must stay readable")
	}
}

// ---- item 5: a user directory is never overwritten, not even by --force-managed ----

func forceManaged(r *adapter.Request) { r.ForceManaged = true }

func TestStateWithoutValidMarkerIsNeverOverwrittenEvenWithForceManaged(t *testing.T) {
	validMarker := func(e *env) string { return e.read(".claude/skills/demo-skill/.forge-managed") }
	cases := map[string]func(e *env) (remove bool, marker string){
		"marker removed": func(e *env) (bool, string) { return true, "" },
		"wrong name": func(e *env) (bool, string) {
			return false, strings.Replace(validMarker(e), `"name": "demo-skill"`, `"name": "other-skill"`, 1)
		},
		"malformed marker": func(e *env) (bool, string) { return false, "{not json" },
		"foreign owner": func(e *env) (bool, string) {
			return false, strings.Replace(validMarker(e), `"managed_by": "forge"`, `"managed_by": "someone"`, 1)
		},
		"empty marker": func(e *env) (bool, string) { return false, "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			standardRegistry(t, e)
			e.mustSync(adapter.ScopeProject) // the state file now lists demo-skill
			remove, marker := mutate(e)
			// The directory is now the user's: different content, marker gone or not ours.
			e.write(".claude/skills/demo-skill/SKILL.md", "---\nname: demo-skill\n---\nMY OWN VERSION\n", 0o644)
			mk := filepath.Join(e.proj, ".claude/skills/demo-skill/.forge-managed")
			if remove {
				if err := os.Remove(mk); err != nil {
					t.Fatal(err)
				}
			} else {
				e.write(".claude/skills/demo-skill/.forge-managed", marker, 0o644)
			}
			mine := snapshot(t, filepath.Join(e.proj, ".claude/skills/demo-skill"))

			for _, mods := range [][]func(*adapter.Request){nil, {forceManaged}} {
				p := e.mustPlan(adapter.ScopeProject, mods...)
				it := itemFor(p, adapter.KindSkill, "demo-skill")
				if it == nil || it.Action != adapter.ActionConflict || it.Overridable {
					t.Fatalf("want a non-overridable conflict (force-managed=%v): %+v", mods != nil, it)
				}
				if _, err := e.apply(p); !errors.Is(err, adapter.ErrConflicts) {
					t.Fatalf("apply must refuse: %v", err)
				}
				p.SkipConflicts = true
				if _, err := e.apply(p); err != nil {
					t.Fatal(err)
				}
				if snapshot(t, filepath.Join(e.proj, ".claude/skills/demo-skill")) != mine {
					t.Fatalf("the user's directory was modified (force-managed=%v)", mods != nil)
				}
			}
		})
	}
}

func TestStateEntryWithoutMarkerIsNotRemovedEvenWithForceManaged(t *testing.T) {
	for _, mods := range [][]func(*adapter.Request){nil, {forceManaged}} {
		e := newEnv(t)
		standardRegistry(t, e)
		e.mustSync(adapter.ScopeProject)
		// The user replaced the managed skill directory by their own (no marker) ...
		if err := os.Remove(filepath.Join(e.proj, ".claude/skills/demo-skill/.forge-managed")); err != nil {
			t.Fatal(err)
		}
		e.write(".claude/skills/demo-skill/SKILL.md", "MINE\n", 0o644)
		mine := snapshot(t, filepath.Join(e.proj, ".claude/skills/demo-skill"))
		// ... and the skill was disabled in the registry: Forge would remove a managed one.
		if err := e.sreg.SetEnabled(t.Context(), "demo-skill", false); err != nil {
			t.Fatal(err)
		}
		p := e.mustPlan(adapter.ScopeProject, mods...)
		wantAction(t, p, adapter.KindSkill, "demo-skill", adapter.ActionSkip)
		for _, op := range p.Ops {
			if strings.HasSuffix(op.Path, "demo-skill") {
				t.Fatalf("plan operates on the user's directory (force-managed=%v): %+v", mods != nil, op.Kind)
			}
		}
		if _, err := e.apply(p); err != nil {
			t.Fatal(err)
		}
		if snapshot(t, filepath.Join(e.proj, ".claude/skills/demo-skill")) != mine {
			t.Fatalf("the user's directory was removed or changed (force-managed=%v)", mods != nil)
		}
	}
}

// ---- item 6: long cells and duplicate keys ----

func TestLongNamesDoNotStretchThePlanTable(t *testing.T) {
	long := strings.Repeat("N", 5000)
	reason := strings.Repeat("r", 4000)
	p := &adapter.Plan{Target: "claude-code", Scope: adapter.ScopeProject, Root: "/x", Items: []adapter.Item{
		{Kind: adapter.KindSkill, Name: long, Action: adapter.ActionConflict, Reason: reason},
		{Kind: adapter.KindMCP, Name: "short", Action: adapter.ActionAdd, Reason: "ok"},
	}}
	var buf bytes.Buffer
	adapter.WriteText(&buf, p, nil, false)
	for _, line := range strings.Split(buf.String(), "\n") {
		if n := utf8.RuneCountInString(line); n > 260 {
			t.Fatalf("a table line has %d characters:\n%.200s", n, line)
		}
	}
	if !strings.Contains(buf.String(), "…") || !strings.Contains(buf.String(), "--json") {
		t.Error("truncation must be visible and point at --json")
	}
	// JSON keeps the (sanitized, 300-character) full strings.
	rep := adapter.BuildReport(p, nil, false)
	if utf8.RuneCountInString(rep.Items[0].Name) < 200 {
		t.Fatalf("JSON name was shortened to %d", utf8.RuneCountInString(rep.Items[0].Name))
	}
	if !strings.Contains(buf.String(), "MCP  short  추가  ok") && !strings.Contains(buf.String(), "short") {
		t.Error("short rows must stay as they are")
	}
}

func TestDuplicateKeyBehaviour(t *testing.T) {
	// Duplicates outside the paths Forge edits are allowed and preserved byte for byte.
	e := newEnv(t)
	standardRegistry(t, e)
	mcpOrig := "{\n  \"note\": 1,\n  \"note\": 2,\n  \"mcpServers\": {}\n}\n"
	setOrig := "{\n  \"theme\": \"a\",\n  \"theme\": \"b\"\n}\n"
	e.write(".mcp.json", mcpOrig, 0o644)
	e.write(".claude/settings.json", setOrig, 0o644)
	e.mustSync(adapter.ScopeProject)
	if !strings.Contains(e.read(".mcp.json"), "\"note\": 1,\n  \"note\": 2,") || !strings.Contains(e.read(".claude/settings.json"), "\"theme\": \"a\",\n  \"theme\": \"b\"") {
		t.Fatal("duplicate keys outside the managed paths must be preserved")
	}
	// hooks / mcpServers duplicated: abort without writing.
	for file, content := range map[string]string{
		".mcp.json":             "{\"mcpServers\": {}, \"mcpServers\": {}}\n",
		".claude/settings.json": "{\"hooks\": {}, \"hooks\": {}}\n",
	} {
		e2 := newEnv(t)
		standardRegistry(t, e2)
		e2.write(file, content, 0o644)
		before := snapshot(t, e2.proj)
		if _, err := e2.plan(adapter.ScopeProject); err == nil {
			t.Errorf("%s: duplicated managed key must abort the plan", file)
		}
		if snapshot(t, e2.proj) != before {
			t.Errorf("%s: files changed", file)
		}
	}
}
