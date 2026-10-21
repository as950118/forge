package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heonjinjeong/forge/internal/mcp/mcptest"
)

func TestMain(m *testing.M) {
	mcptest.MaybeRunStdio()
	os.Exit(m.Run())
}

func TestMCPLifecycleStdio(t *testing.T) {
	home := forgeHome(t)
	const secret = "ghp_REALSECRETVALUE123"
	t.Setenv("GH_TOKEN_FOR_TEST", secret)

	out := mustRun(t, "mcp", "add", "mock", "--env", "MOCK_TOOLS=alpha,beta", "--env", mcptest.EnvMode+"=ok",
		"--env", "API_TOKEN=${GH_TOKEN_FOR_TEST}", "--", os.Args[0])
	if !strings.Contains(out, "추가됨") {
		t.Errorf("%s", out)
	}
	out = mustRun(t, "mcp", "refresh", "mock", "--timeout", "20s")
	if !strings.Contains(out, "OK   mock") || !strings.Contains(out, "도구 2개") {
		t.Errorf("%s", out)
	}
	for _, args := range [][]string{{"mcp", "list"}, {"mcp", "list", "--json"}, {"mcp", "show", "mock"}, {"mcp", "show", "mock", "--json"}} {
		if o := mustRun(t, args...); strings.Contains(o, secret) {
			t.Errorf("%v leaks secret", args)
		}
	}
	out = mustRun(t, "mcp", "show", "mock")
	if !strings.Contains(out, "API_TOKEN = ${GH_TOKEN_FOR_TEST}") || !strings.Contains(out, "alpha") {
		t.Errorf("%s", out)
	}
	var info struct {
		Enabled   bool `json:"enabled"`
		ToolCount int  `json:"tool_count"`
		Discovery struct{ OK bool }
	}
	if err := json.Unmarshal([]byte(mustRun(t, "mcp", "show", "mock", "--json")), &info); err != nil || !info.Enabled || info.ToolCount != 2 || !info.Discovery.OK {
		t.Errorf("%+v %v", info, err)
	}

	mustRun(t, "mcp", "disable", "mock")
	mustRun(t, "mcp", "disable", "mock", "--client", "opencode")
	mustRun(t, "mcp", "expose", "mock", "--client", "claude-code", "--mode", "gateway")
	out = mustRun(t, "mcp", "show", "mock")
	if !strings.Contains(out, "활성:       no") || !strings.Contains(out, "claude-code=gateway, opencode=hidden") {
		t.Errorf("%s", out)
	}
	mustRun(t, "mcp", "enable", "mock", "--client", "opencode")
	if _, err := runErr(t, "", "mcp", "expose", "mock", "--client", "zzz", "--mode", "hidden"); err == nil {
		t.Error("unknown client")
	}
	if _, err := runErr(t, "n\n", "mcp", "remove", "mock"); err == nil {
		t.Error("remove without confirmation must cancel")
	}
	mustRun(t, "mcp", "remove", "mock", "--yes")
	if out := mustRun(t, "mcp", "list"); !strings.Contains(out, "없습니다") {
		t.Errorf("%s", out)
	}
	files, _ := filepath.Glob(filepath.Join(home, "forge.db*"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), secret) {
			t.Errorf("secret in %s", f)
		}
	}
}

func TestMCPAddRejections(t *testing.T) {
	forgeHome(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"mcp", "add", "a", "--env", "API_KEY=abc", "--", "x"}, "비밀"},
		{[]string{"mcp", "add", "a", "--transport", "http", "--url", "https://h/x", "--header", "Authorization=Bearer abc"}, "비밀"},
		{[]string{"mcp", "add", "a", "--transport", "http", "--url", "ftp://h/x"}, "스킴"},
		{[]string{"mcp", "add", "a", "--transport", "http"}, "--url"},
		{[]string{"mcp", "add", "a"}, "command"},
		{[]string{"mcp", "add", "a", "--url", "https://h"}, "--transport"},
		{[]string{"mcp", "add", "a", "--env", "K=keychain:s/a", "--", "x"}, "keychain"},
		{[]string{"mcp", "add", "bad name", "--", "x"}, "이름"},
		{[]string{"mcp", "add", "a", "--env", "X=sk-live-FAKEFAKEFAKE1234", "--", "x"}, "${ENV_NAME}"},
		{[]string{"mcp", "add", "a", "--transport", "http", "--url", "https://h/x", "--header", "X-Foo=Bearer eyJhbGciOiJIUzI1NiJ9.e30.FAKE"}, "${ENV_NAME}"},
		{[]string{"mcp", "add", "a", "--transport", "http", "--url", "https://h/v1/q9Zx7Lm2Vb8Nc4Rt6Yh1Jd3Kf5Sg0Wa/mcp"}, "URL 경로"},
		{[]string{"mcp", "add", "a", "--transport", "http", "--url", "https://h/mcp?t=q9Zx7Lm2Vb8Nc4Rt6Yh1Jd3Kf5Sg0Wa"}, "쿼리"},
		{[]string{"mcp", "add", "a", "--", "x", "--token", "abc123def456ghi789"}, "인자 #1"},
		{[]string{"mcp", "add", "a", "--", "x", "--api-key=abc"}, "--env KEY=${ENV_NAME}"},
		{[]string{"mcp", "list", "--project"}, "지원하지 않습니다"},
		{[]string{"mcp", "show", "nope"}, "등록된 MCP"},
	}
	for _, c := range cases {
		out, err := runErr(t, "", c.args...)
		if err == nil || !strings.Contains(err.Error()+out, c.want) {
			t.Errorf("%v: err=%v out=%s", c.args, err, out)
		}
	}
	for _, ok := range [][]string{
		{"mcp", "add", "p1", "--env", "LANG=ko_KR.UTF-8", "--env", "DEBUG=1", "--", "x", "--port", "8080", "--verbose"},
		{"mcp", "add", "p2", "--transport", "http", "--url", "https://example.com/mcp", "--header", "Accept=application/json"},
	} {
		mustRun(t, ok...)
	}
	mustRun(t, "mcp", "add", "dup", "--", "x")
	if _, err := runErr(t, "", "mcp", "add", "dup", "--", "x"); err == nil {
		t.Error("duplicate")
	}
}

func TestMCPRefreshFailureOutput(t *testing.T) {
	forgeHome(t)
	mustRun(t, "mcp", "add", "ghost", "--", "forge-no-such-command-xyz")
	out, err := runErr(t, "", "mcp", "refresh", "ghost")
	if err == nil || !strings.Contains(out, "command_not_found") || !strings.Contains(out, "해결:") {
		t.Errorf("err=%v out=%s", err, out)
	}
	out = mustRun(t, "mcp", "show", "ghost")
	if !strings.Contains(out, "마지막 오류") || !strings.Contains(out, "해결 안내") {
		t.Errorf("%s", out)
	}
	out, err = runErr(t, "", "mcp", "refresh", "ghost", "--json")
	if err == nil || !strings.Contains(out, `"error_class": "command_not_found"`) {
		t.Errorf("err=%v out=%s", err, out)
	}
}

func TestMCPRefreshHTTP(t *testing.T) {
	forgeHome(t)
	srv := mcptest.HTTP([]string{"a", "b", "c"}, "d")
	defer srv.Close()
	mustRun(t, "mcp", "add", "web", "--transport", "http", "--url", srv.URL)
	if out := mustRun(t, "mcp", "refresh", "--timeout", "10s"); !strings.Contains(out, "도구 3개") {
		t.Errorf("%s", out)
	}
	if out := mustRun(t, "mcp", "list"); !strings.Contains(out, "web") || !strings.Contains(out, "ok") {
		t.Errorf("%s", out)
	}
}

func TestMCPRefreshVerboseShowsRawDetail(t *testing.T) {
	forgeHome(t)
	dead := mcptest.HTTP(nil, "")
	url := dead.URL
	dead.Close()
	mustRun(t, "mcp", "add", "dead", "--transport", "http", "--url", url)
	out, err := runErr(t, "", "mcp", "refresh", "dead")
	if err == nil || strings.Contains(out, "calling ") || !strings.Contains(out, "connection") || strings.Contains(out, "상세(원문)") {
		t.Errorf("err=%v out=%s", err, out)
	}
	out, err = runErr(t, "", "mcp", "refresh", "dead", "--verbose")
	if err == nil || !strings.Contains(out, "상세(원문)") || !strings.Contains(out, "initialize") {
		t.Errorf("err=%v out=%s", err, out)
	}
}
