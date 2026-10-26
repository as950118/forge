package cli

import (
	"math/rand"
	"strings"
	"testing"
)

const macTmpArg = "/var/folders/gr/q4n6pgs90gx_jny4zh7p58dw0000gn/T/forge-demo.abc123/src/variant"

func fakeVal(seed int64, set string, n int) string {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = set[r.Intn(len(set))]
	}
	return string(b)
}

const alnumSet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// Regression for TASK-0026: the macOS default TMPDIR path was refused as
// "인자 #2: 비밀 값처럼 보이는 문자열" when used as an argument.
func TestMCPAddAcceptsMacOSTempDirArgument(t *testing.T) {
	forgeHome(t)
	out, errOut, err := runSplit(t, "", "mcp", "add", "x", "--", "python3", "s.py", macTmpArg)
	if err != nil {
		t.Fatalf("macOS TMPDIR path refused: %v\n%s%s", err, out, errOut)
	}
	if !strings.Contains(errOut, macTmpArg) {
		t.Errorf("the preview must show the path unmasked:\n%s", errOut)
	}
	if shown := mustRun(t, "mcp", "show", "x"); !strings.Contains(shown, macTmpArg) {
		t.Errorf("mcp show hides the path:\n%s", shown)
	}
}

// Call sites of the registration context: mcp add argument/env/header and tool add.
func TestSecretHeuristicPerRegistrationCallSite(t *testing.T) {
	forgeHome(t)
	h40, h64 := fakeVal(1, "0123456789abcdef", 40), fakeVal(2, "0123456789abcdef", 64)
	disguised := "/tmp/sk-" + fakeVal(3, alnumSet, 48)
	opaque := fakeVal(4, alnumSet, 32)
	type c struct {
		name string
		args []string
		ok   bool
	}
	cases := []c{
		{"mcp-arg-path", []string{"mcp", "add", "a1", "--", "x", macTmpArg}, true},
		{"mcp-arg-digest", []string{"mcp", "add", "a2", "--", "docker", "run", "img@sha256:" + h64}, true},
		{"mcp-arg-ref-flag", []string{"mcp", "add", "a3", "--", "x", "--ref", h40}, true},
		{"mcp-arg-disguised", []string{"mcp", "add", "a4", "--", "x", disguised}, false},
		{"mcp-arg-opaque", []string{"mcp", "add", "a5", "--", "x", opaque}, false},
		{"mcp-env-path", []string{"mcp", "add", "e1", "--env", "WORKDIR=" + macTmpArg, "--", "x"}, true},
		{"mcp-env-commit", []string{"mcp", "add", "e2", "--env", "GIT_COMMIT=" + h40, "--", "x"}, true},
		{"mcp-env-opaque", []string{"mcp", "add", "e3", "--env", "SERVICE_ID=" + opaque, "--", "x"}, false},
		{"mcp-env-disguised", []string{"mcp", "add", "e4", "--env", "WORKDIR=" + disguised, "--", "x"}, false},
		{"mcp-header-commit", []string{"mcp", "add", "h1", "--transport", "http", "--url", "https://example.com/mcp", "--header", "X-Commit=" + h40}, true},
		{"mcp-header-opaque", []string{"mcp", "add", "h2", "--transport", "http", "--url", "https://example.com/mcp", "--header", "X-Trace=" + opaque}, false},
		{"mcp-url-pathshaped-token", []string{"mcp", "add", "u1", "--transport", "http", "--url", "https://example.com/v1/" + opaque + "/mcp"}, false},
		{"tool-arg-path", []string{"tool", "add", "t1", "--description", "x", "--", "printf", "%s", macTmpArg}, true},
		{"tool-arg-disguised", []string{"tool", "add", "t2", "--description", "x", "--", "printf", "%s", disguised}, false},
		{"tool-arg-opaque", []string{"tool", "add", "t3", "--description", "x", "--", "printf", "%s", opaque}, false},
		{"tool-env-path", []string{"tool", "add", "t4", "--description", "x", "--env", "WORKDIR=" + macTmpArg, "--", "printf", "x"}, true},
		{"tool-env-disguised", []string{"tool", "add", "t5", "--description", "x", "--env", "WORKDIR=" + disguised, "--", "printf", "x"}, false},
	}
	for _, tc := range cases {
		out, err := runErr(t, "", tc.args...)
		if tc.ok && err != nil {
			t.Errorf("%s: refused: %v\n%s", tc.name, err, out)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("%s: a secret-looking value was registered", tc.name)
			} else if strings.Contains(err.Error()+out, opaque) || strings.Contains(err.Error()+out, disguised) {
				t.Errorf("%s: the refusal leaks the value", tc.name)
			}
		}
	}
}

// TASK-0017 AC-1: ${ENV} in stdio args registers (HEAD behaviour; Claude Code
// expands it) but the preview warns prominently, also with --preview; --strict
// still needs approval like for every registration.
func TestMCPAddWarnsOnEnvRefInArgs(t *testing.T) {
	forgeHome(t)
	_, errOut, err := runSplit(t, "", "mcp", "add", "--preview", "envarg", "--", "node", "server.js", "--root=${ROOT}")
	if err != nil || !strings.Contains(errOut, "인자 #2 에 ${...} 참조") || !strings.Contains(errOut, "--env") {
		t.Errorf("--preview: want warning, err=%v\n%s", err, errOut)
	}
	if list := mustRun(t, "mcp", "list"); strings.Contains(list, "envarg") {
		t.Errorf("--preview registered something:\n%s", list)
	}
	_, errOut, err = runSplit(t, "", "mcp", "add", "envarg", "--", "node", "server.js", "--root=${ROOT}")
	if err != nil || !strings.Contains(errOut, "인자 #2 에 ${...} 참조") {
		t.Errorf("add: must register with a warning, err=%v\n%s", err, errOut)
	}
	if list := mustRun(t, "mcp", "list"); !strings.Contains(list, "envarg") {
		t.Errorf("not registered:\n%s", list)
	}
	if _, _, err = runSplit(t, "", "mcp", "add", "envstrict", "--strict", "--", "node", "--root=${ROOT}"); err == nil {
		t.Error("--strict without approval must refuse (non-interactive, no --yes)")
	}
	// The pin how-to reaches the preview of an unpinned runner (defaults unchanged: no prompt).
	_, errOut, err = runSplit(t, "", "mcp", "add", "pinme", "--", "npx", "-y", "some-pkg")
	if err != nil || !strings.Contains(errOut, "고정 방법:") || !strings.Contains(errOut, "some-pkg@<정확한 버전>") {
		t.Errorf("pin guidance missing (err=%v):\n%s", err, errOut)
	}
}
