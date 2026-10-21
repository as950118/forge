package cli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heonjinjeong/forge/internal/ledger"
	"github.com/heonjinjeong/forge/internal/store"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

// seedLedger fills the ledger of the current FORGE_HOME with realistic events
// through the real recorder (after forge init and a few registrations).
func seedLedger(t *testing.T, home string) {
	t.Helper()
	conn, err := store.Open(context.Background(), store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{
		`INSERT INTO skills(scope,name,description) VALUES ('global','review','d'),('global','idle-skill','d')`,
		`INSERT INTO tools(scope,name,kind,description,command) VALUES ('global','deploy','command','d','echo')`,
		`INSERT INTO mcp_servers(scope,name,transport,command) VALUES ('global','my-github','stdio','x'),('global','unused-srv','stdio','x')`,
	} {
		if _, err := conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	rec := &ledger.SQLite{DB: conn}
	now := time.Now()
	evs := []ledger.Event{
		{TS: now.Add(-3 * time.Hour), Kind: "skill", Name: "review", Client: "claude-code", Source: "hook", ToolUseID: "t1", OK: ledger.Bool(true), LatencyMS: ledger.Int64(12)},
		{TS: now.Add(-3 * time.Hour), Kind: "skill", Name: "review", Client: "claude-code", Source: "gateway", ToolUseID: "t1", OK: ledger.Bool(true), LatencyMS: ledger.Int64(15)},
		{TS: now.Add(-2 * time.Hour), Kind: "tool", Name: "deploy", Client: "claude-code", Source: "gateway", ToolUseID: "t2", OK: ledger.Bool(false), ErrorClass: "exec"},
		{TS: now.Add(-time.Hour), Kind: "mcp", Name: "search", MCPServer: "my_github", Client: "claude-code", Source: "hook", ToolUseID: "t3", OK: ledger.Bool(true)},
		{TS: now.Add(-40 * 24 * time.Hour), Kind: "skill", Name: "ghost", Client: "claude-code", Source: "hook", ToolUseID: "t4"},
		{TS: now.Add(-30 * time.Minute), Kind: "skill", Name: "ghost", Client: "opencode", Source: "gateway", ToolUseID: "t5"},
		{TS: now.Add(-20 * time.Minute), Kind: "skill", Name: "plugin:thing", Client: "claude-code", Source: "hook", ToolUseID: "t6"},
		{TS: now.Add(-10 * time.Minute), Kind: "mcp", Name: "q", MCPServer: "attached-by-user", Client: "claude-code", Source: "hook", ToolUseID: "t7"},
	}
	for _, e := range evs {
		e.SessionID = "sess-1"
		e.Project = "/work/proj"
		if _, err := rec.Record(context.Background(), e); err != nil {
			t.Fatalf("record %+v: %v", e, err)
		}
	}
}

func initHome(t *testing.T) string {
	home := forgeHome(t)
	mustRun(t, "init")
	seedLedger(t, home)
	return home
}

func jsonOut(t *testing.T, args ...string) map[string]any {
	t.Helper()
	out := mustRun(t, args...)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("forge %v: invalid JSON: %v\n%s", args, err, out)
	}
	return m
}

func TestHistoryRequiresInitAndCreatesNothing(t *testing.T) {
	home := forgeHome(t)
	for _, args := range [][]string{
		{"history"}, {"history", "summary"}, {"history", "top"}, {"history", "unused"}, {"history", "misses"}, {"skill", "misses"},
	} {
		out, err := runErr(t, "", args...)
		if err == nil || !strings.Contains(err.Error(), "forge init") {
			t.Errorf("%v: err=%v out=%q", args, err, out)
		}
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Errorf("history created %v", ents)
	}
	// FORGE_HOME that does not exist at all
	t.Setenv("FORGE_HOME", filepath.Join(home, "nope"))
	if _, err := runErr(t, "", "history"); err == nil {
		t.Error("missing home accepted")
	}
	if _, err := os.Stat(filepath.Join(home, "nope")); err == nil {
		t.Error("home created")
	}
}

func TestHistoryCorruptDatabase(t *testing.T) {
	home := forgeHome(t)
	if err := os.WriteFile(store.DBPath(home), []byte(strings.Repeat("garbage! ", 2000)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runErr(t, "", "history")
	if err == nil || !strings.Contains(err.Error(), "손상") {
		t.Errorf("err=%v", err)
	}
	if ents, _ := os.ReadDir(home); len(ents) != 1 {
		t.Errorf("created %v", ents)
	}
}

func TestHistoryListJSONAndMerge(t *testing.T) {
	initHome(t)
	m := jsonOut(t, "history", "--json", "--kind", "skill", "--name", "review")
	calls := m["calls"].([]any)
	if len(calls) != 1 || m["count"] != float64(1) || m["limit"] != float64(50) {
		t.Fatalf("%v", m)
	}
	c := calls[0].(map[string]any)
	if fmt.Sprint(c["sources"]) != "[gateway hook]" || c["client"] != "claude-code" || c["ok"] != true || c["latency_ms"] != float64(15) {
		t.Errorf("%v", c)
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", c["time"].(string)); err != nil || c["ts_ms"] == nil {
		t.Errorf("time fields: %v", c)
	}
	raw := jsonOut(t, "history", "--json", "--raw", "--kind", "skill", "--name", "review")
	if len(raw["calls"].([]any)) != 2 {
		t.Errorf("raw: %v", raw)
	}
	all := jsonOut(t, "history", "--format", "json", "--limit", "100")
	if all["count"] != float64(7) { // 8 events, one call recorded by two sources
		t.Errorf("count %v", all["count"])
	}
	// filters
	for args, want := range map[string]int{
		"--client opencode":                1,
		"--failed":                         1,
		"--ok":                             2,
		"--source gateway":                 3,
		"--source hook --kind mcp":         2,
		"--since 1h":                       3,
		"--since 2h --until 1h":            1,
		"--since 2000-01-01 --kind tool":   1,
		"--name-contains ghost":            2,
		"--name Ghost":                     0,
		"--server my-github":               1,
		"--session sess-1 --project /work": 0,
		"--session sess-1 --limit 2":       2,
		"--since 24h --kind skill":         3,
	} {
		a := append([]string{"history", "--json"}, strings.Fields(args)...)
		if got := jsonOut(t, a...)["count"]; got != float64(want) {
			t.Errorf("%s: count %v want %d", args, got, want)
		}
	}
}

func TestHistoryTableAndTimezone(t *testing.T) {
	initHome(t)
	old := time.Local
	t.Cleanup(func() { time.Local = old })
	out := ""
	for _, loc := range []*time.Location{time.FixedZone("KST", 9*3600), time.FixedZone("PDT", -7*3600)} {
		time.Local = loc
		out = mustRun(t, "history", "--kind", "tool")
		want := time.Now().Add(-2 * time.Hour).In(loc).Format("2006-01-02 15")
		if !strings.Contains(out, want) || !strings.Contains(out, loc.String()[:0]+time.Now().In(loc).Format("-0700")) {
			t.Errorf("%s: table lacks local time %q:\n%s", loc, want, out)
		}
	}
	for _, w := range []string{"TIME", "KIND", "deploy", "failed(exec)", "claude-code", "gateway", "/work/proj", "sess-1"} {
		if !strings.Contains(out, w) {
			t.Errorf("table lacks %q:\n%s", w, out)
		}
	}
	// JSON is UTC regardless of the zone
	a := jsonOut(t, "history", "--json", "--kind", "tool")["calls"].([]any)[0].(map[string]any)["time"].(string)
	time.Local = time.FixedZone("X", 5*3600)
	b := jsonOut(t, "history", "--json", "--kind", "tool")["calls"].([]any)[0].(map[string]any)["time"].(string)
	if a != b || !strings.HasSuffix(a, "Z") {
		t.Errorf("json time %q vs %q", a, b)
	}
}

func TestHistoryValidationErrors(t *testing.T) {
	initHome(t)
	cases := map[string]string{
		"history --since banana":                 "--since",
		"history --until 5x":                     "--until",
		"history --since 1h --until 2h":          "--since 가 --until",
		"history --kind agent":                   "--kind",
		"history --source sql":                   "--source",
		"history --ok --failed":                  "함께 쓸 수 없습니다",
		"history --limit 0":                      "--limit",
		"history --limit 10001":                  "--limit",
		"history --format xml":                   "--format",
		"history --json --format csv":            "--json",
		"history --name a --name-contains b":     "함께 쓸 수 없습니다",
		"history summary --group-by hour":        "--group-by",
		"history --group-by hour":                "--group-by",
		"history unused --client x":              "지원하지 않습니다",
		"history unused --limit 3":               "지원하지 않습니다",
		"history misses --name x":                "지원하지 않습니다",
		"history top --raw":                      "unknown flag",
		"history --group-by kind --raw":          "지원하지 않습니다",
		"skill misses --kind tool":               "unknown flag",
		"skill misses --since zzz":               "--since",
		"history extra":                          "unknown command",
		"history --output x --json --format csv": "--json",
	}
	for cmd, want := range cases {
		_, err := runErr(t, "", strings.Fields(cmd)...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err=%v want %q", cmd, err, want)
		}
	}
	// the evil value is never echoed raw
	_, err := runErr(t, "", "history", "--since", "\x1b[31mred")
	if err == nil || strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("err %q", err)
	}
	_, err = runErr(t, "", "history", "--kind", "\x1b[31mred")
	if err == nil || strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("err %q", err)
	}
}

func TestHistorySummaryTopUnusedMisses(t *testing.T) {
	initHome(t)
	m := jsonOut(t, "history", "summary", "--json")
	gs := m["groups"].([]any)
	if m["group_by"] != "kind" || len(gs) != 3 {
		t.Fatalf("%v", m)
	}
	first := gs[0].(map[string]any)
	if first["key"].(map[string]any)["kind"] != "skill" || first["calls"] != float64(4) {
		t.Errorf("%v", first)
	}
	// forge history --group-by == forge history summary --group-by
	a := mustRun(t, "history", "--group-by", "client", "--json")
	b := mustRun(t, "history", "summary", "--group-by", "client", "--json")
	if a != b {
		t.Errorf("root --group-by differs:\n%s\n%s", a, b)
	}
	for _, g := range []string{"kind", "name", "client", "project", "day", "source"} {
		out := mustRun(t, "history", "summary", "--group-by", g)
		if !strings.Contains(out, "CALLS") {
			t.Errorf("%s: %s", g, out)
		}
	}
	top := jsonOut(t, "history", "top", "--limit", "2", "--json")["groups"].([]any)
	if len(top) != 2 || top[0].(map[string]any)["key"].(map[string]any)["name"] != "ghost" {
		t.Errorf("top: %v", top)
	}
	topTool := jsonOut(t, "history", "top", "--kind", "tool", "--json")["groups"].([]any)
	if len(topTool) != 1 {
		t.Errorf("%v", topTool)
	}

	un := jsonOut(t, "history", "unused", "--json")["unused"].([]any)
	var got []string
	for _, u := range un {
		x := u.(map[string]any)
		got = append(got, fmt.Sprintf("%s/%s", x["kind"], x["name"]))
	}
	if strings.Join(got, ",") != "skill/idle-skill,mcp/unused-srv" {
		t.Errorf("unused: %v", got)
	}
	un = jsonOut(t, "history", "unused", "--kind", "skill", "--since", "1h", "--json")["unused"].([]any)
	if len(un) != 2 { // review (3h ago) and idle-skill
		t.Errorf("unused since: %v", un)
	}
	out := mustRun(t, "history", "unused")
	if !strings.Contains(out, "never") || !strings.Contains(out, "활성") {
		t.Errorf("%s", out)
	}
}

func TestSkillMissesAndFalsePositives(t *testing.T) {
	initHome(t)
	m := jsonOut(t, "skill", "misses", "--json")
	ms := m["misses"].([]any)
	if len(ms) != 1 {
		t.Fatalf("skill misses: %v", m)
	}
	g := ms[0].(map[string]any)
	cl := g["clients"].(map[string]any)
	if g["name"] != "ghost" || g["count"] != float64(2) || cl["claude-code"] != float64(1) || cl["opencode"] != float64(1) {
		t.Errorf("%v", g)
	}
	// a user-attached MCP server (found=0, kind=mcp) and a plugin:skill (found NULL) are not skill misses
	out := mustRun(t, "skill", "misses")
	for _, bad := range []string{"attached-by-user", "plugin:thing", "my_github"} {
		if strings.Contains(out, bad) {
			t.Errorf("false positive %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "ghost") || !strings.Contains(out, "UNREGISTERED_NAME") {
		t.Errorf("%s", out)
	}
	recent := jsonOut(t, "skill", "misses", "--since", "7d", "--json")["misses"].([]any)
	if len(recent) != 1 || recent[0].(map[string]any)["count"] != float64(1) {
		t.Errorf("since 7d: %v", recent)
	}
	// history misses: per kind, and MCP per server
	all := jsonOut(t, "history", "misses", "--json")["misses"].([]any)
	if len(all) != 2 {
		t.Errorf("history misses: %v", all)
	}
	mcp := jsonOut(t, "history", "misses", "--kind", "mcp", "--json")["misses"].([]any)
	if len(mcp) != 1 || mcp[0].(map[string]any)["mcp_server"] != "attached_by_user" {
		t.Errorf("mcp misses: %v", mcp)
	}
	text := mustRun(t, "history", "misses", "--kind", "mcp")
	if !strings.Contains(text, "미등록 호출 이름") || !strings.Contains(text, "거짓 양성") {
		t.Errorf("%s", text)
	}
}

func TestHistoryExportCSVAndOutputFile(t *testing.T) {
	home := initHome(t)
	conn, _ := store.Open(context.Background(), store.DBPath(home))
	// hostile values stored by a (hypothetical) writer: formulas and terminal escapes
	_, err := conn.Exec(`INSERT INTO ledger(ts,kind,name,project,source) VALUES (?, 'skill', ?, ?, 'hook')`,
		time.Now().UnixMilli(), "=cmd|' /C calc'!A0", "@/p\x1b[2J")
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "h.csv")
	msg := mustRun(t, "history", "--format", "csv", "--output", out)
	if !strings.Contains(msg, "저장했습니다") {
		t.Errorf("msg %q", msg)
	}
	st, err := os.Stat(out)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	b, _ := os.ReadFile(out)
	recs, err := csv.NewReader(strings.NewReader(string(b))).ReadAll()
	if err != nil || len(recs) != 9 {
		t.Fatalf("csv: %v, %d records\n%s", err, len(recs), b)
	}
	if !strings.Contains(string(b), `'=cmd|`) || !strings.Contains(string(b), `'@/p\x1b[2J`) {
		t.Errorf("injection not defused / escapes not visible:\n%s", b)
	}
	for _, r := range string(b) {
		if r != '\n' && textsafe.NeedsEscape(r) {
			t.Errorf("raw %U in csv", r)
		}
	}
	// overwrite protection
	if _, err := runErr(t, "", "history", "--format", "csv", "--output", out); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("overwrite: %v", err)
	}
	mustRun(t, "history", "--json", "--output", out, "--force")
	b, _ = os.ReadFile(out)
	if !json.Valid(b) {
		t.Errorf("force output not JSON: %s", b)
	}
	// symlink and DB protection
	link := filepath.Join(dir, "l.csv")
	if err := os.Symlink(out, link); err != nil {
		t.Fatal(err)
	}
	if _, err := runErr(t, "", "history", "--output", link, "--force"); err == nil || !strings.Contains(err.Error(), "심볼릭") {
		t.Errorf("symlink: %v", err)
	}
	if _, err := runErr(t, "", "history", "--output", store.DBPath(home), "--force"); err == nil || !strings.Contains(err.Error(), "저장소") {
		t.Errorf("db overwrite: %v", err)
	}
	if err := store.CheckReadable(context.Background(), store.DBPath(home)); err != nil {
		t.Errorf("db damaged: %v", err)
	}
	// summary/unused/misses export too
	for _, a := range [][]string{{"history", "summary"}, {"history", "top"}, {"history", "unused"}, {"history", "misses"}, {"skill", "misses"}} {
		p := filepath.Join(dir, strings.Join(a, "_")+".csv")
		mustRun(t, append(a, "--format", "csv", "--output", p)...)
		raw, _ := os.ReadFile(p)
		if _, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll(); err != nil {
			t.Errorf("%v: %v", a, err)
		}
	}
}

func TestHistoryTerminalEscapeInjection(t *testing.T) {
	home := initHome(t)
	conn, _ := store.Open(context.Background(), store.DBPath(home))
	_, err := conn.Exec(`INSERT INTO ledger(ts,kind,name,client,project,session_id,source,found) VALUES (?, 'skill', ?, 'claude-code', ?, ?, 'hook', 0)`,
		time.Now().UnixMilli(), "evil\x1b[2Jname\u202etxt\nFORGED  ROW", "/p\x1b]0;x\x07\u009b", "s\r\n\u200bid")
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range [][]string{
		{"history"}, {"history", "--raw"}, {"history", "summary", "--group-by", "name"}, {"history", "summary", "--group-by", "project"},
		{"history", "top"}, {"history", "misses"}, {"skill", "misses"}, {"history", "--json"}, {"skill", "misses", "--json"}, {"history", "--format", "csv"},
	} {
		out := mustRun(t, a...)
		for _, r := range out {
			if r == '\n' || r == '\t' && strings.Contains(strings.Join(a, " "), "csv") || r == '\r' && strings.Contains(strings.Join(a, " "), "csv") {
				continue
			}
			if textsafe.NeedsEscape(r) {
				t.Errorf("forge %v: raw %U in output", a, r)
			}
		}
		csvMode := strings.Contains(strings.Join(a, " "), "csv")
		if csvMode { // a newline inside a quoted CSV cell is one record, not a forged row
			recs, err := csv.NewReader(strings.NewReader(out)).ReadAll()
			if err != nil || len(recs) != 9 {
				t.Errorf("csv records %d err %v", len(recs), err)
			}
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "FORGED") {
				t.Errorf("forge %v: forged line %q", a, line)
			}
		}
	}
	if out := mustRun(t, "history", "--kind", "skill", "--name-contains", "evil"); !strings.Contains(out, `evil\x1b[2Jname\u202etxt\nFORGED`) {
		t.Errorf("escapes not visible:\n%s", out)
	}
}

func TestHistorySQLInjectionAttempt(t *testing.T) {
	home := initHome(t)
	for _, v := range []string{"'; DROP TABLE ledger;--", `" OR 1=1 --`} {
		for _, flag := range []string{"--name", "--name-contains", "--client", "--project", "--session", "--server"} {
			out := mustRun(t, "history", flag, v, "--json")
			if !strings.Contains(out, `"count": 0`) {
				t.Errorf("%s %q: %s", flag, v, out)
			}
		}
	}
	conn, _ := store.Open(context.Background(), store.DBPath(home))
	defer conn.Close()
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM ledger`).Scan(&n); err != nil || n != 8 {
		t.Errorf("ledger rows %d err %v", n, err)
	}
}

func TestHistoryDoesNotModifyDatabase(t *testing.T) {
	home := initHome(t)
	before, _ := os.ReadFile(store.DBPath(home))
	for _, a := range [][]string{{"history"}, {"history", "summary"}, {"history", "unused"}, {"skill", "misses"}} {
		mustRun(t, a...)
	}
	// wal/shm sidecars may exist, but the main file content is identical once checkpointed state is read
	conn, _ := store.Open(context.Background(), store.DBPath(home))
	var n int
	_ = conn.QueryRow(`SELECT count(*) FROM ledger`).Scan(&n)
	conn.Close()
	if n != 8 {
		t.Errorf("rows %d", n)
	}
	_ = before
}

func TestHistoryEmptyDatabase(t *testing.T) {
	forgeHome(t)
	mustRun(t, "init")
	for _, a := range []string{"history", "history summary", "history top", "history unused", "history misses", "skill misses"} {
		out := mustRun(t, strings.Fields(a)...)
		if strings.TrimSpace(out) == "" || strings.Count(out, "\n") != 1 {
			t.Errorf("%s: %q", a, out)
		}
		js := mustRun(t, append(strings.Fields(a), "--json")...)
		if !json.Valid([]byte(js)) || strings.Contains(js, "null") {
			t.Errorf("%s json: %s", a, js)
		}
	}
}
