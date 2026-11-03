package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/as950118/forge/internal/mcp/mcptest"
	"github.com/as950118/forge/internal/store"
	"github.com/as950118/forge/internal/testenv"
)

func TestMain(m *testing.M) {
	mcptest.MaybeRunStdio()
	os.Exit(testenv.Run(m))
}

func newReg(t *testing.T) (*Registry, string) {
	t.Helper()
	home := t.TempDir()
	if err := store.EnsureLayout(home); err != nil {
		t.Fatal(err)
	}
	conn, err := store.Open(context.Background(), store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &Registry{DB: conn}, home
}

func stdioSpec(name string, env map[string]string, vals ...Value) Spec {
	s := Spec{Name: name, Transport: TransportStdio, Command: os.Args[0], Values: vals}
	for k, v := range env {
		s.Values = append(s.Values, Value{Slot: SlotEnv, Key: k, RefType: RefLiteral, Literal: v, Display: v})
	}
	return s
}

func TestParseValue(t *testing.T) {
	ok := []struct{ slot, in, refType, ref string }{
		{SlotEnv, "GITHUB_TOKEN=${GH}", RefEnv, "GH"},
		{SlotEnv, "LOG_LEVEL=debug", RefLiteral, ""},
		{SlotHeader, "Authorization=${TOK}", RefEnv, "TOK"},
		{SlotHeader, "X-Region=eu", RefLiteral, ""},
	}
	for _, c := range ok {
		v, err := ParseValue(c.slot, c.in)
		if err != nil || v.RefType != c.refType || v.Ref != c.ref {
			t.Errorf("%s: %+v %v", c.in, v, err)
		}
		if v.RefType == RefLiteral && v.IsSecret {
			t.Errorf("literal must be non-secret: %s", c.in)
		}
	}
	bad := map[string]string{
		"API_KEY=sk-live-123": "비밀",
		"password=hunter2":    "비밀",
		"X_AUTH=abc":          "비밀",
		"NOEQUALS":            "KEY=VALUE",
		"K=":                  "비어",
		"1BAD=x":              "이름",
		"A=keychain:svc/acct": "keychain",
		"B=Bearer ${TOKEN}":   "전체",
		"C={env:FOO}x":        "전체",
	}
	for in, want := range bad {
		_, err := ParseValue(SlotEnv, in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err=%v want %q", in, err, want)
		}
		if err != nil && (strings.Contains(err.Error(), "sk-live-123") || strings.Contains(err.Error(), "hunter2")) {
			t.Errorf("error leaks value: %v", err)
		}
	}
	if _, err := ParseValue(SlotHeader, "Authorization=Bearer abc"); err == nil {
		t.Error("literal Authorization header must be refused")
	}
}

func TestSpecValidate(t *testing.T) {
	cases := map[string]Spec{
		"이름":        {Name: "-bad", Transport: TransportStdio, Command: "x"},
		"command":   {Name: "a", Transport: TransportStdio},
		"스킴":        {Name: "a", Transport: TransportHTTP, URL: "ftp://h/x"},
		"--url":     {Name: "a", Transport: TransportSSE},
		"호스트":       {Name: "a", Transport: TransportHTTP, URL: "http:///x"},
		"사용자":       {Name: "a", Transport: TransportHTTP, URL: "https://u:p@h/x"},
		"쿼리":        {Name: "a", Transport: TransportHTTP, URL: "https://h/x?api_key=1"},
		"명령/인자":     {Name: "a", Transport: TransportHTTP, URL: "https://h/x", Command: "x"},
		"transport": {Name: "a", Transport: "ws", URL: "https://h"},
		"--header":  {Name: "a", Transport: TransportStdio, Command: "x", Values: []Value{{Slot: SlotHeader, Key: "A", RefType: RefLiteral, Literal: "b"}}},
		"--env":     {Name: "a", Transport: TransportHTTP, URL: "https://h", Values: []Value{{Slot: SlotEnv, Key: "A", RefType: RefLiteral, Literal: "b"}}},
		"중복":        {Name: "a", Transport: TransportStdio, Command: "x", Values: []Value{{Slot: SlotEnv, Key: "A", RefType: RefLiteral, Literal: "b"}, {Slot: SlotEnv, Key: "A", RefType: RefLiteral, Literal: "c"}}},
		"참조":        {Name: "a", Transport: TransportStdio, Command: "x", Values: []Value{{Slot: SlotEnv, Key: "MY_TOKEN", RefType: RefLiteral, Literal: "b"}}},
	}
	for want, s := range cases {
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	good := []Spec{
		{Name: "a1_b-c", Transport: TransportStdio, Command: "x", Args: []string{"-y"}},
		{Name: "h", Transport: TransportHTTP, URL: "https://h/x?v=1"},
		{Name: "s", Transport: TransportSSE, URL: "http://127.0.0.1:1/sse"},
	}
	for _, s := range good {
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", s.Name, err)
		}
	}
}

func TestNoPlaintextSecretInDB(t *testing.T) {
	reg, home := newReg(t)
	ctx := context.Background()
	const secret = "SUPER-SECRET-VALUE-9f3a"
	t.Setenv("GH_TOK", secret)
	tok, _ := ParseValue(SlotEnv, "GITHUB_TOKEN=${GH_TOK}")
	lit, _ := ParseValue(SlotEnv, "LOG_LEVEL=debug")
	if err := reg.Add(ctx, stdioSpec("gh", nil, tok, lit)); err != nil {
		t.Fatal(err)
	}
	hv, _ := ParseValue(SlotHeader, "Authorization=${GH_TOK}")
	if err := reg.Add(ctx, Spec{Name: "web", Transport: TransportHTTP, URL: "https://example.com/mcp", Values: []Value{hv}}); err != nil {
		t.Fatal(err)
	}
	info, err := reg.Get(ctx, "gh")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range info.Values {
		if v.Key == "GITHUB_TOKEN" && (v.RefType != RefEnv || v.Ref != "GH_TOK" || v.Display != "${GH_TOK}" || v.Literal != "") {
			t.Errorf("%+v", v)
		}
	}
	// A forged literal for a secret must be rejected by the schema CHECK as well.
	if _, err := reg.DB.Exec(`INSERT INTO mcp_server_values(server_id, slot, key, is_secret, ref_type, value) VALUES (1,'env','X',1,'literal','p')`); err == nil {
		t.Error("schema must refuse secret literal")
	}
	// Dump every DB-related file (incl. WAL) and look for the resolved secret.
	_, _ = reg.DB.Exec(`PRAGMA wal_checkpoint(FULL)`)
	files, _ := filepath.Glob(filepath.Join(home, "forge.db*"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), secret) {
			t.Errorf("plaintext secret found in %s", f)
		}
	}
}

func TestAddDuplicateRemoveCascade(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	if err := reg.Add(ctx, stdioSpec("one", map[string]string{"A": "b"})); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(ctx, stdioSpec("one", nil)); err == nil || !strings.Contains(err.Error(), "이미 등록") {
		t.Errorf("dup: %v", err)
	}
	if err := reg.SetExposure(ctx, "one", "opencode", ExposureHidden); err != nil {
		t.Fatal(err)
	}
	i, _ := reg.Get(ctx, "one")
	if i.Exposure["opencode"] != ExposureHidden || i.Exposure["claude-code"] != ExposureDirect {
		t.Errorf("exposure %v", i.Exposure)
	}
	if err := reg.SetExposure(ctx, "one", "nope", ExposureHidden); err == nil {
		t.Error("unknown client")
	}
	if err := reg.SetExposure(ctx, "one", "opencode", "bogus"); err == nil {
		t.Error("bad mode")
	}
	if err := reg.SetEnabled(ctx, "one", false); err != nil {
		t.Fatal(err)
	}
	if i, _ := reg.Get(ctx, "one"); i.Enabled {
		t.Error("still enabled")
	}
	if err := reg.Remove(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	var n int
	for _, q := range []string{`mcp_server_values`, `settings WHERE key LIKE 'mcp.exposure.%'`} {
		_ = reg.DB.QueryRow(`SELECT count(*) FROM ` + q).Scan(&n)
		if n != 0 {
			t.Errorf("%s leftover %d", q, n)
		}
	}
	if err := reg.Remove(ctx, "one"); err == nil {
		t.Error("remove missing must fail")
	}
}

func TestConcurrentAdd(t *testing.T) {
	home := t.TempDir()
	if err := store.EnsureLayout(home); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const n = 8
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			conn, err := store.Open(ctx, store.DBPath(home))
			if err != nil {
				errs[i] = err
				return
			}
			defer conn.Close()
			errs[i] = (&Registry{DB: conn}).Add(ctx, stdioSpec(fmt.Sprintf("par-%d", i), map[string]string{"A": "b"}))
		}(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("worker %d: %v", i, e)
		}
	}
	conn, _ := store.Open(ctx, store.DBPath(home))
	defer conn.Close()
	l, _ := (&Registry{DB: conn}).List(ctx)
	if len(l) != n {
		t.Errorf("servers=%d", len(l))
	}
}

func stdioEnv(mode, tools, desc string) map[string]string {
	m := map[string]string{mcptest.EnvMode: mode}
	if tools != "" {
		m["MOCK_TOOLS"] = tools
	}
	if desc != "" {
		m["MOCK_DESC"] = desc
	}
	return m
}

func toolNames(i *Info, stale bool) []string {
	var out []string
	for _, t := range i.Tools {
		if t.Stale == stale {
			out = append(out, t.Name)
		}
	}
	return out
}

func TestRefreshStdioCacheUpdateAndFailurePreserves(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	spec := stdioSpec("mock", stdioEnv("ok", "alpha,beta", "v1"))
	if err := reg.Add(ctx, spec); err != nil {
		t.Fatal(err)
	}
	res, err := reg.Refresh(ctx, "mock", RefreshOptions{Timeout: 20 * time.Second})
	if err != nil || !res.OK || res.ToolCount != 2 || res.Added != 2 || res.ServerName != "mock-mcp" {
		t.Fatalf("%+v %v", res, err)
	}
	info, _ := reg.Get(ctx, "mock")
	if len(info.Tools) != 2 || info.Tools[0].SchemaHash == "" || !strings.Contains(string(info.Tools[0].InputSchema), `"type":"object"`) || info.Discovery == nil || !info.Discovery.OK {
		t.Fatalf("%+v", info)
	}
	h1 := info.Tools[0].SchemaHash

	// Re-discovery: tool list and description change -> changed/added/stale.
	_, _ = reg.DB.Exec(`UPDATE mcp_server_values SET value='alpha,gamma' WHERE key='MOCK_TOOLS'`)
	_, _ = reg.DB.Exec(`UPDATE mcp_server_values SET value='v2' WHERE key='MOCK_DESC'`)
	res, err = reg.Refresh(ctx, "mock", RefreshOptions{Timeout: 20 * time.Second})
	if err != nil || !res.OK || res.Added != 1 || res.Changed != 1 || res.Removed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	info, _ = reg.Get(ctx, "mock")
	if got := toolNames(info, true); len(got) != 1 || got[0] != "beta" {
		t.Errorf("stale=%v", got)
	}
	if info.ToolCount != 2 || info.Tools[0].SchemaHash == h1 {
		t.Errorf("count=%d hash unchanged", info.ToolCount)
	}

	// Failure keeps cache, records the error.
	_, _ = reg.DB.Exec(`UPDATE mcp_server_values SET value='exit' WHERE key=?`, mcptest.EnvMode)
	res, err = reg.Refresh(ctx, "mock", RefreshOptions{Timeout: 20 * time.Second})
	if err != nil || res.OK || res.ErrorClass != ClassProcessExit || res.Hint == "" {
		t.Fatalf("%+v %v", res, err)
	}
	after, _ := reg.Get(ctx, "mock")
	if len(after.Tools) != len(info.Tools) || after.Discovery.OK || ErrorClassOf(after.Discovery.Error) != ClassProcessExit {
		t.Errorf("cache not preserved: %+v", after.Discovery)
	}
	if after.Discovery.ServerName != "mock-mcp" {
		t.Error("last success info lost")
	}
}

func TestRefreshStdioFailures(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	check := func(name string, s Spec, opt RefreshOptions, class, contains string) {
		t.Helper()
		if err := reg.Add(ctx, s); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		res, err := reg.Refresh(ctx, name, opt)
		if err != nil || res.OK || res.ErrorClass != class {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
		if !strings.Contains(res.Error, contains) || res.Hint == "" {
			t.Errorf("%s: error=%q hint=%q", name, res.Error, res.Hint)
		}
		if d := time.Since(start); d > 15*time.Second {
			t.Errorf("%s took %s", name, d)
		}
	}
	check("nocmd", Spec{Name: "nocmd", Transport: TransportStdio, Command: "forge-no-such-cmd-xyz"}, RefreshOptions{}, ClassCommandNotFound, "forge-no-such-cmd-xyz")
	check("badpath", Spec{Name: "badpath", Transport: TransportStdio, Command: "/nonexistent/dir/server"}, RefreshOptions{}, ClassCommandNotFound, "/nonexistent")
	check("hang", stdioSpec("hang", stdioEnv("hang", "", "")), RefreshOptions{Timeout: 700 * time.Millisecond}, ClassTimeout, "응답이 없습니다")
	e := stdioEnv("exit", "", "")
	e["MOCK_STDERR"] = "missing REQUIRED_CONFIG"
	check("exits", stdioSpec("exits", e), RefreshOptions{Timeout: 10 * time.Second}, ClassProcessExit, "missing REQUIRED_CONFIG")
	check("noise", stdioSpec("noise", stdioEnv("noise", "", "")), RefreshOptions{Timeout: 700 * time.Millisecond}, ClassTimeout, "")
}

func TestRefreshEnvRefResolvedOnlyForChild(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	const secret = "tok-12345-abcdef"
	t.Setenv("FORGE_TEST_REF", secret)
	// The mock puts MOCK_DESC into tool descriptions: pass the secret via a reference.
	v, _ := ParseValue(SlotEnv, "MOCK_DESC=${FORGE_TEST_REF}")
	if err := reg.Add(ctx, stdioSpec("envref", stdioEnv("ok", "t1", ""), v)); err != nil {
		t.Fatal(err)
	}
	res, err := reg.Refresh(ctx, "envref", RefreshOptions{Timeout: 20 * time.Second})
	if err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	info, _ := reg.Get(ctx, "envref")
	// The child got the resolved value (it echoes it in the description) but the
	// cache holds only the masked text.
	if len(info.Tools) != 1 || info.Tools[0].Description != "***" {
		t.Errorf("child did not receive resolved env or cache is unmasked: %+v", info.Tools)
	}
	// Unset reference -> env_missing, never run.
	v2, _ := ParseValue(SlotEnv, "OTHER=${FORGE_TEST_UNSET_XYZ}")
	if err := reg.Add(ctx, stdioSpec("envmiss", stdioEnv("ok", "", ""), v2)); err != nil {
		t.Fatal(err)
	}
	res, _ = reg.Refresh(ctx, "envmiss", RefreshOptions{})
	if res.OK || res.ErrorClass != ClassEnvMissing || !strings.Contains(res.Error, "FORGE_TEST_UNSET_XYZ") {
		t.Errorf("%+v", res)
	}
}

func TestRefreshHTTPAndSSE(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	hs := mcptest.HTTP([]string{"h1", "h2", "h3"}, "http tool")
	defer hs.Close()
	ss := mcptest.SSE([]string{"s1"}, "sse tool")
	defer ss.Close()
	if err := reg.Add(ctx, Spec{Name: "web", Transport: TransportHTTP, URL: hs.URL}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(ctx, Spec{Name: "legacy", Transport: TransportSSE, URL: ss.URL}); err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]int{"web": 3, "legacy": 1} {
		res, err := reg.Refresh(ctx, name, RefreshOptions{Timeout: 10 * time.Second})
		if err != nil || !res.OK || res.ToolCount != n {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
	}
}

func TestRefreshHTTPAuthAndStatus(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	const token = "Bearer tkn-xyz-777"
	t.Setenv("FORGE_TEST_BEARER", token)
	secured := mcptest.AuthHTTP([]string{"x"}, "d", token)
	defer secured.Close()

	// without header -> auth_required
	if err := reg.Add(ctx, Spec{Name: "noauth", Transport: TransportHTTP, URL: secured.URL}); err != nil {
		t.Fatal(err)
	}
	res, _ := reg.Refresh(ctx, "noauth", RefreshOptions{Timeout: 10 * time.Second})
	if res.OK || res.ErrorClass != ClassAuthRequired || !strings.Contains(res.Hint, "Authorization") {
		t.Fatalf("%+v", res)
	}
	// with header reference -> ok, and the token never appears in the stored error/DB
	hv, _ := ParseValue(SlotHeader, "Authorization=${FORGE_TEST_BEARER}")
	if err := reg.Add(ctx, Spec{Name: "auth", Transport: TransportHTTP, URL: secured.URL, Values: []Value{hv}}); err != nil {
		t.Fatal(err)
	}
	res, err := reg.Refresh(ctx, "auth", RefreshOptions{Timeout: 10 * time.Second})
	if err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	// non-MCP endpoint answering 404 -> http_status
	nf := httptest.NewServer(http.NotFoundHandler())
	defer nf.Close()
	if err := reg.Add(ctx, Spec{Name: "nf", Transport: TransportHTTP, URL: nf.URL}); err != nil {
		t.Fatal(err)
	}
	res, _ = reg.Refresh(ctx, "nf", RefreshOptions{Timeout: 10 * time.Second})
	if res.OK || res.ErrorClass != ClassHTTPStatus || !strings.Contains(res.Error, "404") {
		t.Errorf("%+v", res)
	}
	// closed port -> connection
	dead := mcptest.HTTP(nil, "")
	url := dead.URL
	dead.Close()
	if err := reg.Add(ctx, Spec{Name: "dead", Transport: TransportHTTP, URL: url}); err != nil {
		t.Fatal(err)
	}
	res, _ = reg.Refresh(ctx, "dead", RefreshOptions{Timeout: 10 * time.Second})
	if res.OK || res.ErrorClass != ClassConnection {
		t.Errorf("%+v", res)
	}
	if err := reg.Add(ctx, Spec{Name: "sse404", Transport: TransportSSE, URL: nf.URL}); err != nil {
		t.Fatal(err)
	}
	res, _ = reg.Refresh(ctx, "sse404", RefreshOptions{Timeout: 10 * time.Second})
	if res.OK || res.ErrorClass != ClassHTTPStatus {
		t.Errorf("%+v", res)
	}
	// the stored error never contains the resolved token
	var stored string
	_ = reg.DB.QueryRow(`SELECT group_concat(COALESCE(error,'')) FROM mcp_discovery`).Scan(&stored)
	if strings.Contains(stored, "tkn-xyz-777") {
		t.Error("token leaked into discovery error")
	}
}

func TestSanitizeRedactsSecrets(t *testing.T) {
	got := sanitize("boom tok-abc failed tok-abc", map[string]bool{"tok-abc": true})
	if strings.Contains(got, "tok-abc") {
		t.Error(got)
	}
}

// ---- value-pattern secret detection -------------------------------------

// All values below are fake.
func TestLooksSecretValue(t *testing.T) {
	secret := []string{
		"sk-live-FAKEFAKEFAKE1234", "sk_test_FAKEFAKE1234", "ghp_FAKEFAKEFAKE1234abcd", "gho_FAKEFAKEFAKE1234",
		"github_pat_11FAKEFAKE_abcdefghijkl", "xoxb-1234-FAKE-abcdefgh", "AKIAFAKEFAKEFAKE1234", "AIzaFAKEFAKEFAKE_abcdefghij",
		"eyJhbGciOiJIUzI1NiJ9.e30.FAKE", "glpat-FAKEFAKEFAKE1234", "npm_FAKEFAKEFAKE1234abcd",
		"Bearer eyJhbGciOiJIUzI1NiJ9", "Basic dXNlcjpwYXNzd29yZDEyMzQ1", "Bearer FAKEtoken0123456789abcd",
		"q9Zx7Lm2Vb8Nc4Rt6Yh1Jd3Kf5Sg0Wa", "a1B2c3D4e5F6g7H8i9J0k1L2m3N4", "0123456789abcdef0123456789abcdef",
	}
	for _, v := range secret {
		if !LooksSecretValue(v) {
			t.Errorf("should look secret: %q", v)
		}
	}
	plain := []string{
		"ko_KR.UTF-8", "1", "debug", "true", "application/json", "text/event-stream", "Bearer", "basic configuration needed here",
		"/Users/someone/projects/my-long-directory-name/subdir", "@modelcontextprotocol/server-filesystem",
		"mcp-server-sequential-thinking-for-everyone", "task-runner-for-all-the-things", "123456789012345678901234567890",
		"550e8400-e29b-41d4-a716-446655440000", "https://example.com/mcp", "v1", "latest", "${TOKEN}", "",
		"UTF-8", "C.UTF-8", "info,warn,error", "disk-usage-report-generator", "my-super-long-package-name-here",
	}
	for _, v := range plain {
		if LooksSecretValue(v) {
			t.Errorf("false positive: %q", v)
		}
	}
}

func TestParseValueByValuePattern(t *testing.T) {
	for _, c := range []struct{ slot, in string }{
		{SlotEnv, "X=sk-live-FAKEFAKEFAKE1234"},
		{SlotEnv, "WHATEVER=ghp_FAKEFAKEFAKE1234abcd"},
		{SlotHeader, "X-Foo=Bearer eyJhbGciOiJIUzI1NiJ9.e30.FAKE"},
		{SlotHeader, "X-Foo=q9Zx7Lm2Vb8Nc4Rt6Yh1Jd3Kf5Sg0Wa"},
	} {
		_, err := ParseValue(c.slot, c.in)
		if err == nil || !strings.Contains(err.Error(), "${ENV_NAME}") {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		_, val, _ := strings.Cut(c.in, "=")
		if strings.Contains(err.Error(), val) {
			t.Errorf("error leaks value: %v", err)
		}
	}
	for _, c := range []struct{ slot, in string }{
		{SlotEnv, "LANG=ko_KR.UTF-8"}, {SlotEnv, "DEBUG=1"}, {SlotHeader, "Accept=application/json"},
	} {
		if _, err := ParseValue(c.slot, c.in); err != nil {
			t.Errorf("%s: %v", c.in, err)
		}
	}
	// programmatic Specs are validated too
	s := stdioSpec("a", map[string]string{"HARMLESS": "sk-live-FAKEFAKEFAKE1234"})
	s.Command = "x"
	if err := s.Validate(); err == nil {
		t.Error("Validate must refuse secret-looking literal")
	}
}

func TestValidateURLValuePatterns(t *testing.T) {
	bad := []string{
		"https://h.example/v1/q9Zx7Lm2Vb8Nc4Rt6Yh1Jd3Kf5Sg0Wa/mcp",
		"https://h.example/mcp?t=q9Zx7Lm2Vb8Nc4Rt6Yh1Jd3Kf5Sg0Wa",
		"https://h.example/mcp?sig=ghp_FAKEFAKEFAKE1234abcd",
		"https://h.example/mcp?x=1&y=eyJhbGciOiJIUzI1NiJ9.e30.FAKE",
		"https://h.example/sk-live-FAKEFAKEFAKE1234/mcp",
		"https://h.example/mcp#q9Zx7Lm2Vb8Nc4Rt6Yh1Jd3Kf5Sg0Wa",
	}
	for _, u := range bad {
		s := Spec{Name: "a", Transport: TransportHTTP, URL: u}
		err := s.Validate()
		if err == nil || strings.Contains(err.Error(), "Zx7Lm2") || strings.Contains(err.Error(), "FAKEFAKE") || strings.Contains(err.Error(), "eyJ") {
			t.Errorf("%s: err=%v", u, err)
		}
	}
	for _, u := range []string{"https://example.com/mcp", "https://example.com/v1/mcp?lang=ko&page=2", "http://127.0.0.1:8080/sse", "https://api.example.com/v2/servers/my-long-server-name-docs/mcp"} {
		s := Spec{Name: "a", Transport: TransportHTTP, URL: u}
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestCheckArgs(t *testing.T) {
	bad := [][]string{
		{"--token=abc123def456"}, {"--token", "abc123def456ghi789"}, {"--api-key=xyz"}, {"--api-key", "xyz"},
		{"--apiKey", "xyz"}, {"--password", "hunter2"}, {"--password=hunter2"}, {"--secret", "s3cr3t"}, {"--auth", "abc"},
		{"--access-token=abc"}, {"-p", "hunter2"}, {"-p=hunter2"}, {"--client-secret=abc"},
		{"-y", "pkg", "--token", "abc"}, {"API_KEY=abc"}, {"--env=GITHUB_TOKEN=abc"}, {"-e", "DB_PASSWORD=abc"},
		{"Authorization: Bearer abc"}, {"--header=X-Api-Key: abc"},
		{"postgres://user:pw@localhost/db"}, {"https://h.example/mcp?token=abc"},
		{"ghp_FAKEFAKEFAKE1234abcd"}, {"--flag=sk-live-FAKEFAKEFAKE1234"}, {"--url", "https://h.example/q9Zx7Lm2Vb8Nc4Rt6Yh1Jd3Kf5Sg0Wa/mcp"},
	}
	for _, a := range bad {
		err := CheckArgs(a)
		if err == nil {
			t.Errorf("must refuse: %v", a)
			continue
		}
		if !strings.Contains(err.Error(), "--env") || !strings.Contains(err.Error(), "${ENV_NAME}") {
			t.Errorf("%v: advice missing: %v", a, err)
		}
		for _, p := range a {
			for _, leak := range []string{"abc123def456", "hunter2", "s3cr3t", "FAKEFAKE", "Zx7Lm2", "pw@"} {
				if strings.Contains(p, leak) && strings.Contains(err.Error(), leak) {
					t.Errorf("%v: error leaks %q: %v", a, leak, err)
				}
			}
		}
	}
	good := [][]string{
		{"-y", "@modelcontextprotocol/server-filesystem", "/Users/me/projects"},
		{"--port", "8080"}, {"--port=8080"}, {"--verbose"}, {"-p", "8080"}, {"-p", "8080:80"}, {"--host", "127.0.0.1"},
		{"--token-file", "/etc/token"}, {"--token-file=/etc/token"}, {"--no-auth", "serve"}, {"--key-file", "k.pem"},
		{"--public-key", "ssh-ed25519"}, {"--keyboard", "us"}, {"--auth-type", "none"}, {"--password-stdin"},
		{"--token", "${GH_TOKEN}"}, {"--token=${GH_TOKEN}"}, {"--token"}, {"--url", "https://example.com/mcp"},
		{"server.js", "--log-level", "debug", "--config", "./config.json"}, {"--model=gpt-4o-mini"}, {"--timeout", "30"},
		{"mcp-server-sequential-thinking-for-everyone"}, {"-e", "LANG=ko_KR.UTF-8"}, {"Accept: application/json"},
		{"--max-tokens", "1000"}, {"--token-limit=5000"}, {"-p", "/tmp/x"},
	}
	for _, a := range good {
		if err := CheckArgs(a); err != nil {
			t.Errorf("false positive %v: %v", a, err)
		}
	}
	// Validate wires it in for stdio.
	if err := (&Spec{Name: "a", Transport: TransportStdio, Command: "x", Args: []string{"--token", "abc123def456"}}).Validate(); err == nil {
		t.Error("Validate must refuse secret args")
	}
}

// ---- diagnostics ---------------------------------------------------------

// If the masking registration in resolveValues is removed, the resolved secret
// echoed by a server on stderr reaches the refresh diagnostics.
func TestRefreshMasksResolvedSecretEchoedOnStderr(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	const secret = "FAKE-echo-secret-7f3a9c"
	t.Setenv("FORGE_TEST_ECHO", secret)
	v, err := ParseValue(SlotEnv, "MOCK_STDERR=${FORGE_TEST_ECHO}") // mock prints this to stderr, then exits 3
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(ctx, stdioSpec("echo", stdioEnv("exit", "", ""), v)); err != nil {
		t.Fatal(err)
	}
	res, err := reg.Refresh(ctx, "echo", RefreshOptions{Timeout: 10 * time.Second})
	if err != nil || res.OK || res.ErrorClass != ClassProcessExit {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(res.Error, "stderr:") || !strings.Contains(res.Error, "***") {
		t.Errorf("stderr not masked with ***: %q", res.Error)
	}
	var stored string
	_ = reg.DB.QueryRow(`SELECT COALESCE(error,'') FROM mcp_discovery`).Scan(&stored)
	for name, s := range map[string]string{"error": res.Error, "detail": res.Detail, "hint": res.Hint, "stored": stored} {
		if strings.Contains(s, secret) {
			t.Errorf("secret leaked in %s: %q", name, s)
		}
	}
}

func TestDiagTransportKeepsAuthStatus(t *testing.T) {
	dt := &diagTransport{}
	dt.record(401)
	dt.record(501)
	if dt.lastStatus() != 401 {
		t.Errorf("auth status overwritten: %d", dt.lastStatus())
	}
	dt = &diagTransport{}
	dt.record(404)
	dt.record(500)
	if dt.lastStatus() != 404 {
		t.Errorf("first error must be kept: %d", dt.lastStatus())
	}
	dt = &diagTransport{}
	dt.record(404)
	dt.record(403)
	if dt.lastStatus() != 403 {
		t.Errorf("auth must take priority: %d", dt.lastStatus())
	}
}

func TestRefreshAuthRequiredNotMisclassifiedByLaterStatus(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	// POST (initialize) -> 401 with a session id, then DELETE -> 501.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Error(w, "nope", http.StatusNotImplemented)
	}))
	defer srv.Close()
	if err := reg.Add(ctx, Spec{Name: "mix", Transport: TransportHTTP, URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	res, _ := reg.Refresh(ctx, "mix", RefreshOptions{Timeout: 10 * time.Second})
	if res.OK || res.ErrorClass != ClassAuthRequired {
		t.Fatalf("%+v", res)
	}
}

func TestRefreshStdioFloodWithoutNewlineIsCut(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	if err := reg.Add(ctx, stdioSpec("flood", stdioEnv("flood", "", ""))); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := reg.Refresh(ctx, "flood", RefreshOptions{Timeout: 20 * time.Second})
	if err != nil || res.OK || res.ErrorClass != ClassProtocol || !strings.Contains(res.Error, "MiB") {
		t.Fatalf("%+v %v", res, err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("took %s (process not cut)", d)
	}
}

func TestLineLimitResetsOnNewline(t *testing.T) {
	l := &lineLimit{max: 10}
	if l.add([]byte("12345\n67890\n123456789")) || l.add([]byte("x")) && false {
		t.Error("lines under the cap must pass")
	}
	if !l.add([]byte("xx")) {
		t.Error("unterminated run over the cap must trip")
	}
}

func TestRefreshErrorsHideSDKWording(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	dead := mcptest.HTTP(nil, "")
	url := dead.URL
	dead.Close()
	nf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })) // 200 but not MCP
	defer nf.Close()
	for name, u := range map[string]string{"dead": url, "notmcp": nf.URL} {
		if err := reg.Add(ctx, Spec{Name: name, Transport: TransportHTTP, URL: u}); err != nil {
			t.Fatal(err)
		}
		res, _ := reg.Refresh(ctx, name, RefreshOptions{Timeout: 10 * time.Second})
		if res.OK {
			t.Fatalf("%s: %+v", name, res)
		}
		if strings.Contains(res.Error, "calling ") || strings.Contains(res.Error, "sending ") {
			t.Errorf("%s: SDK wording in user message: %q", name, res.Error)
		}
		if !strings.Contains(res.Detail, "initialize") {
			t.Errorf("%s: raw detail missing: %q", name, res.Detail)
		}
	}
}

func TestRefreshDoesNotInheritParentEnv(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	// Only in the parent environment, never declared on the server.
	t.Setenv("MOCK_TOOLS", "from_parent")
	t.Setenv("NODE_OPTIONS", "--require=evil")
	t.Setenv("npm_config__authToken", "tok")
	if err := reg.Add(ctx, stdioSpec("undecl", stdioEnv("ok", "", ""))); err != nil {
		t.Fatal(err)
	}
	res, err := reg.Refresh(ctx, "undecl", RefreshOptions{Timeout: 20 * time.Second})
	if err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	info, _ := reg.Get(ctx, "undecl")
	if got := toolNames(info, false); len(got) != 2 || got[0] != "alpha" {
		t.Errorf("parent MOCK_TOOLS leaked into discovery child: %v", got)
	}
	// Declared variables are passed.
	if err := reg.Add(ctx, stdioSpec("decl", stdioEnv("ok", "from_decl", ""))); err != nil {
		t.Fatal(err)
	}
	if res, err = reg.Refresh(ctx, "decl", RefreshOptions{Timeout: 20 * time.Second}); err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	info, _ = reg.Get(ctx, "decl")
	if got := toolNames(info, false); len(got) != 1 || got[0] != "from_decl" {
		t.Errorf("declared env not passed: %v", got)
	}
	for _, kv := range MinimalEnv() {
		if strings.HasPrefix(kv, "NODE_OPTIONS=") || strings.HasPrefix(kv, "npm_config__authToken=") {
			t.Errorf("unsafe variable in MinimalEnv: %s", kv)
		}
	}
	if h := Hint(ClassProcessExit, ""); !strings.Contains(h, "--env KEY=${KEY}") {
		t.Errorf("hint: %s", h)
	}
}

func TestMinimalEnvDropsCredentialBearingValues(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://user:pw@proxy.example:8080")
	t.Setenv("ALL_PROXY", "socks5://onlyuser@proxy.example:1080")
	t.Setenv("NPM_CONFIG_REGISTRY", "https://tok@registry.example/")
	t.Setenv("REQUESTS_CA_BUNDLE", "https://u:p@ca.example/bundle.pem")
	t.Setenv("HTTP_PROXY", "http://proxy.example:8080")
	t.Setenv("NO_PROXY", "localhost,127.0.0.1")
	t.Setenv("npm_config_registry", "https://registry.example/path@v1") // '@' only after the host: no userinfo
	env := strings.Join(MinimalEnv(), "\n")
	for _, bad := range []string{"HTTPS_PROXY=", "ALL_PROXY=", "NPM_CONFIG_REGISTRY=", "REQUESTS_CA_BUNDLE=", "user:pw", "onlyuser", "tok@"} {
		if strings.Contains(env, bad) {
			t.Errorf("%q passed through MinimalEnv:\n%s", bad, env)
		}
	}
	for _, good := range []string{"HTTP_PROXY=http://proxy.example:8080", "NO_PROXY=localhost,127.0.0.1", "npm_config_registry=https://registry.example/path@v1"} {
		if !strings.Contains(env, good) {
			t.Errorf("%q wrongly dropped", good)
		}
	}
	dropped := strings.Join(MinimalEnvDropped(), ",")
	if !strings.Contains(dropped, "HTTPS_PROXY") || strings.Contains(dropped, "pw") {
		t.Errorf("dropped names: %s", dropped)
	}
	// declaring the variable passes it to the child explicitly
	in := &Info{Name: "x", Transport: TransportStdio, Command: os.Args[0]}
	tb, fail := newTransport(in, map[string]map[string]string{SlotEnv: {"HTTPS_PROXY": "http://user:pw@proxy.example:8080"}}, MinimalEnv())
	if fail != nil {
		t.Fatal(fail)
	}
	if got := strings.Join(tb.cmd.Env, "\n"); !strings.Contains(got, "HTTPS_PROXY=http://user:pw@proxy.example:8080") {
		t.Errorf("declared variable not passed")
	}
	tb, _ = newTransport(in, nil, MinimalEnv())
	if got := strings.Join(tb.cmd.Env, "\n"); strings.Contains(got, "HTTPS_PROXY=") {
		t.Errorf("undeclared credential variable reached the child")
	}
	if h := Hint(ClassProcessExit, ""); !strings.Contains(h, "HTTPS_PROXY") {
		t.Errorf("hint does not explain dropped proxy variables: %s", h)
	}
}

// A server that echoes a declared (resolved) env value in a tool description must
// not get it into the cache, and a changed secret is not a tool change.
func TestRefreshMasksSecretsInCache(t *testing.T) {
	reg, home := newReg(t)
	ctx := context.Background()
	const secret = "tok-ABCDEFGH-12345678"
	t.Setenv("FORGE_TEST_MASK", secret)
	v, _ := ParseValue(SlotEnv, "MOCK_DESC=${FORGE_TEST_MASK}")
	if err := reg.Add(ctx, stdioSpec("mask", stdioEnv("ok", "t1", ""), v)); err != nil {
		t.Fatal(err)
	}
	res, err := reg.Refresh(ctx, "mask", RefreshOptions{Timeout: 20 * time.Second})
	if err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	info, _ := reg.Get(ctx, "mask")
	if len(info.Tools) != 1 || info.Tools[0].Description != "***" {
		t.Fatalf("%+v", info.Tools)
	}
	if _, err := reg.DB.ExecContext(ctx, `PRAGMA wal_checkpoint(FULL)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := reg.DB.QueryRowContext(ctx, `SELECT count(*) FROM mcp_tools WHERE description LIKE '%'||?||'%' OR input_schema LIKE '%'||?||'%' OR name LIKE '%'||?||'%'`, secret, secret, secret).Scan(&n); err != nil || n != 0 {
		t.Errorf("secret in mcp_tools: n=%d err=%v", n, err)
	}
	for _, f := range []string{"forge.db", "forge.db-wal"} {
		if b, err := os.ReadFile(filepath.Join(home, f)); err == nil && strings.Contains(string(b), secret) {
			t.Errorf("%s contains the secret", f)
		}
	}
	// Rotating the secret changes the (masked) description input but not the stored
	// masked content: the tool must not be reported as changed.
	t.Setenv("FORGE_TEST_MASK", "tok-ZZZZZZZZ-99999999")
	res, err = reg.Refresh(ctx, "mask", RefreshOptions{Timeout: 20 * time.Second})
	if err != nil || !res.OK || res.Changed != 0 || res.Added != 0 {
		t.Errorf("secret rotation reported as change: %+v %v", res, err)
	}
}

func TestMaskDiscoveredNameAndSchema(t *testing.T) {
	secrets := map[string]bool{"s3cr3t-value": true, "s3cr3t": true}
	d := &discovered{srvName: "srv s3cr3t", tools: []*sdk.Tool{{
		Name: "t_s3cr3t-value", Description: "d s3cr3t-value",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"k":      map[string]any{"type": "string", "description": "x \"s3cr3t-value\" y", "default": "s3cr3t"},
			"s3cr3t": map[string]any{"enum": []any{"s3cr3t-value", 1.0}},
		}},
	}}}
	maskDiscovered(d, secrets)
	raw, _ := json.Marshal(d.tools[0])
	if strings.Contains(string(raw), "s3cr3t") || strings.Contains(d.srvName, "s3cr3t") {
		t.Errorf("unmasked: %s %s", raw, d.srvName)
	}
	if d.tools[0].Name != "t_***" {
		t.Errorf("longest secret should be replaced whole: %q", d.tools[0].Name)
	}
}

// TASK-0020 AC-1: a scheme-less proxy value with userinfo ("u:pw@proxy:1") is a
// credential too; plain host:port values and unrelated '@' values pass.
func TestMinimalEnvDropsSchemelessProxyCredentials(t *testing.T) {
	t.Setenv("HTTPS_PROXY", " u:pw@proxy.example:1") // leading blank is trimmed
	t.Setenv("https_proxy", "onlyuser@proxy.example:1")
	t.Setenv("ALL_PROXY", "socks5://proxy.example:1080")
	t.Setenv("HTTP_PROXY", "proxy.example:3128")
	t.Setenv("NO_PROXY", "a@b,localhost")
	t.Setenv("npm_config_registry", "tok@registry.example/path")
	t.Setenv("NVM_DIR", "/home/a@b/.nvm") // a path: '@' is not userinfo
	env := strings.Join(MinimalEnv(), "\n")
	for _, bad := range []string{"HTTPS_PROXY=", "https_proxy=", "u:pw", "onlyuser", "npm_config_registry=", "tok@"} {
		if strings.Contains(env, bad) {
			t.Errorf("%q passed through MinimalEnv:\n%s", bad, env)
		}
	}
	for _, good := range []string{"ALL_PROXY=socks5://proxy.example:1080", "HTTP_PROXY=proxy.example:3128", "NO_PROXY=a@b,localhost", "NVM_DIR=/home/a@b/.nvm"} {
		if !strings.Contains(env, good) {
			t.Errorf("%q wrongly dropped:\n%s", good, env)
		}
	}
	dropped := strings.Join(MinimalEnvDropped(), ",")
	if !strings.Contains(dropped, "HTTPS_PROXY") || strings.Contains(dropped, "pw") {
		t.Errorf("dropped names: %s", dropped)
	}
}
