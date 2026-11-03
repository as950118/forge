package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/as950118/forge/internal/ledger"
)

// hookRun executes the real command tree and returns stdout and stderr separately.
func hookRun(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := NewRootCmd(BuildInfo{Version: "v", Commit: "c", Date: "d"})
	var o, e bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err = cmd.Execute()
	return o.String(), e.String(), err
}

func TestHookIngestAlwaysSilentSuccess(t *testing.T) {
	home := forgeHome(t)
	good := `{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"Skill","tool_use_id":"t1","tool_input":{"skill":"demo"}}`

	cases := map[string]struct {
		in   string
		args []string
	}{
		"uninitialized home": {good, []string{"hook", "ingest", "--client", "claude-code"}},
		"bad json":           {"{oops", []string{"hook", "ingest", "--client", "claude-code"}},
		"empty":              {"", []string{"hook", "ingest", "--client", "claude-code"}},
		"unknown event":      {`{"hook_event_name":"Stop"}`, []string{"hook", "ingest", "--client", "claude-code"}},
		"huge":               {`{"x":"` + strings.Repeat("a", 3<<20) + `"}`, []string{"hook", "ingest", "--client", "claude-code"}},
		"no client":          {good, []string{"hook", "ingest"}},
		"unknown client":     {good, []string{"hook", "ingest", "--client", "zzz"}},
		"unknown flag":       {good, []string{"hook", "ingest", "--client", "claude-code", "--bogus", "x"}},
		"missing flag value": {good, []string{"hook", "ingest", "--client"}},
		"extra args":         {good, []string{"hook", "ingest", "--client", "claude-code", "extra"}},
	}
	run := func(name string) {
		for n, c := range cases {
			out, errOut, err := hookRun(t, c.in, c.args...)
			if err != nil || out != "" || errOut != "" {
				t.Errorf("%s/%s: err=%v stdout=%q stderr=%q", name, n, err, out, errOut)
			}
		}
	}
	run("uninit")
	if _, err := os.Stat(home + "/forge.db"); err == nil {
		t.Fatal("hook created the database")
	}

	mustRun(t, "init")
	run("init")

	// diagnostics only with FORGE_HOOK_DEBUG=1, and still rc 0 / stdout empty
	t.Setenv("FORGE_HOOK_DEBUG", "1")
	out, errOut, err := hookRun(t, "{oops", "hook", "ingest", "--client", "claude-code")
	if err != nil || out != "" || strings.Count(strings.TrimSpace(errOut), "\n") != 0 || errOut == "" {
		t.Errorf("debug: err=%v out=%q err=%q", err, out, errOut)
	}
	t.Setenv("FORGE_HOOK_DEBUG", "")

	// success path writes the row silently
	out, errOut, err = hookRun(t, good, "hook", "ingest", "--client", "claude-code")
	if err != nil || out != "" || errOut != "" {
		t.Fatalf("%v %q %q", err, out, errOut)
	}
	_, conn, _ := openRegistry(t.Context())
	defer conn.Close()
	var n int
	_ = conn.QueryRow(`SELECT count(*) FROM ledger WHERE kind='skill' AND name='demo' AND source='hook'`).Scan(&n)
	if n != 1 {
		t.Errorf("rows=%d", n)
	}
}

func TestHookSnippetCmd(t *testing.T) {
	forgeHome(t)
	out := mustRun(t, "hook", "snippet", "--client", "claude-code", "--bin", "/usr/local/bin/forge")
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "/usr/local/bin/forge hook ingest --client claude-code --event pre") {
		t.Errorf("%s", out)
	}
	if _, err := runErr(t, "", "hook", "snippet", "--client", "opencode"); err == nil {
		t.Error("opencode snippet should be unsupported")
	}
	if _, err := os.Stat(os.Getenv("FORGE_HOME") + "/forge.db"); err == nil {
		t.Error("snippet must not touch the registry")
	}
}

func TestHookIngestRecoversPanic(t *testing.T) {
	forgeHome(t)
	old := ingestHook
	t.Cleanup(func() { ingestHook = old })
	ingestHook = func(io.Reader, ledger.HookOptions) error { panic("boom") }
	args := []string{"hook", "ingest", "--client", "claude-code"}
	out, errOut, err := hookRun(t, "{}", args...)
	if err != nil || out != "" || errOut != "" {
		t.Errorf("err=%v stdout=%q stderr=%q", err, out, errOut)
	}
	t.Setenv("FORGE_HOOK_DEBUG", "1")
	out, errOut, err = hookRun(t, "{}", args...)
	if err != nil || out != "" || !strings.Contains(errOut, "panic") {
		t.Errorf("debug: err=%v stdout=%q stderr=%q", err, out, errOut)
	}
}

func TestHookTimeoutFromEnv(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false}, {"abc", 0, false}, {"0", 0, false}, {"-5", 0, false}, {"-99999999999999999999", 0, false},
		{"100x", 0, false}, {"1.5", 0, false},
		{"1", time.Millisecond, true}, {"500", 500 * time.Millisecond, true},
		{"10000", 10 * time.Second, true}, {"10001", 10 * time.Second, true},
		{"99999999999999999999", 10 * time.Second, true},
	}
	for _, c := range cases {
		d, ok := hookTimeoutFromEnv(c.in)
		if d != c.want || ok != c.ok {
			t.Errorf("%q -> %v,%v want %v,%v", c.in, d, ok, c.want, c.ok)
		}
	}
}

// UserPromptExpansion end to end (TASK-0028): a registered skill typed by the user is
// recorded as invocation=user with a silent exit; an unregistered name leaves no row.
func TestHookIngestUserPromptExpansion(t *testing.T) {
	forgeHome(t)
	mustRun(t, "init")
	_, conn, _ := openRegistry(t.Context())
	defer conn.Close()
	if _, err := conn.Exec(`INSERT INTO skills(scope,name,description) VALUES ('global','demo','d')`); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) string {
		return `{"hook_event_name":"UserPromptExpansion","session_id":"s-u","cwd":"/tmp/p","expansion_type":"slash_command",` +
			`"command_name":"` + name + `","command_args":"FAKE-SECRET-ARG","command_source":"projectSettings","prompt":"/` + name + ` FAKE-SECRET-ARG"}`
	}
	for _, name := range []string{"demo", "context", "ghost"} {
		out, errOut, err := hookRun(t, mk(name), "hook", "ingest", "--client", "claude-code")
		if err != nil || out != "" || errOut != "" {
			t.Fatalf("%s: %v %q %q", name, err, out, errOut)
		}
	}
	var n, miss int
	_ = conn.QueryRow(`SELECT count(*) FROM ledger WHERE kind='skill' AND name='demo' AND invocation='user' AND source='hook' AND found=1 AND tool_use_id IS NULL`).Scan(&n)
	_ = conn.QueryRow(`SELECT count(*) FROM ledger`).Scan(&miss)
	if n != 1 || miss != 1 {
		t.Errorf("demo rows=%d total=%d", n, miss)
	}
}
