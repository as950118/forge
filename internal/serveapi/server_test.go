package serveapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testToken = "fake-token-0123456789abcdef"
	fakeDBURL = "postgresql://fakeuser:fakepw-S3cret@db.example.invalid:5432/postgres"
)

type fakeStore struct {
	pingErr error
	err     error
	calls   int
	days    int
	kind    string
}

func (f *fakeStore) Ping(context.Context) error { f.calls++; return f.pingErr }
func (f *fakeStore) Summary(_ context.Context, days int) (Summary, error) {
	f.calls++
	f.days = days
	if f.err != nil {
		return Summary{}, f.err
	}
	rate := 0.5
	return Summary{
		Meta:  Meta{SchemaVersion: 1, GeneratedAt: "2026-11-03T00:00:00Z", ForgeVersion: "v", DayBasis: "UTC"},
		Items: []Item{{Kind: "skill", Name: "a", Calls: 2, OK: 1, Failed: 1, SuccessRate: &rate, LastCall: "2026-11-02T10:00:00Z"}},
		Daily: []Day{{Day: "2026-11-02", Kind: "skill", Calls: 2, OK: 1, Failed: 1}},
	}, nil
}
func (f *fakeStore) Items(_ context.Context, kind string) ([]Item, error) {
	f.calls++
	f.kind = kind
	return []Item{{Kind: "mcp", Server: "s", Name: "t", Calls: 1, Unknown: 1, LastCall: "2026-11-02T11:00:00Z"}}, f.err
}
func (f *fakeStore) Daily(_ context.Context, days int) ([]Day, error) {
	f.calls++
	f.days = days
	return []Day{{Day: "2026-11-02", Kind: "tool", Calls: 1, OK: 1}}, f.err
}

type logSink struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logSink) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, f+"\n", a...)
}
func (l *logSink) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func newTest(t *testing.T, fs *fakeStore) (*Server, *logSink) {
	t.Helper()
	ls := &logSink{}
	s, err := New(Options{Store: fs, Token: testToken, Logf: ls.logf,
		Redact: func(x string) string { return strings.ReplaceAll(x, "fakepw-S3cret", "***") }})
	if err != nil {
		t.Fatal(err)
	}
	return s, ls
}

func do(s http.Handler, method, target string, hdr map[string][]string, body io.Reader) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, body)
	for k, v := range hdr {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func bearer(tok string) map[string][]string {
	return map[string][]string{"Authorization": {"Bearer " + tok}}
}

func TestNewRefusesMissingOrShortToken(t *testing.T) {
	for _, tok := range []string{"", "short", strings.Repeat("a", MinTokenLen-1), " " + strings.Repeat("a", 20), strings.Repeat("a", 600)} {
		if _, err := New(Options{Store: &fakeStore{}, Token: tok}); err == nil {
			t.Errorf("token %q accepted", tok)
		}
	}
	if _, err := New(Options{Store: &fakeStore{}, Token: strings.Repeat("a", MinTokenLen)}); err != nil {
		t.Fatal(err)
	}
}

func TestHealthOpenAndNoDB(t *testing.T) {
	fs := &fakeStore{pingErr: errors.New("boom")}
	s, _ := newTest(t, fs)
	w := do(s, "GET", "/health", nil, nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"readOnly":true,"status":"ok"}` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if fs.calls != 0 {
		t.Fatal("/health touched the store")
	}
	if w := do(s, "HEAD", "/health", nil, nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %q", w.Code, w.Body)
	}
}

func TestHealthDBOpenWithoutToken(t *testing.T) {
	fs := &fakeStore{}
	s, _ := newTest(t, fs)
	w := do(s, "GET", "/health/db", nil, nil)
	if w.Code != 200 || fs.calls != 1 {
		t.Fatalf("%d calls=%d", w.Code, fs.calls)
	}
}

func TestHealthDBFailureHidesSecrets(t *testing.T) {
	fs := &fakeStore{pingErr: errors.New("dial " + fakeDBURL + " token=" + testToken + " failed")}
	s, ls := newTest(t, fs)
	w := do(s, "GET", "/health/db", nil, nil)
	if w.Code != 503 {
		t.Fatalf("code %d", w.Code)
	}
	for _, leak := range []string{"fakepw", "fakeuser", "db.example", testToken, "dial", "postgresql"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("body leaks %q: %s", leak, w.Body)
		}
	}
	if strings.Contains(ls.String(), "fakepw-S3cret") || strings.Contains(ls.String(), testToken) {
		t.Errorf("log leaks secret: %s", ls)
	}
	if !strings.Contains(ls.String(), "health/db") {
		t.Errorf("failure not logged: %s", ls)
	}
}

func TestAuthBypassAttempts(t *testing.T) {
	fs := &fakeStore{}
	s, _ := newTest(t, fs)
	long := strings.Repeat("x", 100000)
	cases := map[string]struct {
		target string
		hdr    map[string][]string
	}{
		"no header":        {"/v1/summary", nil},
		"empty token":      {"/v1/summary", map[string][]string{"Authorization": {"Bearer "}}},
		"scheme only":      {"/v1/summary", map[string][]string{"Authorization": {"Bearer"}}},
		"empty value":      {"/v1/summary", map[string][]string{"Authorization": {""}}},
		"basic scheme":     {"/v1/summary", map[string][]string{"Authorization": {"Basic " + testToken}}},
		"raw token":        {"/v1/summary", map[string][]string{"Authorization": {testToken}}},
		"wrong token":      {"/v1/summary", bearer("fake-token-0123456789abcdeX")},
		"prefix of token":  {"/v1/summary", bearer(testToken[:len(testToken)-1])},
		"token plus extra": {"/v1/summary", bearer(testToken + "x")},
		"token with space": {"/v1/summary", bearer(testToken + " ")},
		"long token":       {"/v1/summary", bearer(long)},
		"two headers":      {"/v1/summary", map[string][]string{"Authorization": {"Bearer " + testToken, "Bearer " + testToken}}},
		"good then bad":    {"/v1/summary", map[string][]string{"Authorization": {"Bearer " + testToken, "Bearer nope"}}},
		"query token":      {"/v1/summary?token=" + testToken, nil},
		"query access":     {"/v1/items?access_token=" + testToken + "&api_key=" + testToken, nil},
		"x-api-key":        {"/v1/daily", map[string][]string{"X-Api-Key": {testToken}}},
		"cookie":           {"/v1/daily", map[string][]string{"Cookie": {"token=" + testToken}}},
		"unknown v1 path":  {"/v1/nope", nil},
		"dot segments":     {"/health/../v1/summary", nil},
		"double slash":     {"//v1/summary", nil},
		"trailing slash":   {"/v1/summary/", nil},
		"case path":        {"/V1/summary", nil},
	}
	for name, c := range cases {
		w := do(s, "GET", c.target, c.hdr, nil)
		if w.Code != 401 && w.Code != 404 {
			t.Errorf("%s: status %d", name, w.Code)
		}
		if w.Code == 401 && w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: no WWW-Authenticate", name)
		}
		if strings.Contains(w.Body.String(), "Basic") || strings.Contains(w.Body.String(), `"items"`) || strings.Contains(w.Body.String(), testToken) {
			t.Errorf("%s: body %s", name, w.Body)
		}
	}
	if fs.calls != 0 {
		t.Fatalf("store reached %d times without a valid token", fs.calls)
	}
}

func TestAuthAcceptsBearerCaseInsensitiveScheme(t *testing.T) {
	s, _ := newTest(t, &fakeStore{})
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		w := do(s, "GET", "/v1/summary", map[string][]string{"Authorization": {scheme + " " + testToken}}, nil)
		if w.Code != 200 {
			t.Errorf("%s: %d", scheme, w.Code)
		}
	}
}

func TestWriteMethodsRejected(t *testing.T) {
	fs := &fakeStore{}
	s, _ := newTest(t, fs)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT", "PROPFIND"} {
		for _, p := range []string{"/health", "/health/db", "/v1/summary", "/v1/items", "/anything"} {
			for _, h := range []map[string][]string{nil, bearer(testToken)} {
				w := do(s, m, p, h, strings.NewReader("{}"))
				if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
					t.Errorf("%s %s: %d allow=%q", m, p, w.Code, w.Header().Get("Allow"))
				}
				if w.Header().Get("Access-Control-Allow-Origin") != "" {
					t.Errorf("CORS header on %s %s", m, p)
				}
			}
		}
	}
	if fs.calls != 0 {
		t.Fatal("store reached by a write method")
	}
}

func TestRoutesAndParams(t *testing.T) {
	fs := &fakeStore{}
	s, _ := newTest(t, fs)
	get := func(target string) *httptest.ResponseRecorder { return do(s, "GET", target, bearer(testToken), nil) }

	w := get("/v1/summary")
	var sum struct {
		Meta  map[string]any   `json:"meta"`
		Items []map[string]any `json:"items"`
		Daily []map[string]any `json:"daily"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &sum) != nil || len(sum.Items) != 1 || len(sum.Daily) != 1 || fs.days != 366 {
		t.Fatalf("summary %d %s days=%d", w.Code, w.Body, fs.days)
	}
	if w := get("/v1/items?kind=mcp"); w.Code != 200 || fs.kind != "mcp" {
		t.Fatalf("items %d kind=%q", w.Code, fs.kind)
	}
	if w := get("/v1/items"); w.Code != 200 || fs.kind != "" {
		t.Fatalf("items all %d", w.Code)
	}
	for _, bad := range []string{"/v1/items?kind=x", "/v1/items?kind=SKILL", "/v1/items?kind=skill%27%3B--", "/v1/daily?days=0", "/v1/daily?days=367", "/v1/daily?days=-1", "/v1/daily?days=abc", "/v1/daily?days=99999999999999999999"} {
		if w := get(bad); w.Code != 400 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := get("/v1/daily?days=366"); w.Code != 200 || fs.days != 366 {
		t.Fatalf("days=366: %d %d", w.Code, fs.days)
	}
	if w := get("/v1/daily"); w.Code != 200 || fs.days != defaultDays {
		t.Fatalf("default days: %d %d", w.Code, fs.days)
	}
	if w := do(s, "HEAD", "/v1/summary", bearer(testToken), nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD summary %d %d", w.Code, w.Body.Len())
	}
	if w := get("/v1/unknown"); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
}

func TestEmptyStoreGivesEmptyArraysNotNull(t *testing.T) {
	s, _ := New(Options{Store: emptyStore{}, Token: testToken})
	for _, p := range []string{"/v1/summary", "/v1/items", "/v1/daily"} {
		w := do(s, "GET", p, bearer(testToken), nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "null") {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
}

type emptyStore struct{}

func (emptyStore) Ping(context.Context) error { return nil }
func (emptyStore) Summary(context.Context, int) (Summary, error) {
	return Summary{}, nil
}
func (emptyStore) Items(context.Context, string) ([]Item, error) { return nil, nil }
func (emptyStore) Daily(context.Context, int) ([]Day, error)     { return nil, nil }

func TestNoSnapshotIs404(t *testing.T) {
	s, _ := newTest(t, &fakeStore{err: ErrNoSnapshot})
	if w := do(s, "GET", "/v1/summary", bearer(testToken), nil); w.Code != 404 {
		t.Fatalf("%d", w.Code)
	}
}

func TestStoreErrorsAreGeneric(t *testing.T) {
	raw := `failed to connect to host=db.example.invalid user=fakeuser password=fakepw-S3cret: ` + fakeDBURL + ` tok=` + testToken
	fs := &fakeStore{err: errors.New(raw)}
	s, ls := newTest(t, fs)
	for _, p := range []string{"/v1/summary", "/v1/items", "/v1/daily"} {
		w := do(s, "GET", p, bearer(testToken), nil)
		if w.Code != 503 || strings.TrimSpace(w.Body.String()) != `{"error":"database unavailable"}` {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	if strings.Contains(ls.String(), "fakepw-S3cret") || strings.Contains(ls.String(), testToken) {
		t.Errorf("log leaks: %s", ls)
	}
	if !strings.Contains(ls.String(), "store error") {
		t.Errorf("error not logged: %s", ls)
	}
}

func TestLogsDoNotContainQueryString(t *testing.T) {
	s, ls := newTest(t, &fakeStore{})
	do(s, "GET", "/v1/summary?token="+testToken, nil, nil)
	if strings.Contains(ls.String(), testToken) || strings.Contains(ls.String(), "token=") {
		t.Fatalf("log: %s", ls)
	}
	do(s, "GET", "/v1/%1b%0a[2Jfake", nil, nil) // log injection via path
	if strings.Count(ls.String(), "\n") != 2 || strings.ContainsRune(ls.String(), 0x1b) {
		t.Fatalf("log injection: %q", ls)
	}
}

func TestSecurityHeadersAndNoCORS(t *testing.T) {
	s, _ := newTest(t, &fakeStore{})
	for _, c := range []struct {
		p string
		h map[string][]string
	}{{"/health", nil}, {"/health/db", nil}, {"/v1/summary", bearer(testToken)}, {"/v1/summary", nil}, {"/nope", nil}} {
		w := do(s, "GET", c.p, c.h, nil)
		h := w.Header()
		for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
			if h.Get(k) != v {
				t.Errorf("%s %d: %s=%q", c.p, w.Code, k, h.Get(k))
			}
		}
		if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") || !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
			t.Errorf("%s: csp/type %v", c.p, h)
		}
		for k := range h {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("%s: CORS header %s", c.p, k)
			}
		}
	}
	// a preflight is refused, with no CORS answer
	w := do(s, "OPTIONS", "/v1/summary", map[string][]string{"Origin": {"https://evil.example"}, "Access-Control-Request-Method": {"GET"}}, nil)
	if w.Code != 405 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("preflight: %d %v", w.Code, w.Header())
	}
}

func TestLargeInputs(t *testing.T) {
	fs := &fakeStore{}
	s, _ := newTest(t, fs)
	if w := do(s, "GET", "/v1/summary?x="+strings.Repeat("a", 5000), bearer(testToken), nil); w.Code != 414 {
		t.Errorf("long URI: %d", w.Code)
	}
	if w := do(s, "GET", "/v1/summary", bearer(testToken), strings.NewReader(strings.Repeat("a", 1<<20))); w.Code != 400 {
		t.Errorf("body: %d", w.Code)
	}
	if fs.calls != 0 {
		t.Errorf("store reached")
	}
	// A real server: header and body limits are enforced by net/http.
	srv := httptest.NewUnstartedServer(s)
	srv.Config.MaxHeaderBytes = 8 << 10
	srv.Start()
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/summary", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("X-Big", strings.Repeat("h", 64<<10))
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != 431 {
			t.Errorf("big header: %d", resp.StatusCode)
		}
	}
	req, _ = http.NewRequest("GET", srv.URL+"/v1/summary", strings.NewReader(strings.Repeat("b", 1<<20)))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err = http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("big body: %d", resp.StatusCode)
		}
	}
}

func TestResponseCap(t *testing.T) {
	big := make([]Item, 0, 40000)
	for i := 0; i < 40000; i++ {
		big = append(big, Item{Kind: "tool", Name: strings.Repeat("n", 200), LastCall: "2026-11-02T10:00:00Z"})
	}
	s, _ := New(Options{Store: bigStore{big}, Token: testToken})
	w := do(s, "GET", "/v1/items", bearer(testToken), nil)
	if w.Code != 500 || w.Body.Len() > 200 {
		t.Fatalf("%d len=%d", w.Code, w.Body.Len())
	}
}

type bigStore struct{ items []Item }

func (bigStore) Ping(context.Context) error                    { return nil }
func (bigStore) Summary(context.Context, int) (Summary, error) { return Summary{}, nil }
func (b bigStore) Items(context.Context, string) ([]Item, error) {
	return b.items, nil
}
func (bigStore) Daily(context.Context, int) ([]Day, error) { return nil, nil }

// The JSON shape is the allow list: exactly these keys, nothing else.
func TestResponseFieldsAllowList(t *testing.T) {
	s, _ := newTest(t, &fakeStore{})
	keys := func(m map[string]any) []string {
		var k []string
		for key := range m {
			k = append(k, key)
		}
		return k
	}
	allowed := map[string]map[string]bool{
		"meta":  {"schema_version": true, "generated_at": true, "forge_version": true, "day_basis": true},
		"item":  {"kind": true, "server": true, "name": true, "calls": true, "ok": true, "failed": true, "unknown": true, "success_rate": true, "last_call": true},
		"daily": {"day": true, "kind": true, "calls": true, "ok": true, "failed": true, "unknown": true},
	}
	check := func(what string, m map[string]any) {
		if len(m) != len(allowed[what]) {
			t.Errorf("%s: keys %v", what, keys(m))
		}
		for _, k := range keys(m) {
			if !allowed[what][k] {
				t.Errorf("%s: unexpected field %q", what, k)
			}
		}
	}
	var sum map[string]any
	w := do(s, "GET", "/v1/summary", bearer(testToken), nil)
	_ = json.Unmarshal(w.Body.Bytes(), &sum)
	if len(sum) != 3 {
		t.Errorf("summary keys %v", keys(sum))
	}
	check("meta", sum["meta"].(map[string]any))
	check("item", sum["items"].([]any)[0].(map[string]any))
	check("daily", sum["daily"].([]any)[0].(map[string]any))
	var it map[string]any
	w = do(s, "GET", "/v1/items", bearer(testToken), nil)
	_ = json.Unmarshal(w.Body.Bytes(), &it)
	check("item", it["items"].([]any)[0].(map[string]any))
	var dl map[string]any
	w = do(s, "GET", "/v1/daily?days=7", bearer(testToken), nil)
	_ = json.Unmarshal(w.Body.Bytes(), &dl)
	check("daily", dl["daily"].([]any)[0].(map[string]any))
}

func TestStoreSQLIsReadOnlyConstants(t *testing.T) {
	bad := regexp.MustCompile(`(?i)\b(insert|update|delete|drop|alter|create|truncate|grant|copy|call|do|set\s+(?:session|role))\b`)
	for _, q := range []string{sqlPing, sqlMeta, sqlItems, sqlDaily} {
		if !strings.HasPrefix(q, "SELECT ") || bad.MatchString(q) || strings.Contains(q, ";") {
			t.Errorf("not a plain SELECT: %s", q)
		}
	}
	if !strings.HasPrefix(sqlTimeout, "SET LOCAL statement_timeout") {
		t.Error("statement timeout")
	}
	src, err := os.ReadFile("pg.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "AccessMode: pgx.ReadOnly") {
		t.Error("pg.go must open read-only transactions")
	}
}

func TestRunServesAndShutsDownGracefully(t *testing.T) {
	s, _ := newTest(t, &fakeStore{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen on loopback")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, ln, s, nil) }()
	resp, err := http.Get("http://" + ln.Addr().String() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"readOnly":true`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no graceful shutdown")
	}
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Error("still listening")
	}
}

func TestRawRequestSmuggledMethodAndHeaders(t *testing.T) {
	s, _ := newTest(t, &fakeStore{})
	srv := httptest.NewServer(s)
	defer srv.Close()
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Skip("no loopback")
	}
	defer c.Close()
	// two Authorization headers on the wire, the first one valid
	fmt.Fprintf(c, "GET /v1/summary HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nAuthorization: Bearer nope\r\nConnection: close\r\n\r\n", testToken)
	line, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.Contains(line, "401") {
		t.Fatalf("status line %q", line)
	}
}
