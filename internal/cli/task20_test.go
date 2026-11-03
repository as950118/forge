package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/as950118/forge/internal/textsafe"
)

func noRawTerminalChars(t *testing.T, what, out string) {
	t.Helper()
	for _, r := range strings.NewReplacer("\n", "", "\t", "").Replace(out) {
		if textsafe.NeedsEscape(r) {
			t.Fatalf("%s prints raw %U:\n%q", what, r, out)
		}
	}
}

// TASK-0020 AC-8: every stored text of `forge mcp show` (human output) is escaped, not
// only the tool table; --json keeps structure and values but writes terminal-active
// characters as \uXXXX escapes.
func TestMCPShowEscapesEveryStoredText(t *testing.T) {
	forgeHome(t)
	addMock(t, "mock", "alpha")
	refresh(t, "mock")
	const evil = "x\x1b[2J\x1b]0;t\x07\u202ey\u009bz\u200b"
	dbExec(t, `UPDATE mcp_discovery SET server_name=?, server_version=?, protocol_version=?, error=?, ok=0`, "n"+evil, "v"+evil, "p"+evil, "e"+evil)
	dbExec(t, `UPDATE mcp_tools SET description=?`, "d"+evil)
	dbExec(t, `UPDATE mcp_server_values SET key=key||?`, evil)
	dbExec(t, `UPDATE mcp_servers SET cwd=?`, "/w"+evil)
	out := mustRun(t, "mcp", "show", "mock")
	noRawTerminalChars(t, "mcp show", out)
	for _, want := range []string{`\x1b[2J`, `\u202e`, `\x07`} {
		if !strings.Contains(out, want) {
			t.Errorf("escaped form %q missing:\n%s", want, out)
		}
	}

	js := mustRun(t, "mcp", "show", "mock", "--json")
	noRawTerminalChars(t, "mcp show --json", js)
	var got struct {
		Tools []struct {
			Description string `json:"description"`
		} `json:"tools"`
		Discovery struct {
			ServerName string `json:"server_name"`
		} `json:"discovery"`
	}
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, js)
	}
	if len(got.Tools) != 1 || got.Tools[0].Description != "d"+evil {
		t.Errorf("--json must keep the value: %q", got.Tools)
	}
}

func TestWriteJSONEscapesTerminalCharactersAndKeepsData(t *testing.T) {
	in := map[string]any{"a": "x\u202ey\u009bz\U000e0041", "n": 1, "ok": "plain \u00e9 \ud55c"}
	var b strings.Builder
	if err := writeJSON(&b, in); err != nil {
		t.Fatal(err)
	}
	noRawTerminalChars(t, "writeJSON", b.String())
	var back map[string]any
	if err := json.Unmarshal([]byte(b.String()), &back); err != nil {
		t.Fatal(err)
	}
	if back["a"] != in["a"] || back["ok"] != in["ok"] || back["n"].(float64) != 1 {
		t.Errorf("round trip changed the data: %v", back)
	}
	if !strings.Contains(b.String(), "\u00e9") || !strings.Contains(b.String(), "\ud55c") {
		t.Errorf("printable non-ASCII must stay readable: %s", b.String())
	}
}

// AC-7: forge.db being a directory is reported as such (not "corrupt?") by every opener.
func TestDBIsDirectoryIsReportedDistinctly(t *testing.T) {
	home := forgeHome(t)
	if err := os.MkdirAll(filepath.Join(home, "forge.db", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i, args := range [][]string{
		{"mcp", "serve"}, {"init"}, {"skill", "list"}, {"tool", "list"}, {"mcp", "list"}, {"history"},
	} {
		out, err := runErr(t, "", args...)
		if err == nil {
			t.Errorf("%v: must fail:\n%s", args, out)
			continue
		}
		msg := err.Error()
		if i == 0 { // the gateway must not create anything in a home it refuses
			if _, err := os.Stat(filepath.Join(home, "skills")); err == nil {
				t.Error("mcp serve created skills/ in a refused home")
			}
		}
		if !strings.Contains(msg, "디렉터리") || !strings.Contains(msg, "forge init") {
			t.Errorf("%v: want the directory notice with the way out, got %q", args, msg)
		}
		if strings.Contains(msg, "손상?") || strings.Contains(msg, "초기화되지 않았습니다") {
			t.Errorf("%v: a directory is neither corruption nor an uninitialized home: %q", args, msg)
		}
	}
}

// AC-6: registering a confirm=ask tool tells how it gets approved (or how to change
// the policy); an explicit --confirm none does not print the note.
func TestToolAddExplainsConfirmAsk(t *testing.T) {
	forgeHome(t)
	out := mustRun(t, "tool", "add", "askme", "--description", "d", "--", "printf", "x")
	for _, w := range []string{"confirm=ask", "Elicitation hook", "--confirm none", "--yes-code-exec", "forge tool test"} {
		if !strings.Contains(out, w) {
			t.Errorf("ask note lacks %q:\n%s", w, out)
		}
	}
	out = mustRun(t, "tool", "add", "freely", "--description", "d", "--confirm", "none", "--", "printf", "x")
	if strings.Contains(out, "Elicitation hook") {
		t.Errorf("no ask note for confirm=none:\n%s", out)
	}
}

// AC-12: the allow-private opt-in is stored per remote server and shown.
func TestMCPAllowPrivateCommand(t *testing.T) {
	forgeHome(t)
	mustRun(t, "mcp", "add", "corp", "--transport", "http", "--url", "https://mcp.corp.example/mcp")
	addMock(t, "stdio-srv", "alpha")
	if out := mustRun(t, "mcp", "show", "corp"); !strings.Contains(out, "사설 주소:") || !strings.Contains(out, "forge mcp allow-private corp") {
		t.Errorf("show must explain the default:\n%s", out)
	}
	if _, err := runErr(t, "", "mcp", "allow-private", "stdio-srv"); err == nil {
		t.Error("allow-private on a stdio server must be refused")
	}
	if _, err := runErr(t, "", "mcp", "allow-private", "ghost"); err == nil {
		t.Error("unknown server")
	}
	mustRun(t, "mcp", "allow-private", "corp")
	if out := mustRun(t, "mcp", "show", "corp"); !strings.Contains(out, "허용됨") {
		t.Errorf("show after allow:\n%s", out)
	}
	if out := mustRun(t, "mcp", "show", "corp", "--json"); !strings.Contains(out, `"allow_private": true`) {
		t.Errorf("json:\n%s", out)
	}
	mustRun(t, "mcp", "allow-private", "corp", "--revoke")
	if out := mustRun(t, "mcp", "show", "corp"); strings.Contains(out, "허용됨") {
		t.Errorf("show after revoke:\n%s", out)
	}
}
