package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heonjinjeong/forge/internal/store"
	"github.com/heonjinjeong/forge/internal/tool"
)

// Fake credentials only.
const (
	fakeGH  = "ghp_FAKEFAKEFAKEFAKE1234"
	fakeSK  = "sk-FAKEFAKEFAKEFAKE9876"
	fakePwd = "hunter2-FAKE"
)

func newHome(t *testing.T) (home string, conn *sql.DB) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("FORGE_HOME", home)
	if err := store.EnsureLayout(home); err != nil {
		t.Fatal(err)
	}
	conn, err := store.Open(context.Background(), store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return home, conn
}

func newRec(t *testing.T) (*SQLite, string) {
	home, conn := newHome(t)
	return &SQLite{DB: conn}, home
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func baseEv() Event {
	return Event{Kind: KindSkill, Name: "code-review", Client: "claude-code", SessionID: "s1", ToolUseID: "toolu_1",
		Source: SourceHook, Invocation: InvocationModel}
}

func TestValidate(t *testing.T) {
	long := strings.Repeat("a", 5000)
	bad := map[string]func(*Event){
		"kind":       func(e *Event) { e.Kind = "bash" },
		"source":     func(e *Event) { e.Source = "cli" },
		"empty name": func(e *Event) { e.Name = "" },
		"long name":  func(e *Event) { e.Name = long },
		"ctrl name":  func(e *Event) { e.Name = "a\nb" },
		"nul":        func(e *Event) { e.SessionID = "a\x00b" },
		"long sess":  func(e *Event) { e.SessionID = long },
		"mcp no srv": func(e *Event) { e.Kind, e.Invocation = KindMCP, "" },
		"srv no mcp": func(e *Event) { e.MCPServer = "x" },
		"inv tool":   func(e *Event) { e.Kind = KindTool },
		"inv value":  func(e *Event) { e.Invocation = "cron" },
		"client":     func(e *Event) { e.Client = "Claude Code!" },
		"errclass":   func(e *Event) { e.ErrorClass = "open /etc/passwd: permission denied" },
		"latency":    func(e *Event) { e.LatencyMS = Int64(-1) },
		"long proj":  func(e *Event) { e.Project = strings.Repeat("p", 5000) },
	}
	for n, mut := range bad {
		e := baseEv()
		mut(&e)
		if err := e.Validate(); err == nil {
			t.Errorf("%s: want error", n)
		}
	}
	e := baseEv()
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	rec, _ := newRec(t)
	e.Name = long
	if _, err := rec.Record(context.Background(), e); err == nil {
		t.Error("record should validate")
	}
}

func TestUpsertPrePostAndResend(t *testing.T) {
	rec, _ := newRec(t)
	ctx := context.Background()
	pre := baseEv()
	id1, err := rec.Record(ctx, pre)
	if err != nil {
		t.Fatal(err)
	}
	id1b, _ := rec.Record(ctx, pre) // resend
	post := baseEv()
	post.OK, post.LatencyMS, post.ErrorClass = Bool(false), Int64(42), "failure"
	id2, err := rec.Record(ctx, post)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id1b || id1 != id2 {
		t.Fatalf("ids differ: %d %d %d", id1, id1b, id2)
	}
	rows, _ := rec.Recent(ctx, 10, Filter{})
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	r := rows[0]
	if r.OK == nil || *r.OK || r.LatencyMS == nil || *r.LatencyMS != 42 || r.ErrorClass != "failure" {
		t.Errorf("%+v", r)
	}
	// A late/duplicate pre must not erase the result.
	_, _ = rec.Record(ctx, pre)
	rows, _ = rec.Recent(ctx, 10, Filter{})
	if len(rows) != 1 || rows[0].OK == nil {
		t.Errorf("pre after post lost result: %+v", rows)
	}
	// Another source for the same call is a separate row; same source different id too.
	g := baseEv()
	g.Source = SourceGateway
	_, _ = rec.Record(ctx, g)
	g2 := baseEv()
	g2.ToolUseID = "toolu_2"
	_, _ = rec.Record(ctx, g2)
	if rows, _ := rec.Recent(ctx, 10, Filter{}); len(rows) != 3 {
		t.Errorf("rows=%d", len(rows))
	}
	// Without tool_use_id: row per call.
	n := baseEv()
	n.ToolUseID = ""
	_, _ = rec.Record(ctx, n)
	_, _ = rec.Record(ctx, n)
	if rows, _ := rec.Recent(ctx, 10, Filter{}); len(rows) != 5 {
		t.Errorf("rows=%d", len(rows))
	}
}

func TestRegistryLookupAndMiss(t *testing.T) {
	rec, _ := newRec(t)
	db := rec.DB
	ctx := context.Background()
	exec(t, db, `INSERT INTO skills(id,scope,name,description) VALUES (1,'global','code-review','d')`)
	exec(t, db, `INSERT INTO skill_versions(id,skill_id,version,content_hash,source_type,store_path) VALUES (1,1,'1.2.0',?, 'dir','x')`, strings.Repeat("a", 64))
	exec(t, db, `UPDATE skills SET current_version_id=1 WHERE id=1`)
	exec(t, db, `INSERT INTO tools(id,scope,name,kind,description,command) VALUES (1,'global','deploy-check','command','d','echo')`)
	exec(t, db, `INSERT INTO mcp_servers(id,scope,name,transport,command) VALUES (1,'global','my-server','stdio','x')`)

	for _, e := range []Event{
		{Kind: KindSkill, Name: "/Code-Review", Source: SourceHook, ToolUseID: "a", Client: "claude-code", SessionID: "s", Invocation: InvocationModel},
		{Kind: KindTool, Name: "deploy-check", Source: SourceGateway},
		{Kind: KindMCP, Name: "t", MCPServer: "my_server", Source: SourceHook},
		{Kind: KindSkill, Name: "no-such-skill", Source: SourceHook},
		{Kind: KindSkill, Name: "no-such-skill", Source: SourceHook},
		{Kind: KindTool, Name: "ghost", Source: SourceGateway},
		{Kind: KindMCP, Name: "t", MCPServer: "other", Source: SourceHook},
	} {
		if _, err := rec.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := rec.Recent(ctx, 20, Filter{Kind: KindSkill, Name: "code-review"})
	if len(rows) != 1 || rows[0].SkillID == nil || rows[0].Found == nil || !*rows[0].Found || rows[0].SkillVersion != "1.2.0" {
		t.Errorf("skill hit: %+v", rows)
	}
	if rows, _ := rec.Recent(ctx, 20, Filter{Kind: KindTool, Name: "deploy-check"}); len(rows) != 1 || rows[0].ToolID == nil || !*rows[0].Found {
		t.Errorf("tool hit: %+v", rows)
	}
	if rows, _ := rec.Recent(ctx, 20, Filter{Kind: KindMCP}); len(rows) != 2 {
		t.Fatalf("%+v", rows)
	}
	var mid sql.NullInt64
	_ = db.QueryRow(`SELECT mcp_server_id FROM ledger WHERE mcp_server='my_server'`).Scan(&mid)
	if !mid.Valid {
		t.Error("mcp server id not linked (hyphen/underscore)")
	}
	ms, err := rec.Misses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, m := range ms {
		got[m.Kind+"/"+m.Name] = m.Count
	}
	if got["skill/no-such-skill"] != 2 || got["tool/ghost"] != 1 || got["skill/code-review"] != 0 {
		t.Errorf("v_misses: %+v", ms)
	}
	var vm int
	_ = db.QueryRow(`SELECT miss_count FROM v_misses WHERE name='no-such-skill'`).Scan(&vm)
	if vm != 2 {
		t.Errorf("view miss_count=%d", vm)
	}
	cs, _ := rec.Counts(ctx)
	if len(cs) == 0 || cs[0].Calls < 2 {
		t.Errorf("counts %+v", cs)
	}
}

func TestUnknownClientStoredAsNull(t *testing.T) {
	rec, _ := newRec(t)
	ctx := context.Background()
	e := baseEv()
	e.Client = "some-new-cli"
	if _, err := rec.Record(ctx, e); err != nil {
		t.Fatalf("unknown client must not fail: %v", err)
	}
	rows, _ := rec.Recent(ctx, 5, Filter{})
	if len(rows) != 1 || rows[0].Client != "" {
		t.Errorf("%+v", rows)
	}
	var n int
	_ = rec.DB.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n)
	if n != 0 {
		t.Error("no session row for unknown client")
	}
	// known client creates the session row once
	k := baseEv()
	_, _ = rec.Record(ctx, k)
	k.ToolUseID = "toolu_x"
	_, _ = rec.Record(ctx, k)
	_ = rec.DB.QueryRow(`SELECT count(*) FROM sessions WHERE client='claude-code' AND session_id='s1'`).Scan(&n)
	if n != 1 {
		t.Errorf("sessions=%d", n)
	}
}

func TestPayloadDefaultOffAndOptIn(t *testing.T) {
	rec, home := newRec(t)
	ctx := context.Background()
	in := json.RawMessage(fmt.Sprintf(`{"skill":"x","args":"--token %s","api_key":"%s","note":"password=%s","nested":{"a":[{"Authorization":"Bearer abcdefghijklmnop1234"}],"url":"https://u:%s@h.example/p"},"ok":"hello"}`, fakeGH, fakeSK, fakePwd, fakePwd))
	out := json.RawMessage(fmt.Sprintf(`{"stdout":"key %s done","n":3}`, fakeSK))
	e := baseEv()
	e.Input, e.Output = in, out

	id, err := rec.Record(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, found, _ := rec.Payload(ctx, id); found {
		t.Fatal("payload stored without opt-in")
	}

	exec(t, rec.DB, `INSERT INTO settings(key,value) VALUES ('record_payload','1')`)
	e.ToolUseID = "toolu_2"
	id, err = rec.Record(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	pi, po, found, _ := rec.Payload(ctx, id)
	if !found {
		t.Fatal("payload not stored with opt-in")
	}
	for _, s := range []string{fakeGH, fakeSK, fakePwd, "abcdefghijklmnop1234"} {
		if strings.Contains(pi, s) || strings.Contains(po, s) {
			t.Errorf("secret %q leaked: %s | %s", s, pi, po)
		}
	}
	if !strings.Contains(pi, `"ok":"hello"`) || !strings.Contains(pi, `"api_key":"***"`) {
		t.Errorf("unexpected masking: %s", pi)
	}
	var rv int
	_ = rec.DB.QueryRow(`SELECT redaction_version FROM ledger_payloads WHERE ledger_id=?`, id).Scan(&rv)
	if rv != RedactionVersion {
		t.Errorf("redaction_version=%d", rv)
	}

	// Whole DB (incl. WAL) has no plaintext secret.
	_ = rec.DB.Close()
	entries, _ := os.ReadDir(home)
	for _, en := range entries {
		if en.IsDir() {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(home, en.Name()))
		for _, s := range []string{fakeGH, fakeSK, fakePwd} {
			if strings.Contains(string(b), s) {
				t.Errorf("%s contains plaintext %q", en.Name(), s)
			}
		}
	}
}

func TestMaskRules(t *testing.T) {
	long := strings.Repeat("あ", 3000) // 9000 bytes
	got, err := MaskJSON([]byte(fmt.Sprintf(`{"cookie":{"deep":"x"},"Password":1,"txt":%q,"list":["--password","p4ss","--port","80"],"mid":"export TOKEN=%s ok"}`, long, fakeGH)))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if m["cookie"] != "***" || m["Password"] != "***" {
		t.Errorf("key masking: %v", m)
	}
	if s := m["txt"].(string); len(s) > MaxStringBytes+len(truncMark) || !strings.HasSuffix(s, truncMark) {
		t.Errorf("string cap: %d", len(s))
	}
	l := m["list"].([]any)
	if l[1] != "***" || l[3] != "80" {
		t.Errorf("flag value masking: %v", l)
	}
	if strings.Contains(m["mid"].(string), fakeGH) {
		t.Errorf("inline: %v", m["mid"])
	}

	// whole-document cap
	var parts []string
	for i := 0; i < 40; i++ {
		parts = append(parts, fmt.Sprintf(`"f%d":%q`, i, strings.Repeat("x", 3000)))
	}
	got, _ = MaskJSON([]byte("{" + strings.Join(parts, ",") + "}"))
	if len(got) > MaxPayloadBytes || !json.Valid([]byte(got)) || !strings.Contains(got, `"_truncated":true`) {
		t.Errorf("doc cap: len=%d", len(got))
	}
	// non-JSON never stored verbatim
	got, _ = MaskJSON([]byte("password=" + fakePwd))
	if strings.Contains(got, fakePwd) || !json.Valid([]byte(got)) {
		t.Errorf("non-json: %s", got)
	}
	// deep nesting
	deep := strings.Repeat(`{"a":`, 100) + `"` + fakeGH + `"` + strings.Repeat("}", 100)
	if got, _ := MaskJSON([]byte(deep)); strings.Contains(got, fakeGH) {
		t.Error("deep leak")
	}
}

func TestConcurrentRecord(t *testing.T) {
	home, conn := newHome(t)
	_ = conn
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// own connection per "process"
			c, err := store.OpenRaw(store.DBPath(home))
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			r := &SQLite{DB: c}
			e := baseEv()
			e.ToolUseID = fmt.Sprintf("toolu_%d", i)
			if _, err := r.Record(context.Background(), e); err != nil {
				errs <- err
			}
			e.OK = Bool(true)
			if _, err := r.Record(context.Background(), e); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var n, withOK int
	_ = conn.QueryRow(`SELECT count(*), count(ok) FROM ledger`).Scan(&n, &withOK)
	if n != 32 || withOK != 32 {
		t.Errorf("rows=%d ok=%d", n, withOK)
	}
}

// realRun executes a real command tool through tool.Run (no shell, no deny-listed
// or allowlist-restricted program: printf, ls, sleep only).
func realRun(t *testing.T, d tool.Definition, args map[string]any) *tool.Result {
	t.Helper()
	d.Kind, d.Description = tool.KindCommand, "gateway simulation"
	d.InputSchema = json.RawMessage(`{"type":"object","properties":{"v":{"type":"string"}}}`)
	d.Policy.WorkingDir = t.TempDir()
	if len(d.Policy.Allow) == 0 {
		d.Policy.Allow = []string{d.Command}
	}
	res, _ := tool.Run(context.Background(), &d, args) // a started-but-failed run has a nil error
	if res == nil {
		t.Fatal("nil result")
	}
	return res
}

func TestGatewaySimulation(t *testing.T) {
	rec, _ := newRec(t)
	ctx := context.Background()
	for i, n := range []string{"ok-tool", "fail-tool", "slow-tool", "big-tool", "denied-tool"} {
		exec(t, rec.DB, `INSERT INTO tools(id,scope,name,kind,description,command) VALUES (?,'global',?,'command','d','x')`, i+1, n)
	}
	clock := time.Unix(1_700_000_000, 0)
	g := &GatewayRecorder{Rec: rec, Client: "claude-code", SessionID: "gw1", Project: t.TempDir(),
		Now: func() time.Time { clock = clock.Add(150 * time.Millisecond); return clock }}

	big := realRun(t, tool.Definition{Name: "big-tool", Command: "printf", Args: []string{"%s", "{{v}}"},
		Policy: tool.Policy{MaxOutputBytes: 100}}, map[string]any{"v": strings.Repeat("a", 5000) + " " + fakeGH})
	cases := []struct {
		name      string
		res       *tool.Result
		withID    bool
		wantOK    bool
		wantClass string
	}{
		{"ok-tool", realRun(t, tool.Definition{Name: "ok-tool", Command: "printf", Args: []string{"%s", "{{v}}"}}, map[string]any{"v": "hi"}), true, true, ""},
		{"fail-tool", realRun(t, tool.Definition{Name: "fail-tool", Command: "ls", Args: []string{"no-such-file-forge"}}, nil), false, false, tool.ClassExit},
		{"slow-tool", realRun(t, tool.Definition{Name: "slow-tool", Command: "sleep", Args: []string{"30"}, Policy: tool.Policy{TimeoutMS: 300}}, nil), false, false, "timeout"},
		// output truncation is still a successful run
		{"big-tool", big, false, true, ""},
		{"denied-tool", realRun(t, tool.Definition{Name: "denied-tool", Command: "printf", Args: []string{"x"}, Policy: tool.Policy{Confirm: tool.ConfirmDeny}}, nil), false, false, tool.ClassDenied},
	}
	// Preconditions: these are really what tool.Run returned.
	if r := cases[0].res; !r.OK || r.Stdout != "hi" {
		t.Fatalf("ok run: %+v", r)
	}
	if r := cases[1].res; r.OK || r.ExitCode <= 0 || r.TimedOut || r.ErrorClass == tool.ClassPolicy {
		t.Fatalf("fail run: %+v", r)
	}
	if r := cases[2].res; r.OK || !r.TimedOut {
		t.Fatalf("timeout run: %+v", r)
	}
	if !big.OK || !big.StdoutTruncated || len(big.Stdout) != 100 {
		t.Fatalf("truncated run: %+v", big)
	}

	for _, c := range cases {
		call := &GatewayCall{Kind: KindTool, Name: c.name}
		if c.withID {
			call.ToolUseID = "gw-call-1"
		}
		if err := g.Start(ctx, call); err != nil {
			t.Fatal(err)
		}
		if c.withID {
			if rows, _ := rec.Recent(ctx, 10, Filter{Name: c.name}); len(rows) != 1 || rows[0].OK != nil {
				t.Fatalf("pending row expected: %+v", rows)
			}
		} else if rows, _ := rec.Recent(ctx, 10, Filter{Name: c.name}); len(rows) != 0 {
			t.Fatal("Start without tool_use_id must not write a row")
		}
		if err := g.End(ctx, call, c.res); err != nil {
			t.Fatal(err)
		}
		rows, _ := rec.Recent(ctx, 10, Filter{Name: c.name, Source: SourceGateway})
		if len(rows) != 1 {
			t.Fatalf("%s: rows=%d", c.name, len(rows))
		}
		r := rows[0]
		if r.Client != "claude-code" || r.LatencyMS == nil || r.OK == nil || *r.OK != c.wantOK || r.ErrorClass != c.wantClass ||
			r.Found == nil || !*r.Found || r.ToolID == nil {
			t.Errorf("%s: %+v", c.name, r)
		}
		if c.name == "slow-tool" && *r.LatencyMS < 250 {
			t.Errorf("timeout latency should come from the real run: %d", *r.LatencyMS)
		}
	}
	// http error / unknown name have no safe offline real run: hand-built Result and EndError.
	ch := &GatewayCall{Kind: KindTool, Name: "ok-tool"}
	_ = g.Start(ctx, ch)
	_ = g.End(ctx, ch, &tool.Result{OK: false, HTTPStatus: 503, DurationMS: 7})
	c3 := &GatewayCall{Kind: KindTool, Name: "nope"}
	_ = g.Start(ctx, c3)
	if err := g.EndError(ctx, c3, "not_found", nil); err != nil {
		t.Fatal(err)
	}
	var nHTTP int
	_ = rec.DB.QueryRow(`SELECT count(*) FROM ledger WHERE error_class='http_error'`).Scan(&nHTTP)
	if nHTTP != 1 {
		t.Errorf("http_error rows=%d", nHTTP)
	}
	if ms, _ := rec.Misses(ctx); len(ms) != 1 || ms[0].Name != "nope" {
		t.Errorf("misses %+v", ms)
	}
	// Neither stdout/stderr/error text nor the fake secrets reach the ledger.
	var cnt int
	_ = rec.DB.QueryRow(`SELECT count(*) FROM ledger WHERE error_class LIKE '%FAKE%' OR error_class LIKE '% %'`).Scan(&cnt)
	if cnt != 0 {
		t.Error("raw error text stored")
	}
	if got := ClassifyToolResult(&tool.Result{OK: false}); got != "error" {
		t.Errorf("%s", got)
	}
}

func TestNullClientDuplicatesAreIntended(t *testing.T) {
	rec, _ := newRec(t)
	ctx := context.Background()
	count := func() int {
		var n int
		_ = rec.DB.QueryRow(`SELECT count(*) FROM ledger`).Scan(&n)
		return n
	}
	// Unregistered client -> client NULL -> outside the unique index: a re-sent post duplicates.
	e := baseEv()
	e.Client, e.OK = "some-new-cli", Bool(true)
	_, _ = rec.Record(ctx, e)
	_, _ = rec.Record(ctx, e)
	if count() != 2 {
		t.Errorf("NULL client rows=%d, want 2 (documented: no dedup)", count())
	}
	// Registered client but no session_id / no tool_use_id: also no dedup.
	for _, mut := range []func(*Event){func(e *Event) { e.SessionID = "" }, func(e *Event) { e.ToolUseID = "" }} {
		before := count()
		k := baseEv()
		k.ToolUseID, k.SessionID = "toolu_dup", "s-dup"
		mut(&k)
		_, _ = rec.Record(ctx, k)
		_, _ = rec.Record(ctx, k)
		if count() != before+2 {
			t.Errorf("rows=%d, want %d", count(), before+2)
		}
	}
	// Control: with all three keys the re-send is deduplicated.
	before := count()
	k := baseEv()
	k.ToolUseID = "toolu_same"
	_, _ = rec.Record(ctx, k)
	_, _ = rec.Record(ctx, k)
	if count() != before+1 {
		t.Errorf("registered client with ids must dedup")
	}
}

func TestPluginSkillMissIsUnknown(t *testing.T) {
	rec, _ := newRec(t)
	ctx := context.Background()
	for _, name := range []string{"Plugin:Review", "plain-missing"} {
		e := baseEv()
		e.Name, e.ToolUseID = name, "t-"+name
		if _, err := rec.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := rec.Recent(ctx, 10, Filter{})
	for _, r := range rows {
		switch r.Name {
		case "plugin:review":
			if r.Found != nil {
				t.Errorf("plugin skill found=%v, want NULL", *r.Found)
			}
		case "plain-missing":
			if r.Found == nil || *r.Found {
				t.Errorf("plain miss must be found=0: %+v", r.Found)
			}
		}
	}
	if ms, _ := rec.Misses(ctx); len(ms) != 1 || ms[0].Name != "plain-missing" {
		t.Errorf("misses %+v", ms)
	}
}

func TestMaskKeyAndInlineHardening(t *testing.T) {
	mask := func(in string) map[string]any {
		got, err := MaskJSON([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(got), &m); err != nil {
			t.Fatalf("%s: %v", got, err)
		}
		if strings.Contains(got, fakeGH) || strings.Contains(got, fakeSK) || strings.Contains(got, fakePwd) {
			t.Errorf("leak: %s", got)
		}
		return m
	}
	// (a) a key that is itself a secret; collisions get a suffix, deterministic across runs.
	for i := 0; i < 20; i++ {
		m := mask(fmt.Sprintf(`{%q:"a",%q:"b","ok":"c"}`, fakeGH, fakeSK))
		if len(m) != 3 || m["***"] == nil || m["***#2"] == nil || m["ok"] != "c" || m["***"] != "a" && m["***"] != "b" {
			t.Fatalf("key masking: %v", m)
		}
	}
	if m := mask(fmt.Sprintf(`{"***":"x",%q:"y"}`, fakeGH)); len(m) != 2 {
		t.Errorf("literal *** key must not be overwritten: %v", m)
	}
	// (b) flags and bearer inside one string.
	m := mask(fmt.Sprintf(`{"cmd":"curl --token %s --api-key %s --password %s -H 'Authorization: Bearer %s' --port 80"}`, fakePwd, fakePwd, fakePwd, fakePwd))
	cmd := m["cmd"].(string)
	for _, want := range []string{"--token ***", "--api-key ***", "--password ***", "Authorization: ***", "--port 80"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("want %q in %q", want, cmd)
		}
	}
	// (c) zero-width characters and fullwidth/compat forms in key names.
	m = mask(fmt.Sprintf(`{"pass\u200bword":%q,"to\u2060ken":%q,"ＰＡＳＳＷＯＲＤ":%q,"𝐚𝐩𝐢_𝐤𝐞𝐲":%q,"safe":"v"}`, fakePwd, fakePwd, fakePwd, fakePwd))
	for k, v := range m {
		if k != "safe" && v != "***" {
			t.Errorf("key %q not masked: %v", k, v)
		}
	}
	// Documented limit: a Cyrillic homoglyph key is not recognized by name.
	m = mask(`{"p\u0430ssword":"short"}`)
	if m["p\u0430ssword"] != "short" {
		t.Logf("homoglyph limit changed: %v", m)
	}
}

func TestLatencyRoundsHookDuration(t *testing.T) {
	for in, want := range map[float64]int64{123.7: 124, 123.2: 123, 0.4: 0, 0.5: 1} {
		d := in
		ev, _, err := MapClaudeHook(&claudePayload{HookEventName: "PostToolUse", ToolName: "Skill", SessionID: "s", ToolUseID: "t",
			ToolInput: json.RawMessage(`{"skill":"x"}`), DurationMS: &d}, "")
		if err != nil || ev.LatencyMS == nil || *ev.LatencyMS != want {
			t.Errorf("%v -> %v %v, want %d", in, ev.LatencyMS, err, want)
		}
	}
}
