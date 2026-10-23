package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/heonjinjeong/forge/internal/store"
)

type docCheck struct {
	ID      string   `json:"id"`
	Group   string   `json:"group"`
	Title   string   `json:"title"`
	Status  string   `json:"status"`
	Message string   `json:"message"`
	Cause   string   `json:"cause"`
	Fix     []string `json:"fix"`
}

type docReport struct {
	Schema     int            `json:"schema"`
	Home       string         `json:"home"`
	Project    string         `json:"project"`
	Scope      string         `json:"scope"`
	Strict     bool           `json:"strict"`
	ExitCode   int            `json:"exit_code"`
	Summary    map[string]int `json:"summary"`
	Checks     []docCheck     `json:"checks"`
	DurationMS int64          `json:"duration_ms"`
}

func (r docReport) get(id string) *docCheck {
	for i := range r.Checks {
		if r.Checks[i].ID == id {
			return &r.Checks[i]
		}
	}
	return nil
}

func (r docReport) prefix(p string) []docCheck {
	var out []docCheck
	for _, c := range r.Checks {
		if strings.HasPrefix(c.ID, p) {
			out = append(out, c)
		}
	}
	return out
}

// has reports whether some check has the status and its message/cause/fix contains all words.
func (r docReport) find(status string, words ...string) *docCheck {
next:
	for i := range r.Checks {
		c := &r.Checks[i]
		if c.Status != status {
			continue
		}
		blob := c.Title + "\n" + c.Message + "\n" + c.Cause + "\n" + strings.Join(c.Fix, "\n")
		for _, w := range words {
			if !strings.Contains(blob, w) {
				continue next
			}
		}
		return c
	}
	return nil
}

func (e *syncEnv) doctor(t *testing.T, args ...string) (docReport, error) {
	t.Helper()
	all := append([]string{"doctor", "--json", "--project-dir", e.proj}, args...)
	out, err := runErr(t, "", all...)
	var r docReport
	if jerr := json.Unmarshal([]byte(out), &r); jerr != nil {
		t.Fatalf("doctor output is not JSON: %v\n%s", jerr, out)
	}
	return r, err
}

func (e *syncEnv) mustDoctor(t *testing.T, args ...string) docReport {
	t.Helper()
	r, err := e.doctor(t, args...)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	return r
}

func (e *syncEnv) expect(t *testing.T, r docReport, id, status string) *docCheck {
	t.Helper()
	c := r.get(id)
	if c == nil {
		var ids []string
		for _, x := range r.Checks {
			ids = append(ids, x.ID+"="+x.Status)
		}
		t.Fatalf("no check %q in %v", id, ids)
	}
	if c.Status != status {
		t.Fatalf("%s: status %s, want %s: %s | %s | %v", id, c.Status, status, c.Message, c.Cause, c.Fix)
	}
	if status != "ok" && (c.Cause == "" || len(c.Fix) == 0) && !strings.HasPrefix(id, "client.") {
		t.Fatalf("%s: a %s finding needs a cause and a fix command: %+v", id, status, c)
	}
	return c
}

func (e *syncEnv) applyAll(t *testing.T) {
	t.Helper()
	e.syncOK(t, "--target", "all", "--apply")
}

func (e *syncEnv) forgeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.DBPath(e.homeForge()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func hasFix(c *docCheck, sub string) bool {
	for _, f := range c.Fix {
		if strings.Contains(f, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------

func TestDoctorFreshHomeIsInfoNotFailure(t *testing.T) {
	forgeHome(t)
	fh := os.Getenv("FORGE_HOME")
	_ = os.RemoveAll(fh) // not initialized at all
	proj, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	t.Setenv("HOME", t.TempDir())
	out, err := runErr(t, "", "doctor", "--json", "--project-dir", proj)
	if err != nil {
		t.Fatalf("a fresh machine is fine: %v\n%s", err, out)
	}
	var r docReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	c := r.get("home.dir")
	if c == nil || c.Status != "info" || !hasFix(c, "forge init") {
		t.Fatalf("%+v", c)
	}
	if _, err := os.Stat(fh); err == nil {
		t.Fatal("doctor created the Forge home")
	}
	if r.Summary["fail"] != 0 {
		t.Fatalf("%+v", r.Summary)
	}
}

func TestDoctorHealthyEnvironmentAndJSONSchema(t *testing.T) {
	e := newSyncEnv(t)
	mustRun(t, "mcp", "expose", "gw-srv", "--client", "claude-code", "--mode", "direct") // no gateway-only server without a cache
	e.applyAll(t)
	_ = os.Chmod(e.homeForge(), 0o700)
	r := e.mustDoctor(t)
	if r.Schema != 1 || r.Scope != "project" || r.Strict || r.ExitCode != 0 || r.Project != e.proj || r.Home != e.homeForge() {
		t.Fatalf("%+v", r)
	}
	for _, id := range []string{"home.dir", "db.perm", "db.integrity", "db.migrations", "db.wal", "drift.claude-code.project", "drift.opencode.project",
		"hygiene.backups", "hygiene.perm", "profile.rule.claude-code", "profile.rule.opencode", "history.size"} {
		if c := r.get(id); c == nil {
			t.Errorf("missing check %s", id)
		}
	}
	for _, id := range []string{"home.dir", "db.perm", "db.integrity", "db.migrations", "drift.claude-code.project", "drift.opencode.project"} {
		e.expect(t, r, id, "ok")
	}
	if n := r.Summary["ok"] + r.Summary["info"] + r.Summary["warn"] + r.Summary["fail"]; n != len(r.Checks) {
		t.Fatalf("summary does not add up: %+v / %d checks", r.Summary, len(r.Checks))
	}
	// Stable schema: exactly these keys.
	out, _ := runErr(t, "", "doctor", "--json", "--project-dir", e.proj)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(out), &raw)
	for _, k := range []string{"schema", "home", "scope", "strict", "exit_code", "summary", "checks", "duration_ms"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("JSON misses %q", k)
		}
	}
	var checks []map[string]json.RawMessage
	_ = json.Unmarshal(raw["checks"], &checks)
	for _, c := range checks {
		for _, k := range []string{"id", "group", "title", "status", "message"} {
			if _, ok := c[k]; !ok {
				t.Errorf("a check misses %q: %v", k, c)
			}
		}
	}
	txt, err := runErr(t, "", "doctor", "--project-dir", e.proj)
	if err != nil || !strings.Contains(txt, "== 홈·DB ==") || !strings.Contains(txt, "요약: ok") {
		t.Fatalf("%v\n%s", err, txt)
	}
}

func TestDoctorExitCodesAndStrict(t *testing.T) {
	e := newSyncEnv(t)
	e.applyAll(t)
	// warn only: exit 0, but --strict fails
	if err := os.Chmod(e.homeForge(), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := e.doctor(t)
	if err != nil || r.Summary["warn"] == 0 || r.Summary["fail"] != 0 || r.ExitCode != 0 {
		t.Fatalf("%v %+v", err, r.Summary)
	}
	if r, err := e.doctor(t, "--strict"); err == nil || r.ExitCode != 1 || !r.Strict || !strings.Contains(err.Error(), "--strict") {
		t.Fatalf("strict: %v %+v", err, r)
	}
	_ = os.Chmod(e.homeForge(), 0o700)
	// a fail makes the exit code 1 without --strict
	if err := os.WriteFile(store.DBPath(e.homeForge()), []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = e.doctor(t)
	if err == nil || r.ExitCode != 1 || r.Summary["fail"] == 0 || !strings.Contains(err.Error(), "fail") {
		t.Fatalf("%v %+v", err, r.Summary)
	}
	if _, err := runErr(t, "", "doctor", "--scope", "everywhere"); err == nil {
		t.Fatal("bad --scope")
	}
}

func TestDoctorPermissions(t *testing.T) {
	e := newSyncEnv(t)
	e.applyAll(t)
	_ = os.Chmod(e.homeForge(), 0o755)
	_ = os.Chmod(store.DBPath(e.homeForge()), 0o644)
	_ = os.Chmod(filepath.Join(e.proj, ".mcp.json"), 0o666)
	_ = os.Chmod(filepath.Join(e.homeForge(), "backups"), 0o755)
	r := e.mustDoctor(t)
	c := e.expect(t, r, "home.dir", "warn")
	if !strings.Contains(c.Message, "world-readable") || !hasFix(c, "chmod 700 "+e.homeForge()) {
		t.Fatalf("%+v", c)
	}
	c = e.expect(t, r, "db.perm", "warn")
	if !hasFix(c, "chmod 600 "+store.DBPath(e.homeForge())) {
		t.Fatalf("%+v", c)
	}
	c = e.expect(t, r, "hygiene.perm..mcp.json", "warn")
	if !hasFix(c, "chmod go-w "+filepath.Join(e.proj, ".mcp.json")) || !strings.Contains(c.Message, "0666") {
		t.Fatalf("%+v", c)
	}
	e.expect(t, r, "hygiene.backups.perm", "warn")
	// group-only exposure is a warning too, without the "world" wording
	_ = os.Chmod(e.homeForge(), 0o750)
	if c := e.expect(t, e.mustDoctor(t), "home.dir", "warn"); strings.Contains(c.Message, "world") {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorDriftConflictsPendingAndStale(t *testing.T) {
	e := newSyncEnv(t)
	mustRun(t, "mcp", "expose", "gw-srv", "--client", "claude-code", "--mode", "direct")
	e.applyAll(t)
	r := e.mustDoctor(t)
	e.expect(t, r, "drift.claude-code.project", "ok")

	// 1. the user edits a Forge-managed entry -> conflict, overridable
	p := filepath.Join(e.proj, ".mcp.json")
	b, _ := os.ReadFile(p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), `"srv"`, `"srv-edited"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	r = e.mustDoctor(t)
	c := e.expect(t, r, "drift.claude-code.project", "warn")
	flags := "--target claude-code --scope project --project-dir " + e.proj
	if !strings.Contains(c.Message, "충돌 1건") || !strings.Contains(c.Message, "direct-srv") ||
		!hasFix(c, "forge sync "+flags+" --diff") || !hasFix(c, "forge sync "+flags+" --apply --force-managed") || !hasFix(c, "--skip-conflicts") {
		t.Fatalf("%+v", c)
	}
	e.expect(t, r, "drift.opencode.project", "ok")
	mustRun(t, "sync", "--project-dir", e.proj, "--target", "claude-code", "--apply", "--force-managed")

	// 2. a registry change that is not synced yet
	mustRun(t, "mcp", "add", "late-srv", "--", "/bin/echo")
	r = e.mustDoctor(t)
	c = e.expect(t, r, "drift.opencode.project", "warn")
	if !strings.Contains(c.Message, "미적용 변경 1건") || !strings.Contains(c.Message, "late-srv") || !hasFix(c, "forge sync --target opencode --scope project --project-dir "+e.proj+" --apply") {
		t.Fatalf("%+v", c)
	}
	e.applyAll(t)

	// 3. a server that is disabled still sits in the config -> stale entry
	mustRun(t, "mcp", "disable", "late-srv")
	r = e.mustDoctor(t)
	c = e.expect(t, r, "drift.claude-code.project.stale", "warn")
	if !strings.Contains(c.Message, "late-srv") || !hasFix(c, "--apply") {
		t.Fatalf("%+v", c)
	}
	e.applyAll(t)
	e.expect(t, e.mustDoctor(t), "drift.claude-code.project", "ok")
}

func TestDoctorBrokenPaths(t *testing.T) {
	e := newSyncEnv(t)
	e.applyAll(t)
	// a managed skill directory is deleted
	if err := os.RemoveAll(filepath.Join(e.proj, ".claude", "skills", "demo-skill")); err != nil {
		t.Fatal(err)
	}
	r := e.mustDoctor(t)
	c := e.expect(t, r, "paths.claude-code.project.skilldirs", "warn")
	if !strings.Contains(c.Message, "demo-skill") || !hasFix(c, "--apply") {
		t.Fatalf("%+v", c)
	}
	e.applyAll(t)

	// a gateway-exposed server has no tools/list cache
	c = e.expect(t, r, "paths.claude-code.project.toolcache", "warn")
	if !strings.Contains(c.Message, "gw-srv") || !hasFix(c, "forge mcp refresh gw-srv") {
		t.Fatalf("%+v", c)
	}
	if r.get("paths.opencode.project.toolcache") != nil {
		t.Fatal("gw-srv is direct for opencode: no cache needed")
	}

	// a managed MCP entry whose command does not exist; nothing is executed
	marker := filepath.Join(t.TempDir(), "executed")
	mustRun(t, "mcp", "add", "ghost-srv", "--", "/nonexistent/forge-test/ghost-server")
	mustRun(t, "mcp", "add", "script-srv", "--", writeExe(t, "#!/bin/sh\ntouch "+marker+"\n"))
	e.applyAll(t)
	r = e.mustDoctor(t)
	c = e.expect(t, r, "paths.claude-code.project.mcpcommand", "warn")
	if !strings.Contains(c.Message, "ghost-srv") || strings.Contains(c.Message, "script-srv") || !hasFix(c, "forge mcp show") {
		t.Fatalf("%+v", c)
	}
	e.expect(t, r, "paths.opencode.project.mcpcommand", "warn")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("doctor executed a command found in a client config")
	}
	// a relative command is reported as such
	mustRun(t, "mcp", "add", "rel-srv", "--", "tools/rel-server")
	e.applyAll(t)
	if c := e.expect(t, e.mustDoctor(t), "paths.claude-code.project.mcpcommand", "warn"); !strings.Contains(c.Message, "상대 경로") {
		t.Fatalf("%+v", c)
	}
}

func writeExe(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "exe.sh")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDoctorGatewayEntryChecks(t *testing.T) {
	e := newSyncEnv(t)
	// sync with a forge path that does not exist
	e.syncOK(t, "--target", "claude-code", "--apply", "--bin", "/nonexistent/forge-test/forge")
	r := e.mustDoctor(t)
	c := e.expect(t, r, "paths.claude-code.project.gatewaypath", "warn")
	if !strings.Contains(c.Message, "찾을 수 없습니다") || !hasFix(c, "--apply --bin ") {
		t.Fatalf("%+v", c)
	}
	// the drift check plans with the path that is already in the file: no false drift
	e.expect(t, r, "drift.claude-code.project", "ok") // the path already in the file is not drift
	// a different, existing forge binary
	other := writeExe(t, "#!/bin/sh\nexit 0\n")
	e.syncOK(t, "--target", "claude-code", "--apply", "--bin", other)
	r = e.mustDoctor(t)
	c = e.expect(t, r, "paths.claude-code.project.gatewaypath", "warn")
	if !strings.Contains(c.Message, "다른 바이너리") {
		t.Fatalf("%+v", c)
	}
	// a relative path in the file is flagged (written by hand)
	p := filepath.Join(e.proj, ".mcp.json")
	b, _ := os.ReadFile(p)
	_ = os.WriteFile(p, []byte(strings.Replace(string(b), other, "bin/forge", 1)), 0o644)
	if c := e.mustDoctor(t).get("paths.claude-code.project.gatewaypath"); c != nil && c.Status == "ok" {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorGatewayEntryMissingWhileGatewayIsNeeded(t *testing.T) {
	e := newSyncEnv(t)
	e.applyAll(t)
	p := filepath.Join(e.proj, ".mcp.json")
	b, _ := os.ReadFile(p)
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	delete(doc["mcpServers"].(map[string]any), "forge")
	nb, _ := json.MarshalIndent(doc, "", "  ")
	_ = os.WriteFile(p, append(nb, '\n'), 0o644)
	r := e.mustDoctor(t)
	c := e.expect(t, r, "paths.claude-code.project.gateway", "warn")
	if !strings.Contains(c.Message, "게이트웨이 항목(forge)이 없습니다") || !hasFix(c, "--apply") {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorLinksAndForgedStateDoNotStopOtherChecks(t *testing.T) {
	e := newSyncEnv(t)
	e.applyAll(t)
	// .mcp.json becomes a symlink; opencode's sidecar is garbage
	p := filepath.Join(e.proj, ".mcp.json")
	real := filepath.Join(t.TempDir(), "real.json")
	b, _ := os.ReadFile(p)
	_ = os.WriteFile(real, b, 0o644)
	_ = os.Remove(p)
	if err := os.Symlink(real, p); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(e.proj, ".forge", "sync-state.opencode.json"), []byte("{garbage"), 0o644)
	r := e.mustDoctor(t)
	var link *docCheck
	for _, c := range r.Checks {
		if strings.HasPrefix(c.ID, "drift.claude-code.project") && c.Status == "warn" && strings.Contains(c.Message+c.Cause, "링크") {
			link = &c
		}
	}
	if link == nil || len(link.Fix) == 0 {
		t.Fatalf("symlinked config: %+v", r.prefix("drift.claude-code"))
	}
	oc := r.get("drift.opencode.project")
	if oc == nil || oc.Status != "warn" || !strings.Contains(oc.Message, "점검 불가") && !strings.Contains(oc.Message, "상태") {
		t.Fatalf("%+v", oc)
	}
	// every other group still ran
	for _, id := range []string{"home.dir", "db.integrity", "hygiene.backups", "profile.rule.claude-code"} {
		if r.get(id) == nil {
			t.Errorf("%s did not run", id)
		}
	}
	// hard link
	_ = os.Remove(p)
	_ = os.WriteFile(p, b, 0o644)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.Link(p, other); err != nil {
		t.Fatal(err)
	}
	r = e.mustDoctor(t)
	hl := false
	for _, c := range r.Checks {
		if strings.HasPrefix(c.ID, "drift.claude-code") && strings.Contains(c.Message, "하드링크") && len(c.Fix) > 0 {
			hl = true
		}
	}
	if !hl {
		t.Fatalf("hard linked config: %+v", r.prefix("drift.claude-code"))
	}
}

func TestDoctorInterruptedSyncAndBackupDependency(t *testing.T) {
	e := newSyncEnv(t)
	e.applyAll(t)
	// a journal copy whose backup directory is gone
	j := `{"version":1,"id":"20260101T000000.000Z-abcdef","status":"pending"}`
	jp := filepath.Join(e.proj, ".forge", "sync-journal.json")
	if err := os.WriteFile(jp, []byte(j), 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.mustDoctor(t)
	c := e.expect(t, r, "drift.claude-code.project.backup", "warn")
	if !strings.Contains(c.Message, "20260101T000000.000Z-abcdef") || !strings.Contains(c.Cause, "백업") || !hasFix(c, "--force-managed") || !hasFix(c, "--skip-conflicts") || !hasFix(c, "rm "+jp) {
		t.Fatalf("%+v", c)
	}
	// an unreadable copy
	_ = os.WriteFile(jp, []byte(`{"id":"../../etc/passwd"}`), 0o644)
	if c := e.expect(t, e.mustDoctor(t), "drift.claude-code.project.backup", "warn"); !strings.Contains(c.Message, "해석할 수 없습니다") {
		t.Fatalf("a forged id is never followed: %+v", c)
	}
	_ = os.Remove(jp)
	// the backup exists: no finding
	id := "20260101T000000.000Z-abcdef"
	_ = os.MkdirAll(filepath.Join(e.homeForge(), "backups", id), 0o700)
	_ = os.WriteFile(jp, []byte(j), 0o644)
	if c := e.mustDoctor(t).get("drift.claude-code.project.backup"); c != nil {
		t.Fatalf("%+v", c)
	}
	// forged pending journals in the backup directory are reported, never used
	bd := filepath.Join(e.homeForge(), "backups", "20260102T000000.000Z-111111")
	_ = os.MkdirAll(bd, 0o700)
	_ = os.WriteFile(filepath.Join(bd, "journal.json"), []byte(fmt.Sprintf(`{"version":1,"id":"20260102T000000.000Z-111111","kind":"sync","target":"claude-code","scope":"project","root":%q,"status":"pending","ops":[{"path":"/etc/passwd","op":"delete-file","before_exists":true}]}`, e.proj)), 0o600)
	r = e.mustDoctor(t)
	if c := r.get("drift.claude-code.project.journal"); c == nil || c.Status != "warn" || !hasFix(c, "--list-backups") {
		t.Fatalf("%+v", r.prefix("drift.claude-code"))
	}
	if _, err := os.Stat("/etc/passwd"); err != nil {
		t.Fatal("sanity")
	}
}

func TestDoctorHookActivity(t *testing.T) {
	e := newSyncEnv(t)
	e.applyAll(t)
	// just installed: no verdict
	e.expect(t, e.mustDoctor(t), "history.hook", "info")
	// installed 30 days ago, no events
	sp := filepath.Join(e.proj, ".forge", "sync-state.json")
	b, _ := os.ReadFile(sp)
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	nb := regexp.MustCompile(`"applied_at": "[^"]*"`).ReplaceAllString(string(b), `"applied_at": "`+old+`"`)
	_ = os.WriteFile(sp, []byte(nb), 0o644)
	r := e.mustDoctor(t)
	c := e.expect(t, r, "history.hook", "warn")
	if !strings.Contains(c.Message, "0건") || !hasFix(c, "forge history --source hook --since 7d") || !hasFix(c, "forge sync --target claude-code --project-dir ") || !hasFix(c, " --diff   # hook") {
		t.Fatalf("%+v", c)
	}
	// an event inside the window: ok
	db := e.forgeDB(t)
	if _, err := db.Exec(`INSERT INTO ledger(ts,kind,name,project,source,client) VALUES (?, 'skill', 'x', ?, 'hook', 'claude-code')`, time.Now().UnixMilli(), e.proj); err != nil {
		t.Fatal(err)
	}
	e.expect(t, e.mustDoctor(t), "history.hook", "ok")
	// --hook-days narrows the window
	if _, err := db.Exec(`UPDATE ledger SET ts = ?`, time.Now().Add(-3*24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	e.expect(t, e.mustDoctor(t), "history.hook", "ok")
	e.expect(t, e.mustDoctor(t, "--hook-days", "1"), "history.hook", "warn")
	// without a managed hook there is nothing to check
	mustRun(t, "sync", "--project-dir", e.proj, "--target", "claude-code", "--apply", "--remove-hooks")
	if c := e.mustDoctor(t).get("history.hook"); c != nil {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorHistoryHealth(t *testing.T) {
	e := newSyncEnv(t)
	db := e.forgeDB(t)
	for i := 0; i < 100; i++ {
		if _, err := db.Exec(`INSERT INTO ledger(ts,kind,name,session_id,source,found,client) VALUES (?, 'tool', ?, 'gw-sess', 'gateway', 0, 'claude-code')`, time.Now().UnixMilli(), fmt.Sprintf("unknown%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	r := e.mustDoctor(t)
	c := e.expect(t, r, "history.unknown", "warn")
	if !strings.Contains(c.Message, "100") || !hasFix(c, "forge history misses") {
		t.Fatalf("%+v", c)
	}
	e.expect(t, r, "history.size", "ok")
}

func TestDoctorBackupsHygiene(t *testing.T) {
	e := newSyncEnv(t)
	bd := filepath.Join(e.homeForge(), "backups")
	_ = os.MkdirAll(bd, 0o700)
	for i := 0; i < 22; i++ {
		_ = os.MkdirAll(filepath.Join(bd, fmt.Sprintf("202601%02dT000000.000Z-%06x", i+1, i)), 0o700)
	}
	r := e.mustDoctor(t)
	c := e.expect(t, r, "hygiene.backups", "warn")
	if !strings.Contains(c.Message, "22개") || !hasFix(c, "forge sync --prune-backups --dry-run") || !hasFix(c, "forge sync --prune-backups --keep 20") {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorDatabaseProblems(t *testing.T) {
	// not a database
	e := newSyncEnv(t)
	dbp := store.DBPath(e.homeForge())
	_ = os.WriteFile(dbp, []byte("garbage"), 0o600)
	r, err := e.doctor(t)
	if err == nil {
		t.Fatal("must fail")
	}
	c := r.get("db.integrity")
	if c == nil || c.Status != "fail" || !hasFix(c, "backups") {
		t.Fatalf("%+v", c)
	}
	for _, id := range []string{"home.dir", "hygiene.backups", "client.claude-code"} {
		if r.get(id) == nil {
			t.Errorf("%s must still run", id)
		}
	}

	// a directory in place of the database is not "corruption"
	e = newSyncEnv(t)
	dbp = store.DBPath(e.homeForge())
	_ = os.Remove(dbp)
	_ = os.Remove(dbp + "-wal")
	_ = os.Remove(dbp + "-shm")
	_ = os.Mkdir(dbp, 0o700)
	r, _ = e.doctor(t)
	if c := r.get("db.file"); c == nil || c.Status != "fail" || !strings.Contains(c.Message, "디렉터리") || strings.Contains(c.Message+c.Cause, "손상되었") {
		t.Fatalf("%+v", c)
	}

	// a real SQLite file that is not a Forge database
	e = newSyncEnv(t)
	dbp = store.DBPath(e.homeForge())
	for _, s := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbp + s)
	}
	other, _ := sql.Open("sqlite", "file:"+dbp)
	_, _ = other.Exec(`CREATE TABLE t(x)`)
	_ = other.Close()
	r, err = e.doctor(t)
	if err == nil || r.get("db.integrity") == nil || r.get("db.integrity").Status != "fail" {
		t.Fatalf("%v %+v", err, r.get("db.integrity"))
	}

	// a database from a newer forge, and a modified migration
	e = newSyncEnv(t)
	db := e.forgeDB(t)
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, name, checksum) VALUES (99, '0099_future', 'x')`); err != nil {
		t.Fatal(err)
	}
	r, err = e.doctor(t)
	if err == nil {
		t.Fatal("must fail")
	}
	if c := r.get("db.migrations"); c == nil || c.Status != "fail" || !strings.Contains(c.Message, "최신") || !strings.Contains(c.Cause, "다운그레이드") {
		t.Fatalf("%+v", c)
	}
	_, _ = db.Exec(`DELETE FROM schema_migrations WHERE version=99`)
	_, _ = db.Exec(`UPDATE schema_migrations SET checksum='deadbeef' WHERE version=1`)
	if c := e.mustDocFail(t).get("db.migrations"); c == nil || c.Status != "fail" || !strings.Contains(c.Message, "checksum") {
		t.Fatalf("%+v", c)
	}
}

func (e *syncEnv) mustDocFail(t *testing.T) docReport {
	t.Helper()
	r, err := e.doctor(t)
	if err == nil {
		t.Fatal("doctor must fail")
	}
	return r
}

func TestDoctorClientVersions(t *testing.T) {
	e := newSyncEnv(t)
	_, argv := fakeClients(t, map[string]string{"claude": "2.1.305 (Claude Code)", "opencode": "2.4.0"})
	r := e.mustDoctor(t)
	c := e.expect(t, r, "client.claude-code", "ok")
	if !strings.Contains(c.Message, "2.1.305") {
		t.Fatalf("%+v", c)
	}
	c = e.expect(t, r, "client.opencode", "warn")
	if !strings.Contains(c.Message, "미검증 버전: 생성한 설정이 올바르게 로드되지 않을 수 있습니다") || !hasFix(c, "forge sync --target opencode --project-dir ") || !hasFix(c, " --check-version") {
		t.Fatalf("%+v", c)
	}
	if got := argv(); strings.Count(got, "n=1 [--version]\n") != 2 || strings.Count(got, "\n") != 2 {
		t.Fatalf("doctor may only run <client> --version: %q", got)
	}
	// patch-level difference is info, not warn
	_, _ = fakeClients(t, map[string]string{"claude": "2.1.399"})
	if c := e.expect(t, e.mustDoctor(t), "client.claude-code", "info"); !strings.Contains(c.Message, "미검증 패치") {
		t.Fatalf("%+v", c)
	}
	// not installed: info
	fakeClients(t, map[string]string{})
	if c := e.expect(t, e.mustDoctor(t), "client.claude-code", "info"); !strings.Contains(c.Message, "PATH 에 없습니다") {
		t.Fatalf("%+v", c)
	}
	// a failing --version: warn with a command to try by hand
	dir, _ := fakeClients(t, map[string]string{})
	_ = os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho oops >&2\nexit 7\n"), 0o755)
	c = e.expect(t, e.mustDoctor(t), "client.claude-code", "warn")
	if !strings.Contains(c.Message, "종료 코드 7") || !hasFix(c, "claude --version") {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorProfileChecks(t *testing.T) {
	e := newSyncEnv(t)
	r := e.mustDoctor(t)
	c := e.expect(t, r, "profile.rule.claude-code", "info")
	if !strings.Contains(c.Message, "프로필 없음: 레거시 동작") || !hasFix(c, "forge profile create <이름> --from-current") {
		t.Fatalf("%+v", c)
	}
	// an empty active profile hides everything
	mustRun(t, "profile", "create", "empty")
	mustRun(t, "profile", "use", "empty", "--client", "all")
	r = e.mustDoctor(t)
	e.expect(t, r, "profile.rule.claude-code", "ok")
	c = e.expect(t, r, "profile.health.empty", "warn")
	if !strings.Contains(c.Message, "비어 있습니다") || !hasFix(c, "forge profile add empty") || !hasFix(c, "forge profile unuse --client all") {
		t.Fatalf("%+v", c)
	}
	// the sync plan of the doctor states which rule is applied
	if c := r.get("drift.claude-code.project"); c == nil || !strings.Contains(c.Message+c.Cause, "") {
		t.Fatal("drift check missing")
	}
	// disabled member, claude/opencode skill overlap
	mustRun(t, "profile", "add", "empty", "--skill", "demo-skill")
	mustRun(t, "profile", "add", "empty", "--skill", "demo-skill", "--exposure", "hidden", "--client", "opencode")
	mustRun(t, "profile", "add", "empty", "--mcp", "direct-srv")
	mustRun(t, "mcp", "disable", "direct-srv")
	r = e.mustDoctor(t)
	c = e.expect(t, r, "profile.health.empty.disabled", "warn")
	if !strings.Contains(c.Message, "mcp:direct-srv") || !hasFix(c, "forge mcp enable") {
		t.Fatalf("%+v", c)
	}
	c = e.expect(t, r, "profile.health.empty.overlap", "warn")
	if !strings.Contains(c.Message, "demo-skill") || !strings.Contains(c.Cause, ".claude/skills") || !hasFix(c, "--skill demo-skill --exposure hidden --client claude-code") {
		t.Fatalf("%+v", c)
	}
	// legacy settings are reported as ignored
	mustRun(t, "mcp", "enable", "direct-srv")
	r = e.mustDoctor(t)
	if c := e.expect(t, r, "profile.legacy.claude-code", "info"); !strings.Contains(c.Message, "무시") {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorPinnedSkillStoreMissingAndBrokenMembers(t *testing.T) {
	e := newSyncEnv(t)
	mustRun(t, "profile", "create", "p")
	mustRun(t, "profile", "add", "p", "--skill", "demo-skill", "--pin", pinOf(t, "demo-skill"))
	mustRun(t, "profile", "use", "p", "--client", "all")
	// delete the stored snapshot of the pinned (and current) version
	_ = filepath.WalkDir(filepath.Join(e.homeForge(), "skills"), func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	ents, _ := os.ReadDir(filepath.Join(e.homeForge(), "skills"))
	for _, en := range ents {
		_ = os.RemoveAll(filepath.Join(e.homeForge(), "skills", en.Name()))
	}
	r := e.mustDoctor(t)
	c := e.expect(t, r, "profile.health.p.pins", "warn")
	if !strings.Contains(c.Message, "demo-skill") || !hasFix(c, "forge skill add") {
		t.Fatalf("%+v", c)
	}
	e.expect(t, r, "paths.skillstore", "warn")

	// a damaged database (foreign keys off): dangling member -> fail and exit 1
	raw, err := sql.Open("sqlite", "file:"+store.DBPath(e.homeForge())+"?_pragma=foreign_keys(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE profile_members SET skill_id=9999, pinned_version_id=NULL`); err != nil {
		t.Fatal(err)
	}
	r, derr := e.doctor(t)
	if derr == nil {
		t.Fatal("a dangling profile member is a fail")
	}
	c = r.get("profile.health.p")
	if c == nil || c.Status != "fail" || !strings.Contains(c.Message, "삭제되었거나 손상") || !hasFix(c, "forge profile remove p") {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorScopesAndUnavailableProject(t *testing.T) {
	e := newSyncEnv(t)
	r := e.mustDoctor(t, "--scope", "user")
	if r.get("drift.claude-code.user") == nil || r.get("drift.claude-code.project") != nil {
		t.Fatalf("scope user: %v", r.Checks)
	}
	r = e.mustDoctor(t, "--scope", "all")
	if r.get("drift.claude-code.user") == nil || r.get("drift.claude-code.project") == nil || r.get("drift.opencode.user") == nil {
		t.Fatalf("scope all")
	}
	// a project directory that does not exist: the project part is "점검 불가", the rest runs
	out, err := runErr(t, "", "doctor", "--json", "--project-dir", filepath.Join(e.proj, "missing"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var bad docReport
	_ = json.Unmarshal([]byte(out), &bad)
	if c := bad.get("drift.project"); c == nil || c.Status != "warn" || !strings.Contains(c.Message, "점검 불가") {
		t.Fatalf("%+v", c)
	}
	if bad.get("home.dir") == nil {
		t.Fatal("others continue")
	}
}

func TestDoctorTimeoutBudget(t *testing.T) {
	e := newSyncEnv(t)
	r, err := e.doctor(t, "--timeout", "1ns")
	if err != nil {
		t.Fatalf("%v", err)
	}
	skipped := 0
	for _, c := range r.Checks {
		if strings.HasSuffix(c.ID, ".skipped") && c.Status == "warn" && strings.Contains(c.Message, "점검 불가") {
			skipped++
		}
	}
	if skipped == 0 {
		t.Fatalf("an exhausted budget must be reported per skipped group: %+v", r.Checks)
	}
}

// --- read-only guarantees -----------------------------------------------------

func dirSig(t *testing.T, roots ...string) string {
	t.Helper()
	h := sha256.New()
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if strings.HasSuffix(p, "-shm") { // readers update the shared-memory index by design
				return nil
			}
			fi, _ := os.Lstat(p)
			fmt.Fprintf(h, "%s|%v|%d|", p, fi.Mode(), fi.Size())
			if fi.Mode().IsRegular() {
				b, _ := os.ReadFile(p)
				h.Write(b)
				fmt.Fprintf(h, "|%d", fi.ModTime().UnixNano())
			}
			h.Write([]byte("\n"))
			return nil
		})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func TestDoctorIsReadOnly(t *testing.T) {
	e := newSyncEnv(t)
	addSkillDir(t, "other-skill", "o")
	mustRun(t, "profile", "create", "p", "--from-current")
	mustRun(t, "profile", "use", "p", "--client", "all")
	e.applyAll(t)
	// leave the project in a messy state: conflict, stale temp file, forged journal copy
	p := filepath.Join(e.proj, ".mcp.json")
	b, _ := os.ReadFile(p)
	_ = os.WriteFile(p, []byte(strings.Replace(string(b), `"srv"`, `"x"`, 1)), 0o644)
	_ = os.WriteFile(filepath.Join(e.proj, ".mcp.json.forge-tmp-12345-abcdef"), []byte("tmp"), 0o644)
	_ = os.WriteFile(filepath.Join(e.proj, ".forge", "sync-journal.json"), []byte(`{"version":1,"id":"20260101T000000.000Z-abcdef","status":"pending"}`), 0o644)
	_ = os.Chmod(e.homeForge(), 0o755)
	for _, scope := range []string{"project", "user", "all"} {
		before := dirSig(t, e.homeForge(), e.proj, e.home)
		e.mustDoctor(t, "--scope", scope)
		if _, err := runErr(t, "", "doctor", "--scope", scope, "--project-dir", e.proj); err != nil {
			t.Fatal(err)
		}
		if after := dirSig(t, e.homeForge(), e.proj, e.home); after != before {
			t.Fatalf("doctor --scope %s changed files", scope)
		}
	}
	// the stale temp file is still there (doctor only reports it)
	if !exists(t, e.proj, ".mcp.json.forge-tmp-12345-abcdef") {
		t.Fatal("doctor removed a temp file")
	}
}

func TestDoctorReadOnlyWithALiveWriterAndNoAuxFiles(t *testing.T) {
	e := newSyncEnv(t)
	e.applyAll(t)
	db := e.forgeDB(t) // a live writer: WAL mode with real WAL content
	for i := 0; i < 50; i++ {
		_, _ = db.Exec(`INSERT INTO ledger(ts,kind,name,source) VALUES (?, 'skill', ?, 'hook')`, time.Now().UnixMilli(), fmt.Sprintf("s%d", i))
	}
	dbp := store.DBPath(e.homeForge())
	read := func(p string) []byte { b, _ := os.ReadFile(p); return b }
	beforeDB, beforeWAL := read(dbp), read(dbp+"-wal")
	r := e.mustDoctor(t)
	e.expect(t, r, "db.integrity", "ok")
	if !bytes.Equal(beforeDB, read(dbp)) || !bytes.Equal(beforeWAL, read(dbp+"-wal")) {
		t.Fatal("doctor changed the database or its WAL")
	}
	if c := e.expect(t, r, "history.size", "ok"); !strings.Contains(c.Message, "50") {
		t.Fatalf("doctor must see the WAL content: %+v", c)
	}

	// No other process: the helper files must not appear because of doctor.
	_ = db.Close()
	for _, s := range []string{"-wal", "-shm"} {
		_ = os.Remove(dbp + s)
	}
	before := dirSig(t, e.homeForge())
	e.mustDoctor(t)
	if after := dirSig(t, e.homeForge()); after != before {
		t.Fatal("doctor created or changed files (e.g. -wal/-shm) in the Forge home")
	}
	for _, s := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbp + s); err == nil {
			t.Fatalf("doctor created %s", s)
		}
	}
}

func TestDoctorNeverTouchesForbiddenFiles(t *testing.T) {
	e := newSyncEnv(t)
	// decoys: the Claude Code user file and OpenCode secret files, readable on purpose
	// so that a read would be visible (mtime/atime/content).
	claudeUser := e.decoy
	_ = os.Chmod(claudeUser, 0o600)
	ocCfg := filepath.Join(e.home, ".config", "opencode")
	ocData := filepath.Join(e.home, ".local", "share", "opencode")
	_ = os.MkdirAll(ocCfg, 0o700)
	_ = os.MkdirAll(ocData, 0o700)
	decoys := []string{claudeUser}
	for _, f := range []string{filepath.Join(ocCfg, "opencode.json"), filepath.Join(ocCfg, "auth.json"), filepath.Join(ocData, "service.json"), filepath.Join(ocData, "opencode.db")} {
		_ = os.WriteFile(f, []byte(`{"apiKey":"sk-DECOYDECOYDECOYDECOY"}`), 0o600)
		decoys = append(decoys, f)
	}
	type sig struct {
		sum   [32]byte
		mtime time.Time
		atime time.Time
	}
	snap := func() map[string]sig {
		m := map[string]sig{}
		for _, f := range decoys {
			b, _ := os.ReadFile(f) // test's own read happens after the atime was captured
			fi, _ := os.Stat(f)
			m[f] = sig{sha256.Sum256(b), fi.ModTime(), atimeOf(fi)}
		}
		return m
	}
	// capture atime BEFORE our own hashing read
	atimes := map[string]time.Time{}
	for _, f := range decoys {
		fi, _ := os.Stat(f)
		atimes[f] = atimeOf(fi)
	}
	before := snap()
	e.applyAll(t)
	for _, scope := range []string{"project", "user", "all"} {
		e.mustDoctor(t, "--scope", scope)
	}
	// symlinks/hard links pointing at the decoys from the project are not followed either
	_ = os.Symlink(claudeUser, filepath.Join(e.proj, "opencode.jsonc"))
	_ = os.Link(filepath.Join(ocCfg, "auth.json"), filepath.Join(e.proj, "link-to-secret"))
	e.mustDoctor(t)
	after := snap()
	for _, f := range decoys {
		if before[f].sum != after[f].sum || !before[f].mtime.Equal(after[f].mtime) {
			t.Errorf("decoy %s was modified", f)
		}
	}
	_ = atimes // atime is a bonus check: relatime/APFS do not reliably update it, and the test's own hashing read above also touches it
}

func TestDoctorOutputIsSanitized(t *testing.T) {
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
	// a config file with hostile text inside
	_ = os.WriteFile(filepath.Join(proj, ".mcp.json"), []byte("{\"mcpServers\": \x1b[2J broken"), 0o644)
	out, err := runErr(t, "", "doctor", "--project-dir", proj)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, c := range out {
		if c == 0x1b || c == 0x202e || (c < 0x20 && c != '\n' && c != '\t') {
			t.Fatalf("raw control character %U in doctor output:\n%q", c, out)
		}
	}
	js, _ := runErr(t, "", "doctor", "--json", "--project-dir", proj)
	if strings.ContainsRune(js, 0x1b) || strings.ContainsRune(js, 0x202e) {
		t.Fatalf("raw control character in JSON output")
	}
	if !strings.Contains(out, "\\x1b") && !strings.Contains(out, "\\u001b") && !strings.Contains(out, "\\e") {
		t.Logf("escaped form not found (fine if the path is shortened):\n%s", out)
	}
}

func TestDoctorLargeInputsStayFast(t *testing.T) {
	e := newSyncEnv(t)
	e.applyAll(t)
	db := e.forgeDB(t)
	if _, err := db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 300000)
		INSERT INTO ledger(ts,kind,name,session_id,source,client,found) SELECT 1700000000000 + i, 'skill', 'n' || (i % 50), 's' || (i % 1000), 'gateway', 'claude-code', 1 FROM n`); err != nil {
		t.Fatal(err)
	}
	// a big (but valid) configuration file next to the managed entries
	p := filepath.Join(e.proj, ".mcp.json")
	b, _ := os.ReadFile(p)
	var doc map[string]map[string]any
	_ = json.Unmarshal(b, &doc)
	for i := 0; i < 20000; i++ {
		doc["mcpServers"][fmt.Sprintf("user-%05d", i)] = map[string]any{"type": "stdio", "command": "/bin/echo", "args": []string{strings.Repeat("a", 100)}}
	}
	nb, _ := json.Marshal(doc)
	_ = os.WriteFile(p, nb, 0o644)
	start := time.Now()
	r := e.mustDoctor(t)
	// The point is "no blow-up on large inputs" (4-5s without -race here), not a
	// speed record: the race detector and a busy host multiply the time, so the
	// limit is generous and 3x larger under -race.
	limit := 20 * time.Second
	if raceEnabled {
		limit *= 3
	}
	if d := time.Since(start); d > limit {
		t.Fatalf("doctor took %v on large inputs (limit %v)", d, limit)
	}
	if c := r.get("history.size"); c == nil || !strings.Contains(c.Message, "300000") {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorFixCommandsAreExactAndQuoted(t *testing.T) {
	// A project path with a space and a quote must be quoted in the printed commands.
	forgeHome(t)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(base, "home")
	proj := filepath.Join(base, "my proj's")
	for _, d := range []string{home, filepath.Join(proj, ".git")} {
		_ = os.MkdirAll(d, 0o755)
	}
	t.Setenv("HOME", home)
	mustRun(t, "init")
	mustRun(t, "mcp", "add", "direct-srv", "--", "/bin/echo")
	out, err := runErr(t, "", "doctor", "--json", "--project-dir", proj)
	if err != nil {
		t.Fatal(err)
	}
	var r docReport
	_ = json.Unmarshal([]byte(out), &r)
	c := r.get("drift.claude-code.project")
	if c == nil || c.Status != "warn" {
		t.Fatalf("%+v", c)
	}
	want := `--project-dir '` + strings.ReplaceAll(proj, `'`, `'\''`) + `'`
	if !hasFix(c, want) {
		t.Fatalf("fix commands must shell-quote the path:\n%v\nwant substring %s", c.Fix, want)
	}
}
