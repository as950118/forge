package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/as950118/forge/internal/store"
)

// TASK-0024 follow-ups of forge doctor and profile.

// AC-4: an unusable database is one finding with the reason; the groups that
// needed it are "점검 불가" warnings (not info) that do not repeat the reason.
func TestDoctorUnreadableDatabaseIsWarnedOnceNotInfo(t *testing.T) {
	e := newSyncEnv(t)
	dbp := store.DBPath(e.homeForge())
	for _, s := range []string{"-wal", "-shm"} {
		_ = os.Remove(dbp + s)
	}
	_ = os.WriteFile(dbp, []byte("garbage that is not sqlite"), 0o600)
	r, err := e.doctor(t)
	if err == nil {
		t.Fatal("a corrupt database is a fail")
	}
	reason := "forge.db 를 읽을 수 없습니다"
	n := 0
	for _, c := range r.Checks {
		n += strings.Count(c.Message+"\n"+c.Cause, reason)
	}
	if n != 1 {
		t.Fatalf("the reason must be reported once, got %d times: %+v", n, r.Checks)
	}
	for _, id := range []string{"drift.skipped", "history.skipped", "profile.skipped", "security.skipped"} {
		c := r.get(id)
		if c == nil || c.Status != "warn" || !strings.Contains(c.Message, "점검 불가") || strings.Contains(c.Message, reason) || len(c.Fix) == 0 {
			t.Errorf("%s: %+v", id, c)
		}
	}
}

// A database that does not exist yet (before forge init) is normal: info.
func TestDoctorMissingDatabaseSkippedGroupsStayInfo(t *testing.T) {
	forgeHome(t)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	home, proj := filepath.Join(base, "home"), filepath.Join(base, "proj")
	_ = os.MkdirAll(home, 0o755)
	_ = os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	t.Setenv("HOME", home)
	out, err := runErr(t, "", "doctor", "--json", "--project-dir", proj)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var r docReport
	if jerr := json.Unmarshal([]byte(out), &r); jerr != nil {
		t.Fatal(jerr)
	}
	for _, id := range []string{"drift.skipped", "history.skipped", "profile.skipped"} {
		if c := r.get(id); c == nil || c.Status != "info" || !strings.Contains(c.Message, "아직 없습니다") {
			t.Errorf("%s: %+v", id, c)
		}
	}
}

// AC-3: a project path with control or invisible characters is shown with visible
// escapes; the fix commands then carry a note that pasting them names another path.
func TestDoctorPathologicalPathFixCarriesAPasteWarning(t *testing.T) {
	forgeHome(t)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(base, "home")
	proj := filepath.Join(base, "pro\x1b[31mj\u202ex")
	for _, d := range []string{home, filepath.Join(proj, ".git")} {
		_ = os.MkdirAll(d, 0o755)
	}
	t.Setenv("HOME", home)
	mustRun(t, "init")
	mustRun(t, "mcp", "add", "direct-srv", "--", "/bin/echo")
	out, err := runErr(t, "", "doctor", "--json", "--project-dir", proj)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x202e) {
		t.Fatal("raw control characters in the output")
	}
	var r docReport
	_ = json.Unmarshal([]byte(out), &r)
	c := r.get("drift.claude-code.project")
	if c == nil || c.Status != "warn" {
		t.Fatalf("%+v", c)
	}
	if !hasFix(c, `\x1b`) || !hasFix(c, "붙여 넣으면 실제 경로와 다른 경로") {
		t.Fatalf("a fix command with an escaped path needs the paste warning: %v", c.Fix)
	}
	// an ordinary path gets no such note
	e := newSyncEnv(t)
	rr := e.mustDoctor(t)
	for _, c := range rr.Checks {
		if hasFix(&c, "붙여 넣으면") {
			t.Fatalf("%s: the note is only for escaped paths: %v", c.ID, c.Fix)
		}
	}
}

// AC-6: different profiles per client; a skill direct for claude-code stays
// visible to OpenCode (.claude/skills) although the opencode profile lacks it.
func TestDoctorWarnsWhenClientsUseDifferentProfilesWithSkillOverlap(t *testing.T) {
	e := newSyncEnv(t)
	mustRun(t, "profile", "create", "cc")
	mustRun(t, "profile", "add", "cc", "--skill", "demo-skill")
	mustRun(t, "profile", "create", "oc")
	mustRun(t, "profile", "add", "oc", "--mcp", "direct-srv")
	mustRun(t, "profile", "use", "cc", "--client", "claude-code")
	mustRun(t, "profile", "use", "oc", "--client", "opencode")
	r := e.mustDoctor(t)
	c := e.expect(t, r, "profile.crossclient", "warn")
	if !strings.Contains(c.Message, "demo-skill") || !strings.Contains(c.Cause, ".claude/skills") || !hasFix(c, "forge profile add cc --skill demo-skill --exposure hidden --client claude-code") {
		t.Fatalf("%+v", c)
	}
	// the opencode profile also has the skill, as direct: no finding
	mustRun(t, "profile", "add", "oc", "--skill", "demo-skill")
	if c := e.mustDoctor(t).get("profile.crossclient"); c != nil {
		t.Fatalf("both direct: %+v", c)
	}
	// the same profile for both clients is the per-profile check, not this one
	mustRun(t, "profile", "use", "cc", "--client", "all")
	if c := e.mustDoctor(t).get("profile.crossclient"); c != nil {
		t.Fatalf("same profile: %+v", c)
	}
}

// AC-11: projects rows that are one directory are reported (info), never merged.
func TestDoctorReportsDuplicateProjectRowsWithoutMerging(t *testing.T) {
	e := newSyncEnv(t)
	mustRun(t, "profile", "create", "p")
	mustRun(t, "profile", "use", "p", "--client", "all") // global; creates no projects row
	link := filepath.Join(filepath.Dir(e.proj), "link-to-proj")
	if err := os.Symlink(e.proj, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	db := e.forgeDB(t)
	for _, p := range []string{e.proj, link} {
		if _, err := db.Exec(`INSERT INTO projects(path, name) VALUES (?, 'proj')`, p); err != nil {
			t.Fatal(err)
		}
	}
	r := e.mustDoctor(t)
	c := e.expect(t, r, "profile.projects.duplicate", "info")
	if !strings.Contains(c.Message, "link-to-proj") || !strings.Contains(c.Cause, "자동으로 합치지 않습니다") {
		t.Fatalf("%+v", c)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM projects`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the rows must stay: %d %v", n, err)
	}
	// a missing directory is "unknown", not a duplicate
	_, _ = db.Exec(`INSERT INTO projects(path, name) VALUES (?, 'gone')`, filepath.Join(filepath.Dir(e.proj), "gone"))
	if c := e.mustDoctor(t).get("profile.projects.duplicate"); c == nil || strings.Contains(c.Message, "gone") {
		t.Fatalf("%+v", c)
	}
}

// AC-11: create --from-current says the profile is a snapshot.
func TestProfileCreateFromCurrentExplainsSnapshot(t *testing.T) {
	newSyncEnv(t)
	out := mustRun(t, "profile", "create", "snap", "--from-current")
	for _, w := range []string{"스냅샷", "forge profile add", "비활성화", "사라집니다"} {
		if !strings.Contains(out, w) {
			t.Fatalf("missing %q in:\n%s", w, out)
		}
	}
	if out := mustRun(t, "profile", "create", "plain"); strings.Contains(out, "스냅샷") {
		t.Fatalf("only --from-current:\n%s", out)
	}
}

// The same profile on both clients with different per-client exposure is the
// per-profile ".overlap" finding only; "profile.crossclient" is for DIFFERENT
// profiles and must not double-report it.
func TestDoctorSameProfileDifferentExposureIsOverlapNotCrossclient(t *testing.T) {
	e := newSyncEnv(t)
	mustRun(t, "profile", "create", "both")
	mustRun(t, "profile", "add", "both", "--skill", "demo-skill")
	mustRun(t, "profile", "add", "both", "--skill", "demo-skill", "--exposure", "hidden", "--client", "opencode")
	mustRun(t, "profile", "use", "both", "--client", "all")
	r := e.mustDoctor(t)
	e.expect(t, r, "profile.health.both.overlap", "warn")
	if c := r.get("profile.crossclient"); c != nil {
		t.Fatalf("the same profile must not produce crossclient: %+v", c)
	}
}
