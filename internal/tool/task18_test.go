package tool

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/heonjinjeong/forge/internal/mcp"
)

func TestCodeExecClassification(t *testing.T) {
	yes := [][]string{
		{"sqlite3", "db.sqlite", "{{v}}"}, {"/usr/bin/sqlite3", "--", "{{v}}"}, {"npm", "run", "{{v}}"}, {"npx", "--", "{{v}}"}, {"pnpm", "exec", "{{v}}"},
		{"yarn", "run", "{{v}}"}, {"uv", "run", "{{v}}"}, {"uvx", "--", "{{v}}"}, {"pipx", "run", "{{v}}"}, {"swift", "{{v}}"}, {"java", "-jar", "{{v}}"},
		{"jshell", "{{v}}"}, {"irb", "{{v}}"}, {"go", "run", "{{v}}"}, {"cargo", "run", "--", "{{v}}"}, {"open", "--", "{{v}}"},
		{"npm3", "run", "{{v}}"}, {"SQLITE3.exe", "--", "{{v}}"}, {"go", "-C", "d", "test", "./..."}, {"cargo", "build"},
	}
	for _, argv := range yes {
		d := cmdDef("cx", argv...)
		if got := d.CodeExecBinary(); got == "" {
			t.Errorf("%v must be code-exec", argv)
		}
	}
	no := [][]string{
		{"git", "log", "-n", "{{n}}"}, {"cat", "--", "{{v}}"}, {"ls", "-la"}, {"jq", ".", "{{v}}"}, {"curl", "-sS", "--", "{{v}}"},
		{"go", "list", "./..."}, {"go", "version"}, {"cargo", "metadata"}, {"go"}, {"printf", "%s", "{{v}}"}, {"opener", "x"},
	}
	for _, argv := range no {
		d := cmdDef("nx", argv...)
		if got := d.CodeExecBinary(); got != "" {
			t.Errorf("%v must not be code-exec, got %q", argv, got)
		}
	}
	// non-command kinds are not judged
	h := Definition{Kind: KindHTTP, Command: "sqlite3"}
	if h.CodeExecBinary() != "" {
		t.Error("http kind")
	}
	// shell tools: first word of every command segment of the script
	for script, want := range map[string]bool{
		`sqlite3 db "$FORGE_ARG_q"`: true, `echo hi; npm run "$FORGE_ARG_s"`: true, `FOO=1 uvx tool`: true, `cat f | sqlite3 db`: true,
		`echo "open the door"`: false, `grep npm file`: false, `echo ok`: false,
	} {
		d := Definition{Kind: KindCommand, Shell: true, Command: "sh", Args: []string{"-c", script}}
		if got := d.CodeExecBinary() != ""; got != want {
			t.Errorf("shell %q: code-exec=%v want %v", script, got, want)
		}
	}
	// a link with an innocent name that ends in a code-exec program
	bin := t.TempDir()
	target := filepath.Join(bin, "npm")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "mytool")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	d := cmdDef("lk", link, "--", "{{v}}")
	if d.CodeExecBinary() == "" {
		t.Error("symlink to npm must be code-exec")
	}
}

func TestCodeExecAddRequiresExplicitAckForConfirmNone(t *testing.T) {
	r, _ := newReg(t)
	ctx := context.Background()
	d := cmdDef("sq", "sqlite3", "db.sqlite", "{{v}}")
	// default confirm = ask: allowed, classified, no ack needed
	got, err := r.Add(ctx, d, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Risk != RiskCodeExec || got.Policy.Confirm != ConfirmAsk {
		t.Errorf("risk=%q confirm=%q", got.Risk, got.Policy.Confirm)
	}
	if CodeExecWarning(got) == "" || !strings.Contains(CodeExecWarning(got), "confirm=ask") {
		t.Errorf("warning=%q", CodeExecWarning(got))
	}
	// explicit confirm=none is refused without the acknowledgement
	n := cmdDef("sq2", "sqlite3", "db.sqlite", "{{v}}")
	n.Policy.Confirm = ConfirmNone
	if _, err := r.Add(ctx, n, false); err == nil || !strings.Contains(err.Error(), "--yes-code-exec") {
		t.Fatalf("confirm=none without ack must be refused: %v", err)
	}
	// ... and accepted with it; the ack survives a DB round trip
	n.Policy.AckCodeExec = true
	if _, err := r.Add(ctx, n, false); err != nil {
		t.Fatal(err)
	}
	g, err := r.Get(ctx, "sq2")
	if err != nil {
		t.Fatal(err)
	}
	if g.Risk != RiskCodeExec || !g.Policy.AckCodeExec || g.Policy.Confirm != ConfirmNone || g.ForcedAsk() {
		t.Errorf("%+v forced=%v", g.Policy, g.ForcedAsk())
	}
	// the ack without confirm=none is a mistake worth refusing
	bad := cmdDef("sq3", "sqlite3", "db.sqlite", "{{v}}")
	bad.Policy.AckCodeExec = true
	if _, err := r.Add(ctx, bad, false); err == nil {
		t.Error("ack with confirm=ask must be refused")
	}
	// a harmless program is never affected
	ok := cmdDef("ls1", "ls", "-la")
	ok.Policy.Confirm = ConfirmNone
	if got, err := r.Add(ctx, ok, false); err != nil || got.Risk != "" {
		t.Errorf("%v %+v", err, got)
	}
	list, _ := r.List(ctx)
	risks := map[string]string{}
	for _, x := range list {
		risks[x.Name] = x.Risk
	}
	if risks["sq"] != RiskCodeExec || risks["sq2"] != RiskCodeExec || risks["ls1"] != "" {
		t.Errorf("list risks: %v", risks)
	}
}

// A tool stored before the classification existed (confirm=none, no ack) keeps
// loading and running; only the gateway starts asking (ForcedAsk).
func TestLegacyCodeExecRowStillResolvesButIsForcedToAsk(t *testing.T) {
	r, _ := newReg(t)
	ctx := context.Background()
	d := cmdDef("old", "sqlite3", "db.sqlite", "{{v}}")
	d.Policy.Confirm, d.Policy.AckCodeExec = ConfirmNone, true
	mustAdd(t, r, d)
	r.mustExec(t, `UPDATE settings SET value=json_remove(value,'$.ack_code_exec') WHERE key=?`, extKey("old"))
	got, err := r.Resolve(ctx, "old")
	if err != nil {
		t.Fatalf("legacy row must still load: %v", err)
	}
	if got.Policy.Confirm != ConfirmNone || got.Policy.AckCodeExec || got.Risk != RiskCodeExec || !got.ForcedAsk() {
		t.Errorf("%+v risk=%q forced=%v", got.Policy, got.Risk, got.ForcedAsk())
	}
	// explicit deny and ask are never changed by the classification
	got.Policy.Confirm = ConfirmDeny
	if got.ForcedAsk() {
		t.Error("deny stays deny")
	}
	// no schema column or migration is involved: Risk is derived, never stored
	var n int
	if err := r.DB.QueryRow(`SELECT count(*) FROM settings WHERE value LIKE '%code-exec%'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("risk must not be stored: n=%d err=%v", n, err)
	}
}

func TestPathArgumentsAreNotConfinedByDesign(t *testing.T) {
	// TASK-0018 AC-3 (docs/security.md 4.2): no path-typed variable exists. A value
	// that is an absolute path outside the cwd root is accepted as data, an option
	// such as `--output=` is refused only by the leading-dash rule.
	r, _ := newReg(t)
	d := cmdDef("catf", "cat", "--", "{{v}}")
	d.Policy.WorkingDir = t.TempDir()
	mustAdd(t, r, d)
	res, err := run(t, r, "catf", map[string]any{"v": "/etc/hosts"}, RunOptions{DryRun: true})
	if err != nil || res.DryRun == nil || res.DryRun.Argv[len(res.DryRun.Argv)-1] != "/etc/hosts" {
		t.Errorf("absolute path outside cwd is accepted as data (documented limit): %+v %v", res, err)
	}
	g := cmdDef("gd", "git", "diff", "{{v}}")
	mustAdd(t, r, g)
	if _, err := run(t, r, "gd", map[string]any{"v": "--output=/tmp/x"}, RunOptions{DryRun: true}); err == nil {
		t.Error("an option-looking value must be refused (leading dash)")
	}
}

// The run stage checks the executable twice after ValidateExec (runLocal: right
// after PATH resolution and again right before exec). Each case swaps a benign
// binary for a link to env at a different moment; neither may reach exec.
func TestRunStageRechecksDenyListAfterBinarySwap(t *testing.T) {
	envBin := lookOrSkip(t, "env")
	touch := lookOrSkip(t, "touch")
	setup := func(t *testing.T) (def *Definition, bin, marker string, swap func()) {
		r, _ := newReg(t)
		bin, work := t.TempDir(), t.TempDir()
		marker = filepath.Join(work, "PWNED")
		path := filepath.Join(bin, "mytool")
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		d := cmdDef("toc", "mytool", touch, marker)
		d.InputSchema = json.RawMessage(`{"type":"object"}`)
		d.Policy.WorkingDir = work
		d.Values = []mcp.Value{{Slot: mcp.SlotEnv, Key: "SWAP", RefType: mcp.RefEnv, Ref: "SWAP_TRIGGER"}}
		mustAdd(t, r, d) // benign at registration time
		def, err := r.Resolve(context.Background(), "toc")
		if err != nil {
			t.Fatal(err)
		}
		swap = func() {
			_ = os.Remove(path)
			if err := os.Symlink(envBin, path); err != nil {
				t.Error(err)
			}
		}
		return def, bin, marker, swap
	}
	refused := func(t *testing.T, res *Result, err error, marker string) {
		t.Helper()
		if err == nil || res.ErrorClass != ClassPolicy || res.OK || !strings.Contains(res.Error, "env") {
			t.Fatalf("swapped binary must be refused by the deny list: %+v %v", res, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("the swapped binary ran")
		}
	}
	t.Run("control", func(t *testing.T) {
		def, _, _, _ := setup(t)
		if res, err := Run(context.Background(), def, nil, RunOptions{Getenv: func(k string) string {
			if k == "SWAP_TRIGGER" {
				return "x"
			}
			return os.Getenv(k)
		}}); err != nil || !res.OK {
			t.Fatalf("benign binary must run: %+v %v", res, err)
		}
	})
	t.Run("swapped before the run, invisible to the registration-time PATH lookup", func(t *testing.T) {
		def, bin, marker, swap := setup(t)
		swap()
		t.Setenv("PATH", filepath.Dir(touch)) // ValidateExec (process PATH) no longer finds mytool
		res, err := Run(context.Background(), def, nil, RunOptions{Getenv: func(k string) string {
			switch k {
			case "PATH":
				return bin
			case "SWAP_TRIGGER":
				return "x"
			}
			return os.Getenv(k)
		}})
		refused(t, res, err, marker)
	})
	t.Run("dry-run stops at the first check (the only path where it has an effect of its own)", func(t *testing.T) {
		def, bin, marker, swap := setup(t)
		swap()
		t.Setenv("PATH", filepath.Dir(touch))
		res, err := Run(context.Background(), def, nil, RunOptions{DryRun: true, Getenv: func(k string) string {
			switch k {
			case "PATH":
				return bin
			case "SWAP_TRIGGER":
				return "x"
			}
			return os.Getenv(k)
		}})
		refused(t, res, err, marker)
		if res.DryRun != nil {
			t.Error("a refused dry-run must not produce a plan")
		}
	})
	t.Run("swapped between the first check and exec (re-check before exec)", func(t *testing.T) {
		def, _, marker, swap := setup(t)
		res, err := Run(context.Background(), def, nil, RunOptions{Getenv: func(k string) string {
			if k == "SWAP_TRIGGER" { // resolved while the environment is built, after the first check
				swap()
				return "x"
			}
			return os.Getenv(k)
		}})
		refused(t, res, err, marker)
	})
}

func tlsTrusting(t *testing.T, srv *httptest.Server) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func TestHTTPSToHTTPRedirectOverTLS(t *testing.T) {
	var plainHits, leaked int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&plainHits, 1)
		if req.Header.Get("X-Api-Key") != "" {
			atomic.AddInt32(&leaked, 1)
		}
		fmt.Fprint(w, "plain-body")
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/direct" {
			fmt.Fprint(w, "tls-body")
			return
		}
		http.Redirect(w, req, plain.URL+"/x", http.StatusFound)
	}))
	defer secure.Close()
	testTLSConfig = tlsTrusting(t, secure)
	t.Cleanup(func() { testTLSConfig = nil })
	t.Setenv("RD_TLS_KEY", "keyvalue-abcdefghij")
	vals := []mcp.Value{{Slot: mcp.SlotHeader, Key: "X-Api-Key", IsSecret: true, RefType: mcp.RefEnv, Ref: "RD_TLS_KEY"}}
	mk := func(path string, allowHTTP bool) *Definition {
		return &Definition{Name: "tls", Kind: KindHTTP, Method: "GET", URL: secure.URL + path, Values: vals,
			Policy: Policy{AllowHTTP: allowHTTP, AllowPrivate: true, Allow: []string{"127.0.0.1"}, TimeoutMS: 5000, MaxOutputBytes: 1024, MaxRedirects: 3}}
	}
	// control: the TLS server itself is reachable with the trusted test root
	if res, err := Run(context.Background(), mk("/direct", false), nil); err != nil || res.Stdout != "tls-body" {
		t.Fatalf("control over TLS: %+v %v", res, err)
	}
	// https -> http is refused by default and the plain server is never contacted
	res, err := Run(context.Background(), mk("/redir", false), nil)
	if err == nil || res.OK || res.ErrorClass != ClassPolicy || atomic.LoadInt32(&plainHits) != 0 {
		t.Fatalf("downgrade must be refused: %+v %v hits=%d", res, err, plainHits)
	}
	// even with allow_http the definition's secret header is not sent to the http hop
	res, err = Run(context.Background(), mk("/redir", true), nil)
	if err != nil || res.Stdout != "plain-body" {
		t.Fatalf("allowed downgrade: %+v %v", res, err)
	}
	if atomic.LoadInt32(&plainHits) != 1 || atomic.LoadInt32(&leaked) != 0 {
		t.Errorf("hits=%d leaked=%d: definition header must be dropped on https->http", plainHits, leaked)
	}
}
