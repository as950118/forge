package gateway

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/store"
	"github.com/heonjinjeong/forge/internal/tool"
)

// TASK-0020 AC-9: a miss row is written with a short timeout and never under a
// lock, so a database held by another writer delays each call by at most about the
// timeout instead of serializing 5 seconds per call.
func TestMissWriteDoesNotSerializeCallsOnBusyDB(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.gw(), "claude-code", nil)

	// A second connection holds the write lock (BEGIN IMMEDIATE) for the whole test.
	other, err := store.Open(context.Background(), store.DBPath(e.home))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	const n = 20
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = callRaw(cs, fmt.Sprintf("forge_busy%d", i), nil)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	// Serialized behaviour was ~n*5s; bounded behaviour is a few timeouts at most.
	if elapsed > 2*time.Second {
		t.Fatalf("%d concurrent unknown calls on a busy database took %v (want about missWriteTimeout, not n*5s)", n, elapsed)
	}
	_ = tx.Rollback()
	// Failed writes did not use up slots: the same names are recorded once the lock is gone.
	for i := 0; i < n; i++ {
		_, _ = callRaw(cs, fmt.Sprintf("forge_busy%d", i), nil)
	}
	if got := len(e.rows()); got != n {
		t.Errorf("rows after the lock was released = %d, want %d (failed writes retried, once each)", got, n)
	}
}

// AC-3: a registered server's prefix does not let a client write unlimited distinct
// rows; invented names are deduplicated and bounded per session while a tool the
// server really has keeps one row per attempt (ADR-F0002 attempt counts).
func TestRegisteredServerPrefixRowsAreBounded(t *testing.T) {
	e := newEnv(t)
	e.addMock("mock-srv", "echo", mcp.ExposureGateway)
	cs := e.connect(e.gw(), "claude-code", nil)
	for i := 0; i < 500; i++ {
		_, _ = callRaw(cs, fmt.Sprintf("mock_srv__invented%d", i), nil)
	}
	for i := 0; i < 5; i++ {
		_, _ = callRaw(cs, "mock_srv__invented1", nil) // duplicate
	}
	n := 0
	for _, r := range e.rows() {
		if r.Kind == "mcp" && r.Server == "mock-srv" {
			n++
		}
	}
	if n != maxUnknownRows {
		t.Fatalf("invented names on a registered server: %d rows, want exactly %d", n, maxUnknownRows)
	}
	// invented names of registered-server prefixes and unregistered names share the session budget
	_, _ = callRaw(cs, "forge_other", nil)
	if got := len(e.rows()); got != maxUnknownRows {
		t.Errorf("rows = %d, want the shared session budget %d", got, maxUnknownRows)
	}
}

func TestRegisteredToolMissKeepsOneRowPerAttempt(t *testing.T) {
	e := newEnv(t)
	e.addTool("denied", tool.ConfirmDeny, "true")
	cs := e.connect(e.gw(), "claude-code", nil)
	for i := 0; i < 7; i++ {
		_, _ = callRaw(cs, "forge_denied", nil)
	}
	n := 0
	for _, r := range e.rows() {
		if r.Kind == "tool" && r.Name == "denied" && r.Found.Valid && r.Found.Int64 == 0 {
			n++
		}
	}
	if n != 7 {
		t.Errorf("miss rows of a registered tool = %d, want one per attempt (7)", n)
	}
}

// AC-3: restarting the gateway in a loop (a new session each time) does not grow
// the unattributed miss rows without bound.
func TestRestartLoopIsBoundedByGlobalRetention(t *testing.T) {
	e := newEnv(t)
	e.addTool("denied", tool.ConfirmDeny, "true")
	// registered-tool misses and a real-session row must survive the pruning
	cs0 := e.connect(e.gw(), "claude-code", nil)
	_, _ = callRaw(cs0, "forge_denied", nil)
	if _, err := e.db.Exec(`INSERT INTO ledger(ts,kind,name,session_id,source,found,error_class) VALUES (1,'tool','hookrow','real-sess','hook',0,'not_found')`); err != nil {
		t.Fatal(err)
	}
	const sessions = 30
	for s := 0; s < sessions; s++ {
		g := e.gw(func(o *Options) { o.SessionID = fmt.Sprintf("gw-loop%d", s) })
		cs := e.connect(g, "claude-code", nil)
		for i := 0; i < 100; i++ {
			_, _ = callRaw(cs, fmt.Sprintf("forge_s%d_n%d", s, i), nil)
		}
	}
	var unattributed, other int
	if err := e.db.QueryRow(`SELECT count(*) FROM ledger WHERE source='gateway' AND found=0 AND kind='tool' AND tool_id IS NULL`).Scan(&unattributed); err != nil {
		t.Fatal(err)
	}
	if err := e.db.QueryRow(`SELECT count(*) FROM ledger WHERE NOT (source='gateway' AND found=0 AND kind='tool' AND tool_id IS NULL)`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	// each process prunes on its first row, so the total stays near keep + one session
	if max := globalMissKeep + maxUnknownRows; unattributed > max {
		t.Errorf("unattributed miss rows = %d after %d restarts, want <= %d", unattributed, sessions, max)
	}
	if unattributed < globalMissKeep {
		t.Errorf("unattributed miss rows = %d, retention must keep the newest %d", unattributed, globalMissKeep)
	}
	if other != 2 {
		t.Errorf("rows that must survive pruning (registered tool miss + hook row) = %d, want 2", other)
	}
}

// AC-4: SIGHUP (the controlling terminal went away) shuts the gateway down cleanly,
// so its proxied child does not stay behind. SIGKILL cannot be handled (documented).
func TestE2ESighupStopsGatewayAndChild(t *testing.T) {
	c := setupE2E(t)
	c.run("mcp", "add", "mock-srv", "--env", "FORGE_MCP_MOCK=ok", "--env", "MOCK_TOOLS=echo,pid", "--", os.Args[0])
	c.run("mcp", "refresh", "mock-srv", "--timeout", "30s")
	c.run("mcp", "expose", "mock-srv", "--client", "claude-code", "--mode", "gateway")
	cmd := exec.Command(c.bin, "mcp", "serve")
	cmd.Env = append(os.Environ(), "FORGE_HOME="+c.home)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "claude-code", Version: "1.0"}, nil).Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	pidText, _ := invoke(t, cs, "mock_srv__pid", nil)
	pid, _ := strconv.Atoi(pidText)
	if pid == 0 {
		t.Fatalf("pid %q", pidText)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitGone(t, pid, "proxied child after SIGHUP")
	_ = cs.Wait()
	// a clean shutdown exits 0; the default SIGHUP action would end it by signal (-1)
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 0 {
		t.Errorf("gateway after SIGHUP: %v", cmd.ProcessState)
	}
}

// AC-6: a cancelled confirmation (what Claude Code answers by itself without a UI) says
// how to approve or change the policy; an explicit decline stays the plain refusal. The
// tool is never run in either case.
func TestConfirmCancelExplainsHowToApprove(t *testing.T) {
	e := newEnv(t)
	marker := e.work + "/ran"
	e.addTool("risky", tool.ConfirmAsk, "touch", "-a", "{{v}}")
	g := e.gw()
	answer := func(action string) string {
		cs := e.connect(g, "claude-code", func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
			return &sdk.ElicitResult{Action: action}, nil
		})
		out, isErr := invoke(t, cs, "forge_risky", map[string]any{"v": marker})
		if !isErr {
			t.Fatalf("%s: tool must be refused: %q", action, out)
		}
		return out
	}
	cancelled := answer("cancel")
	for _, w := range []string{"취소", "Elicitation hook", "--confirm none", "--yes-code-exec"} {
		if !strings.Contains(cancelled, w) {
			t.Errorf("cancel message lacks %q: %q", w, cancelled)
		}
	}
	declined := answer("decline")
	if strings.Contains(declined, "Elicitation hook") || !strings.Contains(declined, "거부") {
		t.Errorf("decline message: %q", declined)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the tool ran although the confirmation was not approved")
	}
	var refused int
	for _, r := range e.rows() {
		if r.ErrClass == "confirm_refused" {
			refused++
		}
	}
	if refused != 2 {
		t.Errorf("ledger confirm_refused rows = %d, want 2", refused)
	}
}

// AC-11: in a tools/list description an escape sequence is a real escape of a hidden
// character; literal text that spells one is written with a doubled backslash.
func TestListTextEscapeNotationIsUnambiguous(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"real ESC", "a\x1bb", `a\x1bb`},
		{"literal backslash-x1b", `a\x1bb`, `a\\x1bb`},
		{"real bidi", "a\u202eb", `a\u202eb`},
		{"literal backslash-u202e", `a\u202eb`, `a\\u202eb`},
		{"literal backslash-U", `a\U000e0041b`, `a\\U000e0041b`},
		{"backslash then real ESC", "a\\\x1bb", `a\\\x1bb`},
		{"ordinary backslashes stay", `C:\Users\x and \d+ \n`, `C:\Users\x and \d+ \n`},
		{"quotes stay", `say "hi"`, `say "hi"`},
		{"short hex is not an escape", `\x1 \u12`, `\x1 \u12`},
	}
	for _, c := range cases {
		if got := listText(c.in, 1000); got != c.want {
			t.Errorf("%s: listText(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestCutCleanDoesNotDoubleOrSplitEscapes(t *testing.T) {
	clean := listText("aaaa\u202ebbbb", 1000) // text with one bidi override: it is written as an escape
	if got := cutClean(clean, 1000); got != clean {
		t.Errorf("short text changed: %q", got)
	}
	for max := 6; max < len(clean); max++ {
		got := cutClean(clean, max)
		if strings.Contains(got, `\\`) {
			t.Errorf("max=%d: doubled backslash: %q", max, got)
		}
		if i := strings.LastIndex(got, `\`); i >= 0 && !strings.HasPrefix(got[i:], `\u202e`) {
			t.Errorf("max=%d: half an escape left: %q", max, got)
		}
		if len(got) > max {
			t.Errorf("max=%d: %d bytes", max, len(got))
		}
	}
}
