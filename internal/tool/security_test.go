package tool

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// wrapperSchema declares three string variables a, b, c.
const wrapperSchema = `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"},"c":{"type":"string"}}}`

func touchPayload(marker string) string { return `BEGIN{system("touch ` + marker + `")}` }

func lookOrSkip(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed", name)
	}
	return p
}

// The control proves the payload really executes when nothing guards it; every
// case below must then be refused and leave no marker file.
func TestWrapperAllowlistBypassBlocked(t *testing.T) {
	r, _ := newReg(t)
	awk := lookOrSkip(t, "awk")
	work := t.TempDir()

	ctl := filepath.Join(work, "CONTROL")
	if err := exec.Command("env", awk, touchPayload(ctl), "x").Run(); err != nil {
		t.Skipf("control payload cannot run here: %v", err)
	}
	if _, err := os.Stat(ctl); err != nil {
		t.Fatal("control: payload should create the marker when unguarded")
	}

	marker := filepath.Join(work, "PWNED")
	wrappers := [][]string{
		{"env", "{{a}}", "{{b}}", "{{c}}"}, {"env", "-", "{{a}}"}, {"xargs", "--", "{{a}}"}, {"find", ".", "-exec", "{{a}}", ";"},
		{"nice", "--", "{{a}}"}, {"nohup", "--", "{{a}}"}, {"timeout", "5", "{{a}}"}, {"sudo", "--", "{{a}}"}, {"setsid", "--", "{{a}}"},
		{"stdbuf", "-o0", "{{a}}"}, {"awk", "{{a}}"}, {"gawk", "{{a}}"}, {"sed", "-n", "{{a}}"}, {"ssh", "--", "{{a}}"}, {"make", "-f", "{{a}}"},
		{"/usr/bin/env", "--", "{{a}}"}, {"python3.11", "--", "{{a}}"}, {"busybox", "--", "{{a}}"}, {"tar", "-cf", "x", "{{a}}"},
	}
	for i, argv := range wrappers {
		d := cmdDef("w"+string(rune('a'+i)), argv...)
		d.InputSchema = json.RawMessage(wrapperSchema)
		if _, err := r.Add(context.Background(), d, false); err == nil {
			t.Errorf("%v: registration must be refused", argv)
		}
		// hand-built definitions (tampered DB / direct Run callers) are refused at run time too
		d.Policy.Allow, d.Policy.WorkingDir = []string{argv[0]}, work
		res, err := Run(context.Background(), &d, map[string]any{"a": awk, "b": touchPayload(marker), "c": "x"})
		if err == nil || res.ErrorClass != ClassPolicy {
			t.Errorf("%v: run must be refused: %+v %v", argv, res, err)
		}
	}
	// `env {{v}}` with v=touch: no awk needed
	d := cmdDef("envv", "env", "{{v}}")
	d.Policy.WorkingDir = work
	if _, err := Run(context.Background(), &d, map[string]any{"v": "touch"}); err == nil {
		t.Error("env {{v}} must be refused")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("wrapper bypass executed a program")
	}
	// legitimate tools stay usable
	for _, argv := range [][]string{{"ls", "-1"}, {"cat", "--", "{{v}}"}, {"printf", "%s", "{{v}}"}, {"git", "log", "-n", "{{n}}"}, {"git", "status", "--short"}, {"grep", "-n", "--", "{{v}}"}} {
		d := cmdDef("ok", argv...)
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{"v":{"type":"string"},"n":{"type":"integer"}}}`)
		if err := d.Validate(); err != nil {
			t.Errorf("%v must stay allowed: %v", argv, err)
		}
	}
}

func TestSymlinkDisguiseBlockedByRealBinary(t *testing.T) {
	if _, err := exec.LookPath("env"); err != nil {
		t.Skip("no env")
	}
	envBin := lookOrSkip(t, "env")
	lsBin := lookOrSkip(t, "ls")
	dir := t.TempDir()
	work := t.TempDir()
	mustLink := func(target, name string) string {
		p := filepath.Join(dir, name)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mustLink(envBin, "harmless")                     // innocent name -> env
	mustLink(filepath.Join(dir, "harmless"), "hop2") // two hops
	mustLink(envBin, "busybox-applet")               // another innocent name
	okLink := mustLink(lsBin, "listing")             // link to a safe binary
	getenv := func(k string) string { return map[string]string{"PATH": dir + ":/usr/bin:/bin"}[k] }
	for _, name := range []string{"harmless", "hop2", "busybox-applet", filepath.Join(dir, "harmless")} {
		d := cmdDef("sl", name, "-n")
		d.Policy.WorkingDir, d.Policy.Allow = work, []string{name}
		res, err := Run(context.Background(), &d, map[string]any{"v": "x"}, RunOptions{Getenv: getenv})
		if err == nil || res.ErrorClass != ClassPolicy || !strings.Contains(err.Error(), "env") {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
	d := cmdDef("slok", "listing")
	d.Policy.WorkingDir, d.Policy.Allow = work, []string{"listing"}
	if res, err := Run(context.Background(), &d, map[string]any{"v": "x"}, RunOptions{Getenv: getenv}); err != nil || !res.OK {
		t.Errorf("link to a safe binary must run: %+v %v", res, err)
	}
	_ = okLink
	// registration also follows the link when it is reachable from PATH
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if err := (&Definition{Name: "x", Kind: KindCommand, Description: "x", Command: "harmless"}).Validate(); err == nil {
		t.Error("registration must resolve symlinks")
	}
}

func TestVariableInProgramOrSubcommandSlotRefused(t *testing.T) {
	sch := json.RawMessage(`{"type":"object","properties":{"sub":{"type":"string"},"f":{"type":"string"},"n":{"type":"integer"}}}`)
	mk := func(argv ...string) Definition {
		d := cmdDef("t", argv...)
		d.InputSchema = sch
		return d
	}
	for _, argv := range [][]string{{"git", "{{sub}}"}, {"git", "{{sub}}", "--stat"}, {"cat", "{{f}}"}, {"git", "x{{sub}}"}, {"ls", "{{f}}"}} {
		d := mk(argv...)
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "argv[1]") {
			t.Errorf("%v: %v", argv, err)
		}
	}
	for _, argv := range [][]string{{"cat", "--", "{{f}}"}, {"git", "log", "-n", "{{n}}"}, {"ls", "-l", "{{f}}"}} {
		d := mk(argv...)
		if err := d.Validate(); err != nil {
			t.Errorf("%v: %v", argv, err)
		}
	}
	d := mk("git", "{{sub}}")
	d.Policy.AllowVarSubcommand = true
	if err := d.Validate(); err != nil {
		t.Errorf("explicit flag must lift the rule: %v", err)
	}
	d = mk("env", "{{sub}}")
	d.Policy.AllowVarSubcommand = true
	if err := d.Validate(); err == nil {
		t.Error("the flag must not lift the deny list")
	}
	for _, bad := range [][]string{{"git", "-c", "core.sshCommand=touch x", "fetch"}, {"git", "fetch", "--upload-pack=touch x"}, {"git", "--exec-path=/x", "log"}, {"git", "-c"}} {
		d := mk(bad...)
		if err := d.Validate(); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
}

func TestAllowlistMustContainCommand(t *testing.T) {
	r, _ := newReg(t)
	d := cmdDef("al2", "ls")
	d.Policy.Allow = []string{"cat"}
	if _, err := r.Add(context.Background(), d, false); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("--allow cat -- ls must be refused: %v", err)
	}
	d.Policy.Allow = []string{"cat", "ls"}
	if _, err := r.Add(context.Background(), d, false); err != nil {
		t.Errorf("%v", err)
	}
	p, _ := exec.LookPath("ls")
	d2 := cmdDef("al3", p)
	d2.Policy.Allow = []string{p}
	if _, err := r.Add(context.Background(), d2, false); err != nil {
		t.Errorf("absolute path allowlist: %v", err)
	}
}

func (r *Registry) mustExec(t *testing.T, q string, a ...any) {
	t.Helper()
	if _, err := r.DB.Exec(q, a...); err != nil {
		t.Fatal(err)
	}
}

func TestTamperedDBRowsFailPolicyOnResolve(t *testing.T) {
	r, _ := newReg(t)
	work := t.TempDir()
	marker := filepath.Join(work, "TAMPERED")
	for _, tc := range []struct{ name, cmd, args, ext string }{
		{"t1", "/bin/sh", `["-c","touch ` + marker + `"]`, ""},                         // shell without shell flag
		{"t2", "env", `["touch","` + marker + `"]`, ""},                                // wrapper
		{"t3", "printf", `["x"]`, `{"allow":["cat"]}`},                                 // allowlist no longer contains command
		{"t4", "sh", `["-c","touch ` + marker + `"]`, `{"shell":true,"allow":["sh"]}`}, // shell flag but args contain {{ ... }} below
	} {
		d := cmdDef(tc.name, "printf", "x")
		d.Policy.WorkingDir = work
		mustAdd(t, r, d)
		r.mustExec(t, `UPDATE tools SET command=?, args=? WHERE name=?`, tc.cmd, tc.args, tc.name)
		if tc.ext != "" {
			r.mustExec(t, `UPDATE settings SET value=? WHERE key=?`, tc.ext, extKey(tc.name))
		}
		if tc.name == "t4" {
			r.mustExec(t, `UPDATE tools SET args=? WHERE name=?`, `["-c","echo {{v}}"]`, tc.name) // template in shell script
		}
		if _, err := r.Resolve(context.Background(), tc.name); err == nil || !strings.Contains(err.Error(), "정책") {
			t.Errorf("%s: tampered row must be refused on load: %v", tc.name, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: executed", tc.name)
		}
	}
	// Run alone (no Resolve) refuses a hand-built tampered definition as well
	bad := Definition{Name: "b", Kind: KindCommand, Description: "x", Command: "/bin/sh", Args: []string{"-c", "touch " + marker}}
	bad.Policy.Allow, bad.Policy.WorkingDir = []string{"/bin/sh"}, work
	if res, err := Run(context.Background(), &bad, nil); err == nil || res.ErrorClass != ClassPolicy {
		t.Errorf("%+v %v", res, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("executed")
	}
}

func TestScriptStoreIntegrityAndPermissions(t *testing.T) {
	r, home := newReg(t)
	d := Definition{Name: "sc", Kind: KindScript, Description: "x", Interpreter: "sh", ScriptBody: "echo hi"}
	mustAdd(t, r, d)
	dir := filepath.Join(home, "scripts", "sc@1")
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir perm %v", fi.Mode().Perm())
	}
	for _, f := range []string{"script", "script.sha256"} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
	if _, err := r.Resolve(context.Background(), "sc"); err != nil {
		t.Fatal(err)
	}
	// 1. world-readable file
	if err := os.Chmod(filepath.Join(dir, "script"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), "sc"); err == nil || !strings.Contains(err.Error(), "권한") {
		t.Errorf("loose perms: %v", err)
	}
	_ = os.Chmod(filepath.Join(dir, "script"), 0o600)
	// 2. body and DB hash both rewritten, sidecar record untouched
	evil := "touch PWNED"
	if err := os.WriteFile(filepath.Join(dir, "script"), []byte(evil), 0o600); err != nil {
		t.Fatal(err)
	}
	r.mustExec(t, `UPDATE tools SET script_hash=? WHERE name='sc'`, hashHex([]byte(evil)))
	if _, err := r.Resolve(context.Background(), "sc"); err == nil || !strings.Contains(err.Error(), "변조") {
		t.Errorf("DB+file rewrite must be caught by the recorded hash: %v", err)
	}
	// 3. script file replaced by a symlink
	_ = os.Remove(filepath.Join(dir, "script"))
	target := filepath.Join(t.TempDir(), "elsewhere")
	_ = os.WriteFile(target, []byte("echo hi"), 0o600)
	_ = os.Symlink(target, filepath.Join(dir, "script"))
	if _, err := r.Resolve(context.Background(), "sc"); err == nil {
		t.Error("symlinked script must be refused")
	}
	// 4. interpreter swapped to something outside the policy in the DB
	r.mustExec(t, `UPDATE tools SET interpreter='evilsh' WHERE name='sc'`)
	if _, err := r.Resolve(context.Background(), "sc"); err == nil {
		t.Error("tampered interpreter")
	}
}

func TestHTTPPathTraversalRefused(t *testing.T) {
	d := &Definition{Name: "p", Kind: KindHTTP, Method: "GET", URL: "https://api.example.com/v1/items/{{id}}/detail",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`), Policy: Policy{Allow: []string{"api.example.com"}}}
	for _, v := range []string{"..", ".", "%2e%2e", "%2E%2E", "%252e%252e", "a/b", `a\b`, "..%2f..", "%2e", "a%2fb", "%5c"} {
		_, err := buildHTTP(d, map[string]any{"id": v}, RunOptions{}, &masker{}, true)
		if err == nil || !strings.Contains(err.Error(), "경로") {
			t.Errorf("%q must be refused: %v", v, err)
		}
	}
	for _, v := range []string{"abc", "a.b", "v1..2", "한글", "100%", "a b", "..x", "x.."} {
		if _, err := buildHTTP(d, map[string]any{"id": v}, RunOptions{}, &masker{}, true); err != nil {
			t.Errorf("%q must be allowed: %v", v, err)
		}
	}
}

func TestRedirectDowngradeStripsDefinitionHeaders(t *testing.T) {
	mk := func(s string) *url.URL { u, _ := url.Parse(s); return u }
	orig := mk("https://api.example.com/x")
	for _, tc := range []struct {
		next  string
		strip bool
	}{
		{"https://api.example.com/y", false},
		{"http://api.example.com/y", true}, // same host, https -> http downgrade (was kept before the fix)
		{"https://other.example.com/y", true},
		{"https://api.example.com:8443/y", true},
		{"HTTPS://API.EXAMPLE.COM/y", false},
	} {
		if got := stripDefinitionHeaders(orig, mk(tc.next)); got != tc.strip {
			t.Errorf("%s: strip=%v want %v", tc.next, got, tc.strip)
		}
	}
}

func TestDryRunDoesNotPrintParentPATH(t *testing.T) {
	r, _ := newReg(t)
	mustAdd(t, r, cmdDef("dr", "printf", "%s", "{{v}}"))
	const long = "/very/secret/home/me/bin:/usr/bin:/bin"
	res, err := run(t, r, "dr", map[string]any{"v": "x"}, RunOptions{DryRun: true, Getenv: func(k string) string {
		if k == "PATH" {
			return long
		}
		return ""
	}})
	if err != nil || res.DryRun == nil {
		t.Fatalf("%+v %v", res, err)
	}
	if p := res.DryRun.Env["PATH"]; strings.Contains(p, "/very/secret") || !strings.Contains(p, "PATH") && !strings.Contains(p, "디렉터리") {
		t.Errorf("PATH shown: %q", p)
	}
}

func TestSchemaViolationMessageIsKoreanFirst(t *testing.T) {
	d := cmdDef("sv", "printf", "%s", "{{v}}")
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	_, err := ValidateArgs(&d, map[string]any{"v": 5})
	var ae *ArgsError
	if err == nil || !asArgsError(err, &ae) {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "validating") || !strings.HasPrefix(err.Error(), "입력 인자가") {
		t.Errorf("message must be Korean without raw validator text: %s", err)
	}
	if !strings.Contains(ae.Detail, "validating") {
		t.Errorf("raw text must be kept in Detail: %q", ae.Detail)
	}
	res, _ := Run(context.Background(), &d, map[string]any{"v": 5})
	if res.ErrorClass != ClassInvalidArgs || !strings.Contains(res.ErrorDetail, "validating") || strings.Contains(res.Error, "validating") {
		t.Errorf("%+v", res)
	}
}

func asArgsError(err error, target **ArgsError) bool {
	ae, ok := err.(*ArgsError)
	*target = ae
	return ok
}
