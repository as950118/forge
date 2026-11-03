package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Ledger pruning (TASK-0032 AC-2; ADR-F0002 D1, ADR-F0003 D1).
//
// A call that hook and gateway both recorded is two ledger rows that share
// (client, session_id, tool_use_id) after linking (ADR-F0003); v_ledger_calls and
// the history queries show them as ONE call. Pruning therefore works on those
// groups, never on single rows: a row is deleted only when EVERY row of its group is
// older than the cutoff. A pair that straddles the cutoff (hook just before, gateway
// just after, at most a few seconds apart) is kept whole, so a call is never left
// with only one of its sides and no row newer than the cutoff is ever deleted.
// Rows without a tool_use_id (or with an unregistered client) are single-row groups
// (ADR-F0002 D1: no dedup there) and are judged by their own time.
//
// Deletion runs in batches of group-closed id sets, each batch its own transaction,
// so a large ledger neither holds the write lock for long nor leaves half a pair
// when interrupted. ledger_payloads rows go with their ledger row (ON DELETE
// CASCADE). sessions and llm_usage are not touched.

// MinPruneAge is the protected period: a cutoff newer than now-MinPruneAge is refused.
const MinPruneAge = 24 * time.Hour

// DefaultPruneBatch is the number of ledger rows picked per transaction (a batch
// also takes the partner rows of its groups, so it can be slightly larger).
const DefaultPruneBatch = 5000

// ErrPruneTooRecent is returned for a cutoff inside the protected period.
var ErrPruneTooRecent = errors.New("가장 최근 1일의 기록은 보호됩니다: --before 는 지금으로부터 1일 이전이어야 합니다")

// CheckPruneCutoff refuses a cutoff inside the protected period.
func CheckPruneCutoff(cutoffMS int64) error {
	if cutoffMS > Now().Add(-MinPruneAge).UnixMilli() {
		return ErrPruneTooRecent
	}
	return nil
}

// PrunePlan is what a prune would delete (or did delete).
type PrunePlan struct {
	CutoffMS   int64 `json:"cutoff_ms"`
	Rows       int64 `json:"rows"`        // ledger rows deleted
	Calls      int64 `json:"calls"`       // merged calls those rows form
	LinkedRows int64 `json:"linked_rows"` // rows deleted together with a partner row (Rows - Calls)
	Kept       int64 `json:"kept_straddling_rows"`
	Payloads   int64 `json:"payload_rows"` // ledger_payloads rows removed with them
	OldestMS   int64 `json:"oldest_ms,omitempty"`
	NewestMS   int64 `json:"newest_ms,omitempty"`
}

// eligible is the predicate of a ledger row r that may be deleted: older than the
// cutoff (?1) and no row of its group (same client, session_id and tool_use_id) is
// at or after it.
//
// Speed: the group lookup is written with plain equality on (client, session_id,
// tool_use_id) so SQLite probes the partial unique index ux_ledger_dedup (those three
// columns first) instead of scanning every row of the session (the earlier IS
// comparisons could not use it: cost grew with the square of the session size). Rows
// whose client or session_id is NULL (no dedup guarantee, ADR-F0002 D1) cannot match
// by equality; they are judged against recentKeys, the set of groups that have a row
// at or after the cutoff. The set is uncorrelated, so SQLite builds it once per
// statement (one pass over ix_ledger_ts) with an ephemeral index and each NULL-keyed
// row is a single lookup (TASK-0034 AC-1: 60k such rows took about 106 s with a
// correlated NOT EXISTS that scanned every NULL row per row). A row older than the
// cutoff is in recentKeys only through another member, so the old q.id <> r.id test
// is not needed.
//
// groupKey spells the group of alias a as ONE json_array text in which NULL and the
// empty string stay distinct (and any character in an id is escaped). A single text
// column lets SQLite hash the set; the earlier row-value form (five columns) was
// compared without an index and was quadratic again.
func groupKey(a string) string {
	return "json_array(" + a + ".tool_use_id, " + a + ".client, " + a + ".session_id)"
}

var recentKeys = `SELECT ` + groupKey("g") + ` FROM ledger g WHERE g.ts >= ?1 AND g.tool_use_id IS NOT NULL AND (g.client IS NULL OR g.session_id IS NULL)`

var eligible = `r.ts < ?1 AND (r.tool_use_id IS NULL OR (
    NOT EXISTS (SELECT 1 FROM ledger q WHERE q.tool_use_id = r.tool_use_id AND q.client = r.client
        AND q.session_id = r.session_id AND q.id <> r.id AND q.ts >= ?1)
    AND (r.client IS NOT NULL AND r.session_id IS NOT NULL OR ` + groupKey("r") + ` NOT IN (` + recentKeys + `))))`

// PrunePreview counts what Prune(cutoff) would delete without changing anything. It
// evaluates the eligibility twice (once for the totals, once for the merged-call
// count); everything else is derived.
func PrunePreview(ctx context.Context, db *sql.DB, cutoffMS int64) (PrunePlan, error) {
	p := PrunePlan{CutoffMS: cutoffMS}
	var oldest, newest sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT count(*), MIN(r.ts), MAX(r.ts), count(p.ledger_id)
		FROM ledger r LEFT JOIN ledger_payloads p ON p.ledger_id = r.id WHERE `+eligible, cutoffMS).Scan(&p.Rows, &oldest, &newest, &p.Payloads); err != nil {
		return p, err
	}
	p.OldestMS, p.NewestMS = oldest.Int64, newest.Int64
	if p.Rows > 0 {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM ledger r WHERE `+eligible+`
			GROUP BY r.kind, r.name, COALESCE(r.mcp_server,''), COALESCE(r.client,''), COALESCE(r.session_id,''), COALESCE(r.tool_use_id, 'row:' || r.id))`, cutoffMS).Scan(&p.Calls); err != nil {
			return p, err
		}
		p.LinkedRows = p.Rows - p.Calls
	}
	// rows older than the cutoff that stay because their group has a newer member
	var older int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ledger WHERE ts < ?1`, cutoffMS).Scan(&older); err != nil {
		return p, err
	}
	p.Kept = older - p.Rows
	return p, nil
}

// Prune deletes, in batches, the rows PrunePreview counts and returns how many rows
// it deleted. It does not preview (the caller did, once). db must be writable
// (foreign_keys on, so payloads cascade). batch <= 0 means DefaultPruneBatch.
func Prune(ctx context.Context, db *sql.DB, cutoffMS int64, batch int) (int64, error) {
	if err := CheckPruneCutoff(cutoffMS); err != nil {
		return 0, err
	}
	if batch <= 0 {
		batch = DefaultPruneBatch
	}
	var deleted int64
	for {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		n, err := pruneBatch(ctx, db, cutoffMS, batch)
		if err != nil {
			return deleted, fmt.Errorf("삭제 중 오류(이미 지운 %d행은 그대로입니다; 같은 명령을 다시 실행하면 이어서 지웁니다): %w", deleted, err)
		}
		if n == 0 {
			return deleted, nil
		}
		deleted += n
	}
}

// pruneBatch deletes one batch (a transaction of its own): up to `batch` eligible
// rows plus every other row of their groups, so a group is never split.
func pruneBatch(ctx context.Context, db *sql.DB, cutoffMS int64, batch int) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM ledger WHERE id IN (
		WITH b AS (SELECT r.id, r.client, r.session_id, r.tool_use_id FROM ledger r WHERE `+eligible+` LIMIT ?2)
		SELECT id FROM b
		UNION
		SELECT q.id FROM b JOIN ledger q ON q.tool_use_id = b.tool_use_id AND q.client = b.client AND q.session_id = b.session_id
		UNION
		SELECT q.id FROM ledger q WHERE q.tool_use_id IS NOT NULL AND (q.client IS NULL OR q.session_id IS NULL)
		  AND `+groupKey("q")+` IN (SELECT `+groupKey("b")+` FROM b WHERE b.tool_use_id IS NOT NULL AND (b.client IS NULL OR b.session_id IS NULL)))`, cutoffMS, batch)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// BackupBeforePrune writes a consistent copy of the database (VACUUM INTO) to
// <home>/backups/forge-pre-prune-<time>.db (0600; never overwrites) and returns its
// path. Restoring it is copying it back over forge.db while no forge process runs.
func BackupBeforePrune(ctx context.Context, db *sql.DB, home string) (string, error) {
	dir := filepath.Join(home, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "forge-pre-prune-"+Now().UTC().Format("20060102T150405Z")+".db")
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("백업 파일이 이미 있습니다: %s", path)
	}
	// the quoting of a path inside SQL: double any single quote
	q := ""
	for _, r := range path {
		if r == '\'' {
			q += "'"
		}
		q += string(r)
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+q+"'"); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("백업 실패: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return path, nil
}
