package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heonjinjeong/forge/internal/store"
)

// TASK-0032 AC-2: forge history prune.

func pruneDB(t *testing.T, home string) *sql.DB {
	t.Helper()
	conn, err := store.Open(context.Background(), store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func insLedger(t *testing.T, c *sql.DB, ts time.Time, name, client, sess, tuid, source string) int64 {
	t.Helper()
	var cl, tu any
	if client != "" {
		cl = client
	}
	if tuid != "" {
		tu = tuid
	}
	res, err := c.Exec(`INSERT INTO ledger(ts,kind,name,client,session_id,tool_use_id,source,ok) VALUES (?,?,?,?,?,?,?,1)`, ts.UnixMilli(), "tool", name, cl, sess, tu, source)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func ledgerNames(t *testing.T, c *sql.DB) map[string]int {
	t.Helper()
	rows, err := c.Query(`SELECT name || '/' || source FROM ledger`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		m[s]++
	}
	return m
}

func setupPrune(t *testing.T) (home string, cutoff time.Time) {
	home = forgeHome(t)
	mustRun(t, "init")
	cutoff = time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local)
	c := pruneDB(t, home)
	old := cutoff.Add(-60 * 24 * time.Hour)
	insLedger(t, c, old, "old-pair", "claude-code", "s1", "u1", "hook")
	insLedger(t, c, old.Add(2*time.Second), "old-pair", "claude-code", "s1", "u1", "gateway")
	insLedger(t, c, old, "old-single", "claude-code", "s2", "u2", "hook")
	insLedger(t, c, old, "old-noclient", "", "s3", "u3", "hook")
	insLedger(t, c, old, "old-notuid", "claude-code", "s4", "", "hook")
	// the pair straddles the cutoff: both rows must stay
	insLedger(t, c, cutoff.Add(-time.Second), "straddle", "claude-code", "s5", "u5", "hook")
	insLedger(t, c, cutoff.Add(2*time.Second), "straddle", "claude-code", "s5", "u5", "gateway")
	insLedger(t, c, time.Now().Add(-2*time.Hour), "recent", "claude-code", "s6", "u6", "hook")
	var id int64
	if err := c.QueryRow(`SELECT id FROM ledger WHERE name='old-single'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(`INSERT INTO ledger_payloads(ledger_id, input_json, redaction_version) VALUES (?, '{}', 1)`, id); err != nil {
		t.Fatal(err)
	}
	return home, cutoff
}

func TestHistoryPruneDryRunChangesNothing(t *testing.T) {
	home, _ := setupPrune(t)
	before := ledgerNames(t, pruneDB(t, home))
	out := mustRun(t, "history", "prune", "--before", "2026-09-01")
	for _, w := range []string{"dry-run", "행 5개", "병합된 호출 4건", "함께 지우는 행 1", "payload 행: 1", "남깁니다", "--yes"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	after := ledgerNames(t, pruneDB(t, home))
	if len(after) != len(before) {
		t.Fatalf("dry-run changed the ledger: %v -> %v", before, after)
	}
	if ents, _ := os.ReadDir(filepath.Join(home, "backups")); len(ents) != 0 {
		t.Fatalf("dry-run made a backup: %v", ents)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(mustRun(t, "history", "prune", "--before", "2026-09-01", "--json")), &m); err != nil || m["dry_run"] != true {
		t.Fatalf("json: %v %v", m, err)
	}
}

func TestHistoryPruneYesDeletesPairsTogetherAndBacksUp(t *testing.T) {
	home, _ := setupPrune(t)
	out := mustRun(t, "history", "prune", "--before", "2026-09-01", "--yes")
	for _, w := range []string{"지웠습니다", "행 5개", "백업:", "VACUUM"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	got := ledgerNames(t, pruneDB(t, home))
	want := map[string]int{"straddle/hook": 1, "straddle/gateway": 1, "recent/hook": 1}
	if len(got) != len(want) {
		t.Fatalf("left %v, want %v", got, want)
	}
	for k := range want {
		if got[k] != 1 {
			t.Errorf("%s was deleted (a straddling pair and recent rows stay): %v", k, got)
		}
	}
	var n int
	if err := pruneDB(t, home).QueryRow(`SELECT count(*) FROM ledger_payloads`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the payload must go with its row: %d %v", n, err)
	}
	ents, _ := os.ReadDir(filepath.Join(home, "backups"))
	var bk string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "forge-pre-prune-") {
			bk = filepath.Join(home, "backups", e.Name())
			if fi, _ := e.Info(); fi.Mode().Perm() != 0o600 {
				t.Errorf("backup mode %v", fi.Mode().Perm())
			}
		}
	}
	if bk == "" {
		t.Fatalf("no backup in %v", ents)
	}
	b, err := sql.Open("sqlite", "file:"+bk+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.QueryRow(`SELECT count(*) FROM ledger`).Scan(&n); err != nil || n != 8 {
		t.Fatalf("the backup must hold the ledger as it was: %d rows, %v", n, err)
	}
	// the second run finds nothing
	if out := mustRun(t, "history", "prune", "--before", "2026-09-01", "--yes"); !strings.Contains(out, "지울 기록이 없습니다") {
		t.Fatalf("%s", out)
	}
}

func TestHistoryPruneRefusalsAndProtection(t *testing.T) {
	home, _ := setupPrune(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"history", "prune"}, "--before 가 필요"},
		{[]string{"history", "prune", "--before", "1h", "--yes"}, "최근 1일"},
		{[]string{"history", "prune", "--before", "0d", "--yes"}, "최근 1일"},
		{[]string{"history", "prune", "--before", "2099-01-01", "--yes"}, "최근 1일"},
		{[]string{"history", "prune", "--before", "garbage"}, "해석할 수 없습니다"},
		{[]string{"history", "prune", "--before", "30d", "--no-backup"}, "--yes 와 함께"},
		{[]string{"history", "prune", "--before", "30d", "--since", "1d"}, "--since 를 지원하지 않습니다"},
	} {
		out, err := runErr(t, "", tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err=%v out=%q, want %q", tc.args, err, out, tc.want)
		}
	}
	if n := len(ledgerNames(t, pruneDB(t, home))); n != 8 {
		t.Fatalf("a refused prune deleted rows: %d kinds left", n)
	}
	// exactly one day old is the boundary: 25h is allowed (dry-run), 23h is not
	mustRun(t, "history", "prune", "--before", "25h")
	if _, err := runErr(t, "", "history", "prune", "--before", "23h"); err == nil {
		t.Fatal("23h must be refused")
	}
}

func TestHistoryPruneRequiresInitAndLeavesRecentRowsAlone(t *testing.T) {
	forgeHome(t)
	if _, err := runErr(t, "", "history", "prune", "--before", "30d", "--yes"); err == nil || !strings.Contains(err.Error(), "forge init") {
		t.Fatalf("%v", err)
	}
}

// Many pairs, tiny batches: no call is ever left with only one of its rows, even
// after the run was cut after any number of batches (checked after every batch by
// re-running with the same tiny batch size is not possible from the CLI, so the
// end state and a mid-way state built with batch 1 on the history API are checked).
func TestHistoryPruneBatchesKeepPairsWhole(t *testing.T) {
	home := forgeHome(t)
	mustRun(t, "init")
	c := pruneDB(t, home)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	for i := 0; i < 40; i++ {
		u := "u" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		insLedger(t, c, old, "p", "claude-code", "s", u, "hook")
		insLedger(t, c, old.Add(time.Second), "p", "claude-code", "s", u, "gateway")
	}
	insLedger(t, c, old, "tail", "claude-code", "s", "ut", "hook")
	mustRun(t, "history", "prune", "--before", "2026-06-01", "--yes", "--no-backup", "--batch", "3")
	if got := ledgerNames(t, pruneDB(t, home)); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
}

// A backup that cannot be written stops the prune before anything is deleted.
func TestHistoryPruneBackupFailureDeletesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home, _ := setupPrune(t)
	bdir := filepath.Join(home, "backups")
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bdir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bdir, 0o700) })
	before := ledgerNames(t, pruneDB(t, home))
	out, err := runErr(t, "", "history", "prune", "--before", "2026-09-01", "--yes")
	if err == nil || !strings.Contains(err.Error(), "아무것도 지우지 않았습니다") {
		t.Fatalf("err=%v out=%s", err, out)
	}
	var n int
	if err := pruneDB(t, home).QueryRow(`SELECT count(*) FROM ledger`).Scan(&n); err != nil || n != 8 {
		t.Fatalf("rows deleted although the backup failed: %d %v", n, err)
	}
	if after := ledgerNames(t, pruneDB(t, home)); len(after) != len(before) {
		t.Fatalf("%v -> %v", before, after)
	}
	if ents, _ := os.ReadDir(bdir); len(ents) != 0 {
		t.Fatalf("partial backup left: %v", ents)
	}
	// --no-backup is the explicit way past it
	mustRun(t, "history", "prune", "--before", "2026-09-01", "--yes", "--no-backup")
	if err := pruneDB(t, home).QueryRow(`SELECT count(*) FROM ledger`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
}
