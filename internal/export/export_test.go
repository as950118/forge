package export

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/as950118/forge/internal/history"
	"github.com/as950118/forge/internal/ledger"
	"github.com/as950118/forge/internal/store"
)

// Fake values that must never reach a snapshot. None of them is a real secret.
var forbidden = []string{
	"FAKE-SECRET-TOKEN-0123", "FAKE_CMDLINE_rm_rf", "/home/fake-user/secret-project", "FAKE-SESSION-ID-777",
	"FAKE-ERRCLASS-MSG", "FAKE-AGENT-ID", "FAKE-TOOLUSE-ID", "FAKE-CLIENT-VERSION-9", "FAKE-HEADER-Authorization",
	"FAKE-INPUT-BODY", "FAKE-OUTPUT-BODY", "FAKE-ENV-VALUE", "claude-code",
}

const bsl = "\\"

func b(v bool) *bool { return &v }

func seed(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forge.db")
	conn, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	for _, q := range []string{
		`INSERT INTO skills(scope,name,description) VALUES ('global','review','d')`,
		`INSERT INTO tools(scope,name,kind,description,command) VALUES ('global','deploy','command','d','FAKE_CMDLINE_rm_rf --token=FAKE-SECRET-TOKEN-0123')`,
		`INSERT INTO mcp_servers(scope,name,transport,command) VALUES ('global','my-github','stdio','FAKE_CMDLINE_rm_rf FAKE-ENV-VALUE')`,
	} {
		if _, err := conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	rec := &ledger.SQLite{DB: conn}
	day1 := time.Date(2026, 10, 1, 23, 30, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 2, 0, 30, 0, 0, time.UTC)
	evs := []ledger.Event{
		{TS: day1, Kind: "skill", Name: "review", OK: b(true), ToolUseID: "FAKE-TOOLUSE-ID-1", Source: "hook"},
		{TS: day2, Kind: "skill", Name: "review", OK: b(false), ErrorClass: "exec", ToolUseID: "FAKE-TOOLUSE-ID-2", Source: "hook"},
		{TS: day2, Kind: "skill", Name: "review", OK: b(true), ToolUseID: "FAKE-TOOLUSE-ID-3", Source: "hook"},
		{TS: day2, Kind: "tool", Name: "deploy", ToolUseID: "FAKE-TOOLUSE-ID-4", Source: "gateway"}, // result unknown
		{TS: day2, Kind: "mcp", Name: "search", MCPServer: "my_github", OK: b(true), ToolUseID: "FAKE-TOOLUSE-ID-5", Source: "hook"},
	}
	for _, e := range evs {
		e.Client = "claude-code"
		e.ClientVersion = "FAKE-CLIENT-VERSION-9"
		e.Project = "/home/fake-user/secret-project"
		e.SessionID = "FAKE-SESSION-ID-777"
		e.AgentID = "FAKE-AGENT-ID"
		if _, err := rec.Record(context.Background(), e); err != nil {
			t.Fatalf("record %+v: %v", e, err)
		}
	}
	if _, err := conn.Exec(`UPDATE ledger SET error_class = 'FAKE-ERRCLASS-MSG' WHERE error_class = 'exec'`); err != nil {
		t.Fatal(err)
	}
	// A payload row (only exists with settings.record_payload=1) and a hostile name.
	if _, err := conn.Exec(`INSERT INTO ledger_payloads(ledger_id,input_json,output_json,redaction_version) SELECT id,'{"cmd":"FAKE-INPUT-BODY"}','{"o":"FAKE-OUTPUT-BODY"}',1 FROM ledger LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO ledger(ts,kind,name,source) VALUES (?, 'skill', ?, 'hook')`, day2.UnixMilli(), "evil"+string(rune(0x202e))+"name"+string(rune(7))+string(rune(27))+"[31m"); err != nil {
		t.Fatal(err)
	}
	ro, err := history.Open(context.Background(), filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	return ro
}

func TestBuildContentAndUTCDays(t *testing.T) {
	db := seed(t)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.FixedZone("KST", 9*3600))
	s, err := Build(context.Background(), db, now, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if s.GeneratedAt != "2026-11-03T00:00:00Z" || s.ForgeVersion != "v1.2.3" || s.DayBasis != "UTC" || s.SchemaVersion != 1 {
		t.Fatalf("meta: %+v", s)
	}
	find := func(kind, server, name string) *Item {
		for i := range s.Items {
			if s.Items[i].Kind == kind && s.Items[i].Server == server && s.Items[i].Name == name {
				return &s.Items[i]
			}
		}
		return nil
	}
	rv := find("skill", "", "review")
	if rv == nil || rv.Calls != 3 || rv.OK != 2 || rv.Failed != 1 || rv.SuccessRate == nil || *rv.SuccessRate != 0.6667 || rv.LastCall != "2026-10-02T00:30:00Z" {
		t.Fatalf("review: %+v", rv)
	}
	if d := find("tool", "", "deploy"); d == nil || d.Calls != 1 || d.Unknown != 1 || d.SuccessRate != nil {
		t.Fatalf("deploy: %+v", d)
	}
	if m := find("mcp", "my_github", "search"); m == nil || m.Calls != 1 || m.OK != 1 {
		t.Fatalf("mcp: %+v", m)
	}
	// 23:30Z and 00:30Z fall on different UTC days whatever the local zone is.
	days := map[string]int64{}
	for _, d := range s.Daily {
		if d.Kind == "skill" {
			days[d.Day] = d.Calls
		}
	}
	if days["2026-10-01"] != 1 || days["2026-10-02"] != 3 {
		t.Fatalf("daily: %+v", s.Daily)
	}
	// Hostile name: escaped, no raw control or bidi character left.
	var hostile *Item
	for i := range s.Items {
		if strings.HasPrefix(s.Items[i].Name, "evil") {
			hostile = &s.Items[i]
		}
	}
	if hostile == nil || hostile.Name != "evil"+bsl+"u202Ename"+bsl+"u0007"+bsl+"u001B[31m" {
		t.Fatalf("hostile name: %+v", hostile)
	}
}

func TestNoForbiddenStringsInJSONOrPostgresArgs(t *testing.T) {
	db := seed(t)
	s, err := Build(context.Background(), db, time.Now(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	stmts, err := Plan(s)
	if err != nil {
		t.Fatal(err)
	}
	var pg strings.Builder
	for _, st := range stmts {
		pg.WriteString(st.SQL)
		for _, a := range st.Args {
			fmt.Fprintf(&pg, " %v|%#v", a, a)
		}
	}
	for _, f := range forbidden {
		if strings.Contains(string(raw), f) {
			t.Errorf("JSON contains %q", f)
		}
		if strings.Contains(pg.String(), f) {
			t.Errorf("postgres statements contain %q", f)
		}
	}
	// The allow list is the field list: nothing else may appear as a JSON key.
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	for k := range generic {
		switch k {
		case "schema_version", "generated_at", "forge_version", "day_basis", "items", "daily":
		default:
			t.Errorf("unexpected top-level key %q", k)
		}
	}
	for _, it := range generic["items"].([]any) {
		for k := range it.(map[string]any) {
			switch k {
			case "kind", "server", "name", "calls", "ok", "failed", "unknown", "success_rate", "last_call":
			default:
				t.Errorf("unexpected item key %q", k)
			}
		}
	}
}

func TestSQLUsesOnlyConstantsAndBindings(t *testing.T) {
	s := Snapshot{SchemaVersion: 1, GeneratedAt: "2026-11-03T00:00:00Z", ForgeVersion: "x", DayBasis: "UTC",
		Items: []Item{{Kind: "skill", Name: "'; DROP TABLE forge.items; --", LastCall: "2026-11-03T00:00:00Z"}},
		Daily: []DayRow{{Day: "2026-11-03", Kind: "skill"}}}
	stmts, err := Plan(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range stmts {
		if strings.Contains(st.SQL, "DROP TABLE forge.items; --") {
			t.Fatalf("value reached SQL text: %s", st.SQL)
		}
	}
	// Order: DDL and DELETEs first, then inserts.
	if !strings.HasPrefix(stmts[0].SQL, "CREATE SCHEMA IF NOT EXISTS forge") {
		t.Fatalf("first statement: %s", stmts[0].SQL)
	}
}

func TestSafeNameAndMerge(t *testing.T) {
	bs := "\\"
	r := func(x rune) string { return string(x) }
	cases := map[string]string{
		"plain-name_1":                        "plain-name_1",
		"hangul-ok-\uD55C\uAE00":              "hangul-ok-\uD55C\uAE00",
		"a" + r(0x200b) + "b":                 "a" + bs + "u200Bb",
		"a" + r(10) + "b":                     "a" + bs + "u000Ab",
		"x" + r(0xE0100) + "y":                "x" + bs + "U000E0100y",
		"bad" + string([]byte{0xff}) + "byte": "bad" + bs + "xFFbyte",
		"a" + r(0xa0) + "b":                   "a" + bs + "u00A0b",
		strings.Repeat("n", 250):              strings.Repeat("n", 200) + "...",
	}
	for in, want := range cases {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	rate := 1.0
	merged := mergeItems([]Item{
		{Kind: "skill", Name: "a", Calls: 1, OK: 1, SuccessRate: &rate, LastCall: "2026-01-01T00:00:00Z"},
		{Kind: "skill", Name: "a", Calls: 2, Failed: 1, Unknown: 1, LastCall: "2026-02-01T00:00:00Z"},
	})
	if len(merged) != 1 || merged[0].Calls != 3 || merged[0].SuccessRate == nil || *merged[0].SuccessRate != 0.5 || merged[0].LastCall != "2026-02-01T00:00:00Z" {
		t.Fatalf("merge: %+v", merged)
	}
}

func TestParseTarget(t *testing.T) {
	const pw = "FAKE-PW-s3cr3t"
	good := "postgresql://postgres.ref:" + pw + "@aws-0-x.pooler.supabase.com:5432/postgres"
	tg, err := ParseTarget(good, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tg.Masked, pw) || !strings.Contains(tg.Masked, "xxxxx") {
		t.Fatalf("masked: %s", tg.Masked)
	}
	if tg.Config.Port != 5432 || tg.Config.TLSConfig == nil {
		t.Fatalf("expected TLS on port 5432: %+v", tg.Config)
	}
	bad := map[string]string{
		"6543 authority":   "postgresql://u:" + pw + "@h.example.com:6543/postgres",
		"6543 query":       "postgresql://u:" + pw + "@h.example.com/postgres?port=6543",
		"sslmode disable":  good + "?sslmode=disable",
		"sslmode prefer":   good + "?sslmode=prefer",
		"two hosts":        "postgresql://u:" + pw + "@a.example.com,b.example.com/postgres",
		"host override":    good + "?host=evil.example.com",
		"not url":          "host=h password=" + pw,
		"http scheme":      "http://u:" + pw + "@h.example.com/x",
		"empty":            "",
		"bad port":         "postgresql://u:" + pw + "@h.example.com:notaport/postgres",
		"sslrootcert path": good + "?sslrootcert=/etc/passwd",
	}
	for name, u := range bad {
		_, err := ParseTarget(u, "")
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), pw) {
			t.Errorf("%s: error leaks the password: %v", name, err)
		}
	}
	if _, err := ParseTarget("postgresql://u:"+pw+"@h.example.com:6543/postgres", ""); err != ErrTransactionPooler {
		t.Errorf("6543 error = %v", err)
	}
	if _, err := ParseTarget(good, "not a pem"); err == nil || strings.Contains(err.Error(), pw) {
		t.Errorf("bad CA: %v", err)
	}
	if _, err := ParseTarget("postgresql://u:"+pw+"@127.0.0.1:5432/db?sslmode=disable", ""); err != nil {
		t.Errorf("loopback sslmode=disable should be allowed: %v", err)
	}
	if got := MaskURL(good + "?password=" + pw); strings.Contains(got, pw) {
		t.Errorf("MaskURL: %s", got)
	}
	if got := MaskURL("::bad::" + pw); strings.Contains(got, pw) {
		t.Errorf("MaskURL of garbage: %s", got)
	}
}

func TestWriteConnectErrorIsRedacted(t *testing.T) {
	const pw = "FAKE-PW-s3cr3t"
	tg, err := ParseTarget("postgresql://u:"+pw+"@127.0.0.1:1/db", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = Write(ctx, tg, Snapshot{SchemaVersion: 1, GeneratedAt: "2026-11-03T00:00:00Z"})
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), pw) {
		t.Fatalf("error leaks the password: %v", err)
	}
}
