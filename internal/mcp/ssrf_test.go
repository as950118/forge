package mcp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heonjinjeong/forge/internal/mcp/mcptest"
	"github.com/heonjinjeong/forge/internal/netsafe"
)

// TASK-0020 AC-12. Local test servers only; the lookup is replaced so that no DNS
// query leaves the machine and no packet is sent to a private network.

func fakeLookup(m map[string]string) netsafe.Lookup {
	return func(_ context.Context, host string) ([]net.IP, error) {
		if ip, ok := m[host]; ok {
			return []net.IP{net.ParseIP(ip)}, nil
		}
		if ip := net.ParseIP(host); ip != nil {
			return []net.IP{ip}, nil
		}
		return nil, errors.New("no such host (test lookup): " + host)
	}
}

func get(t *testing.T, rt http.RoundTripper, raw string) error {
	t.Helper()
	c := &http.Client{Transport: rt, Timeout: 5 * time.Second}
	resp, err := c.Get(raw)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func TestRemoteTransportBlocksInternalAddressesByName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	lookup := fakeLookup(map[string]string{
		"intranet.test": "127.0.0.1", // an internal address reached by name
		"meta.test":     "169.254.169.254",
		"v6meta.test":   "fd00:ec2::254",
	})
	// by default a name that resolves to a private/loopback address is refused at connect time
	rt := newRemoteRoundTripperWith(&Info{URL: "https://intranet.test/mcp"}, lookup)
	var be *netsafe.BlockedError
	if err := get(t, rt, "http://intranet.test:"+port+"/"); !errors.As(err, &be) {
		t.Fatalf("name -> loopback must be refused, got %v", err)
	}
	// allowed with the explicit opt-in
	rt = newRemoteRoundTripperWith(&Info{URL: "https://intranet.test/mcp", AllowPrivate: true}, lookup)
	if err := get(t, rt, "http://intranet.test:"+port+"/"); err != nil {
		t.Fatalf("allow-private: %v", err)
	}
	// metadata / link-local stay blocked even with the opt-in
	for _, h := range []string{"meta.test", "v6meta.test"} {
		if err := get(t, rt, "http://"+h+":"+port+"/"); !errors.As(err, &be) {
			t.Errorf("%s must stay blocked with allow-private: %v", h, err)
		}
	}
	// a URL that itself names this machine (localhost or a literal loopback address) needs no flag
	for _, host := range []string{"127.0.0.1", "localhost"} {
		rt = newRemoteRoundTripperWith(&Info{URL: "http://" + host + ":" + port + "/mcp"}, netsafe.DefaultLookup)
		if err := get(t, rt, "http://"+host+":"+port+"/"); err != nil {
			t.Errorf("%s: explicit local server must be allowed: %v", host, err)
		}
	}
	// a literal metadata address in the URL is blocked even though it is "explicit"
	rt = newRemoteRoundTripperWith(&Info{URL: "http://169.254.169.254/latest"}, netsafe.DefaultLookup)
	if err := get(t, rt, "http://169.254.169.254/latest"); !errors.As(err, &be) {
		t.Errorf("literal metadata address: %v", err)
	}
}

// A redirect target is dialed through the same checked dialer, and definition headers
// (secrets) never follow a redirect to another host.
func TestRemoteRedirectIsCheckedAndHeadersStayOnTheirHost(t *testing.T) {
	var gotAuthB atomic.Value
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthB.Store(r.Header.Get("Authorization"))
	}))
	defer b.Close()
	var gotAuthA atomic.Value
	var a *httptest.Server
	a = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthA.Store(r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/to-meta":
			http.Redirect(w, r, "http://meta.test/latest", http.StatusFound)
		case "/to-b":
			bu, _ := url.Parse(b.URL)
			http.Redirect(w, r, "http://localhost:"+bu.Port()+"/", http.StatusFound)
		}
	}))
	defer a.Close()
	in := &Info{URL: a.URL + "/mcp"}
	rt := newRemoteRoundTripperWith(in, fakeLookup(map[string]string{"meta.test": "169.254.169.254", "localhost": "127.0.0.1"}))
	dt := &diagTransport{base: rt, headers: map[string]string{"Authorization": "Bearer fake-token"}, host: urlHost(in.URL)}

	var be *netsafe.BlockedError
	if err := get(t, dt, a.URL+"/to-meta"); !errors.As(err, &be) {
		t.Fatalf("redirect to a metadata address must be refused at connect time: %v", err)
	}
	if v, _ := gotAuthA.Load().(string); v != "Bearer fake-token" {
		t.Errorf("the registered host must get the definition headers, got %q", v)
	}
	if err := get(t, dt, a.URL+"/to-b"); err != nil {
		t.Fatal(err)
	}
	if v, _ := gotAuthB.Load().(string); v != "" {
		t.Errorf("definition headers followed a redirect to another host: %q", v)
	}
}

// End to end through Refresh: blocked with an actionable class and hint, then allowed by SetAllowPrivate.
func TestRefreshRemoteServerBlockedThenAllowed(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	// The SDK handler refuses a Host header that is not the listener's own (its DNS
	// rebinding protection); present the name as the loopback address it resolves to.
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return mcptest.NewServer([]string{"echo"}, "") }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = srv0Host(r)
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	old := remoteLookup
	remoteLookup = fakeLookup(map[string]string{"intranet.test": "127.0.0.1"})
	defer func() { remoteLookup = old }()

	if err := reg.Add(ctx, Spec{Name: "corp", Transport: TransportHTTP, URL: "http://intranet.test:" + u.Port() + "/"}); err != nil {
		t.Fatal(err)
	}
	res, err := reg.Refresh(ctx, "corp", RefreshOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.ErrorClass != ClassBlockedAddress || !strings.Contains(res.Hint, "allow-private") {
		t.Fatalf("want blocked_address with the allow-private hint: %+v", res)
	}
	if err := reg.SetAllowPrivate(ctx, "corp", true); err != nil {
		t.Fatal(err)
	}
	if i, _ := reg.Get(ctx, "corp"); i == nil || !i.AllowPrivate {
		t.Fatalf("flag not stored: %+v", i)
	}
	res, err = reg.Refresh(ctx, "corp", RefreshOptions{Timeout: 10 * time.Second})
	if err != nil || !res.OK {
		t.Fatalf("allowed server must connect: %+v %v", res, err)
	}
	if err := reg.SetAllowPrivate(ctx, "corp", false); err != nil {
		t.Fatal(err)
	}
	if res, _ = reg.Refresh(ctx, "corp", RefreshOptions{Timeout: 10 * time.Second}); res.OK || res.ErrorClass != ClassBlockedAddress {
		t.Errorf("revoked: %+v", res)
	}
	if err := reg.SetAllowPrivate(ctx, "ghost", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown server: %v", err)
	}
	// removing the server removes the setting
	if err := reg.Remove(ctx, "corp"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := reg.DB.QueryRow(`SELECT count(*) FROM settings WHERE key LIKE 'mcp.allow_private.%'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("settings left behind: %d %v", n, err)
	}
}

func srv0Host(r *http.Request) string {
	if c, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		return c.String()
	}
	return r.Host
}

// Same-host behaviour that existed before: a normal local server with headers still works.
func TestRemoteLocalServerStillConnectsWithoutFlag(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	srv := mcptest.HTTP([]string{"echo"}, "")
	defer srv.Close()
	if err := reg.Add(ctx, Spec{Name: "local", Transport: TransportHTTP, URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if res, err := reg.Refresh(ctx, "local", RefreshOptions{Timeout: 10 * time.Second}); err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
}

type captureRT struct{ got map[string]string }

func (c *captureRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.got[r.URL.String()] = r.Header.Get("Authorization")
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
}

// Definition headers belong to one origin: scheme, host and port (default ports are
// normalized), so a scheme downgrade or another port never receives them.
func TestDefinitionHeadersFollowOriginSchemeHostAndPort(t *testing.T) {
	cases := []struct {
		reg, target string
		want        bool
	}{
		{"https://h.example/mcp", "https://h.example/other", true},
		{"https://h.example/mcp", "https://h.example:443/x", true}, // explicit default port, same origin
		{"https://h.example:443/mcp", "https://h.example/x", true},
		{"https://H.Example/mcp", "https://h.example/x", true},
		{"http://h.example/sse", "http://h.example:80/message?s=1", true}, // SSE endpoint URL with the default port spelled out
		{"https://h.example/mcp", "http://h.example/x", false},            // scheme downgrade
		{"https://h.example/mcp", "http://h.example:443/x", false},
		{"https://h.example/mcp", "https://h.example:8443/x", false}, // port variant
		{"http://h.example/mcp", "http://h.example:8080/x", false},
		{"https://h.example/mcp", "https://other.example/x", false},
		{"http://127.0.0.1:9000/mcp", "http://127.0.0.1:9000/x", true},
		{"http://127.0.0.1:9000/mcp", "http://127.0.0.1:9001/x", false},
	}
	for _, c := range cases {
		rt := &captureRT{got: map[string]string{}}
		dt := &diagTransport{base: rt, headers: map[string]string{"Authorization": "Bearer fake"}, host: urlHost(c.reg)}
		req, _ := http.NewRequest("GET", c.target, nil)
		if _, err := dt.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		if got := rt.got[c.target] != ""; got != c.want {
			t.Errorf("registered %s, request %s: headers sent = %v, want %v", c.reg, c.target, got, c.want)
		}
	}
}
