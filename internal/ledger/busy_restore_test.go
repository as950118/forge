package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TASK-0020 AC-9 follow-up: Record lowers busy_timeout on its dedicated connection when
// the context deadline is short; the pooled connection must get the original value back,
// or every later writer on that connection would silently wait less than intended.
func TestRecordRestoresBusyTimeoutOfPooledConnections(t *testing.T) {
	rec, _ := newRec(t)
	const n = 6
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k := 0; k < 5; k++ {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // short, never expires here
				ev := baseEv()
				ev.ToolUseID = fmt.Sprintf("toolu_%d_%d", i, k)
				if _, err := rec.Record(ctx, ev); err != nil {
					t.Errorf("record: %v", err)
				}
				cancel()
			}
		}(i)
	}
	wg.Wait()
	// hold several connections at once so that every pooled connection is inspected
	var conns []*sql.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < n; i++ {
		c, err := rec.DB.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var v int64
		if err := c.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != 5000 {
			t.Errorf("connection %d has busy_timeout=%d after short-deadline Records, want 5000", i, v)
		}
	}
}
