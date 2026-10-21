package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolLifecycleCommand(t *testing.T) {
	forgeHome(t)
	work := t.TempDir()
	const schema = `{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`
	out := mustRun(t, "tool", "add", "say", "--description", "echo", "--schema", schema, "--cwd", work, "--timeout", "10s", "--", "printf", "<%s>", "{{msg}}")
	if !strings.Contains(out, "추가됨") {
		t.Errorf("%s", out)
	}
	if o := mustRun(t, "tool", "test", "say", "--args", `{"msg":"hi; rm -rf /"}`); !strings.Contains(o, "<hi; rm -rf />") || !strings.Contains(o, "[OK]") {
		t.Errorf("%s", o)
	}
	// invalid args -> error (rc=1)
	if o, err := runErr(t, "", "tool", "test", "say", "--args", `{"msg":1}`); err == nil || !strings.Contains(o+err.Error(), "스키마") {
		t.Errorf("%q %v", o, err)
	}
	if _, err := runErr(t, "", "tool", "test", "say", "--args", `{"msg":"--x"}`); err == nil {
		t.Error("leading dash")
	}
	if _, err := runErr(t, "", "tool", "test", "say", "--args", `{"msg":"a"}`, "--cwd", ".."); err == nil {
		t.Error("cwd escape")
	}
	var res struct {
		OK       bool   `json:"ok"`
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "tool", "test", "say", "--args", `{"msg":"j"}`, "--json")), &res); err != nil || !res.OK || res.Stdout != "<j>" {
		t.Errorf("%+v %v", res, err)
	}
	if o := mustRun(t, "tool", "test", "say", "--args", `{"msg":"d"}`, "--dry-run"); !strings.Contains(o, "dry-run") || !strings.Contains(o, `"d"`) {
		t.Errorf("%s", o)
	}
	if o := mustRun(t, "tool", "list"); !strings.Contains(o, "say") || !strings.Contains(o, "command") {
		t.Errorf("%s", o)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(mustRun(t, "tool", "list", "--json")), &list); err != nil || len(list) != 1 || list[0]["revision"].(float64) != 1 {
		t.Errorf("%v %v", list, err)
	}
	if o := mustRun(t, "tool", "show", "say"); !strings.Contains(o, "timeout=10000ms") || !strings.Contains(o, "allow=printf") {
		t.Errorf("%s", o)
	}
	// duplicate / replace
	if _, err := runErr(t, "", "tool", "add", "say", "--description", "x", "--", "printf", "x"); err == nil {
		t.Error("duplicate")
	}
	mustRun(t, "tool", "add", "say", "--replace", "--description", "echo2", "--", "printf", "x")
	if o := mustRun(t, "tool", "show", "say", "--json"); !strings.Contains(o, `"revision": 2`) {
		t.Errorf("%s", o)
	}
	mustRun(t, "tool", "disable", "say")
	if o := mustRun(t, "tool", "show", "say"); !strings.Contains(o, "활성:       no") {
		t.Errorf("%s", o)
	}
	mustRun(t, "tool", "enable", "say")
	if _, err := runErr(t, "n\n", "tool", "remove", "say"); err == nil {
		t.Error("remove must ask")
	}
	mustRun(t, "tool", "remove", "say", "--yes")
	if _, err := runErr(t, "", "tool", "show", "say"); err == nil {
		t.Error("removed")
	}
	if _, err := runErr(t, "", "tool", "list", "--project"); err == nil || !strings.Contains(err.Error(), "미지원") && !strings.Contains(err.Error(), "지원하지 않습니다") {
		t.Errorf("%v", err)
	}
}

func TestToolScriptAndShellCLI(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	home := forgeHome(t)
	dir := t.TempDir()
	sc := filepath.Join(dir, "calc.py")
	if err := os.WriteFile(sc, []byte("import json,sys\nprint(json.load(sys.stdin)['n']*2)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "tool", "add", "dbl", "--description", "x", "--schema", `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`, "--interpreter", "python3", "--script-file", sc)
	if o := mustRun(t, "tool", "test", "dbl", "--args", `{"n":21}`); !strings.Contains(o, "42") {
		t.Errorf("%s", o)
	}
	if _, err := os.Stat(filepath.Join(home, "scripts", "dbl@1", "script")); err != nil {
		t.Error(err)
	}
	mustRun(t, "tool", "add", "sh1", "--description", "x", "--schema", `{"type":"object","properties":{"m":{"type":"string"}},"required":["m"]}`, "--shell", `printf '%s' "$FORGE_ARG_m"`)
	if o := mustRun(t, "tool", "test", "sh1", "--args", `{"m":"$(id)"}`); !strings.Contains(o, "$(id)") {
		t.Errorf("%s", o)
	}
	if _, err := runErr(t, "", "tool", "add", "bad", "--description", "x", "--", "sh", "-c", "echo"); err == nil {
		t.Error("sh as command")
	}
}

func TestToolHTTPCLIAndSecretHandling(t *testing.T) {
	forgeHome(t)
	const secret = "sekret-token-1234567890abcdef"
	t.Setenv("TOOL_API_TOKEN", secret)
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		fmt.Fprintf(w, "path=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	const schema = `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`
	// literal secret refused, URL with a loopback host refused without --allow-private
	if _, err := runErr(t, "", "tool", "add", "h1", "--description", "x", "--schema", schema, "--url", srv.URL+"/i/{{id}}", "--allow-http", "--allow-private", "--header", "Authorization=Bearer "+secret); err == nil {
		t.Fatal("literal header secret must be refused")
	}
	if _, err := runErr(t, "", "tool", "add", "h1", "--description", "x", "--schema", schema, "--url", srv.URL+"/i/{{id}}", "--allow-http"); err == nil {
		t.Fatal("loopback must be refused without --allow-private")
	}
	if _, err := runErr(t, "", "tool", "add", "h1", "--description", "x", "--schema", schema, "--url", srv.URL+"/i/{{id}}", "--allow-http", "--allow-private", "--header", "Authorization=Bearer-ref"); err == nil {
		t.Fatal("secret-looking header key must not accept a literal")
	}
	mustRun(t, "tool", "add", "h1", "--description", "x", "--schema", schema, "--url", srv.URL+"/i/{{id}}", "--allow-http", "--allow-private", "--header", "Authorization=${TOOL_API_TOKEN}")
	o := mustRun(t, "tool", "test", "h1", "--args", `{"id":"42"}`)
	if !strings.Contains(o, "path=/i/42") || strings.Contains(o, secret) || seen != secret {
		t.Errorf("seen=%q out=%s", seen, o)
	}
	for _, args := range [][]string{{"tool", "list"}, {"tool", "list", "--json"}, {"tool", "show", "h1"}, {"tool", "show", "h1", "--json"},
		{"tool", "test", "h1", "--args", `{"id":"1"}`, "--dry-run"}, {"tool", "test", "h1", "--args", `{"id":"1"}`, "--json"}} {
		if out := mustRun(t, args...); strings.Contains(out, secret) {
			t.Errorf("%v leaks the secret", args)
		}
	}
	if o := mustRun(t, "tool", "show", "h1"); !strings.Contains(o, "Authorization = ${TOOL_API_TOKEN}") {
		t.Errorf("%s", o)
	}
	// SSRF through the CLI
	if _, err := runErr(t, "", "tool", "add", "meta", "--description", "x", "--url", "https://169.254.169.254/latest/meta-data/"); err == nil {
		t.Error("metadata URL")
	}
}

func TestToolCLIWrapperBypassAndAllowlistGuards(t *testing.T) {
	forgeHome(t)
	const schema = `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"},"c":{"type":"string"},"n":{"type":"integer"}}}`
	add := func(name string, extra ...string) (string, error) {
		return runErr(t, "", append([]string{"tool", "add", name, "--description", "x", "--schema", schema}, extra...)...)
	}
	// env/xargs wrappers (allowlist bypass) are refused
	if o, err := add("w1", "--argv", `["env","{{a}}","{{b}}","{{c}}"]`); err == nil || !strings.Contains(o+err.Error(), "env") {
		t.Errorf("env wrapper: %q %v", o, err)
	}
	if _, err := add("w2", "--", "xargs", "-n1", "{{a}}"); err == nil {
		t.Error("xargs wrapper")
	}
	// --allow must contain the command
	if _, err := add("w3", "--allow", "cat", "--", "ls"); err == nil {
		t.Error("allowlist without command")
	}
	// variable in the subcommand slot needs the explicit flag
	if _, err := add("w4", "--", "git", "{{a}}"); err == nil || !strings.Contains(err.Error(), "argv[1]") {
		t.Errorf("argv[1] var: %v", err)
	}
	mustRun(t, "tool", "add", "w5", "--description", "x", "--schema", schema, "--allow-var-subcommand", "--", "git", "{{a}}")
	// ordinary tools keep working
	mustRun(t, "tool", "add", "log", "--description", "x", "--schema", schema, "--", "git", "log", "-n", "{{n}}")
	mustRun(t, "tool", "add", "ls1", "--description", "x", "--", "ls")
	mustRun(t, "tool", "add", "cat1", "--description", "x", "--schema", schema, "--", "cat", "--", "{{a}}")
	// schema violation: Korean first, raw validator text only with --verbose
	o, err := runErr(t, "", "tool", "test", "log", "--args", `{"n":"x"}`)
	if err == nil || strings.Contains(o+err.Error(), "validating") {
		t.Errorf("raw validator text leaked: %q %v", o, err)
	}
	o, err = runErr(t, "", "tool", "test", "log", "--args", `{"n":"x"}`, "--verbose")
	if err == nil || !strings.Contains(o, "validating") {
		t.Errorf("--verbose must show the raw text: %q %v", o, err)
	}
	// dry-run does not echo the parent's PATH
	t.Setenv("PATH", "/very/secret/home/bin:"+os.Getenv("PATH"))
	if o := mustRun(t, "tool", "test", "ls1", "--dry-run"); strings.Contains(o, "/very/secret") {
		t.Errorf("PATH leaked in dry-run: %s", o)
	}
}
