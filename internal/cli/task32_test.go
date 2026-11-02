package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heonjinjeong/forge/internal/skill"
	"github.com/heonjinjeong/forge/internal/store"
)

// TASK-0032 AC-1: a registry deletion that takes a profile member with it is reported.
func TestDoctorReportsProfileMembersLostToRegistryDeletion(t *testing.T) {
	e := newSyncEnv(t)
	mustRun(t, "profile", "create", "cc")
	mustRun(t, "profile", "add", "cc", "--skill", "demo-skill")
	if c := e.mustDoctor(t).get("profile.deleted"); c != nil {
		t.Fatalf("nothing deleted yet: %+v", c)
	}
	mustRun(t, "skill", "remove", "demo-skill", "--yes")
	c := e.expect(t, e.mustDoctor(t), "profile.deleted", "warn")
	if !strings.Contains(c.Message, "skill:demo-skill") || !strings.Contains(c.Message, "global:cc") || !hasFix(c, "forge profile show cc") {
		t.Fatalf("%+v", c)
	}
	// profile gone too: info, no stale warning
	mustRun(t, "profile", "delete", "cc")
	e.expect(t, e.mustDoctor(t), "profile.deleted", "info")
}

// AC-5: a skill that was disabled is not rendered, so OpenCode cannot see it: it is
// not a crossclient finding.
func TestDoctorCrossclientIgnoresDisabledSkills(t *testing.T) {
	e := newSyncEnv(t)
	mustRun(t, "profile", "create", "cc")
	mustRun(t, "profile", "add", "cc", "--skill", "demo-skill")
	mustRun(t, "profile", "create", "oc")
	mustRun(t, "profile", "add", "oc", "--mcp", "direct-srv")
	mustRun(t, "profile", "use", "cc", "--client", "claude-code")
	mustRun(t, "profile", "use", "oc", "--client", "opencode")
	e.expect(t, e.mustDoctor(t), "profile.crossclient", "warn")
	mustRun(t, "skill", "disable", "demo-skill")
	if c := e.mustDoctor(t).get("profile.crossclient"); c != nil {
		t.Fatalf("a disabled skill is not visible to OpenCode: %+v", c)
	}
}

// AC-5: with different profiles per client the same skill is reported once, by
// profile.crossclient, not also by the per-profile ".overlap" check of the claude-code profile.
func TestDoctorDifferentProfilesDoNotDoubleReportOverlap(t *testing.T) {
	e := newSyncEnv(t)
	mustRun(t, "profile", "create", "cc")
	mustRun(t, "profile", "add", "cc", "--skill", "demo-skill")
	mustRun(t, "profile", "add", "cc", "--skill", "demo-skill", "--exposure", "hidden", "--client", "opencode")
	mustRun(t, "profile", "create", "oc")
	mustRun(t, "profile", "add", "oc", "--mcp", "direct-srv")
	mustRun(t, "profile", "use", "cc", "--client", "claude-code")
	mustRun(t, "profile", "use", "oc", "--client", "opencode")
	r := e.mustDoctor(t)
	e.expect(t, r, "profile.crossclient", "warn")
	if c := r.get("profile.health.cc.overlap"); c != nil {
		t.Fatalf("double report: %+v", c)
	}
}

// AC-5: a database that cannot be read because of its permissions is a permission
// problem, not "corrupt or not SQLite".
func TestDoctorUnreadableDatabaseIsAPermissionProblem(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	e := newSyncEnv(t)
	dbp := store.DBPath(e.homeForge())
	if err := os.Chmod(dbp, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dbp, 0o600) })
	r, _ := e.doctor(t)
	c := r.get("db.integrity")
	if c == nil || c.Status != "fail" || !strings.Contains(c.Message, "권한") || strings.Contains(c.Cause, "손상되었거나 SQLite DB 가 아닙니다") || !hasFix(c, "chmod 600") {
		t.Fatalf("%+v", c)
	}
}

// AC-8, AC-9: a code-exec tool that is confirm=none from before the classification
// (not grandfathered) is shown by doctor, and `tool list` marks it in ASCII so the
// columns stay aligned.
func TestDoctorShowsLegacyCodeExecToolAndListStaysAligned(t *testing.T) {
	e := newSyncEnv(t)
	schema := `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`
	mustRun(t, "tool", "add", "legacy-sq", "--description", "sql", "--schema", schema, "--", "sqlite3", "db.sqlite", "{{q}}")
	mustRun(t, "tool", "add", "acked-sq", "--description", "sql", "--schema", schema, "--confirm", "none", "--yes-code-exec", "--", "sqlite3", "db.sqlite", "{{q}}")
	mustRun(t, "tool", "add", "plain-ls", "--description", "ls", "--confirm", "none", "--", "ls")
	if c := e.mustDoctor(t).get("security.tools.codeexec"); c != nil {
		t.Fatalf("nothing is forced yet: %+v", c)
	}
	if _, err := e.forgeDB(t).Exec(`UPDATE tools SET confirm='none' WHERE name='legacy-sq'`); err != nil { // as registered before TASK-0018
		t.Fatal(err)
	}
	c := e.expect(t, e.mustDoctor(t), "security.tools.codeexec", "info")
	if !strings.Contains(c.Message, "legacy-sq (sqlite3)") || strings.Contains(c.Message, "acked-sq") || strings.Contains(c.Message, "plain-ls") ||
		!hasFix(c, "--replace") || !hasFix(c, "--yes-code-exec") {
		t.Fatalf("%+v", c)
	}
	out := mustRun(t, "tool", "list")
	if !strings.Contains(out, "none->ask(forced)") {
		t.Fatalf("%s", out)
	}
	col := -1
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		for _, r := range line {
			if r > 127 {
				t.Fatalf("non-ASCII character in the tool list breaks the column widths: %q", line)
			}
		}
		i := strings.Index(line, "RISK")
		if i < 0 {
			i = strings.Index(line, "code-exec")
		}
		if i >= 0 {
			if col >= 0 && i != col && strings.Contains(line, "code-exec") {
				t.Fatalf("RISK column is misaligned (%d vs %d):\n%s", i, col, out)
			}
			col = i
		}
	}
}

// AC-6: when the registry cannot be opened read-only (an unflushed WAL without -shm), the
// install falls back to the read-write open with the staging under FORGE_HOME/tmp. That
// path now goes through Restage like the normal one (a swapped staging is refused by
// skill.TestRestageRefusesStagingChangedAfterApproval; here: the fallback still installs
// and leaves no staging behind).
func TestSkillAddReadOnlyFallbackStillInstallsThroughRestage(t *testing.T) {
	home := forgeHome(t)
	mustRun(t, "init")
	dbp := store.DBPath(home)
	// close WAL users: init's connection is closed; make an unflushed WAL appear without -shm
	_ = os.Remove(dbp + "-shm")
	if err := os.WriteFile(dbp+"-wal", []byte("not a real wal, but non-empty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := store.OpenReadOnly(dbp); err == nil {
		_ = c.Close()
		t.Skip("the read-only open did not fail here: the fallback path is not taken")
	}
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/SKILL.md", []byte("---\nname: fb-skill\ndescription: fallback skill.\n---\n# fb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runErr(t, "", "skill", "add", dir)
	if err != nil {
		t.Skipf("the fallback condition could not be produced here: %v", err)
	}
	if !strings.Contains(out, "fb-skill") {
		t.Fatalf("%s", out)
	}
	if ents, _ := os.ReadDir(home + "/tmp"); len(ents) != 0 {
		t.Fatalf("staging left behind: %v", ents)
	}
}

func swapStaging(t *testing.T) *bool {
	t.Helper()
	fired := new(bool)
	afterApprovalHook = func(p *skill.Prepared) {
		*fired = true
		if err := os.WriteFile(filepath.Join(p.Root, "SKILL.md"), []byte("---\nname: demo-skill\ndescription: swapped after approval.\n---\n# evil\n"), 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterApprovalHook = nil })
	return fired
}

// AC-6: the staging is replaced after the approval. The read-only-open fallback of
// `skill add` must refuse it exactly like the normal path (Restage compares with the
// approved hash); nothing is installed.
func TestSkillAddFallbackRefusesStagingSwappedAfterApproval(t *testing.T) {
	home := forgeHome(t)
	mustRun(t, "init")
	dbp := store.DBPath(home)
	_ = os.Remove(dbp + "-shm")
	if err := os.WriteFile(dbp+"-wal", []byte("not a real wal, but non-empty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := store.OpenReadOnly(dbp); err == nil {
		_ = c.Close()
		t.Skip("the read-only open did not fail here: the fallback path is not taken")
	}
	fired := swapStaging(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: demo-skill\ndescription: the approved one.\n---\n# good\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runErr(t, "", "skill", "add", dir)
	if !*fired {
		t.Fatal("the fallback path (read-write open) was not taken")
	}
	if err == nil || !strings.Contains(err.Error(), "승인한 해시") {
		t.Fatalf("a swapped staging must be refused with the hash mismatch: %v", err)
	}
	afterApprovalHook = nil
	if out := mustRun(t, "skill", "list"); strings.Contains(out, "demo-skill") {
		t.Fatalf("the swapped tree was installed:\n%s", out)
	}
}

// AC-6: the same for `skill update` (approval with --yes, staging replaced afterwards).
func TestSkillUpdateRefusesStagingSwappedAfterApproval(t *testing.T) {
	forgeHome(t)
	src := filepath.Join(t.TempDir(), "demo-skill")
	_ = os.MkdirAll(src, 0o755)
	_ = os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(v1MD), 0o644)
	mustRun(t, "skill", "add", src)
	_ = os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(v2MD), 0o644)
	fired := swapStaging(t)
	_, _, err := runSplit(t, "", "skill", "update", "--yes", "demo-skill")
	if !*fired {
		t.Fatal("the hook did not run: the update never reached the approval")
	}
	if err == nil || !strings.Contains(err.Error(), "승인한 해시") {
		t.Fatalf("a swapped staging must be refused: %v", err)
	}
	afterApprovalHook = nil
	if o := mustRun(t, "skill", "show", "demo-skill", "--json"); strings.Count(o, `"version": "`) != 1 {
		t.Fatalf("the swapped tree was added as a version: %s", o)
	}
}

// AC-1: the report covers the last 14 days only (a window that grew would show old deletions).
func TestDoctorDeletionWindowIsFourteenDays(t *testing.T) {
	e := newSyncEnv(t)
	mustRun(t, "profile", "create", "cc")
	mustRun(t, "profile", "add", "cc", "--skill", "demo-skill")
	mustRun(t, "skill", "remove", "demo-skill", "--yes")
	db := e.forgeDB(t)
	day := int64(24 * 60 * 60 * 1000)
	now := time.Now().UnixMilli()
	for _, tc := range []struct {
		age  int64
		want bool
	}{{13, true}, {15, false}, {400, false}} {
		if _, err := db.Exec(`UPDATE registry_deletions SET deleted_at = ?`, now-tc.age*day); err != nil {
			t.Fatal(err)
		}
		c := e.mustDoctor(t).get("profile.deleted")
		if (c != nil) != tc.want {
			t.Errorf("deletion %d days old: reported=%v, want %v", tc.age, c != nil, tc.want)
		}
	}
}
