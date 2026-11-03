package history

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/as950118/forge/internal/store"
)

// After every single batch (cut at any point) no call has lost one of its rows
// while the other remains (TASK-0032 AC-2, ADR-F0003).
func TestPruneBatchNeverSplitsALinkedPair(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ins := func(ts int64, tuid any, source string) {
		if _, err := db.Exec(`INSERT INTO ledger(ts,kind,name,client,session_id,tool_use_id,source) VALUES (?,'tool','t','claude-code','s',?,?)`, ts, tuid, source); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 30; i++ {
		u := string(rune('A'+i/26)) + string(rune('a'+i%26))
		ins(int64(1000+i), u, "hook")
		ins(int64(1001+i), u, "gateway")
		ins(int64(1000+i), nil, "hook") // single rows in between
	}
	const cutoff = 1_000_000
	total := 0
	for step := 0; step < 200; step++ {
		n, err := pruneBatch(ctx, db, cutoff, 1)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		total += int(n)
		var lonely int
		if err := db.QueryRow(`SELECT count(*) FROM (SELECT tool_use_id FROM ledger WHERE tool_use_id IS NOT NULL GROUP BY tool_use_id HAVING count(*) = 1)`).Scan(&lonely); err != nil {
			t.Fatal(err)
		}
		if lonely != 0 {
			t.Fatalf("after %d deleted rows %d calls have only one side left", total, lonely)
		}
	}
	var left int
	_ = db.QueryRow(`SELECT count(*) FROM ledger`).Scan(&left)
	if left != 0 || total != 90 {
		t.Fatalf("deleted %d, left %d; want 90 and 0", total, left)
	}
}

func TestPruneKeepsRowsAtOrAfterTheCutoff(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, ts := range []int64{99, 100, 101} {
		if _, err := db.Exec(`INSERT INTO ledger(ts,kind,name,source) VALUES (?,'tool','t','hook')`, ts); err != nil {
			t.Fatal(err)
		}
	}
	var minTS sql.NullInt64
	plan, err := PrunePreview(ctx, db, 100)
	if err != nil || plan.Rows != 1 {
		t.Fatalf("%+v %v", plan, err)
	}
	if n, err := pruneBatch(ctx, db, 100, 10); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	_ = db.QueryRow(`SELECT MIN(ts) FROM ledger`).Scan(&minTS)
	if minTS.Int64 != 100 {
		t.Fatalf("the cutoff itself is kept (before is exclusive): min ts %d", minTS.Int64)
	}
}

// TASK-0032 AC-2 (performance): a 300k-row ledger with ~100 rows per session and with
// ~6000 rows per session. The previous eligibility query grew with the square of the
// session size (76 s dry-run at 100 rows per session, > 5 min at 6000). Gated like the
// other large tests: FORGE_LARGE_TESTS=1.
func TestPruneLargeLedgerIsFast(t *testing.T) {
	if os.Getenv("FORGE_LARGE_TESTS") == "" {
		t.Skip("set FORGE_LARGE_TESTS=1")
	}
	for _, perSession := range []int{100, 6000} {
		t.Run(fmt.Sprintf("per-session-%d", perSession), func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(ctx, filepath.Join(t.TempDir(), "forge.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			const total = 300_000
			const cutoff = int64(1_000_000_000)
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			stmt, err := tx.Prepare(`INSERT INTO ledger(ts,kind,name,client,session_id,tool_use_id,source) VALUES (?,'tool','t','claude-code',?,?,?)`)
			if err != nil {
				t.Fatal(err)
			}
			calls := total / 2
			straddle := 0
			for i := 0; i < calls; i++ {
				sess := fmt.Sprintf("s%d", i*2/perSession)
				tu := fmt.Sprintf("u%d", i)
				ts := int64(i) * 1000 // calls 0..~1e9/1000: the older 2/3 are before the cutoff
				if i >= calls*2/3 {
					ts = cutoff + int64(i)
				}
				gts := ts + 1
				if i == calls*2/3-1 {
					gts = cutoff + 5 // one pair straddles the cutoff
					straddle++
				}
				if _, err := stmt.Exec(ts, sess, tu, "hook"); err != nil {
					t.Fatal(err)
				}
				if _, err := stmt.Exec(gts, sess, tu, "gateway"); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			plan, err := PrunePreview(ctx, db, cutoff)
			if err != nil {
				t.Fatal(err)
			}
			prev := time.Since(start)
			wantCalls := int64(calls*2/3 - straddle)
			if plan.Calls != wantCalls || plan.Rows != 2*wantCalls || plan.Kept != 1 {
				t.Fatalf("plan %+v, want %d calls / %d rows / 1 kept", plan, wantCalls, 2*wantCalls)
			}
			start = time.Now()
			n, err := Prune(ctx, db, cutoff, 0)
			if err != nil {
				t.Fatal(err)
			}
			del := time.Since(start)
			var left, recentLeft int64
			_ = db.QueryRow(`SELECT count(*), count(CASE WHEN ts >= ? THEN 1 END) FROM ledger`, cutoff).Scan(&left, &recentLeft)
			if n != plan.Rows || left != int64(total)-n || recentLeft != int64(calls-calls*2/3)*2+1 {
				t.Fatalf("deleted %d, left %d (recent %d)", n, left, recentLeft)
			}
			t.Logf("per-session %d: preview %s, prune %s", perSession, prev, del)
			if prev > 20*time.Second || del > 60*time.Second {
				t.Fatalf("too slow: preview %s, prune %s", prev, del)
			}
		})
	}
}

// legacyEligible is the eligibility predicate as it was before TASK-0034 (two NOT
// EXISTS, the second with IS comparisons). It is kept here as the SQL oracle.
const legacyEligible = `r.ts < ?1 AND (r.tool_use_id IS NULL OR (
    NOT EXISTS (SELECT 1 FROM ledger q WHERE q.tool_use_id = r.tool_use_id AND q.client = r.client
        AND q.session_id = r.session_id AND q.id <> r.id AND q.ts >= ?1)
    AND (r.client IS NOT NULL AND r.session_id IS NOT NULL OR NOT EXISTS (
        SELECT 1 FROM ledger q WHERE q.tool_use_id = r.tool_use_id AND q.session_id IS r.session_id
            AND q.client IS r.client AND q.id <> r.id AND q.ts >= ?1))))`

type oracleRow struct {
	id         int64
	ts         int64
	client     sql.NullString
	session    sql.NullString
	toolUseID  sql.NullString
	wantDelete bool
}

func idSet(t *testing.T, db *sql.DB) map[int64]bool {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM ledger`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	m := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		m[id] = true
	}
	return m
}

// The deletion set equals a plain Go oracle (and the previous SQL) for random ledgers
// mixing NULL / empty client and session_id, NULL tool_use_id, hook/gateway pairs and
// rows straddling the cutoff (TASK-0034 AC-1). A group is (tool_use_id, client,
// session_id) with NULL equal to NULL; it is deleted iff every member is older than
// the cutoff; rows without tool_use_id are judged alone.
func TestPruneDeletionSetMatchesOracleWithNullKeys(t *testing.T) {
	ctx := context.Background()
	const cutoff = int64(500)
	rng := rand.New(rand.NewSource(34))
	clients := []any{nil, "claude-code", "opencode"}
	sessions := []any{nil, "", "s1", "s2"}
	tuids := []any{nil, "a", "b", "c", "d", "e", "f", "g", "h"}
	sources := []string{"hook", "gateway", "transcript"}
	for round := 0; round < 40; round++ {
		db, err := store.Open(ctx, filepath.Join(t.TempDir(), "forge.db"))
		if err != nil {
			t.Fatal(err)
		}
		var rows []oracleRow
		for i := 0; i < 120; i++ {
			c, s, u := clients[rng.Intn(len(clients))], sessions[rng.Intn(len(sessions))], tuids[rng.Intn(len(tuids))]
			ts := int64(400 + rng.Intn(200)) // both sides of the cutoff
			res, err := db.Exec(`INSERT OR IGNORE INTO ledger(ts,kind,name,client,session_id,tool_use_id,source) VALUES (?,'tool','t',?,?,?,?)`,
				ts, c, s, u, sources[rng.Intn(len(sources))])
			if err != nil {
				t.Fatal(err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue
			}
			id, _ := res.LastInsertId()
			r := oracleRow{id: id, ts: ts}
			if c != nil {
				r.client = sql.NullString{String: c.(string), Valid: true}
			}
			if s != nil {
				r.session = sql.NullString{String: s.(string), Valid: true}
			}
			if u != nil {
				r.toolUseID = sql.NullString{String: u.(string), Valid: true}
			}
			rows = append(rows, r)
		}
		want := map[int64]bool{}
		for _, r := range rows {
			del := r.ts < cutoff
			if del && r.toolUseID.Valid {
				for _, q := range rows {
					if q.toolUseID == r.toolUseID && q.client == r.client && q.session == r.session && q.ts >= cutoff {
						del = false
						break
					}
				}
			}
			if del {
				want[r.id] = true
			}
		}
		// legacy SQL agrees with the oracle (validates the oracle itself)
		legacy := map[int64]bool{}
		lr, err := db.Query(`SELECT r.id FROM ledger r WHERE `+legacyEligible, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		for lr.Next() {
			var id int64
			_ = lr.Scan(&id)
			legacy[id] = true
		}
		lr.Close()
		if !reflect.DeepEqual(legacy, want) {
			t.Fatalf("round %d: oracle and legacy SQL disagree (%d vs %d)", round, len(want), len(legacy))
		}
		plan, err := PrunePreview(ctx, db, cutoff)
		if err != nil || plan.Rows != int64(len(want)) {
			t.Fatalf("round %d: preview %+v %v, want %d rows", round, plan, err, len(want))
		}
		// batch of 7 exercises group closure across batches
		before := idSet(t, db)
		var total int64
		for {
			n, err := pruneBatch(ctx, db, cutoff, 7)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
			total += n
		}
		after := idSet(t, db)
		got := map[int64]bool{}
		for id := range before {
			if !after[id] {
				got[id] = true
			}
		}
		if !reflect.DeepEqual(got, want) || total != int64(len(want)) {
			t.Fatalf("round %d: deleted %d rows, oracle %d (sets differ: %v)", round, total, len(want), !reflect.DeepEqual(got, want))
		}
		db.Close()
	}
}

// TASK-0034 AC-1 (performance): 60k rows whose client (or session_id) is NULL and that
// carry a tool_use_id took about 106 s to prune (second NOT EXISTS scanned every NULL
// row per row). Gated: FORGE_LARGE_TESTS=1.
func TestPruneNullClientLedgerIsFast(t *testing.T) {
	if os.Getenv("FORGE_LARGE_TESTS") == "" {
		t.Skip("set FORGE_LARGE_TESTS=1")
	}
	for _, variant := range []string{"client-null", "client-and-session-null", "session-null"} {
		t.Run(variant, func(t *testing.T) { pruneNullLedgerSpeed(t, variant) })
	}
}

func pruneNullLedgerSpeed(t *testing.T, variant string) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const total = 60_000
	const cutoff = int64(1_000_000_000)
	tx, _ := db.BeginTx(ctx, nil)
	stmt, err := tx.Prepare(`INSERT INTO ledger(ts,kind,name,client,session_id,tool_use_id,source) VALUES (?,'tool','t',?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	calls := total / 2
	for i := 0; i < calls; i++ {
		ts := int64(i) * 1000
		if i >= calls*2/3 {
			ts = cutoff + int64(i)
		}
		gts := ts + 1
		if i == calls*2/3-1 {
			gts = cutoff + 5
		}
		var client, sess any = nil, fmt.Sprintf("s%d", i/100)
		switch variant {
		case "client-and-session-null":
			sess = nil
		case "session-null":
			client, sess = "claude-code", nil
		}
		if _, err := stmt.Exec(ts, client, sess, fmt.Sprintf("u%d", i), "hook"); err != nil {
			t.Fatal(err)
		}
		if _, err := stmt.Exec(gts, client, sess, fmt.Sprintf("u%d", i), "gateway"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	plan, err := PrunePreview(ctx, db, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	prev := time.Since(start)
	wantCalls := int64(calls*2/3 - 1)
	if plan.Calls != wantCalls || plan.Rows != 2*wantCalls || plan.Kept != 1 {
		t.Fatalf("plan %+v, want %d calls", plan, wantCalls)
	}
	start = time.Now()
	n, err := Prune(ctx, db, cutoff, 0)
	if err != nil {
		t.Fatal(err)
	}
	del := time.Since(start)
	if n != plan.Rows {
		t.Fatalf("deleted %d, want %d", n, plan.Rows)
	}
	t.Logf("%s, %d rows: preview %s, prune %s", variant, total, prev, del)
	if prev > 10*time.Second || del > 20*time.Second {
		t.Fatalf("too slow: preview %s, prune %s", prev, del)
	}
}
