package export

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestPostgresReplace runs only with FORGE_PG_TEST_URL (a throwaway database: the
// test creates, fills and empties schema forge). Without it the test is skipped;
// the real-database behavior is then unverified.
func TestPostgresReplace(t *testing.T) {
	url := os.Getenv("FORGE_PG_TEST_URL")
	if url == "" {
		t.Skip("FORGE_PG_TEST_URL 이 없어 실제 Postgres 시험을 건너뜁니다 (미확인)")
	}
	tg, err := ParseTarget(url, os.Getenv(EnvCA))
	if err != nil {
		t.Fatal(tg.Redact(err.Error()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rate := 0.5
	s := Snapshot{SchemaVersion: 1, GeneratedAt: "2026-11-03T00:00:00Z", ForgeVersion: "test", DayBasis: "UTC",
		Items: []Item{{Kind: "skill", Name: "a", Calls: 2, OK: 1, Failed: 1, SuccessRate: &rate, LastCall: "2026-11-02T10:00:00Z"},
			{Kind: "tool", Name: "b", Calls: 1, Unknown: 1, LastCall: "2026-11-02T11:00:00Z"}},
		Daily: []DayRow{{Day: "2026-11-02", Kind: "skill", Calls: 2, OK: 1, Failed: 1}}}
	count := func(table string) int {
		c, err := pgx.ConnectConfig(ctx, tg.Config)
		if err != nil {
			t.Fatal(tg.Redact(err.Error()))
		}
		defer c.Close(ctx)
		var n int
		if err := c.QueryRow(ctx, "SELECT count(*) FROM forge."+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i := 0; i < 2; i++ { // idempotent: the second run leaves the same rows
		if err := Write(ctx, tg, s); err != nil {
			t.Fatal(err)
		}
		if count("items") != 2 || count("daily") != 1 || count("meta") != 1 {
			t.Fatalf("run %d: unexpected row counts", i)
		}
	}
	// A failing replacement (duplicate key) rolls back and keeps the old rows.
	bad := s
	bad.Items = append(append([]Item{}, s.Items...), s.Items[0])
	if err := Write(ctx, tg, bad); err == nil {
		t.Fatal("duplicate key must fail")
	}
	if count("items") != 2 {
		t.Fatal("rollback did not keep the previous content")
	}
}
