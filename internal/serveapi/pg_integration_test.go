package serveapi

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/as950118/forge/internal/export"
)

// TestPGStoreAgainstPostgres runs only with FORGE_PG_TEST_URL (a throwaway
// database). It exports a small snapshot with export.Write and reads it back.
// Without the variable it is skipped and the real-database behavior is unverified.
func TestPGStoreAgainstPostgres(t *testing.T) {
	url := os.Getenv("FORGE_PG_TEST_URL")
	if url == "" {
		t.Skip("FORGE_PG_TEST_URL 이 없어 실제 Postgres 시험을 건너뜁니다 (미확인)")
	}
	tg, err := export.ParseTarget(url, os.Getenv(export.EnvCA))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	day := time.Now().UTC().Format("2006-01-02")
	snap := export.Snapshot{SchemaVersion: 1, GeneratedAt: "2026-11-03T00:00:00Z", ForgeVersion: "test", DayBasis: "UTC",
		Items: []export.Item{{Kind: "skill", Name: "a", Calls: 2, OK: 1, Failed: 1, LastCall: "2026-11-02T10:00:00Z"},
			{Kind: "mcp", Server: "s", Name: "t", Calls: 1, Unknown: 1, LastCall: "2026-11-02T11:00:00Z"}},
		Daily: []export.DayRow{{Day: day, Kind: "skill", Calls: 2, OK: 1, Failed: 1}}}
	if err := export.Write(ctx, tg, snap); err != nil {
		t.Fatal(err)
	}
	p := NewPGStore(tg)
	if err := p.Ping(ctx); err != nil {
		t.Fatal(tg.Redact(err.Error()))
	}
	sum, err := p.Summary(ctx, 366)
	if err != nil || sum.Meta.ForgeVersion != "test" || len(sum.Items) != 2 || len(sum.Daily) != 1 {
		t.Fatalf("summary: %+v %v", sum, err)
	}
	if it, err := p.Items(ctx, "mcp"); err != nil || len(it) != 1 || it[0].Server != "s" {
		t.Fatalf("items: %+v %v", it, err)
	}
	if d, err := p.Daily(ctx, 1); err != nil || len(d) != 1 {
		t.Fatalf("daily: %+v %v", d, err)
	}
}
