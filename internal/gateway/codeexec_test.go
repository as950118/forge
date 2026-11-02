package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heonjinjeong/forge/internal/tool"
)

// addCodeExecTool registers a harmless fake "npm" script (the name makes it
// risk=code-exec; nothing real is ever run) that touches <dir>/ran.
func (e *env) addCodeExecTool(name, confirm string, ack, legacy bool) (marker string) {
	e.t.Helper()
	dir := e.t.TempDir()
	marker = filepath.Join(dir, "ran")
	bin := filepath.Join(dir, "npm")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0o755); err != nil {
		e.t.Fatal(err)
	}
	d := tool.Definition{Name: name, Kind: tool.KindCommand, Description: "fake npm " + name,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"v":{"type":"string"}},"required":["v"],"additionalProperties":false}`),
		Command:     bin, Args: []string{"--", "{{v}}"}, Policy: tool.Policy{Confirm: confirm, WorkingDir: e.work, AckCodeExec: ack}}
	if _, err := e.tools.Add(context.Background(), d, false); err != nil {
		e.t.Fatal(err)
	}
	if legacy { // a row stored before the classification existed: no acknowledgement recorded
		if _, err := e.db.Exec(`UPDATE settings SET value=json_remove(value,'$.ack_code_exec') WHERE key=?`, "tool.ext."+name); err != nil {
			e.t.Fatal(err)
		}
	}
	return marker
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestCodeExecToolIsForcedToAskEvenWithConfirmNone(t *testing.T) {
	e := newEnv(t)
	marker := e.addCodeExecTool("cx", tool.ConfirmNone, true, true) // legacy confirm=none, never acknowledged
	g := e.gw()

	// listing tells the model a confirmation is needed
	cs := e.connect(g, "claude-code", approve(false))
	if out, _ := invoke(t, cs, MetaList, nil); !strings.Contains(out, "cx") || !strings.Contains(out, "사용자 확인 필요") {
		t.Errorf("forge_list must mark the tool as confirm-required: %s", out)
	}
	// declined: not run, and the approval text carries the risk line
	var msg string
	cs = e.connect(g, "claude-code", func(_ context.Context, r *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		msg = r.Params.Message
		return &sdk.ElicitResult{Action: "accept", Content: map[string]any{"approve": false}}, nil
	})
	if out, isErr := invoke(t, cs, "forge_cx", map[string]any{"v": "x"}); !isErr || fileExists(marker) {
		t.Errorf("declined: %q ran=%v", out, fileExists(marker))
	}
	if !strings.Contains(msg, "위험: ") || !strings.Contains(msg, "risk=code-exec") || !strings.Contains(msg, "npm") {
		t.Errorf("approval message lacks the risk line: %q", msg)
	}
	// client without elicitation: refused with the code-exec specific guidance
	cs = e.connect(g, "claude-code", nil)
	out, isErr := invoke(t, cs, "forge_cx", map[string]any{"v": "x"})
	if !isErr || fileExists(marker) || !strings.Contains(out, "--yes-code-exec") || strings.Contains(out, "--confirm none)") {
		t.Errorf("no-elicitation: %q ran=%v", out, fileExists(marker))
	}
	// approved: runs
	cs = e.connect(g, "claude-code", approve(true))
	if out, isErr := invoke(t, cs, "forge_cx", map[string]any{"v": "x"}); isErr || !fileExists(marker) {
		t.Errorf("approved: %q %v ran=%v", out, isErr, fileExists(marker))
	}
}

func TestAcknowledgedCodeExecToolRunsWithoutAsking(t *testing.T) {
	e := newEnv(t)
	marker := e.addCodeExecTool("cxack", tool.ConfirmNone, true, false)
	g := e.gw()
	cs := e.connect(g, "claude-code", nil) // no elicitation support needed
	if out, isErr := invoke(t, cs, "forge_cxack", map[string]any{"v": "x"}); isErr || !fileExists(marker) {
		t.Errorf("acknowledged confirm=none must run: %q %v", out, isErr)
	}
	// an ordinary tool with confirm=none is unaffected by the classification
	e.addTool("plain", tool.ConfirmNone, "printf", "%s", "{{v}}")
	if out, isErr := invoke(t, cs, "forge_plain", map[string]any{"v": "ok"}); isErr || out != "ok" {
		t.Errorf("plain: %q %v", out, isErr)
	}
}

func TestCodeExecDenyStaysDeny(t *testing.T) {
	e := newEnv(t)
	marker := e.addCodeExecTool("cxdeny", tool.ConfirmDeny, false, false)
	g := e.gw()
	cs := e.connect(g, "claude-code", approve(true))
	if has(listNames(t, cs), "forge_cxdeny") {
		t.Error("confirm=deny must stay hidden")
	}
	if _, err := callRaw(cs, "forge_cxdeny", map[string]any{"v": "x"}); err == nil && fileExists(marker) {
		t.Error("confirm=deny ran")
	}
}
