package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/as950118/forge/internal/mcp"
)

const httpSchema = `{"type":"object","properties":{"id":{"type":"string"},"q":{"type":"string"}},"required":["id"]}`

func httpDef(name, rawURL string) Definition {
	return Definition{Name: name, Kind: KindHTTP, Description: "x", URL: rawURL, InputSchema: json.RawMessage(httpSchema),
		Policy: Policy{AllowHTTP: true, AllowPrivate: true}}
}

func TestHTTPGetHeadersBodyAndMasking(t *testing.T) {
	r, _ := newReg(t)
	const secret = "tok-0123456789abcdef"
	t.Setenv("API_SECRET_ENV", "Bearer "+secret)
	var gotAuth, gotPath, gotBody, gotCT, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth, gotPath, gotCT, gotUA = req.Header.Get("Authorization"), req.URL.RequestURI(), req.Header.Get("Content-Type"), req.Header.Get("User-Agent")
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		fmt.Fprintf(w, "echo:%s", req.Header.Get("Authorization")) // server echoes the secret back
	}))
	defer srv.Close()
	d := httpDef("api", srv.URL+"/items/{{id}}?q={{q}}")
	d.Method = "POST"
	d.BodyTemplate = `{"query":"{{q}}"}`
	d.Values = []mcp.Value{
		{Slot: mcp.SlotHeader, Key: "Authorization", IsSecret: true, RefType: mcp.RefEnv, Ref: "API_SECRET_ENV"},
		{Slot: mcp.SlotHeader, Key: "Accept", RefType: mcp.RefLiteral, Literal: "text/plain"},
	}
	mustAdd(t, r, d)
	res, err := run(t, r, "api", map[string]any{"id": "a b", "q": `x&y="z"`}, RunOptions{})
	if err != nil || !res.OK || res.HTTPStatus != 200 {
		t.Fatalf("%+v %v", res, err)
	}
	if gotAuth != "Bearer "+secret || gotCT != "application/json" || gotUA != "forge-tool/1" {
		t.Errorf("headers: %q %q %q", gotAuth, gotCT, gotUA)
	}
	if gotPath != "/items/a%20b?q=x%26y%3D%22z%22" {
		t.Errorf("escaped path/query: %s", gotPath)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil || body["query"] != `x&y="z"` {
		t.Errorf("body %q %v", gotBody, err)
	}
	if strings.Contains(res.Stdout, secret) || res.Stdout != "echo:***" {
		t.Errorf("secret echoed by server must be masked: %q", res.Stdout)
	}
	// dry-run
	res, err = run(t, r, "api", map[string]any{"id": "1", "q": "z"}, RunOptions{DryRun: true})
	if err != nil || res.DryRun == nil || strings.Contains(res.DryRun.Headers["Authorization"], secret) || !strings.Contains(res.DryRun.Headers["Authorization"], "${API_SECRET_ENV}") || res.DryRun.Headers["Accept"] != "text/plain" {
		t.Errorf("%+v %v", res, err)
	}
	// unset secret env
	t.Setenv("API_SECRET_ENV", "")
	if _, err = run(t, r, "api", map[string]any{"id": "1", "q": "z"}, RunOptions{}); err == nil || !strings.Contains(err.Error(), "API_SECRET_ENV") {
		t.Errorf("%v", err)
	}
}

func TestHTTPStatusIsAResult(t *testing.T) {
	r, _ := newReg(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", 404) }))
	defer srv.Close()
	mustAdd(t, r, httpDef("nf", srv.URL+"/{{id}}"))
	res, err := run(t, r, "nf", map[string]any{"id": "x"}, RunOptions{})
	if err != nil || res.OK || res.HTTPStatus != 404 || res.ErrorClass != ClassHTTPStatus {
		t.Errorf("%+v %v", res, err)
	}
}

func TestHTTPRedirectLimitAndResponseCapAndTimeout(t *testing.T) {
	r, _ := newReg(t)
	var hops int32
	mux := http.NewServeMux()
	mux.HandleFunc("/loop", func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&hops, 1)
		http.Redirect(w, req, "/loop", http.StatusFound)
	})
	mux.HandleFunc("/once", func(w http.ResponseWriter, req *http.Request) { http.Redirect(w, req, "/ok", http.StatusFound) })
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "fine") })
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(strings.Repeat("z", 100000))) })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-req.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := httpDef("loop", srv.URL+"/loop?x={{id}}")
	d.Policy.MaxRedirects = 2
	mustAdd(t, r, d)
	res, err := run(t, r, "loop", map[string]any{"id": "1"}, RunOptions{})
	if err == nil || res.ErrorClass != ClassPolicy || !strings.Contains(err.Error(), "리다이렉트") || atomic.LoadInt32(&hops) > 4 {
		t.Errorf("hops=%d %+v %v", hops, res, err)
	}
	mustAdd(t, r, httpDef("once", srv.URL+"/once?x={{id}}"))
	if res, err = run(t, r, "once", map[string]any{"id": "1"}, RunOptions{}); err != nil || res.Stdout != "fine" {
		t.Errorf("%+v %v", res, err)
	}
	d = httpDef("big", srv.URL+"/big?x={{id}}")
	d.Policy.MaxOutputBytes = 1024
	mustAdd(t, r, d)
	if res, err = run(t, r, "big", map[string]any{"id": "1"}, RunOptions{}); err != nil || len(res.Stdout) != 1024 || !res.StdoutTruncated || !res.OK {
		t.Errorf("%d %+v %v", len(res.Stdout), res, err)
	}
	d = httpDef("slow", srv.URL+"/slow?x={{id}}")
	d.Policy.TimeoutMS = 300
	mustAdd(t, r, d)
	start := time.Now()
	if res, err = run(t, r, "slow", map[string]any{"id": "1"}, RunOptions{}); err == nil || !res.TimedOut || res.ErrorClass != ClassTimeout || time.Since(start) > 3*time.Second {
		t.Errorf("%+v %v", res, err)
	}
}

func TestHTTPSSRFLiteralIPsRefusedAtAdd(t *testing.T) {
	r, _ := newReg(t)
	for _, u := range []string{"https://127.0.0.1/x", "https://[::1]/x", "https://10.1.2.3/x", "https://192.168.0.1/x", "https://169.254.169.254/latest/meta-data/",
		"https://localhost/x", "https://[::ffff:127.0.0.1]/x", "https://0.0.0.0/", "https://100.100.100.200/"} {
		d := httpDef("ssrf", u)
		d.Policy.AllowPrivate, d.Policy.AllowHTTP = false, false
		if _, err := r.Add(context.Background(), d, false); err == nil {
			t.Errorf("%s must be refused", u)
		}
	}
	// plain http refused by default, ok when explicit
	d := httpDef("plain", "http://example.com/x")
	d.Policy.AllowHTTP = false
	if _, err := r.Add(context.Background(), d, false); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("%v", err)
	}
	// metadata stays blocked even with allow_private
	d = httpDef("meta", "https://169.254.169.254/latest")
	if _, err := r.Add(context.Background(), d, false); err == nil {
		t.Error("metadata IP must be refused even with allow_private")
	}
	// templates in the host are refused
	d = httpDef("hostvar", "https://{{id}}.example.com/x")
	if _, err := r.Add(context.Background(), d, false); err == nil || !strings.Contains(err.Error(), "호스트") {
		t.Errorf("%v", err)
	}
	// host allowlist must contain the URL host
	d = httpDef("al", "https://api.example.com/x")
	d.Policy.Allow = []string{"other.example.com"}
	if _, err := r.Add(context.Background(), d, false); err == nil {
		t.Error("allowlist must include host")
	}
}

// Definitions bypassing Add (as if the DB were edited) are still stopped at connect time.
func TestHTTPSSRFEnforcedAtConnectTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "reachable") }))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	_ = host
	for _, tc := range []struct{ name, url string }{
		{"v4", "http://127.0.0.1:" + port + "/"},
		{"v6", "http://[::1]:" + port + "/"},
		{"private", "http://10.9.8.7:" + port + "/"},
		{"meta", "http://169.254.169.254/latest/meta-data/"},
		{"mapped", "http://[::ffff:127.0.0.1]:" + port + "/"},
	} {
		u := strings.TrimPrefix(tc.url, "http://")
		h, _, _ := net.SplitHostPort(u)
		if h == "" {
			h = strings.Split(u, "/")[0]
		}
		d := &Definition{Name: tc.name, Kind: KindHTTP, Method: "GET", URL: tc.url, Policy: Policy{AllowHTTP: true, Allow: []string{h}, TimeoutMS: 3000, MaxOutputBytes: 1024, MaxRedirects: 3}}
		res, err := Run(context.Background(), d, nil)
		if err == nil || res.ErrorClass != ClassSSRF || strings.Contains(res.Stdout, "reachable") {
			t.Errorf("%s: %+v %v", tc.name, res, err)
		}
	}
	// metadata blocked even with AllowPrivate
	d := &Definition{Name: "m", Kind: KindHTTP, Method: "GET", URL: "http://169.254.169.254/", Policy: Policy{AllowHTTP: true, AllowPrivate: true, Allow: []string{"169.254.169.254"}, TimeoutMS: 3000, MaxOutputBytes: 1024}}
	if res, err := Run(context.Background(), d, nil); err == nil || res.ErrorClass != ClassSSRF {
		t.Errorf("%+v %v", res, err)
	}
}

func TestHTTPSSRFDNSResolvingToPrivate(t *testing.T) {
	var hit int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { atomic.AddInt32(&hit, 1); fmt.Fprint(w, "reachable") }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	lookup := lookupMap(map[string]string{"evil.example.com": "127.0.0.1", "rebind.example.com": "10.0.0.5", "meta.example.com": "169.254.169.254"})
	for _, h := range []string{"evil.example.com", "rebind.example.com", "meta.example.com"} {
		d := &Definition{Name: "dns", Kind: KindHTTP, Method: "GET", URL: "http://" + h + ":" + port + "/", Policy: Policy{AllowHTTP: true, Allow: []string{h}, TimeoutMS: 3000, MaxOutputBytes: 1024}}
		res, err := Run(context.Background(), d, nil, RunOptions{Lookup: lookup})
		if err == nil || res.ErrorClass != ClassSSRF {
			t.Errorf("%s: %+v %v", h, res, err)
		}
	}
	if atomic.LoadInt32(&hit) != 0 {
		t.Error("the server must never be contacted")
	}
	// with allow_private the same loopback-resolving host works (explicit opt-in)
	d := &Definition{Name: "dns", Kind: KindHTTP, Method: "GET", URL: "http://evil.example.com:" + port + "/", Policy: Policy{AllowHTTP: true, AllowPrivate: true, Allow: []string{"evil.example.com"}, TimeoutMS: 3000, MaxOutputBytes: 1024}}
	if res, err := Run(context.Background(), d, nil, RunOptions{Lookup: lookup}); err != nil || res.Stdout != "reachable" {
		t.Errorf("%+v %v", res, err)
	}
}

func TestHTTPRedirectToInternalIsBlocked(t *testing.T) {
	var secretSeen int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Api-Key") != "" {
			atomic.AddInt32(&secretSeen, 1)
		}
		fmt.Fprint(w, "other")
	}))
	defer other.Close()
	for _, target := range []string{"http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/", "http://[::1]:1/"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { http.Redirect(w, req, target, http.StatusFound) }))
		t.Setenv("RD_KEY_ENV", "keyvalue-abcdefghij")
		d := &Definition{Name: "rd", Kind: KindHTTP, Method: "GET", URL: srv.URL + "/", Policy: Policy{AllowHTTP: true, AllowPrivate: true, Allow: []string{"127.0.0.1"}, TimeoutMS: 3000, MaxOutputBytes: 1024, MaxRedirects: 3},
			Values: []mcp.Value{{Slot: mcp.SlotHeader, Key: "X-Api-Key", IsSecret: true, RefType: mcp.RefEnv, Ref: "RD_KEY_ENV"}}}
		res, err := Run(context.Background(), d, nil)
		srv.Close()
		if err == nil || res.OK || strings.Contains(res.Stdout, "other") {
			t.Errorf("redirect to %s must fail: %+v %v", target, res, err)
		}
	}
	// an allowlisted redirect target on another origin works but never receives definition headers
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, other.URL+"/x", http.StatusFound)
	}))
	defer srv2.Close()
	d2 := &Definition{Name: "rd3", Kind: KindHTTP, Method: "GET", URL: srv2.URL + "/", Policy: Policy{AllowHTTP: true, AllowPrivate: true, Allow: []string{"127.0.0.1"}, TimeoutMS: 3000, MaxOutputBytes: 1024, MaxRedirects: 3},
		Values: []mcp.Value{{Slot: mcp.SlotHeader, Key: "X-Api-Key", IsSecret: true, RefType: mcp.RefEnv, Ref: "RD_KEY_ENV"}}}
	if res, err := Run(context.Background(), d2, nil); err != nil || res.Stdout != "other" {
		t.Errorf("%+v %v", res, err)
	}
	if atomic.LoadInt32(&secretSeen) != 0 {
		t.Error("header secret leaked to a redirect target")
	}
	// even if the redirect target host is allowlisted, metadata IPs fail at dial
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "http://169.254.169.254/x", http.StatusFound)
	}))
	defer srv.Close()
	d := &Definition{Name: "rd2", Kind: KindHTTP, Method: "GET", URL: srv.URL + "/", Policy: Policy{AllowHTTP: true, AllowPrivate: true, Allow: []string{"127.0.0.1", "169.254.169.254"}, TimeoutMS: 3000, MaxOutputBytes: 1024, MaxRedirects: 3}}
	if res, err := Run(context.Background(), d, nil); err == nil || res.ErrorClass != ClassSSRF {
		t.Errorf("%+v %v", res, err)
	}
}

func TestIPBlocked(t *testing.T) {
	for ip, want := range map[string]bool{"8.8.8.8": false, "1.1.1.1": false, "2606:4700::1111": false,
		"127.0.0.1": true, "10.0.0.1": true, "172.16.5.5": true, "192.168.1.1": true, "169.254.169.254": true, "100.64.0.1": true,
		"::1": true, "fe80::1": true, "fd00::1": true, "::ffff:10.0.0.1": true, "0.0.0.0": true, "224.0.0.1": true, "255.255.255.255": true} {
		if got := ipBlocked(net.ParseIP(ip), false); got != want {
			t.Errorf("%s: got %v want %v", ip, got, want)
		}
	}
	if ipBlocked(net.ParseIP("127.0.0.1"), true) || ipBlocked(net.ParseIP("10.0.0.1"), true) {
		t.Error("allow_private")
	}
	if !ipBlocked(net.ParseIP("169.254.169.254"), true) || !ipBlocked(net.ParseIP("fe80::1"), true) {
		t.Error("link-local must stay blocked")
	}
}
