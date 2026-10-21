package tool

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/store"
)

func newReg(t *testing.T) (*Registry, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix only")
	}
	home := t.TempDir()
	conn, err := store.Open(context.Background(), store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &Registry{DB: conn, Home: home}, home
}

const nameSchema = `{"type":"object","properties":{"v":{"type":"string"},"n":{"type":"integer","minimum":1},"mode":{"type":"string","enum":["a","b"]}},"required":["v"]}`

func cmdDef(name string, argv ...string) Definition {
	return Definition{Name: name, Kind: KindCommand, Description: "test", InputSchema: json.RawMessage(nameSchema),
		Command: argv[0], Args: argv[1:]}
}

func mustAdd(t *testing.T, r *Registry, d Definition) *Definition {
	t.Helper()
	got, err := r.Add(context.Background(), d, false)
	if err != nil {
		t.Fatalf("add %s: %v", d.Name, err)
	}
	return got
}

func run(t *testing.T, r *Registry, name string, args map[string]any, o RunOptions) (*Result, error) {
	t.Helper()
	d, err := r.Resolve(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return Run(context.Background(), d, args, o)
}

func TestSchemaValidation(t *testing.T) {
	r, _ := newReg(t)
	mustAdd(t, r, cmdDef("echo1", "printf", "[%s]", "{{v}}"))
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"required", map[string]any{}, "v"},
		{"type", map[string]any{"v": 5}, "타입"},
		{"min", map[string]any{"v": "a", "n": 0}, "minimum"},
		{"enum", map[string]any{"v": "a", "mode": "zzz"}, "enum"},
		{"additional", map[string]any{"v": "a", "extra": 1}, "정의되지 않은"},
	} {
		res, err := run(t, r, "echo1", tc.args, RunOptions{})
		if err == nil || res.ErrorClass != ClassInvalidArgs || !strings.Contains(err.Error(), "스키마") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: res=%+v err=%v", tc.name, res, err)
		}
	}
	res, err := run(t, r, "echo1", map[string]any{"v": "ok", "n": 2, "mode": "a"}, RunOptions{})
	if err != nil || !res.OK || res.Stdout != "[ok]" {
		t.Errorf("%+v %v", res, err)
	}
}

func TestUndeclaredAndNonScalarTemplateVars(t *testing.T) {
	r, _ := newReg(t)
	if _, err := r.Add(context.Background(), cmdDef("bad1", "printf", "%s", "{{missing}}"), false); err == nil || !strings.Contains(err.Error(), "선언되어 있지 않습니다") {
		t.Errorf("%v", err)
	}
	d := cmdDef("bad2", "printf", "%s", "{{o}}")
	d.InputSchema = json.RawMessage(`{"type":"object","properties":{"o":{"type":"object"}}}`)
	if _, err := r.Add(context.Background(), d, false); err == nil || !strings.Contains(err.Error(), "string/integer") {
		t.Errorf("%v", err)
	}
	d = cmdDef("bad3", "printf", "{{v")
	if _, err := r.Add(context.Background(), d, false); err == nil {
		t.Error("stray braces")
	}
	d = cmdDef("{{v}}", "printf")
	if _, err := r.Add(context.Background(), cmdDef("bad4", "{{v}}"), false); err == nil {
		t.Error("template in argv[0]")
	}
	for _, sh := range []string{"sh", "bash", "python3", "/bin/sh"} {
		if _, err := r.Add(context.Background(), cmdDef("bad5", sh, "-c", "{{v}}"), false); err == nil {
			t.Errorf("%s as command must be refused", sh)
		}
	}
	if _, err := r.Add(context.Background(), Definition{Name: "bad name", Kind: KindCommand, Description: "x", Command: "ls"}, false); err == nil {
		t.Error("name")
	}
	// non-object root schema
	d = cmdDef("bad6", "ls")
	d.InputSchema = json.RawMessage(`{"type":"string"}`)
	if _, err := r.Add(context.Background(), d, false); err == nil {
		t.Error("root schema type")
	}
	_ = d
}

func TestCommandInjectionIsJustAnArgument(t *testing.T) {
	r, _ := newReg(t)
	work := t.TempDir()
	d := cmdDef("inj", "printf", "<%s>", "{{v}}")
	d.Policy.WorkingDir = work
	mustAdd(t, r, d)
	for _, v := range []string{"; rm -rf .; touch pwned", "$(touch pwned)", "`touch pwned`", "a b  c", "x\ny\nz", "| cat", "&& touch pwned", "'\"", "\\", "*"} {
		res, err := run(t, r, "inj", map[string]any{"v": v}, RunOptions{})
		if err != nil || !res.OK || res.Stdout != "<"+v+">" {
			t.Errorf("%q: %+v %v", v, res, err)
		}
	}
	if _, err := os.Stat(filepath.Join(work, "pwned")); err == nil {
		t.Fatal("injection executed")
	}
	// option injection
	res, err := run(t, r, "inj", map[string]any{"v": "--flag"}, RunOptions{})
	if err == nil || res.ErrorClass != ClassInvalidArgs || !strings.Contains(err.Error(), "'-'") {
		t.Errorf("leading dash: %+v %v", res, err)
	}
	// NUL byte
	if res, err = run(t, r, "inj", map[string]any{"v": "a\x00b"}, RunOptions{}); err == nil || res.ErrorClass != ClassInvalidArgs {
		t.Errorf("nul: %+v %v", res, err)
	}
	// allowed by explicit policy
	d2 := cmdDef("inj2", "printf", "<%s>", "{{v}}")
	d2.Policy.AllowLeadingDash = true
	mustAdd(t, r, d2)
	if res, err = run(t, r, "inj2", map[string]any{"v": "--flag"}, RunOptions{}); err != nil || res.Stdout != "<--flag>" {
		t.Errorf("%+v %v", res, err)
	}
	// a template that itself starts with '-' is the author's choice
	d3 := cmdDef("inj3", "printf", "%s", "--x={{v}}")
	mustAdd(t, r, d3)
	if res, err = run(t, r, "inj3", map[string]any{"v": "-y"}, RunOptions{}); err != nil || res.Stdout != "--x=-y" {
		t.Errorf("%+v %v", res, err)
	}
}

func TestShellModeUsesEnvNotConcatenation(t *testing.T) {
	r, _ := newReg(t)
	work := t.TempDir()
	d := Definition{Name: "sh1", Kind: KindCommand, Description: "x", InputSchema: json.RawMessage(nameSchema), Shell: true,
		Command: "sh", Args: []string{"-c", `printf '%s' "$FORGE_ARG_v"`}}
	d.Policy.WorkingDir = work
	mustAdd(t, r, d)
	payload := "$(touch pwned); `touch pwned2`; \"; touch pwned3 #"
	res, err := run(t, r, "sh1", map[string]any{"v": payload}, RunOptions{})
	if err != nil || res.Stdout != payload {
		t.Errorf("%+v %v", res, err)
	}
	for _, f := range []string{"pwned", "pwned2", "pwned3"} {
		if _, err := os.Stat(filepath.Join(work, f)); err == nil {
			t.Errorf("%s created", f)
		}
	}
	bad := d
	bad.Name = "sh2"
	bad.Args = []string{"-c", "echo {{v}}"}
	if _, err := r.Add(context.Background(), bad, false); err == nil {
		t.Error("placeholders in shell script must be refused")
	}
}

func TestAllowlistDenied(t *testing.T) {
	r, _ := newReg(t)
	mustAdd(t, r, cmdDef("al", "printf", "x"))
	d, _ := r.Resolve(context.Background(), "al")
	d.Policy.Allow = []string{"ls"}
	res, err := Run(context.Background(), d, map[string]any{"v": "x"})
	if err == nil || res.ErrorClass != ClassPolicy || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("%+v %v", res, err)
	}
	// absolute path allowlist
	p, _ := lookPath("printf", RunOptions{Getenv: os.Getenv})
	d.Policy.Allow = []string{p}
	if res, err = Run(context.Background(), d, map[string]any{"v": "x"}); err != nil || !res.OK {
		t.Errorf("%+v %v", res, err)
	}
	// unknown interpreter for script
	sd := Definition{Name: "s", Kind: KindScript, Description: "x", Interpreter: "evilsh", ScriptBody: "x"}
	if err := sd.Validate(); err == nil {
		t.Error("interpreter allowlist")
	}
	// confirm=deny
	d.Policy.Confirm = ConfirmDeny
	if res, err = Run(context.Background(), d, map[string]any{"v": "x"}); err == nil || res.ErrorClass != ClassDenied {
		t.Errorf("%+v %v", res, err)
	}
}

func TestCwdPolicy(t *testing.T) {
	r, _ := newReg(t)
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	d := cmdDef("pwd1", "pwd")
	d.Policy.WorkingDir = root
	mustAdd(t, r, d)
	realRoot, _ := filepath.EvalSymlinks(root)
	res, err := run(t, r, "pwd1", map[string]any{"v": "x"}, RunOptions{})
	if err != nil || strings.TrimSpace(res.Stdout) != realRoot {
		t.Errorf("%+v %v", res, err)
	}
	if res, err = run(t, r, "pwd1", map[string]any{"v": "x"}, RunOptions{Cwd: "sub"}); err != nil || strings.TrimSpace(res.Stdout) != filepath.Join(realRoot, "sub") {
		t.Errorf("%+v %v", res, err)
	}
	for _, c := range []string{"..", "../outside", "link", outside, "sub/../.."} {
		if res, err = run(t, r, "pwd1", map[string]any{"v": "x"}, RunOptions{Cwd: c}); err == nil || res.ErrorClass != ClassPolicy || !strings.Contains(err.Error(), "벗어납니다") {
			t.Errorf("cwd %q must be refused: %+v %v", c, res, err)
		}
	}
}

func TestEnvIsolation(t *testing.T) {
	r, _ := newReg(t)
	const secret = "ghp_FAKEPARENTSECRET0123456789"
	t.Setenv("PARENT_ONLY_TOKEN", secret)
	t.Setenv("DECLARED_TOKEN", "declaredvalue-xyz-123")
	d := cmdDef("envt", "printenv")
	d.Values = []mcp.Value{{Slot: mcp.SlotEnv, Key: "API_TOKEN", IsSecret: true, RefType: mcp.RefEnv, Ref: "DECLARED_TOKEN"}}
	mustAdd(t, r, d)
	res, err := run(t, r, "envt", map[string]any{"v": "x"}, RunOptions{})
	if err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Contains(res.Stdout, "PARENT_ONLY_TOKEN") || strings.Contains(res.Stdout, secret) {
		t.Errorf("parent secret leaked to child: %s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "API_TOKEN=***") || strings.Contains(res.Stdout, "declaredvalue") {
		t.Errorf("declared secret must reach the child but be masked in the result: %s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "FORGE_TOOL_NAME=envt") || !strings.Contains(res.Stdout, "PATH=") {
		t.Errorf("%s", res.Stdout)
	}
	// unset reference
	os.Unsetenv("DECLARED_TOKEN")
	if res, err = run(t, r, "envt", map[string]any{"v": "x"}, RunOptions{}); err == nil || !strings.Contains(err.Error(), "DECLARED_TOKEN") {
		t.Errorf("%+v %v", res, err)
	}
	// FORGE_ prefix reserved, literal secret refused
	bad := cmdDef("envbad", "printenv")
	bad.Values = []mcp.Value{{Slot: mcp.SlotEnv, Key: "FORGE_X", RefType: mcp.RefLiteral, Literal: "1"}}
	if _, err := r.Add(context.Background(), bad, false); err == nil {
		t.Error("FORGE_ prefix")
	}
}

func TestTimeoutKillsGrandchildren(t *testing.T) {
	r, _ := newReg(t)
	work := t.TempDir()
	pidFile := filepath.Join(work, "pid")
	d := Definition{Name: "gc", Kind: KindCommand, Description: "x", Shell: true, Command: "sh",
		Args: []string{"-c", "sleep 60 & echo $! > pid; wait"}}
	d.Policy.WorkingDir = work
	d.Policy.TimeoutMS = 600
	mustAdd(t, r, d)
	start := time.Now()
	res, err := run(t, r, "gc", nil, RunOptions{})
	if err == nil || !res.TimedOut || res.ErrorClass != ClassTimeout {
		t.Fatalf("%+v %v", res, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	b, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	var pid int
	if _, err := fmtSscan(string(b), &pid); err != nil || pid <= 1 {
		t.Fatalf("pid %q", b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("grandchild still alive after timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// per-call override above the cap is refused
	if _, err := run(t, r, "gc", nil, RunOptions{Timeout: MaxTimeout + time.Second}); err == nil {
		t.Error("timeout cap")
	}
}

func TestOutputCap(t *testing.T) {
	r, _ := newReg(t)
	d := Definition{Name: "big", Kind: KindCommand, Description: "x", Shell: true, Command: "sh",
		Args: []string{"-c", "head -c 300000 /dev/zero | tr '\\0' a; head -c 5000 /dev/zero | tr '\\0' b >&2"}}
	d.Policy.MaxOutputBytes = 1000
	mustAdd(t, r, d)
	res, err := run(t, r, "big", nil, RunOptions{})
	if err != nil || !res.OK || len(res.Stdout) != 1000 || !res.StdoutTruncated || len(res.Stderr) != 1000 || !res.StderrTruncated {
		t.Errorf("out=%d/%v err=%d/%v %v", len(res.Stdout), res.StdoutTruncated, len(res.Stderr), res.StderrTruncated, err)
	}
	if _, err := r.Add(context.Background(), Definition{Name: "huge", Kind: KindCommand, Description: "x", Command: "ls", Policy: Policy{MaxOutputBytes: MaxOutputCap + 1}}, false); err == nil {
		t.Error("output cap upper bound")
	}
	// non-zero exit is a result, not an error
	d2 := Definition{Name: "fail", Kind: KindCommand, Description: "x", Shell: true, Command: "sh", Args: []string{"-c", "echo oops >&2; exit 3"}}
	mustAdd(t, r, d2)
	res, err = run(t, r, "fail", nil, RunOptions{})
	if err != nil || res.OK || res.ExitCode != 3 || res.ErrorClass != ClassExit || !strings.Contains(res.Stderr, "oops") {
		t.Errorf("%+v %v", res, err)
	}
}

func havePython(t *testing.T) {
	t.Helper()
	if _, err := lookPath("python3", RunOptions{Getenv: os.Getenv}); err != nil {
		t.Skip("python3 not available")
	}
}

func TestScriptKind(t *testing.T) {
	havePython(t)
	r, home := newReg(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	body := `import json, os, stat, sys
a = json.load(sys.stdin)
print(json.dumps({"sum": a["x"] + a["y"], "tool": os.environ["FORGE_TOOL_NAME"], "kind": os.environ["FORGE_TOOL_KIND"], "mode": oct(stat.S_IMODE(os.stat(sys.argv[0]).st_mode)), "tok": os.environ.get("MY_TOKEN", "")}))
`
	d := Definition{Name: "adder", Kind: KindScript, Description: "x", Interpreter: "python3", ScriptBody: body,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"},"y":{"type":"integer"}},"required":["x","y"]}`),
		Values:      []mcp.Value{{Slot: mcp.SlotEnv, Key: "MY_TOKEN", IsSecret: true, RefType: mcp.RefEnv, Ref: "FAKE_SCRIPT_TOKEN"}}}
	t.Setenv("FAKE_SCRIPT_TOKEN", "scripttoken-value-1234")
	saved := mustAdd(t, r, d)
	if saved.ScriptPath != "scripts/adder@1/script" {
		t.Errorf("%s", saved.ScriptPath)
	}
	res, err := run(t, r, "adder", map[string]any{"x": 2, "y": 40}, RunOptions{})
	if err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil || out["sum"].(float64) != 42 || out["tool"] != "adder" || out["kind"] != "script" || out["mode"] != "0o600" {
		t.Errorf("%s %v", res.Stdout, err)
	}
	if out["tok"] != "***" {
		t.Errorf("secret echoed by the script must be masked: %v", out["tok"])
	}
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Errorf("temp files left: %v", ents)
	}
	// replace -> revision 2, old script dir pruned
	d.ScriptBody = body + "\n# v2\n"
	got, err := r.Add(context.Background(), d, true)
	if err != nil || got.Revision != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(home, "scripts", "adder@1")); err == nil {
		t.Error("old script dir must be removed")
	}
	// tamper detection
	if err := os.WriteFile(filepath.Join(home, "scripts", "adder@2", "script"), []byte("print('evil')"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), "adder"); err == nil || !strings.Contains(err.Error(), "해시") {
		t.Errorf("tamper: %v", err)
	}
	// remove deletes scripts
	if err := r.Remove(context.Background(), "adder"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "scripts", "adder@2")); err == nil {
		t.Error("script dir must be removed on remove")
	}
	// secret-looking script body refused
	d.Name, d.ScriptBody = "leaky", `KEY = "ghp_abcdefghijklmnop1234"`
	if _, err := r.Add(context.Background(), d, false); err == nil {
		t.Error("literal secret in script")
	}
}

func TestRegistryLifecycleAndRevision(t *testing.T) {
	r, _ := newReg(t)
	ctx := context.Background()
	mustAdd(t, r, cmdDef("a", "printf", "%s", "{{v}}"))
	if _, err := r.Add(ctx, cmdDef("a", "printf", "x"), false); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Errorf("%v", err)
	}
	if err := r.SetEnabled(ctx, "a", false); err != nil {
		t.Fatal(err)
	}
	d := cmdDef("a", "printf", "y")
	d.Policy.Allow = []string{"printf"}
	got, err := r.Add(ctx, d, true)
	if err != nil || got.Revision != 2 || got.Enabled {
		t.Fatalf("replace keeps enabled flag and bumps revision: %+v %v", got, err)
	}
	g, _ := r.Get(ctx, "a")
	if g.Args[0] != "y" || g.Revision != 2 || g.CreatedAt > g.UpdatedAt {
		t.Errorf("%+v", g)
	}
	if err := r.SetEnabled(ctx, "nope", true); err == nil {
		t.Error("not found")
	}
	if err := r.Remove(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = r.DB.QueryRow(`SELECT count(*) FROM settings WHERE key LIKE 'tool.ext.%'`).Scan(&n)
	if n != 0 {
		t.Error("ext settings must be removed")
	}
	if _, err := r.Get(ctx, "a"); err == nil {
		t.Error("removed")
	}
}

func TestConcurrentAdd(t *testing.T) {
	r, home := newReg(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, exists, other := 0, 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := store.Open(context.Background(), store.DBPath(home))
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			rr := &Registry{DB: conn, Home: home}
			_, err = rr.Add(context.Background(), cmdDef("same", "printf", "x"), false)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case strings.Contains(err.Error(), ErrExists.Error()):
				exists++
			default:
				other++
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || exists != 7 || other != 0 {
		t.Errorf("ok=%d exists=%d other=%d", ok, exists, other)
	}
	_ = r
}

func TestNoPlaintextSecretInDB(t *testing.T) {
	r, home := newReg(t)
	const secret = "supersecret-value-for-test-9f8e7d"
	t.Setenv("HDR_SECRET_ENV", secret)
	d := Definition{Name: "api", Kind: KindHTTP, Description: "x", URL: "https://api.example.com/v1", Values: []mcp.Value{
		{Slot: mcp.SlotHeader, Key: "Authorization", IsSecret: true, RefType: mcp.RefEnv, Ref: "HDR_SECRET_ENV"}}}
	mustAdd(t, r, d)
	rows, err := r.DB.Query(`SELECT slot, key, is_secret, ref_type, ref, value FROM tool_values`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s, k, rt string
		var sec int
		var ref, val *string
		_ = rows.Scan(&s, &k, &sec, &rt, &ref, &val)
		if rt != "env" || *ref != "HDR_SECRET_ENV" || val != nil || sec != 1 {
			t.Errorf("%s %s %s %v %v", s, k, rt, ref, val)
		}
	}
	// Dry-run resolves nothing and a real run would only keep the value in memory.
	res, err := run(t, r, "api", nil, RunOptions{DryRun: true})
	if err != nil || res.DryRun == nil || strings.Contains(res.DryRun.Headers["Authorization"], secret) {
		t.Errorf("%+v %v", res, err)
	}
	// raw file bytes (db + wal)
	_ = r.DB.QueryRow(`PRAGMA wal_checkpoint(FULL)`).Scan(new(int), new(int), new(int))
	for _, f := range []string{"forge.db", "forge.db-wal"} {
		if b, err := os.ReadFile(filepath.Join(home, f)); err == nil && strings.Contains(string(b), secret) {
			t.Errorf("%s contains the plaintext secret", f)
		}
	}
	// literal secrets are refused at every entry
	for _, v := range []mcp.Value{
		{Slot: mcp.SlotHeader, Key: "Authorization", RefType: mcp.RefLiteral, Literal: "Bearer abcdefghijklmnopqrstuvwxyz"},
		{Slot: mcp.SlotHeader, Key: "X-Note", RefType: mcp.RefLiteral, Literal: "ghp_abcdefghijklmnop1234"},
		{Slot: mcp.SlotHeader, Key: "X-Api-Key", IsSecret: true, RefType: mcp.RefLiteral, Literal: "abc"},
	} {
		bad := d
		bad.Name, bad.Values = "bad", []mcp.Value{v}
		if _, err := r.Add(context.Background(), bad, false); err == nil {
			t.Errorf("literal %q must be refused", v.Key)
		}
	}
	for _, u := range []string{"https://api.example.com/v1?token=abcdef123456", "https://api.example.com/ghp_abcdefghijklmnop1234", "https://user:pw@api.example.com/"} {
		bad := d
		bad.Name, bad.URL, bad.Values = "bad", u, nil
		if _, err := r.Add(context.Background(), bad, false); err == nil {
			t.Errorf("url %q must be refused", u)
		}
	}
	bad := cmdDef("bad", "printf", "ghp_abcdefghijklmnop1234")
	if _, err := r.Add(context.Background(), bad, false); err == nil {
		t.Error("secret in argv")
	}
	if _, err := mcp.ParseValue(mcp.SlotHeader, "Authorization=Bearer sk-abcdefghijklmnop"); err == nil {
		t.Error("ParseValue")
	}
}

func TestDryRunMasksAndExecutesNothing(t *testing.T) {
	r, _ := newReg(t)
	work := t.TempDir()
	t.Setenv("DRY_TOKEN", "dryvalue-abcdefghij")
	d := Definition{Name: "dry", Kind: KindCommand, Description: "x", InputSchema: json.RawMessage(nameSchema), Command: "touch", Args: []string{"--", "{{v}}"},
		Values: []mcp.Value{{Slot: mcp.SlotEnv, Key: "API_TOKEN", IsSecret: true, RefType: mcp.RefEnv, Ref: "DRY_TOKEN"}}}
	d.Policy.WorkingDir = work
	mustAdd(t, r, d)
	res, err := run(t, r, "dry", map[string]any{"v": "created"}, RunOptions{DryRun: true})
	if err != nil || res.DryRun == nil {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(work, "created")); err == nil {
		t.Error("dry-run executed")
	}
	if res.DryRun.Argv[len(res.DryRun.Argv)-1] != "created" || strings.Contains(res.DryRun.Env["API_TOKEN"], "dryvalue") || !strings.Contains(res.DryRun.Env["API_TOKEN"], "${DRY_TOKEN}") {
		t.Errorf("%+v", res.DryRun)
	}
	// a secret-looking argument value is masked
	res, _ = run(t, r, "dry", map[string]any{"v": "ghp_abcdefghijklmnop1234"}, RunOptions{DryRun: true})
	if res.DryRun.Argv[len(res.DryRun.Argv)-1] != "***" {
		t.Errorf("%+v", res.DryRun)
	}
}

func lookupMap(m map[string]string) func(context.Context, string) ([]net.IP, error) {
	return func(_ context.Context, host string) ([]net.IP, error) {
		if v, ok := m[host]; ok {
			return []net.IP{net.ParseIP(v)}, nil
		}
		if ip := net.ParseIP(host); ip != nil {
			return []net.IP{ip}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
}

func fmtSscan(s string, pid *int) (int, error) {
	n := 0
	var err error
	s = strings.TrimSpace(s)
	for _, c := range s {
		if c < '0' || c > '9' {
			err = os.ErrInvalid
			break
		}
		n = n*10 + int(c-'0')
	}
	*pid = n
	return 1, err
}
