package cli

import (
	"bytes"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) string {
	t.Helper()
	cmd := NewRootCmd(BuildInfo{Version: "v1.2.3", Commit: "abc", Date: "now"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("forge %v: %v", args, err)
	}
	return out.String()
}

func TestHelp(t *testing.T) {
	out := run(t, "--help")
	for _, n := range []string{"init", "skill", "mcp", "tool", "profile", "sync", "history", "doctor"} {
		if !strings.Contains(out, n) {
			t.Errorf("help missing %q", n)
		}
	}
}

func TestVersion(t *testing.T) {
	out := run(t, "--version")
	if !strings.Contains(out, "v1.2.3") || !strings.Contains(out, "abc") {
		t.Errorf("unexpected version output: %q", out)
	}
}

func TestStubs(t *testing.T) {
	for _, n := range []string{"tool", "profile", "sync", "history"} {
		out := run(t, n)
		if !strings.Contains(out, "not implemented yet (TASK-") {
			t.Errorf("%s: unexpected output %q", n, out)
		}
	}
	if out := run(t, "mcp", "serve"); !strings.Contains(out, "not implemented yet (TASK-0007)") {
		t.Errorf("mcp serve: %q", out)
	}
	if out := run(t, "skill", "misses"); !strings.Contains(out, "not implemented yet (TASK-0009)") {
		t.Errorf("skill misses: %q", out)
	}
}

func TestDoctor(t *testing.T) {
	out := run(t, "doctor")
	if !strings.Contains(out, "not implemented yet (TASK-0012)") || !strings.Contains(out, "schema.sql embedded: true") {
		t.Errorf("unexpected doctor output: %q", out)
	}
}
