package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/as950118/forge/internal/ledger"
)

// TASK-0030: tools/call params._meta["claudecode/toolUseId"] becomes the gateway row's
// tool_use_id (Claude Code only; anything unsafe is ignored without an error).

func callMeta(t *testing.T, cs *sdk.ClientSession, name string, meta map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Meta: meta, Name: name, Arguments: map[string]any{"v": "x"}})
	if err != nil {
		t.Fatalf("a bad _meta must never be a protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("call failed: %+v", res.Content)
	}
}

func (e *env) toolUseIDs() []string {
	e.t.Helper()
	rs, err := e.db.Query(`SELECT COALESCE(tool_use_id,'<null>') FROM ledger WHERE source='gateway' ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var s string
		if err := rs.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestToolUseIDRecordedForClaudeCode(t *testing.T) {
	e := newEnv(t)
	e.addTool("say", "none", "printf", "%s", "{{v}}")
	cs := e.connect(e.gw(), "claude-code", nil)
	callMeta(t, cs, "forge_say", map[string]any{"claudecode/toolUseId": "toolu_01AbC-d_9"})
	if got := e.toolUseIDs(); len(got) != 1 || got[0] != "toolu_01AbC-d_9" {
		t.Errorf("tool_use_id = %v", got)
	}
}

func TestToolUseIDIgnoredForOtherClients(t *testing.T) {
	e := newEnv(t)
	e.addTool("say", "none", "printf", "%s", "{{v}}")
	g := e.gw()
	callMeta(t, e.connect(g, "opencode", nil), "forge_say", map[string]any{"claudecode/toolUseId": "toolu_oc"})
	callMeta(t, e.connect(g, "some-ide", nil), "forge_say", map[string]any{"claudecode/toolUseId": "toolu_x"})
	for _, id := range e.toolUseIDs() {
		if id != "<null>" {
			t.Errorf("a non-claude-code client's key must be ignored, got %q", id)
		}
	}
}

func TestToolUseIDUnsafeValuesIgnored(t *testing.T) {
	e := newEnv(t)
	e.addTool("say", "none", "printf", "%s", "{{v}}")
	cs := e.connect(e.gw(), "claude-code", nil)
	bad := []any{
		123, 1.5, true, nil, []any{"toolu_1"}, map[string]any{"a": "b"}, "",
		"toolu_1\nINJECT", "toolu_1\x1b[2K", "toolu 1", "toolu_1\u202e", "toolu_\u200b1", "toolu_é", "toolu_1\x00",
		"a/b", "toolu_1;DROP", strings.Repeat("a", maxToolUseIDLen+1),
	}
	for _, v := range bad {
		callMeta(t, cs, "forge_say", map[string]any{"claudecode/toolUseId": v})
	}
	callMeta(t, cs, "forge_say", map[string]any{"other": "toolu_k"}) // different key
	callMeta(t, cs, "forge_say", nil)                                // no _meta
	got := e.toolUseIDs()
	if len(got) != len(bad)+2 {
		t.Fatalf("rows=%d, want %d", len(got), len(bad)+2)
	}
	for i, id := range got {
		if id != "<null>" {
			t.Errorf("row %d: unsafe/absent id recorded as %q", i, id)
		}
	}
	// the longest accepted value
	callMeta(t, cs, "forge_say", map[string]any{"claudecode/toolUseId": strings.Repeat("a", maxToolUseIDLen)})
	if got := e.toolUseIDs(); got[len(got)-1] != strings.Repeat("a", maxToolUseIDLen) {
		t.Errorf("max-length id rejected")
	}
}

// With an id the gateway writes a pending row at Start that End completes: exactly one
// row per call, with the outcome.
func TestToolUseIDOneRowPerCall(t *testing.T) {
	e := newEnv(t)
	e.addTool("say", "none", "printf", "%s", "{{v}}")
	cs := e.connect(e.gw(), "claude-code", nil)
	callMeta(t, cs, "forge_say", map[string]any{"claudecode/toolUseId": "toolu_one"})
	rows := e.rows()
	if len(rows) != 1 || rows[0].OK.Int64 != 1 {
		t.Errorf("rows = %+v", rows)
	}
}

// AC-3: the gateway answers the "server/discover" request some clients send before
// initialize with a JSON-RPC error and keeps serving the connection.
func TestUnsupportedMethodBeforeInitializeKeepsConnection(t *testing.T) {
	e := newEnv(t)
	g := e.gw()
	cr, sw := io.Pipe() // server -> test
	sr, cw := io.Pipe() // test -> server
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ss, err := g.Server().Connect(ctx, &sdk.IOTransport{Reader: sr, Writer: sw}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	defer cw.Close()
	send := func(s string) { _, _ = cw.Write([]byte(s + "\n")) }
	br := bufio.NewReader(cr)
	read := func() map[string]any {
		type r struct {
			m   map[string]any
			err error
		}
		ch := make(chan r, 1)
		go func() {
			line, err := br.ReadBytes('\n')
			var m map[string]any
			if err == nil {
				err = json.Unmarshal(line, &m)
			}
			ch <- r{m, err}
		}()
		select {
		case x := <-ch:
			if x.err != nil {
				t.Fatalf("read: %v", x.err)
			}
			return x.m
		case <-time.After(20 * time.Second):
			t.Fatal("timeout")
		}
		return nil
	}
	send(`{"jsonrpc":"2.0","id":7,"method":"server/discover","params":{}}`)
	m := read()
	errObj, _ := m["error"].(map[string]any)
	if errObj == nil || m["id"] != float64(7) {
		t.Fatalf("server/discover must get a JSON-RPC error response: %v", m)
	}
	t.Logf("server/discover response: %v", m)
	// the connection still works: normal initialize + tools/list
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"1"}}}`)
	if m := read(); m["result"] == nil {
		t.Fatalf("initialize after server/discover failed: %v", m)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if m := read(); m["result"] == nil {
		t.Fatalf("tools/list failed: %v", m)
	}
}

// End to end: the hook row of the same tool_use_id and the gateway row are one call.
func TestToolUseIDMergesWithHookRow(t *testing.T) {
	e := newEnv(t)
	e.addTool("say", "none", "printf", "%s", "{{v}}")
	cs := e.connect(e.gw(), "claude-code", nil)
	if _, err := e.rec.Record(context.Background(), ledger.Event{Kind: ledger.KindTool, Name: "forge_say", Client: "claude-code",
		SessionID: "sess-real", ToolUseID: "toolu_m1", Source: ledger.SourceHook}); err != nil {
		t.Fatal(err)
	}
	callMeta(t, cs, "forge_say", map[string]any{"claudecode/toolUseId": "toolu_m1"})
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM v_ledger_calls`).Scan(&n); err != nil || n != 1 {
		t.Errorf("calls=%d err=%v, want 1", n, err)
	}
}
