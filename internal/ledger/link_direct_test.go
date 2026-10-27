package ledger

import (
	"testing"
	"time"
)

// TASK-0030 AC-2 (ADR-F0004): a gateway row that carries the client's tool_use_id is
// merged with the hook row of the same id directly, without the time window.

func gwEvID(kind, name, server, tuid string, at time.Time, ok *bool) Event {
	ev := gwEv(kind, name, server, at, ok)
	ev.ToolUseID = tuid
	return ev
}

func sessionsOf(t *testing.T, r *SQLite, where string) []string {
	t.Helper()
	rows, err := r.DB.Query(`SELECT COALESCE(session_id,'') FROM ledger WHERE ` + where + ` ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestDirectMergeIgnoresTimeWindow(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	// 2 minutes apart: far outside LinkWindow, same id.
	rec1(t, r, hookEv("forge_greet", "toolu_d1", t0, nil))
	rec1(t, r, gwEvID(KindTool, "greet", "", "toolu_d1", t0.Add(2*time.Minute), Bool(true)))
	if n := callCount(t, r.DB); n != 1 {
		t.Fatalf("calls=%d, want 1 (direct merge by tool_use_id)", n)
	}
	if ss := sessionsOf(t, r, `source='gateway'`); len(ss) != 1 || ss[0] != "sess-real" {
		t.Errorf("gateway row must take the hook's session: %v", ss)
	}
}

func TestDirectMergeGatewayPendingThenHookThenEnd(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	// gateway Start (pending, ok NULL) -> hook Pre -> gateway End -> hook Post
	rec1(t, r, gwEvID(KindTool, "greet", "", "toolu_d2", t0, nil))
	rec1(t, r, hookEv("forge_greet", "toolu_d2", t0.Add(10*time.Millisecond), nil))
	rec1(t, r, gwEvID(KindTool, "greet", "", "toolu_d2", t0.Add(50*time.Millisecond), Bool(true)))
	rec1(t, r, hookEv("forge_greet", "toolu_d2", t0.Add(90*time.Millisecond), Bool(true)))
	if n := callCount(t, r.DB); n != 1 {
		t.Fatalf("calls=%d, want 1", n)
	}
	if ss := sessionsOf(t, r, `source='gateway'`); len(ss) != 1 {
		t.Errorf("gateway End after the join must update the joined row, got rows %v", ss)
	}
	var ok int
	if err := r.DB.QueryRow(`SELECT ok FROM ledger WHERE source='gateway'`).Scan(&ok); err != nil || ok != 1 {
		t.Errorf("gateway End result lost: ok=%d err=%v", ok, err)
	}
}

func TestDirectMergeHookFirstThenGatewayStartEnd(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	rec1(t, r, hookEv("forge_greet", "toolu_d3", t0, nil))
	rec1(t, r, gwEvID(KindTool, "greet", "", "toolu_d3", t0.Add(10*time.Millisecond), nil))
	rec1(t, r, gwEvID(KindTool, "greet", "", "toolu_d3", t0.Add(60*time.Millisecond), Bool(true)))
	if n := callCount(t, r.DB); n != 1 {
		t.Fatalf("calls=%d, want 1", n)
	}
	if ss := sessionsOf(t, r, `source='gateway'`); len(ss) != 1 {
		t.Errorf("gateway rows %v, want 1", ss)
	}
}

func TestDirectMergeProxiedServer(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	rec1(t, r, gwEvID(KindMCP, "echo", "my-srv", "toolu_d4", t0, Bool(true)))
	rec1(t, r, hookEv("my_srv__echo", "toolu_d4", t0.Add(time.Minute), Bool(true)))
	if n := callCount(t, r.DB); n != 1 {
		t.Fatalf("calls=%d, want 1", n)
	}
}

// False-positive guards.

func TestDirectMergeNeverCrossesClients(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	oc := gwEvID(KindTool, "greet", "", "toolu_x", t0, Bool(true))
	oc.Client = "opencode"
	rec1(t, r, oc)
	rec1(t, r, hookEv("forge_greet", "toolu_x", t0.Add(time.Second), Bool(true)))
	if n := callCount(t, r.DB); n != 2 {
		t.Errorf("calls=%d, want 2 (same id, different clients)", n)
	}
	if v := tuidOf(t, r, `client='opencode'`); v.String != "toolu_x" {
		t.Errorf("opencode row changed: %v", v)
	}
	if ss := sessionsOf(t, r, `client='opencode'`); len(ss) != 1 || ss[0] != "gw-abc123" {
		t.Errorf("opencode row session changed: %v", ss)
	}
}

func TestDirectMergeDifferentIDWithinWindowStaysSeparate(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	rec1(t, r, hookEv("forge_greet", "toolu_A", t0, Bool(true)))
	rec1(t, r, gwEvID(KindTool, "greet", "", "toolu_B", t0.Add(50*time.Millisecond), Bool(true)))
	if n := callCount(t, r.DB); n != 2 {
		t.Errorf("calls=%d, want 2: a gateway row with its own id is never window-linked", n)
	}
}

func TestDirectMergeNameMismatchStaysSeparate(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	rec1(t, r, hookEv("forge_other", "toolu_n", t0, Bool(true)))
	rec1(t, r, gwEvID(KindTool, "greet", "", "toolu_n", t0.Add(10*time.Millisecond), Bool(true)))
	if n := callCount(t, r.DB); n != 2 {
		t.Errorf("calls=%d, want 2 (same id, different tool)", n)
	}
}

func TestDirectMergeIsOneToOne(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	// hook row already joined with a gateway row of the same id
	rec1(t, r, hookEv("forge_greet", "toolu_o", t0, nil))
	rec1(t, r, gwEvID(KindTool, "greet", "", "toolu_o", t0.Add(10*time.Millisecond), Bool(true)))
	// a second hook for another call must not be taken by the already-joined gateway row
	rec1(t, r, hookEv("forge_greet", "toolu_p", t0.Add(20*time.Millisecond), nil))
	if n := callCount(t, r.DB); n != 2 {
		t.Errorf("calls=%d, want 2 (a joined gateway row is not reused)", n)
	}
	// a re-sent gateway record of the same id (id reuse) updates the joined row, no new row
	rec1(t, r, gwEvID(KindTool, "greet", "", "toolu_o", t0.Add(30*time.Millisecond), Bool(true)))
	if ss := sessionsOf(t, r, `source='gateway'`); len(ss) != 1 {
		t.Errorf("gateway rows %v, want 1", ss)
	}
}

func TestDirectMergeUnknownClientNotLinked(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	g := gwEvID(KindTool, "greet", "", "toolu_u", t0, Bool(true))
	g.Client = "not-registered"
	h := hookEv("forge_greet", "toolu_u", t0, Bool(true))
	h.Client = "not-registered"
	rec1(t, r, g)
	rec1(t, r, h)
	if ss := sessionsOf(t, r, `source='gateway'`); len(ss) != 1 || ss[0] != "gw-abc123" {
		t.Errorf("unregistered-client row must stay untouched: %v", ss)
	}
}

// Without an id the old time-window fallback still links.
func TestWindowFallbackStillLinks(t *testing.T) {
	r, _ := newRec(t)
	seedGWRegistry(t, r)
	rec1(t, r, hookEv("forge_greet", "toolu_w", t0, nil))
	rec1(t, r, gwEv(KindTool, "greet", "", t0.Add(40*time.Millisecond), Bool(true)))
	if n := callCount(t, r.DB); n != 1 {
		t.Errorf("calls=%d, want 1 (fallback)", n)
	}
}
