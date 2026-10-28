package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heonjinjeong/forge/internal/store"
)

// TASK-0025 follow-ups of install preview and pinning.

// AC-3: the security group has a title (no "==  ==").
func TestDoctorTextSecurityGroupHasTitle(t *testing.T) {
	e := newSyncEnv(t)
	out, _ := runErr(t, "", "doctor", "--project-dir", e.proj)
	if strings.Contains(out, "==  ==") {
		t.Fatalf("empty group title:\n%s", out)
	}
	if !strings.Contains(out, "== 보안 ==") {
		t.Fatalf("security group title missing:\n%s", out)
	}
	for _, g := range []string{"home", "client", "drift", "paths", "history", "hygiene", "profile", "security"} {
		if groupLabel[g] == "" {
			t.Errorf("group %q has no label", g)
		}
	}
}

// AC-4: a tampered registry snapshot is explained as tampering (with its own
// resolution), not as a hand edit or a name clash.
func TestDoctorTamperedSkillConflictHasOwnReasonAndFix(t *testing.T) {
	e := newSyncEnv(t)
	home := os.Getenv("FORGE_HOME")
	ents, _ := filepath.Glob(filepath.Join(home, "skills", "demo-skill@*"))
	if len(ents) != 1 {
		t.Fatalf("snapshots=%v", ents)
	}
	rel, _ := filepath.Rel(filepath.Join(home, "skills"), ents[0])
	tamper(t, home, filepath.Join(rel, "SKILL.md"), demoMD+"\n# injected\n")

	r, _ := e.doctor(t)
	var found bool
	for _, c := range r.Checks {
		if !strings.HasPrefix(c.ID, "drift.") || !strings.Contains(c.Message, "충돌") || !strings.Contains(c.Message, "demo-skill") {
			continue
		}
		found = true
		if strings.Contains(c.Cause, "직접 수정했거나") {
			t.Errorf("tampering is not a hand edit / name clash: %q", c.Cause)
		}
		if !strings.Contains(c.Cause, "변조 또는 손상") || !strings.Contains(c.Cause, "forge skill check") {
			t.Errorf("cause must name tampering and the way out: %q", c.Cause)
		}
		if len(c.Fix) == 0 || !strings.Contains(c.Fix[0], "forge skill check") {
			t.Errorf("fix: %v", c.Fix)
		}
		for _, f := range c.Fix {
			if strings.Contains(f, "--force-managed") {
				t.Errorf("--force-managed does not help a tampered snapshot: %q", f)
			}
		}
	}
	if !found {
		t.Fatalf("no drift conflict for the tampered skill: %+v", r.Checks)
	}
	// the sync plan itself carries the same specific reason and command
	out, serr := e.sync(t, "--target", "claude-code")
	_ = serr
	if !strings.Contains(out, "변조·손상") || !strings.Contains(out, "forge skill remove <이름> --yes") || strings.Contains(out, "사용자 소유이거나") || strings.Contains(out, "사용자 항목과 이름 충돌") {
		t.Errorf("sync plan:\n%s", out)
	}
}

// AC-5: a refused install (no terminal and no --yes, hash mismatch, bad
// source) creates neither FORGE_HOME nor an empty database.
func TestSkillAddRefusalCreatesNoForgeHome(t *testing.T) {
	useGit(t, &fakeGit{})
	fh := forgeHome(t) // read-only snapshots are made writable again at cleanup
	if err := os.Remove(fh); err != nil {
		t.Fatal(err)
	}
	gone := func(what string) {
		t.Helper()
		if _, err := os.Lstat(fh); !os.IsNotExist(err) {
			t.Fatalf("%s: FORGE_HOME must not exist (err=%v)", what, err)
		}
	}
	if _, _, err := runSplit(t, "", "skill", "add", gitURL); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-tty without --yes must refuse: %v", err)
	}
	gone("non-tty refusal")
	if _, _, err := runSplit(t, "", "skill", "add", gitURL, "--sha256", strings.Repeat("0", 64), "--yes"); err == nil {
		t.Fatal("hash mismatch must refuse")
	}
	gone("hash mismatch")
	if _, _, err := runSplit(t, "", "skill", "add", gitURL, "--json"); err == nil {
		t.Fatal("refuse with --json")
	}
	gone("json refusal")
	if _, _, err := runSplit(t, "", "skill", "add", filepath.Join(t.TempDir(), "missing-dir")); err == nil {
		t.Fatal("bad source must fail")
	}
	gone("bad source")
	asTerminal(t)
	if _, _, err := runSplit(t, "n\n", "skill", "add", gitURL); err == nil {
		t.Fatal("n declines")
	}
	gone("declined at the prompt")
	if _, _, err := runSplit(t, "", "skill", "add", "--preview", gitURL); err != nil {
		t.Fatal(err)
	}
	gone("--preview")

	// approving creates it and installs (staging then moves into FORGE_HOME)
	if _, _, err := runSplit(t, "y\n", "skill", "add", gitURL); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fh, "forge.db")); err != nil {
		t.Fatalf("approved install must create the database: %v", err)
	}
	if out := mustRun(t, "skill", "list"); !strings.Contains(out, "demo-skill") {
		t.Errorf("not installed:\n%s", out)
	}
	if ents, _ := os.ReadDir(filepath.Join(fh, "tmp")); len(ents) != 0 {
		t.Errorf("staging left behind: %v", ents)
	}
}

// AC-5 (existing registry): a refused install does not touch it.
func TestSkillAddRefusalLeavesExistingHomeUntouched(t *testing.T) {
	useGit(t, &fakeGit{})
	home := forgeHome(t)
	mustRun(t, "init")
	before := snapshotTree(t, home)
	if _, _, err := runSplit(t, "", "skill", "add", gitURL); err == nil {
		t.Fatal("must refuse")
	}
	if !sameTree(before, snapshotTree(t, home)) {
		t.Error("a refused install changed FORGE_HOME")
	}
}

// AC-7: every --strict / --yes combination of mcp add, and the doctor checks
// for approvals and enforcement, in one table.
func TestMCPAddStrictYesMatrix(t *testing.T) {
	forgeHome(t)
	type tc struct {
		name string
		args []string
		in   string
		tty  bool
		want bool // registered
	}
	cases := []tc{
		{"default", nil, "", false, true},
		{"yes alone is harmless", []string{"--yes"}, "", false, true},
		{"strict, no tty, no yes", []string{"--strict"}, "", false, false},
		{"strict, no tty, yes", []string{"--strict", "--yes"}, "", false, true},
		{"strict, tty, y", []string{"--strict"}, "y\n", true, true},
		{"strict, tty, yes word", []string{"--strict"}, "yes\n", true, true},
		{"strict, tty, n", []string{"--strict"}, "n\n", true, false},
		{"strict, tty, empty", []string{"--strict"}, "\n", true, false},
		{"strict, tty, EOF", []string{"--strict"}, "", true, false},
		{"strict, tty, junk", []string{"--strict"}, "maybe\n", true, false},
		{"strict preview registers nothing", []string{"--strict", "--preview"}, "", false, false},
		{"strict yes preview registers nothing", []string{"--strict", "--yes", "--preview"}, "", false, false},
	}
	for i, c := range cases {
		name := "m" + string(rune('a'+i))
		if c.tty {
			asTerminal(t)
		}
		args := append([]string{"mcp", "add"}, c.args...)
		args = append(args, name, "--", "echo", "hi")
		_, _, err := runSplit(t, c.in, args...)
		list := mustRun(t, "mcp", "list")
		got := strings.Contains(list, name)
		if got != c.want || (err == nil) != (c.want || strings.Contains(strings.Join(c.args, " "), "--preview")) {
			t.Errorf("%s: registered=%v want %v err=%v", c.name, got, c.want, err)
		}
	}
}

// AC-7: doctor security.mcp.unapproved / security.mcp.enforce scenarios that
// the flow tests do not cover: enforce on without any approval, a tool that
// appears after approval, and a disabled server.
func TestDoctorMCPApprovalEnforceScenarios(t *testing.T) {
	e := newSyncEnv(t)
	addMock(t, "mock", "alpha,beta")
	refresh(t, "mock")

	// never approved + enforce on: unapproved info AND the enforce check warns (everything hidden)
	mustRun(t, "mcp", "pin", "mock", "--enforce")
	r, _ := e.doctor(t)
	if c := r.get("security.mcp.unapproved"); c == nil || c.Status != "info" || !strings.Contains(c.Message, "mock") {
		t.Errorf("never approved: %+v", c)
	}
	if c := r.get("security.mcp.enforce"); c == nil || c.Status != "warn" || !strings.Contains(c.Message, "숨김 2") {
		t.Errorf("enforce, nothing approved: %+v", c)
	}

	// approve one tool only: still not 'never approved', one tool hidden
	mustRun(t, "mcp", "approve", "mock", "--tool", "alpha")
	r, _ = e.doctor(t)
	if r.get("security.mcp.unapproved") != nil {
		t.Error("a server with an approval is not 'never approved'")
	}
	if c := r.get("security.mcp"); c == nil || c.Status != "warn" || !strings.Contains(c.Message, "새로 생긴 도구 1") {
		t.Errorf("one tool without approval after an approval: %+v", c)
	}
	if c := r.get("security.mcp.enforce"); c == nil || c.Status != "warn" || !strings.Contains(c.Message, "숨김 1") {
		t.Errorf("enforce, one hidden: %+v", c)
	}

	// everything approved, enforce on: ok
	mustRun(t, "mcp", "approve", "mock")
	r, _ = e.doctor(t)
	if c := r.get("security.mcp"); c == nil || c.Status != "ok" {
		t.Errorf("%+v", c)
	}
	if c := r.get("security.mcp.enforce"); c == nil || c.Status != "ok" {
		t.Errorf("%+v", c)
	}

	// a disabled server is not reported
	mustRun(t, "mcp", "pin", "mock", "--no-enforce")
	dbExec(t, `UPDATE mcp_servers SET enabled=0 WHERE name='mock'`)
	dbExec(t, `DELETE FROM mcp_tool_approvals`)
	r, _ = e.doctor(t)
	if r.get("security.mcp.unapproved") != nil {
		t.Errorf("disabled server must be skipped: %+v", r.get("security.mcp.unapproved"))
	}
}

// AC-6: removing a server removes its enforce key (and re-adding starts clean).
func TestMCPRemoveDropsEnforceSetting(t *testing.T) {
	forgeHome(t)
	addMock(t, "mock", "alpha")
	refresh(t, "mock")
	mustRun(t, "mcp", "pin", "mock", "--enforce")
	mustRun(t, "mcp", "remove", "mock", "--yes")
	home, err := store.Home()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.Open(context.Background(), store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM settings WHERE key LIKE 'mcp.enforce.%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("enforce keys left: %d", n)
	}
}

// AC-4 (mixed): a tampered snapshot next to an ordinary conflict keeps both
// explanations, and --force-managed stays advised only for the ordinary one.
func TestDoctorMixedTamperedAndOrdinaryConflict(t *testing.T) {
	e := newSyncEnv(t)
	home := os.Getenv("FORGE_HOME")
	ents, _ := filepath.Glob(filepath.Join(home, "skills", "demo-skill@*"))
	rel, _ := filepath.Rel(filepath.Join(home, "skills"), ents[0])
	tamper(t, home, filepath.Join(rel, "SKILL.md"), demoMD+"\n# injected\n")
	// a second skill, installed and synced, then edited by hand in the project
	other := dirSkill2(t)
	mustRun(t, "skill", "add", other)
	if _, err := e.sync(t, "--target", "claude-code", "--apply", "--skip-conflicts"); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(e.proj, ".claude", "skills", "other-skill", "SKILL.md")
	_ = os.Chmod(filepath.Dir(managed), 0o755)
	_ = os.Chmod(managed, 0o644)
	if err := os.WriteFile(managed, []byte("---\nname: other-skill\ndescription: edited\n---\nhand edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := e.doctor(t)
	var ok bool
	for _, c := range r.Checks {
		if !strings.HasPrefix(c.ID, "drift.claude-code") || !strings.Contains(c.Message, "충돌 2건") {
			continue
		}
		ok = true
		if !strings.Contains(c.Cause, "직접 수정했거나") || !strings.Contains(c.Cause, "변조 또는 손상") {
			t.Errorf("both explanations expected: %q", c.Cause)
		}
		if !strings.Contains(strings.Join(c.Fix, "\n"), "--force-managed") || !strings.Contains(strings.Join(c.Fix, "\n"), "forge skill check") {
			t.Errorf("fix: %v", c.Fix)
		}
	}
	if !ok {
		t.Fatalf("no 2-conflict drift check: %+v", r.Checks)
	}
	out, _ := e.sync(t, "--target", "claude-code")
	if !strings.Contains(out, "사용자 소유이거나") || !strings.Contains(out, "변조 의심 스킬 1건") {
		t.Errorf("sync footer:\n%s", out)
	}
}

func dirSkill2(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "other-skill")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: other-skill\ndescription: Another skill.\n---\n# Other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}
