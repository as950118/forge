package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/heonjinjeong/forge/internal/ledger"
	"github.com/heonjinjeong/forge/internal/profile"
)

// TASK-0021 AC-8: a registered tool that the active profile hides is not "unknown":
// the ledger row is error_class=hidden_by_profile with found=1 (so it never reaches
// the miss list) and the message says so. Unregistered names stay misses.
func TestHiddenByProfileIsNotAMiss(t *testing.T) {
	e, ps := profileEnv(t)
	ctx := context.Background()
	gl := profile.Ref{Name: "min", Scope: profile.ScopeGlobal}
	if _, err := ps.Create(ctx, gl, "", false); err != nil {
		t.Fatal(err)
	}
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindTool, Name: "t1"})
	add(t, ps, gl, profile.AddSpec{Kind: profile.KindMCP, Name: "gw-srv", Exposure: "direct", Clients: []string{"claude-code"}})
	if _, _, err := ps.Use(ctx, "min", "", []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	g := e.gw(func(o *Options) { o.Exposure = ProfileExposure{DB: e.db} })
	cs := e.connect(g, "claude-code", nil)

	for _, name := range []string{"forge_t2", "gw_srv__alpha"} {
		_, err := callRaw(cs, name, nil)
		if err == nil || !strings.Contains(err.Error(), "등록된 도구이지만") || strings.Contains(err.Error(), "알 수 없는 도구") {
			t.Errorf("%s: hidden tool must be told apart from an unknown one: %v", name, err)
		}
	}
	_, _ = callRaw(cs, "gw_srv__typo", nil) // hidden server, tool the server does not have
	_, _ = callRaw(cs, "forge_ghost", nil)  // not registered at all

	byName := map[string]ledgerRow{}
	for _, r := range e.rows() {
		byName[r.Name] = r
	}
	for _, n := range []string{"t2", "alpha"} {
		r, ok := byName[n]
		if !ok || r.ErrClass != "hidden_by_profile" || !r.Found.Valid || r.Found.Int64 != 1 || r.OK.Int64 != 0 || r.Source != "gateway" {
			t.Errorf("%s: want hidden_by_profile/found=1/failed, got %+v", n, r)
		}
	}
	for _, n := range []string{"typo", "forge_ghost"} {
		r, ok := byName[n]
		if !ok || r.ErrClass != "not_found" || r.Found.Int64 != 0 {
			t.Errorf("%s: must stay a miss, got %+v", n, r)
		}
	}
	rows, err := e.db.Query(`SELECT name FROM v_misses ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var misses []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		misses = append(misses, n)
	}
	if strings.Join(misses, ",") != "forge_ghost,typo" {
		t.Errorf("misses=%v, want only the unregistered names", misses)
	}
}

// TASK-0021 AC-1 end to end: a real gateway call followed by the hook's PostToolUse of
// the same call is one call, not two.
func TestGatewayCallAndHookAreOneCall(t *testing.T) {
	e := newEnv(t)
	e.addTool("say", "none", "printf", "<%s>", "{{v}}")
	cs := e.connect(e.gw(), "claude-code", nil)
	if out, isErr := invoke(t, cs, "forge_say", map[string]any{"v": "hi"}); isErr || out != "<hi>" {
		t.Fatalf("%q %v", out, isErr)
	}
	rec := &ledger.SQLite{DB: e.db}
	if _, err := rec.Record(context.Background(), ledger.Event{Kind: ledger.KindTool, Name: "forge_say", Client: "claude-code",
		SessionID: "sess-real", ToolUseID: "toolu_x", Source: ledger.SourceHook, OK: ledger.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM v_ledger_calls`).Scan(&n); err != nil || n != 1 {
		t.Errorf("calls=%d (%v), want 1", n, err)
	}
	var sources string
	var found int
	if err := e.db.QueryRow(`SELECT sources, found FROM v_ledger_calls`).Scan(&sources, &found); err != nil || found != 1 || !strings.Contains(sources, "hook") || !strings.Contains(sources, "gateway") {
		t.Errorf("sources=%q found=%d %v", sources, found, err)
	}
}
