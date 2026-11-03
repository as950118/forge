package history

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/as950118/forge/internal/store"
)

// ---- AC-5: each read-only defense is verified on its own ----------------------

func TestReadOnlyDefensesEachStandAlone(t *testing.T) {
	home, rw := newHome(t)
	ins(t, rw, row{ts: 1, name: "a"})
	path := store.DBPath(home)
	write := `INSERT INTO ledger(ts,kind,name,source) VALUES (1,'skill','x','hook')`

	open := func(dsn string) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	pragma := func(db *sql.DB) int {
		var v int
		if err := db.QueryRow(`PRAGMA query_only`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	// 1. mode=ro alone: the connection is NOT query_only, yet the write is refused.
	onlyRO := open(roDSN(path, true, false, false))
	if pragma(onlyRO) != 0 {
		t.Fatal("test setup: query_only must be off on the mode=ro-only handle")
	}
	if _, err := onlyRO.Exec(write); err == nil {
		t.Error("mode=ro alone must refuse writes")
	}

	// 2. query_only alone: the file is writable (no mode=ro), yet the write is refused.
	onlyQ := open(roDSN(path, false, true, false))
	if pragma(onlyQ) != 1 {
		t.Fatal("test setup: query_only must be on the query_only-only handle")
	}
	if _, err := onlyQ.Exec(write); err == nil {
		t.Error("query_only alone must refuse writes")
	}
	// ... and it is the only thing in the way: switching it off lets the same write through.
	if _, err := onlyQ.Exec(`PRAGMA query_only=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := onlyQ.Exec(write); err != nil {
		t.Errorf("without query_only (and without mode=ro) the write must succeed, proving query_only was the sole guard: %v", err)
	}
	if _, err := onlyQ.Exec(`DELETE FROM ledger WHERE name='x'`); err != nil {
		t.Fatal(err)
	}

	// 3. The handle Open returns carries both: query_only is on, and even after it is
	// switched off mode=ro still refuses.
	db := openRO(t, home)
	if pragma(db) != 1 {
		t.Error("Open must set query_only")
	}
	if _, err := db.Exec(`PRAGMA query_only=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(write); err == nil {
		t.Error("Open's handle must still refuse writes with query_only switched off (mode=ro)")
	}
	// 4. The DSN Open uses names both defenses.
	dsn := roDSN(path, true, true, false)
	if !strings.Contains(dsn, "mode=ro") || !strings.Contains(dsn, "query_only") || strings.Contains(dsn, "immutable") {
		t.Errorf("dsn=%s", dsn)
	}
}

// ---- AC-3: read-only location ---------------------------------------------------

func TestOpenReadOnlyDirectoryFallsBackToImmutable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	home, rw := newHome(t)
	ins(t, rw, row{ts: 1, name: "a"})
	ins(t, rw, row{ts: 2, name: "b"})
	_ = rw.Close() // last connection: the WAL is checkpointed and removed
	dir := filepath.Dir(store.DBPath(home))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()

	db, note, err := OpenNote(ctx, home)
	if err != nil {
		t.Fatalf("a read-only directory must still be readable: %v", err)
	}
	defer db.Close()
	if note != ImmutableNote || !strings.Contains(note, "immutable") {
		t.Errorf("note=%q", note)
	}
	cs, err := List(ctx, db, Filter{}, 10)
	if err != nil || len(cs) != 2 {
		t.Errorf("immutable read: %d calls, %v", len(cs), err)
	}
	if _, err := db.Exec(`DELETE FROM ledger`); err == nil {
		t.Error("the immutable handle must not write")
	}
	for _, n := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(store.DBPath(home) + n); err == nil {
			t.Errorf("immutable open created %s", n)
		}
	}
}

func TestOpenNoteIsEmptyWhenNormal(t *testing.T) {
	home, rw := newHome(t)
	ins(t, rw, row{ts: 1, name: "a"})
	db, note, err := OpenNote(ctx, home)
	if err != nil || note != "" {
		t.Fatalf("note=%q err=%v", note, err)
	}
	_ = db.Close()
	// not a read-only problem: still the "corrupt?" guidance, no immutable retry
	bad := t.TempDir()
	if err := os.WriteFile(store.DBPath(bad), []byte(strings.Repeat("not sqlite ", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, note, err := OpenNote(ctx, bad); err == nil || note != "" || !strings.Contains(err.Error(), "손상") {
		t.Errorf("corrupt: note=%q err=%v", note, err)
	}
}

// ---- AC-6: small usability items ------------------------------------------------

func TestCSVCellPlainNegativeValuesKeepTheirText(t *testing.T) {
	for _, s := range []string{"-", "-1", "-0.5", "-42"} {
		if got := CSVCell(s); got != s {
			t.Errorf("CSVCell(%q)=%q, want it unchanged", s, got)
		}
	}
	for _, s := range []string{"-1+1", "--", "-cmd", "-1 ", "-.5", "-1.", "-1e5", "=1", "+1", "@a", "-1\t", "- 1"} {
		if got := CSVCell(s); !strings.HasPrefix(got, "'") {
			t.Errorf("CSVCell(%q)=%q, want a leading apostrophe", s, got)
		}
	}
}

func TestParseTimeGarbageGetsAFormatMessage(t *testing.T) {
	for _, v := range []string{"foo", "yesterday", "7days", "2026/10/01", "x7"} {
		_, err := ParseTime("since", v, nil)
		if err == nil {
			t.Errorf("%q accepted", v)
			continue
		}
		if strings.Contains(err.Error(), "단위") {
			t.Errorf("%q: a non-period value must not be blamed on its last letter as a unit: %v", v, err)
		}
		if !strings.Contains(err.Error(), "--since") || !strings.Contains(err.Error(), "RFC3339") {
			t.Errorf("%q: message should name the flag and the accepted forms: %v", v, err)
		}
	}
	// a period-shaped value with a wrong unit still says so
	if _, err := ParseTime("since", "5x", nil); err == nil || !strings.Contains(err.Error(), "단위") {
		t.Errorf("5x: %v", err)
	}
	// valid forms still parse, including the RFC3339 'Z' suffix
	for _, v := range []string{"7d", "90m", "2026-10-01", "2026-10-01T09:00:00Z", "2026-10-01T09:00:00+09:00"} {
		if _, err := ParseTime("since", v, nil); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
}

func TestWriteFileFollowsParentSymlinkButNotTheFinalOne(t *testing.T) {
	real := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "via")
	if err := os.Symlink(real, linkDir); err != nil {
		t.Skip("symlinks unavailable")
	}
	// parent directory is a symlink: allowed (the user chose the path)
	if err := WriteFile(filepath.Join(linkDir, "out.csv"), []byte("x"), false); err != nil {
		t.Fatalf("symlinked parent directory must be allowed: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(real, "out.csv")); err != nil || string(b) != "x" {
		t.Errorf("file not written through the link: %q %v", b, err)
	}
	// the final component as a symlink: refused, with or without force
	if err := os.Symlink(filepath.Join(real, "out.csv"), filepath.Join(real, "ln.csv")); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		if err := WriteFile(filepath.Join(linkDir, "ln.csv"), []byte("y"), force); err == nil {
			t.Errorf("force=%v: final symlink accepted", force)
		}
	}
	// protected Forge files stay protected when reached through a symlinked parent
	home, rw := newHome(t)
	_ = rw.Close()
	dbDir := filepath.Dir(store.DBPath(home))
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dbDir, alias); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(filepath.Join(alias, filepath.Base(store.DBPath(home))), []byte("x"), true, store.DBPath(home)); err == nil {
		t.Error("the registry database must stay protected through a symlinked parent")
	}
}

// ---- AC-7: size hint ----------------------------------------------------------

func TestLedgerSizeHint(t *testing.T) {
	home, rw := newHome(t)
	db := openRO(t, home)
	if n := LedgerSizeHint(ctx, db); n != 0 {
		t.Errorf("empty ledger: %d", n)
	}
	if _, err := rw.Exec(`INSERT INTO ledger(id,ts,kind,name,source) VALUES (?,1,'skill','a','hook')`, LargeLedgerRows+10); err != nil {
		t.Fatal(err)
	}
	if n := LedgerSizeHint(ctx, db); n < LargeLedgerRows {
		t.Errorf("hint=%d", n)
	}
}

// ---- AC-1/2 in the query layer -----------------------------------------------

// Rows of one call that disagree on ok: the merged call is a failure; error_class comes along.
func TestListMergedFailureWins(t *testing.T) {
	home, rw := newHome(t)
	ins(t, rw, row{ts: 1000, kind: "mcp", name: "x", server: "s", client: "claude-code", sess: "s1", tuid: "t1", source: "hook", ok: p(1)})
	ins(t, rw, row{ts: 1001, kind: "mcp", name: "x", server: "s", client: "claude-code", sess: "s1", tuid: "t1", source: "gateway", ok: p(0), errc: "timeout"})
	db := openRO(t, home)
	cs, err := List(ctx, db, Filter{}, 10)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v %+v", err, cs)
	}
	if cs[0].OK == nil || *cs[0].OK || cs[0].ErrorClass != "timeout" {
		t.Errorf("merged call: ok=%v class=%q, want failed(timeout)", cs[0].OK, cs[0].ErrorClass)
	}
	gs, _ := Summary(ctx, db, Filter{}, "kind", 10)
	if len(gs) != 1 || gs[0].Failed != 1 || gs[0].OK != 0 {
		t.Errorf("summary: %+v", gs)
	}
	if f, _ := List(ctx, db, Filter{OK: func() *bool { b := true; return &b }()}, 10); len(f) != 0 {
		t.Errorf("--ok must not match a call with a failed source: %d", len(f))
	}
}

// AC-8 display: hidden_by_profile calls are not misses and the list explains the class.
func TestHiddenByProfileIsNotListedAsMissAndIsExplained(t *testing.T) {
	home, rw := newHome(t)
	ins(t, rw, row{ts: 1, kind: "tool", name: "greet", client: "claude-code", found: p(1), errc: "hidden_by_profile", ok: p(0), source: "gateway"})
	ins(t, rw, row{ts: 2, kind: "tool", name: "ghost", client: "claude-code", found: p(0), errc: "not_found", ok: p(0), source: "gateway"})
	db := openRO(t, home)
	ms, err := Misses(ctx, db, Filter{}, 10)
	if err != nil || len(ms) != 1 || ms[0].Name != "ghost" {
		t.Fatalf("misses: %v %+v", err, ms)
	}
	cs, _ := List(ctx, db, Filter{}, 10)
	rep := ListReport(cs, 10, false, time.UTC)
	var out strings.Builder
	if err := rep.Write(&out, FormatTable); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "failed(hidden_by_profile)") || !strings.Contains(out.String(), "미등록이 아님") {
		t.Errorf("table:\n%s", out.String())
	}
	var mo strings.Builder
	_ = MissesReport(ms, time.UTC).Write(&mo, FormatTable)
	if !strings.Contains(mo.String(), "hidden_by_profile") {
		t.Errorf("misses report must point at hidden_by_profile:\n%s", mo.String())
	}
}
