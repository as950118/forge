package history

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
