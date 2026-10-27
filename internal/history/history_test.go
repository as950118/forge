package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/heonjinjeong/forge/internal/ledger"
	"github.com/heonjinjeong/forge/internal/store"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

var ctx = context.Background()

func newHome(t *testing.T) (string, *sql.DB) {
	t.Helper()
	home := t.TempDir()
	if err := store.EnsureLayout(home); err != nil {
		t.Fatal(err)
	}
	rw, err := store.Open(ctx, store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rw.Close() })
	return home, rw
}

func openRO(t *testing.T, home string) *sql.DB {
	t.Helper()
	db, err := Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// row is one ledger row; empty strings and nil pointers are stored as NULL.
type row struct {
	ts                                        int64
	kind, name, server, client, project, sess string
	tuid, source, errc, inv                   string
	ok, found, lat                            *int
}

func p(n int) *int { return &n }

func ins(t *testing.T, db *sql.DB, r row) {
	t.Helper()
	ns := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	ni := func(n *int) any {
		if n == nil {
			return nil
		}
		return *n
	}
	if r.source == "" {
		r.source = "hook"
	}
	if r.kind == "" {
		r.kind = "skill"
	}
	_, err := db.Exec(`INSERT INTO ledger(ts,kind,name,mcp_server,client,project,session_id,tool_use_id,source,error_class,invocation,ok,found,latency_ms)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ts, r.kind, r.name, ns(r.server), ns(r.client), ns(r.project), ns(r.sess), ns(r.tuid), r.source,
		ns(r.errc), ns(r.inv), ni(r.ok), ni(r.found), ni(r.lat))
	if err != nil {
		t.Fatalf("insert %+v: %v", r, err)
	}
}

func names(cs []Call) string {
	var s []string
	for _, c := range cs {
		s = append(s, c.Name)
	}
	return strings.Join(s, ",")
}

func ptr[T any](v T) *T { return &v }

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	Now = func() time.Time { return now }
	t.Cleanup(func() { Now = time.Now })
	kst := time.FixedZone("KST", 9*3600)
	ms := func(tm time.Time) int64 { return tm.UnixMilli() }
	good := map[string]int64{
		"0s":                        ms(now),
		"30s":                       ms(now.Add(-30 * time.Second)),
		"90m":                       ms(now.Add(-90 * time.Minute)),
		"24h":                       ms(now.Add(-24 * time.Hour)),
		"7d":                        ms(now.Add(-7 * 24 * time.Hour)),
		"2w":                        ms(now.Add(-14 * 24 * time.Hour)),
		" 7d ":                      ms(now.Add(-7 * 24 * time.Hour)),
		"2026-10-01":                ms(time.Date(2026, 10, 1, 0, 0, 0, 0, kst)),
		"2026-10-01T09:00:00Z":      ms(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)),
		"2026-10-01T09:00:00+09:00": ms(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)),
		"2026-10-01T09:00:00.250Z":  ms(time.Date(2026, 10, 1, 9, 0, 0, 250e6, time.UTC)),
		"2026-02-29":                -1, // placeholder, replaced below (invalid date)
	}
	delete(good, "2026-02-29")
	for in, want := range good {
		got, err := ParseTime("since", in, kst)
		if err != nil || got != want {
			t.Errorf("ParseTime(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "  ", "abc", "7x", "-1d", "d", "1.5h", "99999999999d", "999999999d", "2026-13-01", "2026-02-30", "7 d", "2026-10-01 09:00", "now", "7D\x1b[31m", "1e3s"} {
		_, err := ParseTime("since", in, kst)
		if err == nil {
			t.Errorf("ParseTime(%q) accepted", in)
			continue
		}
		if !strings.Contains(err.Error(), "--since") || strings.ContainsAny(err.Error(), "\x1b") {
			t.Errorf("ParseTime(%q) error %q", in, err)
		}
	}
}

func TestListMergesSourcesByToolUseID(t *testing.T) {
	home, rw := newHome(t)
	ins(t, rw, row{ts: 1000, kind: "mcp", name: "create_issue", server: "gh", client: "claude-code", sess: "s1", tuid: "t1", source: "hook", ok: p(1), lat: p(20)})
	ins(t, rw, row{ts: 1005, kind: "mcp", name: "create_issue", server: "gh", client: "claude-code", sess: "s1", tuid: "t1", source: "gateway", ok: p(1), lat: p(35)})
	ins(t, rw, row{ts: 2000, kind: "skill", name: "demo", client: "claude-code", sess: "s1", tuid: "t2", source: "hook"})
	// no tool_use_id: never merged
	ins(t, rw, row{ts: 3000, kind: "skill", name: "demo", client: "claude-code", sess: "s1", source: "hook"})
	ins(t, rw, row{ts: 3001, kind: "skill", name: "demo", client: "claude-code", sess: "s1", source: "hook"})
	db := openRO(t, home)

	cs, err := List(ctx, db, Filter{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 4 {
		t.Fatalf("merged list has %d calls: %+v", len(cs), cs)
	}
	var merged *Call
	for i := range cs {
		if cs[i].Name == "create_issue" {
			merged = &cs[i]
		}
	}
	if merged == nil || !reflect.DeepEqual(merged.Sources, []string{"gateway", "hook"}) || merged.TS != 1000 || *merged.LatencyMS != 35 {
		t.Fatalf("merged call: %+v", merged)
	}
	// same count as the schema's own merge view
	var viewN int
	if err := db.QueryRow(`SELECT count(*) FROM v_ledger_calls`).Scan(&viewN); err != nil || viewN != len(cs) {
		t.Errorf("v_ledger_calls=%d (%v), list=%d", viewN, err, len(cs))
	}
	raw, err := List(ctx, db, Filter{Raw: true}, 50)
	if err != nil || len(raw) != 5 {
		t.Fatalf("raw list: %d %v", len(raw), err)
	}
	// --source matches the merged call when it contains the source
	for _, src := range []string{"hook", "gateway"} {
		cs, _ := List(ctx, db, Filter{Kind: "mcp", Source: src}, 50)
		if len(cs) != 1 {
			t.Errorf("source=%s: %d", src, len(cs))
		}
	}
	if cs, _ := List(ctx, db, Filter{Source: "transcript"}, 50); len(cs) != 0 {
		t.Errorf("transcript: %d", len(cs))
	}
	// summaries count the merged call once
	gs, err := Summary(ctx, db, Filter{}, "kind", 50)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, g := range gs {
		counts[g.Key["kind"]] = g.Calls
	}
	if counts["mcp"] != 1 || counts["skill"] != 3 {
		t.Errorf("kind counts %v", counts)
	}
}

func TestFilters(t *testing.T) {
	home, rw := newHome(t)
	ins(t, rw, row{ts: 1000, kind: "skill", name: "alpha", client: "claude-code", project: "/p/a", sess: "s1", tuid: "1", ok: p(1)})
	ins(t, rw, row{ts: 2000, kind: "skill", name: "Alpha-Two", client: "opencode", project: "/p/b", sess: "s2", tuid: "2", ok: p(0), errc: "timeout", source: "gateway"})
	ins(t, rw, row{ts: 3000, kind: "tool", name: "deploy", client: "claude-code", project: "/p/a", sess: "s1", tuid: "3", ok: p(1), source: "gateway"})
	ins(t, rw, row{ts: 4000, kind: "mcp", name: "search", server: "my-github", client: "claude-code", project: "/p/b", sess: "s3", tuid: "4"})
	ins(t, rw, row{ts: 5000, kind: "mcp", name: "search", server: "other", sess: "s3", tuid: "5", ok: p(1)})
	db := openRO(t, home)
	list := func(f Filter, limit int) string {
		cs, err := List(ctx, db, f, limit)
		if err != nil {
			t.Fatal(err)
		}
		return names(cs)
	}
	cases := []struct {
		name  string
		f     Filter
		limit int
		want  string
	}{
		{"all newest first", Filter{}, 50, "search,search,deploy,Alpha-Two,alpha"},
		{"limit", Filter{}, 2, "search,search"},
		{"kind", Filter{Kind: "skill"}, 50, "Alpha-Two,alpha"},
		{"client", Filter{Client: "claude-code"}, 50, "search,deploy,alpha"},
		{"project", Filter{Project: "/p/a"}, 50, "deploy,alpha"},
		{"session", Filter{Session: "s3"}, 50, "search,search"},
		{"name exact", Filter{Name: "alpha"}, 50, "alpha"},
		{"name exact is case sensitive", Filter{Name: "ALPHA"}, 50, ""},
		{"name contains ci", Filter{Name: "ALPHA", NameContains: true}, 50, "Alpha-Two,alpha"},
		{"name contains percent is literal", Filter{Name: "%", NameContains: true}, 50, ""},
		{"server normalized", Filter{Server: "my_github"}, 50, "search"},
		{"server dash", Filter{Server: "my-github", Kind: "mcp"}, 50, "search"},
		{"source gateway", Filter{Source: "gateway"}, 50, "deploy,Alpha-Two"},
		{"ok", Filter{OK: ptr(true)}, 50, "search,deploy,alpha"},
		{"failed", Filter{OK: ptr(false)}, 50, "Alpha-Two"},
		{"combo", Filter{Kind: "skill", Client: "opencode", OK: ptr(false), Source: "gateway", Project: "/p/b", Session: "s2"}, 50, "Alpha-Two"},
		{"combo miss", Filter{Kind: "skill", Client: "opencode", Project: "/p/a"}, 50, ""},
		{"since inclusive", Filter{Since: ptr(int64(3000))}, 50, "search,search,deploy"},
		{"until exclusive", Filter{Until: ptr(int64(3000))}, 50, "Alpha-Two,alpha"},
		{"window", Filter{Since: ptr(int64(2000)), Until: ptr(int64(4000))}, 50, "deploy,Alpha-Two"},
		{"empty window", Filter{Since: ptr(int64(2500)), Until: ptr(int64(2500))}, 50, ""},
	}
	for _, c := range cases {
		if got := list(c.f, c.limit); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	// unknown result matches neither --ok nor --failed
	cs, _ := List(ctx, db, Filter{Kind: "mcp", Client: "claude-code"}, 5)
	if len(cs) != 1 || cs[0].OK != nil {
		t.Errorf("unknown ok: %+v", cs)
	}
}

func TestSQLInjectionAttemptsAreInert(t *testing.T) {
	home, rw := newHome(t)
	ins(t, rw, row{ts: 1, name: "a", client: "claude-code", sess: "s", tuid: "1", project: "/p", found: p(0)})
	db := openRO(t, home)
	evil := []string{"'; DROP TABLE ledger;--", `" OR 1=1 --`, "x' OR '1'='1", "%", "a'); DELETE FROM ledger; --"}
	for _, e := range evil {
		for _, f := range []Filter{{Name: e}, {Name: e, NameContains: true}, {Client: e}, {Project: e}, {Session: e}, {Server: e}, {Kind: e}, {Source: e}} {
			cs, err := List(ctx, db, f, 10)
			if err != nil || len(cs) != 0 {
				t.Errorf("filter %+v: %d rows, err %v", f, len(cs), err)
			}
			if _, err := Summary(ctx, db, f, "name", 10); err != nil {
				t.Errorf("summary %+v: %v", f, err)
			}
			if _, err := Misses(ctx, db, f, 10); err != nil {
				t.Errorf("misses %+v: %v", f, err)
			}
		}
	}
	if _, err := Summary(ctx, db, Filter{}, "name; DROP TABLE ledger", 10); err == nil {
		t.Error("bad group-by accepted")
	}
	var n int
	if err := rw.QueryRow(`SELECT count(*) FROM ledger`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("ledger damaged: %d %v", n, err)
	}
}

func TestSummaryAndTop(t *testing.T) {
	home, rw := newHome(t)
	day := int64(24 * 3600 * 1000)
	base := 20000 * day // a UTC midnight
	ins(t, rw, row{ts: base + 1, kind: "skill", name: "b", client: "claude-code", project: "/p", ok: p(1), lat: p(10)})
	ins(t, rw, row{ts: base + 2, kind: "skill", name: "b", client: "claude-code", project: "/p", ok: p(0), lat: p(31)})
	ins(t, rw, row{ts: base + day + 3, kind: "skill", name: "a", client: "opencode", project: "/q", source: "gateway"})
	ins(t, rw, row{ts: base + day + 4, kind: "tool", name: "z", source: "gateway", ok: p(1)})
	ins(t, rw, row{ts: base + day + 5, kind: "tool", name: "y", source: "gateway", ok: p(1)})
	ins(t, rw, row{ts: base + 6, kind: "mcp", name: "t", server: "my-gh", ok: p(1), lat: p(5)})
	ins(t, rw, row{ts: base + 7, kind: "mcp", name: "t", server: "my_gh", ok: p(1), lat: p(7)})
	db := openRO(t, home)

	byKind, err := Summary(ctx, db, Filter{}, "kind", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(byKind) != 3 || byKind[0].Key["kind"] != "skill" || byKind[0].Calls != 3 || byKind[0].OK != 1 || byKind[0].Failed != 1 || byKind[0].Unknown != 1 {
		t.Fatalf("by kind: %+v", byKind)
	}
	if *byKind[0].AvgMS != 20.5 || *byKind[0].MaxMS != 31 || byKind[0].LastTS != base+day+3 {
		t.Errorf("latency/last: %+v", byKind[0])
	}
	if byKind[0].LastTime != time.UnixMilli(base+day+3).UTC().Format("2006-01-02T15:04:05.000Z") {
		t.Errorf("last time %q", byKind[0].LastTime)
	}
	// ties (tool z/y, 1 call each): ascending by name; mcp names collapse '-' and '_'
	byName, _ := Summary(ctx, db, Filter{}, "name", 50)
	var got []string
	for _, g := range byName {
		got = append(got, fmt.Sprintf("%s/%s/%s=%d", g.Key["kind"], g.Key["mcp_server"], g.Key["name"], g.Calls))
	}
	want := "mcp/my_gh/t=2,skill//b=2,skill//a=1,tool//y=1,tool//z=1"
	// mcp/my_gh (2) and skill b (2) tie on calls: kind ascending puts mcp first
	if strings.Join(got, ",") != want {
		t.Errorf("by name:\n got %v\nwant %s", got, want)
	}
	top, _ := Summary(ctx, db, Filter{Kind: "tool"}, "name", 1)
	if len(top) != 1 || top[0].Key["name"] != "y" {
		t.Errorf("top: %+v", top)
	}
	byDay, _ := Summary(ctx, db, Filter{}, "day", 50)
	if len(byDay) != 2 || byDay[0].Key["day"] != time.UnixMilli(base+day).UTC().Format("2006-01-02") || byDay[0].Calls != 3 || byDay[1].Calls != 4 {
		t.Errorf("by day: %+v", byDay)
	}
	byClient, _ := Summary(ctx, db, Filter{}, "client", 50)
	if len(byClient) != 3 || byClient[0].Key["client"] != "" || byClient[0].Calls != 4 {
		t.Errorf("by client: %+v", byClient)
	}
	byProj, _ := Summary(ctx, db, Filter{}, "project", 50)
	if len(byProj) != 3 {
		t.Errorf("by project: %+v", byProj)
	}
	bySrc, _ := Summary(ctx, db, Filter{}, "source", 50)
	if len(bySrc) != 2 || bySrc[0].Key["source"] != "hook" || bySrc[0].Calls != 4 || bySrc[1].Key["source"] != "gateway" {
		t.Errorf("by source: %+v", bySrc)
	}
	if empty, _ := Summary(ctx, db, Filter{Name: "nothing"}, "kind", 5); empty == nil || len(empty) != 0 {
		t.Errorf("empty summary must be an empty slice: %#v", empty)
	}
}

func seedRegistry(t *testing.T, rw *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO skills(scope,name,description,enabled) VALUES ('global','used-skill','d',1),('global','old-skill','d',1),('global','never-skill','d',1),('global','off-skill','d',0),('global','missed-skill','d',1)`,
		`INSERT INTO tools(scope,name,kind,description,command) VALUES ('global','used-tool','command','d','echo'),('global','idle-tool','command','d','echo')`,
		`INSERT INTO mcp_servers(scope,name,transport,command) VALUES ('global','my-github','stdio','x'),('global','idle_server','stdio','x'),('global','Dash-Off','stdio','x')`,
		`UPDATE mcp_servers SET enabled=0 WHERE name='Dash-Off'`,
	} {
		if _, err := rw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnused(t *testing.T) {
	home, rw := newHome(t)
	seedRegistry(t, rw)
	ins(t, rw, row{ts: 5000, kind: "skill", name: "used-skill", found: p(1)})
	ins(t, rw, row{ts: 1000, kind: "skill", name: "old-skill", found: p(1)})
	ins(t, rw, row{ts: 6000, kind: "skill", name: "missed-skill", found: p(0)}) // call from before it was registered
	ins(t, rw, row{ts: 5000, kind: "tool", name: "used-tool", found: p(1)})
	ins(t, rw, row{ts: 5000, kind: "mcp", name: "search", server: "my_github", found: p(1)}) // client spelling
	ins(t, rw, row{ts: 5000, kind: "mcp", name: "x", server: "Dash_Off", found: p(1)})
	db := openRO(t, home)

	key := func(us []Unused) string {
		var s []string
		for _, u := range us {
			e := ""
			if !u.Enabled {
				e = "(off)"
			}
			s = append(s, u.Kind+"/"+u.Name+e)
		}
		return strings.Join(s, ",")
	}
	all, err := UnusedItems(ctx, db, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := key(all), "skill/missed-skill,skill/never-skill,skill/off-skill(off),tool/idle-tool,mcp/idle_server"; got != want {
		t.Errorf("never called:\n got %s\nwant %s", got, want)
	}
	for _, u := range all {
		if u.Name == "never-skill" && (u.LastTS != nil || u.Last != "") {
			t.Errorf("never-skill last: %+v", u)
		}
	}
	since := int64(2000)
	win, _ := UnusedItems(ctx, db, "skill", &since, nil)
	if got, want := key(win), "skill/missed-skill,skill/never-skill,skill/off-skill(off),skill/old-skill"; got != want {
		t.Errorf("since: got %s want %s", got, want)
	}
	for _, u := range win {
		if u.Name == "old-skill" && (u.LastTS == nil || *u.LastTS != 1000) {
			t.Errorf("old-skill last call must be reported: %+v", u)
		}
	}
	until := int64(5000) // exclusive: the call at 5000 is outside
	early, _ := UnusedItems(ctx, db, "tool", nil, &until)
	if got := key(early); got != "tool/idle-tool,tool/used-tool" {
		t.Errorf("until: %s", got)
	}
	if mcp, _ := UnusedItems(ctx, db, "mcp", nil, nil); key(mcp) != "mcp/idle_server" {
		t.Errorf("mcp: %s", key(mcp))
	}
	// the disabled MCP server was used (via '_' spelling) so it is not listed; make it unused
	mcpLate := int64(9000)
	late, _ := UnusedItems(ctx, db, "mcp", &mcpLate, nil)
	if got := key(late); got != "mcp/Dash-Off(off),mcp/idle_server,mcp/my-github" {
		t.Errorf("mcp window: %s", got)
	}
}

func TestMissesSeparatePerServerAndExcludeUnknown(t *testing.T) {
	home, rw := newHome(t)
	rec := &ledger.SQLite{DB: rw}
	ev := func(kind, name, server, client string, ts int64) ledger.Event {
		e := ledger.Event{TS: time.UnixMilli(ts), Kind: kind, Name: name, MCPServer: server, Client: client, SessionID: "s", Source: "hook"}
		if kind == "mcp" {
			// An unregistered MCP server is no miss by itself (TASK-0019); an explicit
			// found=false keeps this per-server grouping test meaningful.
			e.Found = ledger.Bool(false)
		}
		return e
	}
	for i, e := range []ledger.Event{
		ev("skill", "ghost", "", "claude-code", 1000),
		ev("skill", "ghost", "", "claude-code", 2000),
		ev("skill", "ghost", "", "opencode", 3000),
		ev("skill", "plugin:foo", "", "claude-code", 4000), // client plugin: found NULL, no miss
		ev("skill", "phantom", "", "", 5000),
		ev("mcp", "search", "user_a", "claude-code", 6000),
		ev("mcp", "search", "user_b", "claude-code", 7000),
		ev("mcp", "search", "user-b", "claude-code", 8000),
		ev("tool", "mystery", "", "claude-code", 9000),
	} {
		e.ToolUseID = fmt.Sprintf("t%d", i)
		if _, err := rec.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	db := openRO(t, home)
	ms, err := Misses(ctx, db, Filter{Kind: "skill"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Name != "ghost" || ms[0].Count != 3 || ms[0].FirstTS != 1000 || ms[0].LastTS != 3000 ||
		!reflect.DeepEqual(ms[0].Clients, map[string]int{"claude-code": 2, "opencode": 1}) ||
		ms[1].Name != "phantom" || ms[1].Clients[UnknownClient] != 1 {
		t.Fatalf("skill misses: %+v", ms)
	}
	for _, m := range ms {
		if m.Name == "plugin:foo" {
			t.Error("plugin skill is a false positive")
		}
	}
	all, _ := Misses(ctx, db, Filter{}, 50)
	if len(all) != 5 { // ghost, phantom, user_a/search, user_b/search, tool mystery
		t.Fatalf("all misses: %+v", all)
	}
	mcp, _ := Misses(ctx, db, Filter{Kind: "mcp"}, 50)
	if len(mcp) != 2 || mcp[0].MCPServer != "user_b" || mcp[0].Count != 2 || mcp[1].MCPServer != "user_a" || mcp[1].Count != 1 {
		t.Errorf("mcp misses must be per server ('-' = '_'): %+v", mcp)
	}
	// since migration 0003 the schema view separates MCP servers as well
	var vA, vB int
	_ = db.QueryRow(`SELECT miss_count FROM v_misses WHERE kind='mcp' AND mcp_server='user_a'`).Scan(&vA)
	_ = db.QueryRow(`SELECT miss_count FROM v_misses WHERE kind='mcp' AND mcp_server='user_b'`).Scan(&vB)
	if vA != 1 || vB != 1 { // '-' vs '_' spellings stay distinct rows in the view; the Go query merges them
		t.Errorf("v_misses per server: user_a=%d user_b=%d", vA, vB)
	}
	since := int64(2500)
	late, _ := Misses(ctx, db, Filter{Kind: "skill", Since: &since}, 50)
	if len(late) != 2 || late[0].Name != "ghost" && late[0].Name != "phantom" {
		t.Errorf("since: %+v", late)
	}
	for _, m := range late {
		if m.Name == "ghost" && m.Count != 1 {
			t.Errorf("ghost since: %+v", m)
		}
	}
	if one, _ := Misses(ctx, db, Filter{Kind: "skill"}, 1); len(one) != 1 || one[0].Name != "ghost" {
		t.Errorf("limit: %+v", one)
	}
	if oc, _ := Misses(ctx, db, Filter{Kind: "skill", Client: "opencode"}, 5); len(oc) != 1 || oc[0].Count != 1 {
		t.Errorf("client: %+v", oc)
	}
}

func TestMissesDoNotCountMergedDuplicatesTwice(t *testing.T) {
	home, rw := newHome(t)
	ins(t, rw, row{ts: 1, name: "ghost", client: "claude-code", sess: "s", tuid: "x", source: "hook", found: p(0)})
	ins(t, rw, row{ts: 2, name: "ghost", client: "claude-code", sess: "s", tuid: "x", source: "gateway", found: p(0)})
	db := openRO(t, home)
	ms, _ := Misses(ctx, db, Filter{Kind: "skill"}, 5)
	if len(ms) != 1 || ms[0].Count != 1 {
		t.Errorf("%+v", ms)
	}
}

func TestMissesUnregisteredToolNamesAreSanitizedValues(t *testing.T) {
	home, rw := newHome(t)
	// what the gateway stores for an unknown tool: cleaned and cut at 120 bytes
	long := strings.Repeat("n", 130)
	ins(t, rw, row{ts: 1, kind: "tool", name: `bad\x1b[31m\u202e`, client: "claude-code", found: p(0), errc: "not_found", source: "gateway"})
	ins(t, rw, row{ts: 2, kind: "tool", name: long[:119] + "…", client: "claude-code", found: p(0), errc: "not_found", source: "gateway"})
	db := openRO(t, home)
	ms, _ := Misses(ctx, db, Filter{Kind: "tool"}, 5)
	if len(ms) != 2 {
		t.Fatalf("%+v", ms)
	}
	r := MissesReport(ms, time.UTC)
	if !strings.Contains(strings.Join(r.Notes, "\n"), "미등록 호출 이름") || r.Headers[2] != "UNREGISTERED_NAME" {
		t.Errorf("label: %+v", r)
	}
}

func TestCSVInjectionDefense(t *testing.T) {
	cells := []string{"=1+1", "+1", "-1", "@SUM(A1)", "\tx", "\rx", "ok", "a,b", `say "hi"`, "line1\nline2", "x=1", "'=already"}
	want := []string{"'=1+1", "'+1", "-1", "'@SUM(A1)", "'\tx", "'\rx", "ok", "a,b", `say "hi"`, "line1\nline2", "x=1", "'=already"}
	var buf bytes.Buffer
	if err := writeCSV(&buf, []string{"c"}, func() [][]string {
		var rows [][]string
		for _, c := range cells {
			rows = append(rows, []string{c})
		}
		return rows
	}()); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("not valid CSV: %v\n%s", err, buf.String())
	}
	// encoding/csv folds "\r" inside a quoted field to "\n" when reading; compare loosely for that cell
	if len(recs) != len(cells)+1 {
		t.Fatalf("records %d", len(recs))
	}
	for i, w := range want {
		got := recs[i+1][0]
		if got != w && !(strings.HasPrefix(w, "'\r") && strings.HasPrefix(got, "'\n")) {
			t.Errorf("cell %q -> %q want %q", cells[i], got, w)
		}
	}
	// the seven OWASP starters are all covered, and nothing starting with one is left raw
	for _, s := range []string{"=", "+", "-", "@", "\t", "\r"} {
		if c := CSVCell(s + "cmd|' /C calc'!A0"); c[0] != '\'' {
			t.Errorf("starter %q not defused: %q", s, c)
		}
	}
	// quote escaping per RFC 4180 and a line break inside a cell stays one record
	var b2 bytes.Buffer
	_ = writeCSV(&b2, []string{"a", "b"}, [][]string{{`he said "x"`, "l1\nl2"}})
	if got := b2.String(); got != "a,b\n\"he said \"\"x\"\"\",\"l1\nl2\"\n" {
		t.Errorf("csv bytes %q", got)
	}
	// control characters other than tab/CR/LF never reach a CSV raw
	c := CSVCell("a\x1b[31m\u202eb\u009bc\x7f")
	for _, r := range c {
		if textsafe.NeedsEscape(r) {
			t.Errorf("raw %U in %q", r, c)
		}
	}
}

func TestCSVReportOfHostileLedgerValues(t *testing.T) {
	calls := []Call{{ID: 1, TS: 1000, Time: msToTime(1000), Kind: "skill", Name: "=HYPERLINK(\"http://x\")", Project: "@/p", SessionID: "-s", Sources: []string{"hook"}}}
	var buf bytes.Buffer
	if err := ListReport(calls, 50, false, time.UTC).Write(&buf, FormatCSV); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil || len(recs) != 2 {
		t.Fatalf("%v %v", err, recs)
	}
	h := map[string]string{}
	for i, k := range recs[0] {
		h[k] = recs[1][i]
	}
	if h["name"] != `'=HYPERLINK("http://x")` || h["project"] != "'@/p" || h["session_id"] != "'-s" {
		t.Errorf("%v", h)
	}
}

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.csv")
	if err := WriteFile(out, []byte("one"), false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	if err := WriteFile(out, []byte("two"), false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("overwrite without force: %v", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "one" {
		t.Errorf("content changed: %q", b)
	}
	// force replaces a regular file; permissions are 0600 even if the old file was looser
	if err := os.Chmod(out, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(out, []byte("three"), true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "three" {
		t.Errorf("force: %q", b)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Errorf("mode after force %v", st.Mode())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("temp file left behind: %v", ents)
	}

	// symlinks are refused with and without --force, existing or dangling; target untouched
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.csv")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling.csv")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{link, dangling} {
		for _, force := range []bool{false, true} {
			if err := WriteFile(l, []byte("x"), force); err == nil || !strings.Contains(err.Error(), "심볼릭") {
				t.Errorf("symlink %s force=%v: %v", l, force, err)
			}
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("symlink target modified: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "nowhere")); err == nil {
		t.Error("dangling target created")
	}
	// directory, missing parent, protected file
	if err := WriteFile(dir, []byte("x"), true); err == nil {
		t.Error("directory accepted")
	}
	if err := WriteFile(filepath.Join(dir, "nope", "x"), []byte("x"), false); err == nil {
		t.Error("missing parent accepted")
	}
	if err := WriteFile(target, []byte("x"), true, target); err == nil || !strings.Contains(err.Error(), "저장소") {
		t.Errorf("protected: %v", err)
	}
	if err := WriteFile("", nil, false); err == nil {
		t.Error("empty path accepted")
	}
}

func TestTerminalEscapeInjectionIsNeutralized(t *testing.T) {
	hostile := "evil\x1b[2J\x1b]0;pwned\x07\u202e\u200b\u009b31m\nFAKE-ROW\r\x7f"
	calls := []Call{{ID: 1, TS: 1000, Time: msToTime(1000), Kind: "tool", Name: hostile, MCPServer: "srv" + hostile, Client: "claude-code",
		Project: "/p/" + hostile, SessionID: "s" + hostile, Sources: []string{"hook"}, ErrorClass: "x"}}
	gs := []Group{{Key: map[string]string{"kind": "tool", "mcp_server": hostile, "name": hostile}, Calls: 1, LastTS: 1000, LastTime: msToTime(1000)}}
	us := []Unused{{Kind: "skill", Name: hostile, Scope: "global", Enabled: true}}
	ms := []Miss{{Kind: "tool", Name: hostile, Count: 1, FirstTS: 1, LastTS: 2, Clients: map[string]int{hostile: 1}}}
	reports := map[string]Report{
		"list": ListReport(calls, 50, false, time.UTC), "summary": SummaryReport(gs, "name", time.UTC),
		"unused": UnusedReport(us, time.UTC), "misses": MissesReport(ms, time.UTC),
	}
	for n, r := range reports {
		var tbl bytes.Buffer
		if err := r.Write(&tbl, FormatTable); err != nil {
			t.Fatal(err)
		}
		for _, ru := range tbl.String() {
			if ru != '\n' && textsafe.NeedsEscape(ru) {
				t.Errorf("%s table: raw %U in %q", n, ru, tbl.String())
			}
		}
		// one header + one row + notes: the injected newline did not forge a row
		for _, line := range strings.Split(tbl.String(), "\n") {
			if strings.HasPrefix(line, "FAKE-ROW") {
				t.Errorf("%s table: forged line %q", n, line)
			}
		}
		if !strings.Contains(tbl.String(), `\x1b`) || !strings.Contains(tbl.String(), `\u202e`) {
			t.Errorf("%s table does not show visible escapes: %q", n, tbl.String())
		}
		var js bytes.Buffer
		if err := r.Write(&js, FormatJSON); err != nil {
			t.Fatal(err)
		}
		for _, ru := range js.String() {
			if ru != '\n' && textsafe.NeedsEscape(ru) {
				t.Errorf("%s json: raw %U", n, ru)
			}
		}
		var back any
		if err := json.Unmarshal(js.Bytes(), &back); err != nil {
			t.Fatalf("%s json invalid: %v", n, err)
		}
		var csvb bytes.Buffer
		if err := r.Write(&csvb, FormatCSV); err != nil {
			t.Fatal(err)
		}
		for _, ru := range csvb.String() {
			if ru != '\n' && ru != '\r' && ru != '\t' && textsafe.NeedsEscape(ru) {
				t.Errorf("%s csv: raw %U", n, ru)
			}
		}
	}
	// JSON decodes back to the original value (escapes are lossless) — apart from masking
	b, _ := SafeJSON(map[string]string{"a": "x\u202e\x7f\u009b\U000e0100y\U0001f600"})
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil || m["a"] != "x\u202e\x7f\u009b\U000e0100y\U0001f600" {
		t.Errorf("SafeJSON round trip: %q %v", m, err)
	}
	if strings.ContainsRune(string(b), '\u202e') || strings.ContainsRune(string(b), '\u009b') || strings.ContainsRune(string(b), 0x7f) {
		t.Errorf("raw bidi in %s", b)
	}
	if !strings.Contains(string(b), "\U0001f600") { // ordinary emoji stays readable
		t.Errorf("emoji escaped: %s", b)
	}
}

func TestMaskingOnOutput(t *testing.T) {
	calls := []Call{{ID: 1, TS: 1, Time: msToTime(1), Kind: "skill", Name: "token=sk-FAKEFAKEFAKEFAKE9876", Project: "/home/u/proj", Sources: []string{"hook"}}}
	var tbl, js bytes.Buffer
	r := ListReport(calls, 50, false, time.UTC)
	_ = r.Write(&tbl, FormatTable)
	_ = r.Write(&js, FormatJSON)
	for _, out := range []string{tbl.String(), js.String()} {
		if strings.Contains(out, "FAKEFAKE") || !strings.Contains(out, "***") || !strings.Contains(out, "/home/u/proj") {
			t.Errorf("masking: %s", out)
		}
	}
	// ordinary names and paths pass unchanged
	for _, s := range []string{"code-review", "plugin:skill", "create_issue", "/Users/a/IdeaProjects/toyproject/forge", "/var/folders/gr/q4n6pgs90gx_jny4zh7p58dw0000gn/T/TestX3565954151/001", "gateway_name_ab12cd34ef56ab78cd90ef12ab34cd56", "mcp__server__tool", "forge_deploy-check"} {
		if mk(s) != s {
			t.Errorf("mask altered %q -> %q", s, mk(s))
		}
	}
}

func TestOpenReadOnlyAndFailureModes(t *testing.T) {
	// missing database: nothing is created
	home := t.TempDir()
	if _, err := Open(ctx, home); err == nil || !strings.Contains(err.Error(), "forge init") {
		t.Fatalf("missing db: %v", err)
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Errorf("Open created %v", ents)
	}
	missing := filepath.Join(home, "nested", "deeper")
	if _, err := Open(ctx, missing); err == nil {
		t.Fatal("missing home accepted")
	}
	if _, err := os.Stat(filepath.Join(home, "nested")); err == nil {
		t.Error("home created")
	}

	// corrupt database
	bad := t.TempDir()
	if err := os.WriteFile(store.DBPath(bad), []byte(strings.Repeat("this is not sqlite ", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, bad); err == nil || !strings.Contains(err.Error(), "손상") {
		t.Errorf("corrupt: %v", err)
	}
	// valid SQLite file that is not a Forge database
	foreign := t.TempDir()
	fdb, err := sql.Open("sqlite", "file:"+store.DBPath(foreign))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fdb.Exec(`CREATE TABLE x(a)`); err != nil {
		t.Fatal(err)
	}
	_ = fdb.Close()
	if _, err := Open(ctx, foreign); err == nil || !strings.Contains(err.Error(), "테이블이 없습니다") {
		t.Errorf("foreign: %v", err)
	}

	// the handle cannot write
	h, rw := newHome(t)
	ins(t, rw, row{ts: 1, name: "a"})
	db := openRO(t, h)
	for _, q := range []string{`INSERT INTO ledger(ts,kind,name,source) VALUES (1,'skill','x','hook')`, `DELETE FROM ledger`, `DROP TABLE ledger`, `CREATE TABLE z(a)`} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("write succeeded: %s", q)
		}
	}
	var n int
	_ = rw.QueryRow(`SELECT count(*) FROM ledger`).Scan(&n)
	if n != 1 {
		t.Errorf("ledger rows %d", n)
	}
}

func TestEmptyDatabase(t *testing.T) {
	home, _ := newHome(t)
	db := openRO(t, home)
	cs, err := List(ctx, db, Filter{}, 10)
	if err != nil || cs == nil || len(cs) != 0 {
		t.Fatalf("%#v %v", cs, err)
	}
	var buf bytes.Buffer
	_ = ListReport(cs, 10, false, time.UTC).Write(&buf, FormatJSON)
	var v struct {
		Calls []any `json:"calls"`
		Count int   `json:"count"`
	}
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil || v.Calls == nil || v.Count != 0 {
		t.Errorf("empty json: %s", buf.String())
	}
	buf.Reset()
	_ = ListReport(cs, 10, false, time.UTC).Write(&buf, FormatCSV)
	if strings.Count(buf.String(), "\n") != 1 {
		t.Errorf("empty csv: %q", buf.String())
	}
	for _, f := range []func() error{
		func() error { _, e := Summary(ctx, db, Filter{}, "kind", 5); return e },
		func() error { _, e := UnusedItems(ctx, db, "", nil, nil); return e },
		func() error { _, e := Misses(ctx, db, Filter{}, 5); return e },
	} {
		if err := f(); err != nil {
			t.Error(err)
		}
	}
}

func TestTimezoneDisplayOnlyAffectsTables(t *testing.T) {
	ms := time.Date(2026, 10, 21, 0, 30, 0, 0, time.UTC).UnixMilli()
	calls := []Call{{ID: 1, TS: ms, Time: msToTime(ms), Kind: "skill", Name: "a", Sources: []string{"hook"}}}
	kst, la := time.FixedZone("KST", 9*3600), time.FixedZone("PDT", -7*3600)
	render := func(loc *time.Location, f string) string {
		var b bytes.Buffer
		_ = ListReport(calls, 50, false, loc).Write(&b, f)
		return b.String()
	}
	if !strings.Contains(render(kst, FormatTable), "2026-10-21 09:30:00 +0900") || !strings.Contains(render(la, FormatTable), "2026-10-20 17:30:00 -0700") {
		t.Errorf("table:\n%s\n%s", render(kst, FormatTable), render(la, FormatTable))
	}
	if render(kst, FormatJSON) != render(la, FormatJSON) || render(kst, FormatCSV) != render(la, FormatCSV) {
		t.Error("JSON/CSV depend on the time zone")
	}
	if j := render(kst, FormatJSON); !strings.Contains(j, `"time": "2026-10-21T00:30:00.000Z"`) || !strings.Contains(j, fmt.Sprintf(`"ts_ms": %d`, ms)) {
		t.Errorf("json time: %s", j)
	}
}

func TestJSONFieldNamesAreFixed(t *testing.T) {
	keys := func(v any) []string {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}
	eq := func(name string, got []string, want string) {
		if g := strings.Join(got, " "); g != want {
			t.Errorf("%s fields:\n got %s\nwant %s", name, g, want)
		}
	}
	eq("call", keys(Call{}), "client error_class found id invocation kind latency_ms mcp_server name ok project session_id sources time ts_ms")
	eq("group", keys(Group{}), "avg_latency_ms calls failed key last_time last_ts_ms max_latency_ms ok unknown")
	eq("unused", keys(Unused{}), "enabled kind last_time last_ts_ms name scope")
	eq("miss", keys(Miss{}), "clients count first_time first_ts_ms kind last_time last_ts_ms mcp_server name")
	var b bytes.Buffer
	_ = ListReport(nil, 7, true, time.UTC).Write(&b, FormatJSON)
	var top map[string]any
	_ = json.Unmarshal(b.Bytes(), &top)
	if top["limit"] != float64(7) || top["raw"] != true || top["count"] != float64(0) {
		t.Errorf("list envelope: %v", top)
	}
	b.Reset()
	_ = SummaryReport(nil, "kind", time.UTC).Write(&b, FormatJSON)
	_ = json.Unmarshal(b.Bytes(), &top)
	if top["group_by"] != "kind" || top["groups"] == nil {
		t.Errorf("summary envelope: %v", top)
	}
}

func TestRawListShowsToolUseID(t *testing.T) {
	home, rw := newHome(t)
	ins(t, rw, row{ts: 1, name: "a", client: "claude-code", sess: "s", tuid: "tool-1"})
	db := openRO(t, home)
	cs, _ := List(ctx, db, Filter{Raw: true}, 5)
	if len(cs) != 1 || cs[0].ToolUseID != "tool-1" {
		t.Fatalf("%+v", cs)
	}
	var b bytes.Buffer
	_ = ListReport(cs, 5, true, time.UTC).Write(&b, FormatTable)
	if !strings.Contains(b.String(), "TOOL_USE_ID") || !strings.Contains(b.String(), "tool-1") {
		t.Error(b.String())
	}
	m, _ := List(ctx, db, Filter{}, 5)
	if m[0].ToolUseID != "" {
		t.Error("merged call exposes tool_use_id")
	}
}

func TestQueryPlansUseIndexes(t *testing.T) {
	home, _ := newHome(t)
	db := openRO(t, home)
	plan := func(q string, args ...any) string {
		rows, err := db.Query("EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var s []string
		for rows.Next() {
			var a, b, c int
			var d string
			_ = rows.Scan(&a, &b, &c, &d)
			s = append(s, d)
		}
		return strings.Join(s, " | ")
	}
	cte, args := mergedCTE(Filter{Kind: "skill", Name: "x"})
	if pl := plan("WITH "+cte+" SELECT * FROM calls", args...); !strings.Contains(pl, "ix_ledger_kind_name") {
		t.Errorf("kind+name filter: %s", pl)
	}
	cte, args = mergedCTE(Filter{Client: "claude-code", Session: "s"})
	if pl := plan("WITH "+cte+" SELECT * FROM calls", args...); !strings.Contains(pl, "ix_ledger_session") {
		t.Errorf("client+session filter: %s", pl)
	}
	if pl := plan(`SELECT DISTINCT name FROM ledger WHERE kind='skill' AND found IS NOT 0 AND ts >= ? AND ts < ?`, 0, 1); !strings.Contains(pl, "ix_ledger_kind_name") {
		t.Errorf("unused used-set: %s", pl)
	}
	if pl := plan(`SELECT kind FROM ledger WHERE found = 0 AND kind = ? GROUP BY kind, name`, "skill"); !strings.Contains(pl, "ix_ledger_miss") {
		t.Errorf("miss: %s", pl)
	}
	t.Logf("list plan (no filter): %s", func() string {
		c, a := mergedCTE(Filter{})
		return plan("WITH "+c+" SELECT * FROM calls ORDER BY ts DESC LIMIT 5", a...)
	}())
}

func TestLargeLedgerPerformance(t *testing.T) {
	if testing.Short() || os.Getenv("FORGE_LARGE_TESTS") != "1" {
		t.Skip("large synthetic ledger (1M rows, ~40 s): set FORGE_LARGE_TESTS=1 to run")
	}
	if raceEnabled {
		t.Skip("1M-row timing is meaningless (and very slow) under the race detector")
	}
	home, rw := newHome(t)
	seedRegistry(t, rw)
	const n = 1_000_000
	start := time.Now()
	if _, err := rw.Exec(`
WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i < ?)
INSERT INTO ledger(ts, kind, name, mcp_server, client, project, session_id, tool_use_id, source, ok, latency_ms, found)
SELECT 1700000000000 + i*60000,
  CASE i%3 WHEN 0 THEN 'skill' WHEN 1 THEN 'tool' ELSE 'mcp' END,
  'name-' || (i % 500),
  CASE i%3 WHEN 2 THEN 'srv-' || (i % 20) END,
  CASE i%2 WHEN 0 THEN 'claude-code' ELSE 'opencode' END,
  '/p/' || (i % 40), 'sess-' || (i % 5000), 'tu-' || (i/2),
  CASE i%2 WHEN 0 THEN 'hook' ELSE 'gateway' END,
  i%7=0, i%900, CASE WHEN i%1000=0 THEN 0 ELSE 1 END
FROM s`, n); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d rows in %v", n, time.Since(start))
	db := openRO(t, home)
	since := int64(1700000000000 + int64(n-5000)*60000)
	steps := []struct {
		name string
		fn   func() (int, error)
	}{
		{"list default", func() (int, error) { c, e := List(ctx, db, Filter{}, 50); return len(c), e }},
		{"list --since (last 5000)", func() (int, error) { c, e := List(ctx, db, Filter{Since: &since}, 50); return len(c), e }},
		{"list --kind skill --name", func() (int, error) {
			c, e := List(ctx, db, Filter{Kind: "skill", Name: "name-3"}, 50)
			return len(c), e
		}},
		{"summary kind", func() (int, error) { c, e := Summary(ctx, db, Filter{}, "kind", 50); return len(c), e }},
		{"summary name", func() (int, error) { c, e := Summary(ctx, db, Filter{}, "name", 50); return len(c), e }},
		{"summary day", func() (int, error) { c, e := Summary(ctx, db, Filter{}, "day", 50); return len(c), e }},
		{"top 10", func() (int, error) { c, e := Summary(ctx, db, Filter{}, "name", 10); return len(c), e }},
		{"top 10 --since", func() (int, error) { c, e := Summary(ctx, db, Filter{Since: &since}, "name", 10); return len(c), e }},
		{"unused", func() (int, error) { c, e := UnusedItems(ctx, db, "", nil, nil); return len(c), e }},
		{"unused --since", func() (int, error) { c, e := UnusedItems(ctx, db, "", &since, nil); return len(c), e }},
		{"misses", func() (int, error) { c, e := Misses(ctx, db, Filter{}, 50); return len(c), e }},
	}
	// Correctness is checked per step (no error); the time bound is loose (3x the
	// old 10s: a loaded host made the aggregation 3-4x slower) unless
	// FORGE_PERF_TESTS=1 asks for the strict bound on an idle machine.
	limit := 30 * time.Second
	if os.Getenv("FORGE_PERF_TESTS") == "1" {
		limit = 10 * time.Second
	}
	for _, s := range steps {
		t0 := time.Now()
		got, err := s.fn()
		d := time.Since(t0)
		t.Logf("%-28s %6d rows  %v", s.name, got, d.Round(time.Millisecond))
		if err != nil {
			t.Errorf("%s: %v", s.name, err)
		}
		if d > limit {
			t.Errorf("%s took %v (limit %v)", s.name, d, limit)
		}
	}
}

func TestSanitizedDisplayCutsLongValues(t *testing.T) {
	long := strings.Repeat("あ", 200)
	got := textsafe.Display(long, maxCell)
	if len(got) > maxCell || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("%d %q", len(got), got)
	}
}

func TestStagedListEqualsFullQueryAndSinceWindowKeepsMergeExact(t *testing.T) {
	home, rw := newHome(t)
	day := int64(24 * 3600 * 1000)
	base := int64(1_700_000_000_000)
	// 300 calls spread over 600 days, each recorded by hook and (5s later) gateway
	for i := 0; i < 300; i++ {
		ts := base + int64(i)*2*day
		ins(t, rw, row{ts: ts, name: fmt.Sprintf("n%03d", i), client: "claude-code", sess: "s", tuid: fmt.Sprintf("t%d", i), source: "hook"})
		ins(t, rw, row{ts: ts + 5000, name: fmt.Sprintf("n%03d", i), client: "claude-code", sess: "s", tuid: fmt.Sprintf("t%d", i), source: "gateway"})
	}
	db := openRO(t, home)
	for _, limit := range []int{1, 5, 50, 299, 300, 400} {
		got, err := List(ctx, db, Filter{}, limit)
		if err != nil {
			t.Fatal(err)
		}
		want, err := listWindow(ctx, db, Filter{}, limit)
		if err != nil {
			t.Fatal(err)
		}
		if names(got) != names(want) || len(got) != min(limit, 300) {
			t.Errorf("limit %d: staged %d calls != full %d", limit, len(got), len(want))
		}
	}
	// since falls between the two rows of one call: the call began before since, so it is excluded
	since := base + 10*2*day + 2000
	cs, _ := List(ctx, db, Filter{Since: &since}, 1000)
	if len(cs) != 289 || cs[len(cs)-1].Name != "n011" {
		t.Errorf("since between siblings: %d calls, oldest %q", len(cs), cs[len(cs)-1].Name)
	}
	until := base + 10*2*day + 2000 // call n010 starts before until, its gateway row is after
	cs, _ = List(ctx, db, Filter{Until: &until}, 1000)
	if len(cs) != 11 || len(cs[0].Sources) != 2 {
		t.Errorf("until between siblings: %d calls, sources %v", len(cs), cs[0].Sources)
	}
}

func TestMergeTreatsServerSpellingsAsOne(t *testing.T) {
	home, rw := newHome(t)
	ins(t, rw, row{ts: 1, kind: "mcp", name: "create_issue", server: "my_github", client: "claude-code", sess: "s", tuid: "g1", source: "hook"})
	ins(t, rw, row{ts: 2, kind: "mcp", name: "create_issue", server: "my-github", client: "claude-code", sess: "s", tuid: "g1", source: "gateway", ok: p(1)})
	db := openRO(t, home)
	cs, _ := List(ctx, db, Filter{}, 5)
	if len(cs) != 1 || len(cs[0].Sources) != 2 {
		t.Errorf("%+v", cs)
	}
	gs, _ := Summary(ctx, db, Filter{}, "name", 5)
	if len(gs) != 1 || gs[0].Calls != 1 || gs[0].Key["mcp_server"] != "my_github" {
		t.Errorf("%+v", gs)
	}
}
