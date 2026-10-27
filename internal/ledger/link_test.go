package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// TASK-0021 AC-1/AC-2 (ADR-F0003): a hook row and a gateway row of the same call are
// linked at record time, so the ordinary merge key counts them once; a failure in
// either source wins.

var t0 = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

func hookEv(name, tuid string, at time.Time, ok *bool) Event {
	return Event{Kind: KindTool, Name: name, Client: "claude-code", SessionID: "sess-real", ToolUseID: tuid,
		Source: SourceHook, TS: at, OK: ok}
}

func gwEv(kind, name, server string, at time.Time, ok *bool) Event {
	return Event{Kind: kind, Name: name, MCPServer: server, Client: "claude-code", SessionID: "gw-abc123",
		Source: SourceGateway, TS: at, OK: ok}
}

func rec1(t *testing.T, r *SQLite, ev Event) {
	t.Helper()
	if _, err := r.Record(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

func callCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM v_ledger_calls`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seedGWRegistry(t *testing.T, r *SQLite) {
	exec(t, r.DB, `INSERT INTO tools(id,scope,name,kind,description,command) VALUES (1,'global','greet','command','d','echo')`)
	exec(t, r.DB, `INSERT INTO mcp_servers(id,scope,name,transport,command) VALUES (1,'global','my-srv','stdio','x')`)
}

func TestLinkHookThenGatewayCustomTool(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	rec1(t, r, hookEv("forge_greet", "toolu_1", t0, nil))                                 // PreToolUse
	rec1(t, r, gwEv(KindTool, "greet", "", t0.Add(40*time.Millisecond), Bool(true)))      // gateway End
	rec1(t, r, hookEv("forge_greet", "toolu_1", t0.Add(90*time.Millisecond), Bool(true))) // PostToolUse
	if n := callCount(t, r.DB); n != 1 {
		t.Fatalf("calls=%d, want 1 (hook+gateway double count)", n)
	}
	var kind, name, sources string
	var found sql.NullInt64
	if err := r.DB.QueryRow(`SELECT kind, name, sources, found FROM v_ledger_calls`).Scan(&kind, &name, &sources, &found); err != nil {
		t.Fatal(err)
	}
	if len(sources) < 8 || kind != "tool" || name != "greet" || !found.Valid || found.Int64 != 1 {
		t.Errorf("merged call kind=%s name=%s found=%v", kind, name, found)
	}
	if miss, _ := r.Misses(context.Background()); len(miss) != 0 {
		t.Errorf("a registered gateway tool must not be a miss: %+v", miss)
	}
}

func TestLinkGatewayThenHookAndProxiedServer(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	// gateway first (no PreToolUse hook installed), then the hook post: server spelled my_srv by the client
	rec1(t, r, gwEv(KindMCP, "echo", "my-srv", t0, Bool(true)))
	rec1(t, r, hookEv("my_srv__echo", "toolu_9", t0.Add(300*time.Millisecond), Bool(true)))
	if n := callCount(t, r.DB); n != 1 {
		t.Fatalf("calls=%d, want 1", n)
	}
	var kind, server string
	if err := r.DB.QueryRow(`SELECT kind, COALESCE(mcp_server,'') FROM ledger WHERE source='hook'`).Scan(&kind, &server); err != nil {
		t.Fatal(err)
	}
	if kind != "mcp" || server != "my-srv" {
		t.Errorf("hook row must take the gateway's identity: kind=%s server=%s", kind, server)
	}
}

func TestLinkIsOneToOneAndNearestWins(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	// two hook calls, one gateway row: only one pair is formed, the other hook call stays single
	rec1(t, r, hookEv("forge_greet", "toolu_a", t0, nil))
	rec1(t, r, hookEv("forge_greet", "toolu_b", t0.Add(time.Second), nil))
	rec1(t, r, gwEv(KindTool, "greet", "", t0.Add(900*time.Millisecond), Bool(true)))
	if n := callCount(t, r.DB); n != 2 {
		t.Fatalf("calls=%d, want 2 (a gateway row links to exactly one hook row)", n)
	}
	var tuid string
	if err := r.DB.QueryRow(`SELECT tool_use_id FROM ledger WHERE source='gateway'`).Scan(&tuid); err != nil || tuid != "toolu_b" {
		t.Errorf("nearest hook row (toolu_b) must win: %q %v", tuid, err)
	}
	// a second gateway row takes the remaining hook row; a third finds none and stays single
	rec1(t, r, gwEv(KindTool, "greet", "", t0.Add(50*time.Millisecond), Bool(true)))
	rec1(t, r, gwEv(KindTool, "greet", "", t0.Add(60*time.Millisecond), Bool(true)))
	if n := callCount(t, r.DB); n != 3 {
		t.Errorf("calls=%d, want 3 (2 pairs + 1 unmatched gateway row)", n)
	}
}

func TestLinkRefusals(t *testing.T) {
	cases := map[string]func(t *testing.T, r *SQLite){
		"other name": func(t *testing.T, r *SQLite) {
			rec1(t, r, hookEv("forge_greet", "t1", t0, nil))
			rec1(t, r, gwEv(KindTool, "other", "", t0, Bool(true)))
		},
		"outside window": func(t *testing.T, r *SQLite) {
			rec1(t, r, hookEv("forge_greet", "t1", t0, nil))
			rec1(t, r, gwEv(KindTool, "greet", "", t0.Add(LinkWindow+time.Second), Bool(true)))
		},
		"other client": func(t *testing.T, r *SQLite) {
			rec1(t, r, hookEv("forge_greet", "t1", t0, nil))
			ev := gwEv(KindTool, "greet", "", t0, Bool(true))
			ev.Client = "opencode"
			rec1(t, r, ev)
		},
		"meta tool": func(t *testing.T, r *SQLite) {
			rec1(t, r, hookEv("forge_skill", "t1", t0, nil))
			rec1(t, r, gwEv(KindTool, "skill", "", t0, Bool(true)))
		},
		"other server": func(t *testing.T, r *SQLite) {
			rec1(t, r, hookEv("a_srv__echo", "t1", t0, nil))
			rec1(t, r, gwEv(KindMCP, "echo", "b-srv", t0, Bool(true)))
		},
		"gateway row with its own tool_use_id": func(t *testing.T, r *SQLite) {
			rec1(t, r, hookEv("forge_greet", "t1", t0, nil))
			ev := gwEv(KindTool, "greet", "", t0, Bool(true))
			ev.ToolUseID = "t-gw"
			rec1(t, r, ev)
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			r, _ := newRec(t)
			seedGWRegistry(t, r)
			run(t, r)
			if n := callCount(t, r.DB); n != 2 {
				t.Errorf("calls=%d, want 2 (no link)", n)
			}
		})
	}
}

func TestMergedOKIsFailureFirst(t *testing.T) {
	cases := []struct {
		hook, gw *bool
		want     sql.NullInt64
	}{
		{Bool(true), Bool(false), sql.NullInt64{Int64: 0, Valid: true}},
		{Bool(false), Bool(true), sql.NullInt64{Int64: 0, Valid: true}},
		{Bool(true), Bool(true), sql.NullInt64{Int64: 1, Valid: true}},
		{nil, Bool(false), sql.NullInt64{Int64: 0, Valid: true}},
		{Bool(true), nil, sql.NullInt64{Int64: 1, Valid: true}},
		{nil, nil, sql.NullInt64{}},
	}
	for i, c := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			r, _ := newRec(t)
			seedGWRegistry(t, r)
			rec1(t, r, hookEv("forge_greet", "t1", t0, c.hook))
			rec1(t, r, gwEv(KindTool, "greet", "", t0, c.gw))
			if n := callCount(t, r.DB); n != 1 {
				t.Fatalf("calls=%d", n)
			}
			var ok sql.NullInt64
			if err := r.DB.QueryRow(`SELECT ok FROM v_ledger_calls`).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok != c.want {
				t.Errorf("hook=%v gateway=%v: merged ok=%v want %v", c.hook, c.gw, ok, c.want)
			}
		})
	}
}

func TestHookNameOfUnregisteredGatewayToolIsStillAMiss(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	rec1(t, r, hookEv("forge_ghost", "t1", t0, Bool(true)))
	rec1(t, r, hookEv("forge_list", "t2", t0, Bool(true)))
	rec1(t, r, hookEv("nosrv__x", "t3", t0, Bool(true)))
	miss, err := r.Misses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range miss {
		got[m.Name] = true
	}
	if !got["forge_ghost"] || !got["nosrv__x"] || got["forge_list"] {
		t.Errorf("misses=%v (forge_ghost and nosrv__x are unregistered, the meta tool forge_list is not)", got)
	}
}

func tuidOf(t *testing.T, r *SQLite, where string, args ...any) sql.NullString {
	t.Helper()
	var v sql.NullString
	if err := r.DB.QueryRow(`SELECT tool_use_id FROM ledger WHERE `+where, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Another client's gateway row (nearer in time) must not take the hook's tool_use_id.
func TestLinkNeverCrossesClients(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	rec1(t, r, hookEv("forge_greet", "toolu_cc", t0, nil))
	oc := gwEv(KindTool, "greet", "", t0.Add(10*time.Millisecond), Bool(true))
	oc.Client = "opencode"
	rec1(t, r, oc) // arrives first and is nearer
	rec1(t, r, gwEv(KindTool, "greet", "", t0.Add(2*time.Second), Bool(true)))
	if v := tuidOf(t, r, `client='opencode'`); v.Valid {
		t.Errorf("opencode gateway row took the claude-code hook's tool_use_id: %v", v)
	}
	if v := tuidOf(t, r, `client='claude-code' AND source='gateway'`); v.String != "toolu_cc" {
		t.Errorf("the claude-code gateway row must be the partner: %v", v)
	}
	if n := callCount(t, r.DB); n != 2 {
		t.Errorf("calls=%d, want 2 (one merged claude-code call + one opencode call)", n)
	}
	// and the other direction: the opencode hook-less row never links to a later claude-code hook
	r2, _ := newRec(t)
	seedGWRegistry(t, r2)
	oc2 := gwEv(KindTool, "greet", "", t0, Bool(true))
	oc2.Client = "opencode"
	rec1(t, r2, oc2)
	rec1(t, r2, hookEv("forge_greet", "toolu_cc", t0, nil))
	if v := tuidOf(t, r2, `client='opencode'`); v.Valid {
		t.Errorf("opencode row linked by a claude-code hook: %v", v)
	}
}

// The window is exactly 5 s (literal, independent of the LinkWindow constant).
func TestLinkWindowIsFiveSeconds(t *testing.T) {
	for _, gwFirst := range []bool{false, true} {
		for _, c := range []struct {
			gap  time.Duration
			want int
		}{{5000 * time.Millisecond, 1}, {5001 * time.Millisecond, 2}} {
			r, _ := newRec(t)
			seedGWRegistry(t, r)
			h, g := hookEv("forge_greet", "t1", t0, nil), gwEv(KindTool, "greet", "", t0.Add(c.gap), Bool(true))
			if gwFirst {
				rec1(t, r, g)
				rec1(t, r, h)
			} else {
				rec1(t, r, h)
				rec1(t, r, g)
			}
			if n := callCount(t, r.DB); n != c.want {
				t.Errorf("gwFirst=%v gap=%v: calls=%d want %d", gwFirst, c.gap, n, c.want)
			}
		}
	}
}

// Attribution stays exact once a pair exists: a later row never steals either partner.
func TestLinkedPartnersAreNotStolen(t *testing.T) {
	// a new gateway row cannot take a hook row that already has its gateway row
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	rec1(t, r, hookEv("forge_greet", "toolu_a", t0, nil))
	rec1(t, r, gwEv(KindTool, "greet", "", t0.Add(10*time.Millisecond), Bool(true)))
	rec1(t, r, gwEv(KindTool, "greet", "", t0.Add(20*time.Millisecond), Bool(true)))
	if v := tuidOf(t, r, `source='gateway' AND ts=?`, t0.UnixMilli()+10); v.String != "toolu_a" {
		t.Errorf("first gateway row lost its partner: %v", v)
	}
	if v := tuidOf(t, r, `source='gateway' AND ts=?`, t0.UnixMilli()+20); v.Valid {
		t.Errorf("second gateway row stole the linked hook row: %v", v)
	}
	// the hook's PostToolUse of a linked call does not pull in another gateway row
	rec1(t, r, hookEv("forge_greet", "toolu_a", t0.Add(30*time.Millisecond), Bool(true)))
	if v := tuidOf(t, r, `source='gateway' AND ts=?`, t0.UnixMilli()+20); v.Valid {
		t.Errorf("post event of a linked call linked another gateway row: %v", v)
	}
	if n := callCount(t, r.DB); n != 2 {
		t.Errorf("calls=%d, want 2", n)
	}
	// a gateway row that has its own tool_use_id is never taken by a later hook row
	r2, _ := newRec(t)
	seedGWRegistry(t, r2)
	own := gwEv(KindTool, "greet", "", t0, Bool(true))
	own.ToolUseID = "t-gw"
	rec1(t, r2, own)
	rec1(t, r2, hookEv("forge_greet", "toolu_b", t0, nil))
	if v := tuidOf(t, r2, `source='gateway'`); v.String != "t-gw" {
		t.Errorf("gateway row's own tool_use_id was overwritten: %v", v)
	}
	if n := callCount(t, r2.DB); n != 2 {
		t.Errorf("calls=%d, want 2", n)
	}
}
