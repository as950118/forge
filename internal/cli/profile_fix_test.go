package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/heonjinjeong/forge/internal/clientver"
	"github.com/heonjinjeong/forge/internal/store"
)

// ---- helpers ---------------------------------------------------------------

func (e *syncEnv) rawDB(t *testing.T, fk bool) *sql.DB {
	t.Helper()
	v := "1"
	if !fk {
		v = "0"
	}
	db, err := sql.Open("sqlite", "file:"+store.DBPath(e.homeForge())+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys("+v+")")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (e *syncEnv) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.rawDB(t, true).QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *syncEnv) projectRows(t *testing.T) []string {
	t.Helper()
	rows, err := e.rawDB(t, true).Query(`SELECT path FROM projects ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// newProject makes <base>/<name>/.git and returns the directory.
func (e *syncEnv) newProject(t *testing.T, name string) string {
	t.Helper()
	d := filepath.Join(filepath.Dir(e.proj), name)
	if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// caseInsensitive reports whether dir's file system treats names case-insensitively.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

// runFix runs the first fix command that starts with prefix (comment stripped).
func runFix(t *testing.T, c *docCheck, prefix string) string {
	t.Helper()
	for _, f := range c.Fix {
		if strings.HasPrefix(f, prefix) {
			cmd, _, _ := strings.Cut(f, "   #")
			return mustRun(t, strings.Fields(strings.TrimPrefix(cmd, "forge "))...)
		}
	}
	t.Fatalf("no fix starts with %q: %+v", prefix, c.Fix)
	return ""
}

// ---- M1: project identity ---------------------------------------------------

func TestProjectCaseVariantIsTheSameProject(t *testing.T) {
	e := newSyncEnv(t)
	base := filepath.Dir(e.proj)
	real := filepath.Join(base, "RealCase")
	if err := os.MkdirAll(filepath.Join(real, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	insens := caseInsensitive(t, base)
	mustRun(t, "profile", "create", "P")
	variant := filepath.Join(base, "realcase")
	mustRun(t, "profile", "use", "P", "--client", "claude-code", "--project-dir", variant)
	if insens {
		mustRun(t, "profile", "use", "P", "--client", "opencode", "--project-dir", real)
		mustRun(t, "profile", "use", "P", "--client", "opencode", "--project-dir", filepath.Join(base, "REALCASE"))
		rows := e.projectRows(t)
		if len(rows) != 1 || rows[0] != real {
			t.Fatalf("one row stored under the on-disk spelling %q, got %v", real, rows)
		}
		if n := e.count(t, `SELECT count(*) FROM profile_activations WHERE project_id IS NOT NULL`); n != 2 {
			t.Fatalf("2 activations (claude-code, opencode) on one project, got %d", n)
		}
		// every spelling sees the activation
		for _, d := range []string{real, variant, filepath.Join(base, "rEaLcAsE")} {
			out := mustRun(t, "profile", "current", "--client", "claude-code", "--project-dir", d)
			if !strings.Contains(out, "프로필 P") || !strings.Contains(out, "이 프로젝트에 활성화") {
				t.Errorf("%s:\n%s", d, out)
			}
		}
		// unuse through another spelling removes the one activation
		out := mustRun(t, "profile", "unuse", "--client", "all", "--project-dir", filepath.Join(base, "REALCASE"))
		if !strings.Contains(out, "claude-code") || e.count(t, `SELECT count(*) FROM profile_activations`) != 0 {
			t.Fatalf("%s", out)
		}
	} else {
		// a case-sensitive file system: different names are different directories
		other := filepath.Join(base, "realcase")
		if err := os.MkdirAll(filepath.Join(other, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustRun(t, "profile", "use", "P", "--client", "opencode", "--project-dir", real)
		if rows := e.projectRows(t); len(rows) != 2 {
			t.Fatalf("different inodes must stay apart: %v", rows)
		}
	}
}

func TestProjectIdentityFoldsLinksAndDots(t *testing.T) {
	e := newSyncEnv(t)
	base := filepath.Dir(e.proj)
	mustRun(t, "profile", "create", "P")
	sub := filepath.Join(e.proj, "src", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link-to-proj")
	if err := os.Symlink(e.proj, link); err != nil {
		t.Fatal(err)
	}
	for i, d := range []string{
		e.proj, e.proj + "/", filepath.Join(e.proj, "."), filepath.Join(e.proj, "src", ".."),
		sub, filepath.Join(sub, "..", ".."), link, link + "/./src/pkg",
	} {
		client := []string{"claude-code", "opencode"}[i%2]
		mustRun(t, "profile", "use", "P", "--client", client, "--project-dir", d)
	}
	if rows := e.projectRows(t); len(rows) != 1 || rows[0] != e.proj {
		t.Fatalf("all spellings are one project: %v", rows)
	}
	// another project stays apart and is not merged
	other := e.newProject(t, "other")
	mustRun(t, "profile", "use", "P", "--client", "claude-code", "--project-dir", other)
	if rows := e.projectRows(t); len(rows) != 2 {
		t.Fatalf("%v", rows)
	}
	if out := mustRun(t, "profile", "current", "--client", "opencode", "--project-dir", other); !strings.Contains(out, "전역") && !strings.Contains(out, "프로필 없음") {
		t.Fatalf("opencode has no activation in the other project:\n%s", out)
	}
}

func TestConcurrentUseWithTwoSpellingsMakesOneRow(t *testing.T) {
	e := newSyncEnv(t)
	base := filepath.Dir(e.proj)
	insens := caseInsensitive(t, base)
	mustRun(t, "profile", "create", "P")
	spell := []string{e.proj, e.proj + "/./", filepath.Join(base, "proj", "..", "proj")}
	if insens {
		spell = append(spell, filepath.Join(base, "PROJ"), filepath.Join(base, "Proj"))
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = runErr(t, "", "profile", "use", "P", "--client", []string{"claude-code", "opencode"}[i%2], "--project-dir", spell[i%len(spell)])
		}(i)
	}
	wg.Wait()
	if rows := e.projectRows(t); len(rows) != 1 {
		t.Fatalf("one project row expected: %v", rows)
	}
	if n := e.count(t, `SELECT count(*) FROM profile_activations WHERE project_id IS NOT NULL`); n != 2 {
		t.Fatalf("one activation per client, got %d", n)
	}
}

// ---- M2: stale activations ---------------------------------------------------

func TestStaleProjectDoctorPruneAndUnuse(t *testing.T) {
	e := newSyncEnv(t)
	gone := e.newProject(t, "gone")
	moved := e.newProject(t, "moved")
	mustRun(t, "profile", "create", "G")                         // global
	mustRun(t, "profile", "create", "PP", "--project-dir", gone) // project profile of the doomed project
	mustRun(t, "profile", "use", "G", "--client", "claude-code") // global activation
	mustRun(t, "profile", "use", "G", "--client", "claude-code", "--project-dir", e.proj)
	mustRun(t, "profile", "use", "PP", "--client", "claude-code", "--project-dir", gone)
	mustRun(t, "profile", "use", "G", "--client", "opencode", "--project-dir", gone)
	mustRun(t, "profile", "use", "G", "--client", "opencode", "--project-dir", moved)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	movedTo := filepath.Join(filepath.Dir(moved), "moved-new")
	if err := os.Rename(moved, movedTo); err != nil {
		t.Fatal(err)
	}

	// doctor: warn with the exact commands, nothing changed
	before := e.count(t, `SELECT count(*) FROM profile_activations`)
	r := e.mustDoctor(t)
	c := e.expect(t, r, "profile.stale", "warn")
	// the message shortens long paths (120 characters), so compare a prefix only and
	// count the projects instead
	if pre := gone[:min(len(gone), 100)]; !strings.Contains(c.Message, pre) || !strings.Contains(c.Message, "프로젝트 2개") || !hasFix(c, "forge profile prune-stale --dry-run") || !hasFix(c, "forge profile prune-stale --yes") {
		t.Fatalf("%+v", c)
	}
	if e.count(t, `SELECT count(*) FROM profile_activations`) != before {
		t.Fatal("doctor changed the database")
	}

	// dry run (default): lists path, clients and the last activation time, deletes nothing
	out := mustRun(t, "profile", "prune-stale")
	for _, want := range []string{gone, moved, "dry-run", "claude-code -> 프로필 PP", "opencode -> 프로필 G", "마지막 활성화 20", "prune-stale --yes", "프로젝트 프로필 1개(PP)"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, e.proj) {
		t.Errorf("an existing project must not be listed:\n%s", out)
	}
	if out2 := mustRun(t, "profile", "prune-stale", "--dry-run"); out2 != out {
		t.Errorf("--dry-run equals the default:\n%s\n%s", out, out2)
	}
	if e.count(t, `SELECT count(*) FROM profile_activations`) != before {
		t.Fatal("dry run deleted something")
	}
	if _, err := runErr(t, "", "profile", "prune-stale", "--dry-run", "--yes"); err == nil {
		t.Fatal("--dry-run with --yes is contradictory")
	}
	var js struct {
		DryRun   bool `json:"dry_run"`
		Projects []struct {
			Path        string `json:"path"`
			RemoveRow   bool   `json:"remove_row"`
			Activations []struct{ Client string }
		}
	}
	if err := json.Unmarshal([]byte(mustRun(t, "profile", "prune-stale", "--json")), &js); err != nil {
		t.Fatal(err)
	}
	if !js.DryRun || len(js.Projects) != 2 {
		t.Fatalf("%+v", js)
	}

	// profile delete is refused while active, and works after pruning
	if _, err := runErr(t, "", "profile", "delete", "PP", "--project-dir", gone); err == nil || !strings.Contains(err.Error(), "활성화되어") {
		t.Fatalf("an active profile of a deleted project cannot be deleted yet: %v", err)
	}

	// --yes: one transaction; existing projects and the global activation survive
	out = mustRun(t, "profile", "prune-stale", "--yes")
	if !strings.Contains(out, "정리함: 프로젝트 2개") {
		t.Fatalf("%s", out)
	}
	if got := e.count(t, `SELECT count(*) FROM profile_activations`); got != before-3 {
		t.Fatalf("exactly the 3 stale activations go: %d -> %d", before, got)
	}
	if e.count(t, `SELECT count(*) FROM profile_activations WHERE project_id IS NULL`) != 1 {
		t.Fatal("the global activation must stay")
	}
	rows := e.projectRows(t)
	if len(rows) != 2 || !contains(rows, e.proj) || !contains(rows, gone) {
		t.Fatalf("the existing project stays, the project that still owns a profile stays, the orphan row goes: %v", rows)
	}
	if e.count(t, `SELECT count(*) FROM profiles`) != 2 {
		t.Fatal("prune-stale must never delete profiles")
	}
	if out := mustRun(t, "profile", "prune-stale"); !strings.Contains(out, "정리할 항목이 없습니다") {
		t.Fatalf("%s", out)
	}
	r = e.mustDoctor(t)
	if r.get("profile.stale") != nil {
		t.Fatal("no stale warning after pruning")
	}
	// the profile of the deleted project can now be deleted by its path string
	mustRun(t, "profile", "delete", "PP", "--project-dir", gone)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestUnuseAndCurrentWorkForAMissingProjectWithoutCreatingRows(t *testing.T) {
	e := newSyncEnv(t)
	gone := e.newProject(t, "gone")
	mustRun(t, "profile", "create", "G")
	mustRun(t, "profile", "use", "G", "--client", "all", "--project-dir", gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, "profile", "current", "--client", "claude-code", "--project-dir", gone)
	if !strings.Contains(out, "프로필 G") || !strings.Contains(out, "존재하지 않습니다") {
		t.Fatalf("current must still show the stored activation:\n%s", out)
	}
	// via a path with a symlinked ancestor and "..": the string normalization agrees
	link := filepath.Join(filepath.Dir(e.proj), "alias")
	if err := os.Symlink(filepath.Dir(e.proj), link); err != nil {
		t.Fatal(err)
	}
	out = mustRun(t, "profile", "current", "--client", "all", "--project-dir", filepath.Join(link, "x", "..", "gone"))
	if !strings.Contains(out, "프로필 G") {
		t.Fatalf("--client all and a normalized missing path:\n%s", out)
	}
	rowsBefore := len(e.projectRows(t))
	out = mustRun(t, "profile", "unuse", "--client", "all", "--project-dir", gone)
	if !strings.Contains(out, "claude-code") || !strings.Contains(out, "opencode") {
		t.Fatalf("%s", out)
	}
	if e.count(t, `SELECT count(*) FROM profile_activations`) != 0 {
		t.Fatal("not removed")
	}
	// a path that was never a project: nothing happens and no row is created
	nowhere := filepath.Join(filepath.Dir(e.proj), "never-existed")
	if out := mustRun(t, "profile", "unuse", "--client", "all", "--project-dir", nowhere); !strings.Contains(out, "해제할 활성화가 없습니다") {
		t.Fatalf("%s", out)
	}
	mustRun(t, "profile", "current", "--project-dir", nowhere)
	if len(e.projectRows(t)) != rowsBefore {
		t.Fatalf("a missing path must never create a projects row: %v", e.projectRows(t))
	}
	// use / create still need an existing directory (they would write a row)
	if _, err := runErr(t, "", "profile", "use", "G", "--client", "all", "--project-dir", nowhere); err == nil {
		t.Fatal("use must refuse a missing directory")
	}
	if _, err := runErr(t, "", "profile", "create", "Z", "--project-dir", nowhere); err == nil {
		t.Fatal("create must refuse a missing directory")
	}
}

// ---- L3: resolution commands carry the (client, project) -------------------

func TestDoctorFixCommandsCarryClientAndProject(t *testing.T) {
	e := newSyncEnv(t)
	other := e.newProject(t, "other")
	mustRun(t, "profile", "create", "empty", "--project-dir", e.proj)
	mustRun(t, "profile", "use", "empty", "--client", "claude-code", "--project-dir", e.proj)
	r := e.mustDoctor(t)
	c := e.expect(t, r, "profile.health.empty@project", "warn")
	if !hasFix(c, "forge profile unuse --client claude-code --project-dir "+e.proj) {
		t.Fatalf("the unuse command must name the project: %+v", c.Fix)
	}
	// legacy suggestion for the other client also offers the project form
	lc := e.expect(t, r, "profile.rule.opencode", "info")
	if !hasFix(lc, "forge profile use <이름> --client opencode --project-dir "+e.proj) {
		t.Fatalf("%+v", lc.Fix)
	}
	// run the printed unuse command: it must remove the activation
	runFix(t, c, "forge profile unuse")
	if e.count(t, `SELECT count(*) FROM profile_activations`) != 0 {
		t.Fatal("the printed command did not deactivate the project activation")
	}
	// global activation and several clients: no --project-dir; "all" when both
	mustRun(t, "profile", "create", "g-empty")
	mustRun(t, "profile", "use", "g-empty", "--client", "all")
	mustRun(t, "profile", "use", "g-empty", "--client", "opencode", "--project-dir", other)
	r = e.mustDoctor(t)
	c = e.expect(t, r, "profile.health.g-empty", "warn")
	if !hasFix(c, "forge profile unuse --client all   #") || !hasFix(c, "forge profile unuse --client opencode --project-dir "+other) {
		t.Fatalf("%+v", c.Fix)
	}
}

// ---- L4: members -------------------------------------------------------------

func TestBrokenAndDisabledMemberFixCommandsRun(t *testing.T) {
	e := newSyncEnv(t)
	mustRun(t, "profile", "create", "p")
	mustRun(t, "profile", "add", "p", "--skill", "demo-skill")
	mustRun(t, "profile", "add", "p", "--mcp", "direct-srv")
	mustRun(t, "profile", "use", "p", "--client", "all")

	// disabled members: the exact enable command with the real name
	mustRun(t, "mcp", "disable", "direct-srv")
	r := e.mustDoctor(t)
	c := e.expect(t, r, "profile.health.p.disabled", "warn")
	if !hasFix(c, "forge mcp enable direct-srv") || hasFix(c, "<이름>") || !hasFix(c, "forge profile remove p --mcp direct-srv") {
		t.Fatalf("%+v", c.Fix)
	}
	runFix(t, c, "forge mcp enable")
	if r = e.mustDoctor(t); r.get("profile.health.p.disabled") != nil {
		t.Fatal("the enable command did not fix it")
	}

	// a damaged member (dangling skill id) has no name: remove it by member id
	if _, err := e.rawDB(t, false).Exec(`UPDATE profile_members SET skill_id=9999, pinned_version_id=NULL WHERE kind='skill'`); err != nil {
		t.Fatal(err)
	}
	r, derr := e.doctor(t)
	c = r.get("profile.health.p")
	if derr == nil || c == nil || c.Status != "fail" || !strings.Contains(c.Message, "멤버 번호") || hasFix(c, "--skill|--tool|--mcp") {
		t.Fatalf("%v %+v", derr, c)
	}
	show := mustRun(t, "profile", "show", "p")
	if !strings.Contains(show, "멤버 번호") {
		t.Fatalf("profile show must print the member number of damaged members:\n%s", show)
	}
	runFix(t, c, "forge profile remove p --member-id")
	if r, derr = e.doctor(t); derr != nil || r.get("profile.health.p").Status != "ok" {
		t.Fatalf("after removing the damaged member: %v %+v", derr, r.get("profile.health.p"))
	}
	// --member-id is scoped to the profile and exclusive with the name flags
	mustRun(t, "profile", "create", "q")
	if _, err := runErr(t, "", "profile", "remove", "q", "--member-id", "1"); err == nil {
		t.Fatal("a member of another profile must not be removable through q")
	}
	if _, err := runErr(t, "", "profile", "remove", "p", "--member-id", "1", "--mcp", "direct-srv"); err == nil {
		t.Fatal("--member-id excludes --mcp")
	}
	if _, err := runErr(t, "", "profile", "remove", "p", "--member-id", "0"); err == nil {
		t.Fatal("ids start at 1")
	}
}

func TestDoctorProfileCheckIDsAreUnique(t *testing.T) {
	e := newSyncEnv(t)
	// "x" with a disabled member produces profile.health.x.disabled; a profile
	// named "x.disabled" must not produce the same id.
	mustRun(t, "profile", "create", "x")
	mustRun(t, "profile", "create", "x.disabled")
	mustRun(t, "profile", "add", "x", "--mcp", "direct-srv")
	mustRun(t, "profile", "use", "x", "--client", "claude-code")
	mustRun(t, "profile", "use", "x.disabled", "--client", "opencode")
	mustRun(t, "mcp", "disable", "direct-srv")
	// a project profile named like the global one
	mustRun(t, "profile", "create", "x", "--project-dir", e.proj)
	mustRun(t, "profile", "use", "x", "--client", "claude-code", "--project-dir", e.proj)
	r := e.mustDoctor(t)
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if seen[c.ID] {
			t.Errorf("duplicate check id %q", c.ID)
		}
		seen[c.ID] = true
	}
	if r.get("profile.health.x%2Edisabled") == nil || r.get("profile.health.x@project") == nil {
		t.Fatalf("expected escaped / scoped ids: %v", ids(r))
	}
}

func ids(r docReport) []string {
	var out []string
	for _, c := range r.Checks {
		out = append(out, c.ID)
	}
	return out
}

func TestProfileCurrentAcceptsAll(t *testing.T) {
	newSyncEnv(t)
	all := mustRun(t, "profile", "current", "--client", "all")
	none := mustRun(t, "profile", "current")
	if all != none || !strings.Contains(all, "claude-code") || !strings.Contains(all, "opencode") {
		t.Fatalf("--client all means every client:\n%s\n%s", all, none)
	}
	if _, err := runErr(t, "", "profile", "current", "--client", "emacs"); err == nil {
		t.Fatal("unknown client")
	}
}

// ---- C: removal warns about profile memberships ------------------------------

func TestRemoveWarnsWhichProfilesLoseTheItem(t *testing.T) {
	e := newSyncEnv(t)
	mustRun(t, "tool", "add", "say", "--description", "d", "--", "printf", "x")
	mustRun(t, "profile", "create", "p")
	mustRun(t, "profile", "create", "idle")
	mustRun(t, "profile", "create", "scoped", "--project-dir", e.proj)
	for _, name := range []string{"p", "idle"} {
		mustRun(t, "profile", "add", name, "--skill", "demo-skill")
		mustRun(t, "profile", "add", name, "--mcp", "direct-srv")
		mustRun(t, "profile", "add", name, "--tool", "say")
	}
	mustRun(t, "profile", "add", "scoped", "--mcp", "direct-srv", "--project-dir", e.proj)
	mustRun(t, "profile", "use", "p", "--client", "claude-code")

	// declined at the prompt: the warning is shown before the question, nothing removed
	out, err := runErr(t, "n\n", "mcp", "remove", "direct-srv")
	if err == nil || !strings.Contains(out, "다음 프로필에서 제외됩니다") || !strings.Contains(out, "p [활성 프로필: claude-code@전역]") ||
		!strings.Contains(out, "idle") || !strings.Contains(out, "scoped (프로젝트 ") ||
		strings.Index(out, "다음 프로필에서 제외됩니다") > strings.Index(out, "계속할까요?") {
		t.Fatalf("%v\n%s", err, out)
	}
	if e.count(t, `SELECT count(*) FROM mcp_servers WHERE name='direct-srv'`) != 1 {
		t.Fatal("declined remove removed the server")
	}
	// --yes: the warning is still printed, the removal is not blocked
	out = mustRun(t, "mcp", "remove", "direct-srv", "--yes")
	if !strings.Contains(out, "다음 프로필에서 제외됩니다") || !strings.Contains(out, "삭제됨: direct-srv") {
		t.Fatalf("%s", out)
	}
	if e.count(t, `SELECT count(*) FROM profile_members WHERE kind='mcp'`) != 0 {
		t.Fatal("the membership cascade is unchanged")
	}
	out = mustRun(t, "skill", "remove", "demo-skill", "--yes")
	if !strings.Contains(out, "다음 프로필에서 제외됩니다: ") || !strings.Contains(out, "p [활성 프로필: claude-code@전역]") {
		t.Fatalf("%s", out)
	}
	out = mustRun(t, "tool", "remove", "say", "--yes")
	if !strings.Contains(out, "다음 프로필에서 제외됩니다") {
		t.Fatalf("%s", out)
	}
	// an item in no profile: no warning
	mustRun(t, "mcp", "add", "lonely", "--", "npx", "x")
	if out := mustRun(t, "mcp", "remove", "lonely", "--yes"); strings.Contains(out, "제외됩니다") {
		t.Fatalf("%s", out)
	}
}

// ---- W at the CLI --------------------------------------------------------------

func TestSyncApplyRefusesAWritableClientDirectoryEvenWithAllowUntested(t *testing.T) {
	e := newSyncEnv(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o777); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\ntouch "+marker+"\necho 2.1.305\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := versionChecker
	versionChecker = func(dirs []string) *clientver.Checker { return &clientver.Checker{PathEnv: bin, ProjectDirs: dirs} }
	defer func() { versionChecker = old }()
	for _, args := range [][]string{{"--target", "claude-code", "--apply"}, {"--target", "claude-code", "--apply", "--allow-untested-version"}} {
		_, err := e.sync(t, args...)
		if err == nil || !strings.Contains(err.Error(), "다른 사용자가 수정할 수 있는 위치") || !strings.Contains(err.Error(), "우회할 수 없습니다") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("the world-writable binary was executed")
	}
	if exists(t, e.proj, ".mcp.json") {
		t.Fatal("nothing may be written")
	}
	// doctor: warn with chmod advice, never executed
	r := e.mustDoctor(t)
	c := e.expect(t, r, "client.claude-code", "warn")
	if !strings.Contains(c.Message, "실행하지 않았습니다") || !hasFix(c, "chmod go-w "+bin) {
		t.Fatalf("%+v", c)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("doctor executed the world-writable binary")
	}
	// after chmod go-w the same binary is accepted
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	e.syncOK(t, "--target", "claude-code", "--apply")
	if _, serr := os.Stat(marker); serr != nil || !exists(t, e.proj, ".mcp.json") {
		t.Fatal("a 0755 user-owned directory must be accepted and applied")
	}
}

// T1: only "does not exist" (ENOENT/ENOTDIR) makes a project stale. A path that
// cannot be examined (permission denied, symlink loop) is not proven gone.
func TestStaleOnlyWhenTheDirectoryIsProvenGone(t *testing.T) {
	e := newSyncEnv(t)
	base := filepath.Dir(e.proj)
	// EACCES: a project below a directory without search permission
	locked := filepath.Join(base, "locked")
	inner := filepath.Join(locked, "inner")
	if err := os.MkdirAll(filepath.Join(inner, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// ELOOP: a project directory replaced by a symlink to itself
	loop := e.newProject(t, "loop")
	// a real deletion, as the control that must be pruned
	gone := e.newProject(t, "gone")
	mustRun(t, "profile", "create", "G")
	for _, d := range []string{inner, loop, gone} {
		mustRun(t, "profile", "use", "G", "--client", "claude-code", "--project-dir", d)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(loop); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := os.Stat(inner); err == nil || os.IsNotExist(err) {
		t.Skipf("cannot make %s unreadable here (running as root?): %v", inner, err)
	}
	before := e.count(t, `SELECT count(*) FROM profile_activations`)

	out := mustRun(t, "profile", "prune-stale")
	if !strings.Contains(out, gone) || strings.Contains(out, "inner") || strings.Contains(out, "/loop") {
		t.Fatalf("only the deleted project is stale:\n%s", out)
	}
	r := e.mustDoctor(t)
	if c := r.get("profile.stale"); c == nil || strings.Contains(c.Message, "inner") || strings.Contains(c.Message, "/loop") || !strings.Contains(c.Message, "1개") {
		t.Fatalf("doctor must report only the deleted project: %+v", c)
	}
	mustRun(t, "profile", "prune-stale", "--yes")
	if got := e.count(t, `SELECT count(*) FROM profile_activations`); got != before-1 {
		t.Fatalf("exactly one activation (the deleted project) goes: %d -> %d", before, got)
	}
	rows := e.projectRows(t)
	if !contains(rows, inner) || !contains(rows, loop) || contains(rows, gone) {
		t.Fatalf("unexaminable projects keep their rows: %v", rows)
	}
	if out := mustRun(t, "profile", "prune-stale"); !strings.Contains(out, "정리할 항목이 없습니다") {
		t.Fatalf("%s", out)
	}
}

// The "does not exist" wording is only for a missing path; an unreadable one says so.
func TestProfileCurrentDistinguishesMissingFromUnreadable(t *testing.T) {
	e := newSyncEnv(t)
	base := filepath.Dir(e.proj)
	locked := filepath.Join(base, "locked2")
	if err := os.MkdirAll(filepath.Join(locked, "p", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := os.Stat(filepath.Join(locked, "p")); err == nil {
		t.Skip("cannot make the directory unreadable here (running as root?)")
	}
	out := mustRun(t, "profile", "current", "--project-dir", filepath.Join(locked, "p"))
	if strings.Contains(out, "존재하지 않습니다") || !strings.Contains(out, "확인할 수 없습니다") {
		t.Fatalf("an unreadable path is not a missing path:\n%s", out)
	}
	out = mustRun(t, "profile", "current", "--project-dir", filepath.Join(base, "never-existed"))
	if !strings.Contains(out, "존재하지 않습니다") {
		t.Fatalf("%s", out)
	}
}
