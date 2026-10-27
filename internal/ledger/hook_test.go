package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heonjinjeong/forge/internal/store"
)

func hookOpts(home string) HookOptions {
	return HookOptions{Client: ClientClaudeCode, Home: home}
}

func payload(m map[string]any) *strings.Reader {
	b, _ := json.Marshal(m)
	return strings.NewReader(string(b))
}

func TestHookMapping(t *testing.T) {
	rec, home := newRec(t)
	ctx := context.Background()
	cwd := t.TempDir()
	mk := func(evn, tool string, in any, extra map[string]any) map[string]any {
		m := map[string]any{"hook_event_name": evn, "session_id": "sess-1", "cwd": cwd, "tool_name": tool,
			"tool_use_id": "toolu_" + tool, "tool_input": in}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := []map[string]any{
		mk("PreToolUse", "Skill", map[string]any{"skill": "Code-Review", "args": "medium"}, nil),
		mk("PostToolUse", "Skill", map[string]any{"skill": "Code-Review"}, map[string]any{"duration_ms": 87, "tool_response": map[string]any{"success": true}}),
		mk("PostToolUse", "mcp__github__create_issue", map[string]any{"title": "t"}, map[string]any{"agent_id": "ag1"}),
		mk("PostToolUseFailure", "mcp__my-srv__a__b", map[string]any{}, map[string]any{"error": "boom " + fakePwd}),
		mk("PreToolUse", "mcp__forge__echo-tool", map[string]any{"x": 1}, nil),
		mk("PreToolUse", "Bash", map[string]any{"command": "ls"}, nil),
		mk("PostToolUse", "Read", map[string]any{"file_path": "/x"}, nil),
		mk("UserPromptSubmit", "Skill", map[string]any{"skill": "x"}, nil),
		mk("PreToolUse", "mcp__broken", map[string]any{}, nil),
		mk("PreToolUse", "Skill", map[string]any{}, nil),
	}
	for _, c := range cases {
		_ = IngestHook(payload(c), hookOpts(home)) // errors are diagnostics only
	}
	rows, err := rec.Recent(ctx, 20, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows=%d: %+v", len(rows), rows)
	}
	by := map[string]Row{}
	for _, r := range rows {
		by[r.Kind+"/"+r.MCPServer+"/"+r.Name] = r
	}
	sk := by["skill//code-review"]
	if sk.Invocation != "model" || sk.OK == nil || !*sk.OK || sk.LatencyMS == nil || *sk.LatencyMS != 87 || sk.Source != "hook" ||
		sk.Client != "claude-code" || sk.SessionID != "sess-1" || sk.Project == "" {
		t.Errorf("skill: %+v", sk)
	}
	if g := by["mcp/github/create_issue"]; g.AgentID != "ag1" || g.OK == nil || !*g.OK {
		t.Errorf("mcp: %+v", g)
	}
	if f := by["mcp/my-srv/a__b"]; f.OK == nil || *f.OK || f.ErrorClass != "failure" {
		t.Errorf("failure: %+v", f)
	}
	if by["tool//echo-tool"].MCPServer != "" || by["tool//echo-tool"].OK != nil {
		t.Errorf("forge gateway tool: %+v", by)
	}
	for _, r := range rows {
		if strings.Contains(r.ErrorClass, "boom") {
			t.Error("error text stored")
		}
	}
	// --event fallback without hook_event_name
	_ = IngestHook(payload(map[string]any{"session_id": "s9", "tool_name": "Skill", "tool_use_id": "t9", "tool_input": map[string]any{"skill": "fb"}}),
		HookOptions{Client: ClientClaudeCode, Home: home, Event: "post"})
	if rows, _ := rec.Recent(ctx, 5, Filter{Name: "fb"}); len(rows) != 1 || rows[0].OK == nil {
		t.Errorf("fallback: %+v", rows)
	}
}

func TestHookPayloadOptIn(t *testing.T) {
	rec, home := newRec(t)
	ctx := context.Background()
	p := map[string]any{"hook_event_name": "PostToolUse", "session_id": "s", "tool_name": "Skill", "tool_use_id": "t1",
		"tool_input":    map[string]any{"skill": "x", "args": "token=" + fakeGH},
		"tool_response": map[string]any{"success": true, "api_key": fakeSK},
		"prompt":        "SECRET PROMPT BODY"}
	_ = IngestHook(payload(p), hookOpts(home))
	rows, _ := rec.Recent(ctx, 5, Filter{})
	if _, _, found, _ := rec.Payload(ctx, rows[0].ID); found {
		t.Fatal("payload stored by default")
	}
	exec(t, rec.DB, `INSERT INTO settings(key,value) VALUES ('record_payload','1')`)
	p["tool_use_id"] = "t2"
	_ = IngestHook(payload(p), hookOpts(home))
	rows, _ = rec.Recent(ctx, 5, Filter{})
	in, out, found, _ := rec.Payload(ctx, rows[0].ID)
	if !found || strings.Contains(in+out, fakeGH) || strings.Contains(in+out, fakeSK) || strings.Contains(in+out, "PROMPT BODY") {
		t.Errorf("found=%v in=%s out=%s", found, in, out)
	}
}

func TestHookFailurePathsDoNotCreateOrFail(t *testing.T) {
	// no FORGE_HOME dir, uninitialized dir: nothing is created.
	missing := t.TempDir() + "/nope"
	good := payload(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Skill", "tool_input": map[string]any{"skill": "x"}})
	if err := IngestHook(good, hookOpts(missing)); err == nil {
		t.Error("want diagnostic error")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("FORGE_HOME must not be created by a hook")
	}
	empty := t.TempDir()
	_ = IngestHook(payload(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Skill", "tool_input": map[string]any{"skill": "x"}}), hookOpts(empty))
	if ents, _ := os.ReadDir(empty); len(ents) != 0 {
		t.Errorf("hook created files in uninitialized home: %v", ents)
	}

	_, home := newRec(t)
	for name, in := range map[string]string{
		"bad json": "{not json", "empty": "", "array": "[1,2]", "null": "null",
		"unknown event": `{"hook_event_name":"Stop","tool_name":"Skill"}`,
		"huge":          `{"tool_name":"Skill","pad":"` + strings.Repeat("x", 2<<20) + `"}`,
	} {
		if err := IngestHook(strings.NewReader(in), hookOpts(home)); err == nil && name != "null" {
			t.Errorf("%s: want diagnostic error", name)
		}
	}
	if err := IngestHook(strings.NewReader(`{}`), HookOptions{Client: "other", Home: home}); err == nil {
		t.Error("unknown client")
	}
}

type blockReader struct{ ch chan struct{} }

func (b blockReader) Read([]byte) (int, error) { <-b.ch; return 0, os.ErrClosed }

func TestHookTimeouts(t *testing.T) {
	_, home := newRec(t)
	// stdin that never closes
	br := blockReader{make(chan struct{})}
	defer close(br.ch)
	start := time.Now()
	o := hookOpts(home)
	o.Timeout = 200 * time.Millisecond
	if err := IngestHook(br, o); err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("blocked stdin: err=%v after %v", err, time.Since(start))
	}
	// database write-locked by someone else for longer than the timeout
	c, err := store.OpenRaw(store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tx, err := c.BeginTx(context.Background(), nil) // BEGIN IMMEDIATE
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	start = time.Now()
	err = IngestHook(payload(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Skill", "session_id": "s", "tool_use_id": "t", "tool_input": map[string]any{"skill": "x"}}), o)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("locked db: err=%v after %v", err, time.Since(start))
	}
}

func TestHookConcurrent32(t *testing.T) {
	rec, home := newRec(t)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for _, evn := range []string{"PreToolUse", "PostToolUse"} {
				err := IngestHook(payload(map[string]any{"hook_event_name": evn, "session_id": "cs", "tool_name": "mcp__srv__t",
					"tool_use_id": fmt.Sprintf("toolu_%d", i), "tool_input": map[string]any{"i": i}}), hookOpts(home))
				if err != nil {
					errs <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var n, ok int
	_ = rec.DB.QueryRow(`SELECT count(*), count(ok) FROM ledger`).Scan(&n, &ok)
	if n != 32 || ok != 32 {
		t.Errorf("rows=%d with ok=%d", n, ok)
	}
}

func TestSnippet(t *testing.T) {
	for _, bin := range []string{"", "/opt/my tools/forge"} {
		b, err := ClaudeCodeSnippet(bin)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Hooks map[string][]struct {
				Matcher string
				Hooks   []struct {
					Type, Command string
					Timeout       int
				}
			}
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("invalid json: %v\n%s", err, b)
		}
		for _, ev := range []string{"PreToolUse", "PostToolUse"} {
			g := doc.Hooks[ev]
			if len(g) != 1 || g[0].Matcher != "Skill|mcp__.*" || len(g[0].Hooks) != 1 || g[0].Hooks[0].Type != "command" || g[0].Hooks[0].Timeout <= 0 {
				t.Fatalf("%s: %+v", ev, g)
			}
			want := " hook ingest --client claude-code --event " + map[string]string{"PreToolUse": "pre", "PostToolUse": "post"}[ev]
			if !strings.HasSuffix(g[0].Hooks[0].Command, want) {
				t.Errorf("command %q", g[0].Hooks[0].Command)
			}
		}
		if bin != "" && !strings.HasPrefix(doc.Hooks["PreToolUse"][0].Hooks[0].Command, "'/opt/my tools/forge'") {
			t.Errorf("bin not quoted: %s", doc.Hooks["PreToolUse"][0].Hooks[0].Command)
		}
	}
}

func TestIngestHookRecoversPanic(t *testing.T) {
	old := ingestFn
	t.Cleanup(func() { ingestFn = old })
	ingestFn = func(context.Context, io.Reader, HookOptions) error { panic("boom " + fakePwd) }
	done := make(chan error, 1)
	go func() { done <- IngestHook(strings.NewReader("{}"), HookOptions{Client: ClientClaudeCode}) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "panic") {
			t.Errorf("want panic diagnostic, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IngestHook did not return after panic")
	}
}

func TestHookDurationDecodingIsLenient(t *testing.T) {
	rec, home := newRec(t)
	ctx := context.Background()
	cwd := t.TempDir()
	// Raw JSON so that 1e999 (overflows float64) reaches the decoder as written.
	for i, d := range []string{`1e999`, `"87"`, `-5`, `null`, `{"a":1}`, `12.6`} {
		raw := fmt.Sprintf(`{"hook_event_name":"PostToolUse","session_id":"s","cwd":%q,"tool_name":"Skill","tool_use_id":"t%d","tool_input":{"skill":"lenient-%d"},"duration_ms":%s}`, cwd, i, i, d)
		if err := IngestHook(strings.NewReader(raw), hookOpts(home)); err != nil {
			t.Fatalf("duration_ms=%s dropped the payload: %v", d, err)
		}
	}
	rows, err := rec.Recent(ctx, 20, Filter{})
	if err != nil || len(rows) != 6 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	for _, r := range rows {
		switch r.Name {
		case "lenient-5":
			if r.LatencyMS == nil || *r.LatencyMS != 13 {
				t.Errorf("12.6 -> %v", r.LatencyMS)
			}
		default:
			if r.LatencyMS != nil {
				t.Errorf("%s: latency=%d, want NULL", r.Name, *r.LatencyMS)
			}
		}
	}
	// A structurally broken payload is still rejected.
	if err := IngestHook(strings.NewReader(`{"hook_event_name":`), hookOpts(home)); err == nil {
		t.Error("broken JSON accepted")
	}
}
