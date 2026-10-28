package mcp

import (
	"context"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ---- TASK-0017 AC-1: ${ENV} in stdio args (warning, not refusal) -----------

func argRefWarnings(p *Preview) []string {
	var out []string
	for _, w := range p.Warnings() {
		if strings.Contains(w, "${...} 참조") {
			out = append(out, w)
		}
	}
	return out
}

func TestEnvRefInArgsRegistersWithWarning(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	for i, args := range [][]string{
		{"server.js", "${ROOT}"}, {"--root=${ROOT}"}, {"--root", "${ROOT:-DEFAULTSENTINEL9}"}, {"prefix-${ROOT}-suffix"},
		{"{env:ROOT}"}, {"--cfg={file:/etc/x}"},
	} {
		sp := Spec{Name: "r" + string(rune('a'+i)), Transport: TransportStdio, Command: "node", Args: args}
		if err := reg.Add(ctx, sp); err != nil {
			t.Errorf("%v: registration must still succeed (HEAD behaviour, Claude Code expands it): %v", args, err)
			continue
		}
		w := argRefWarnings(BuildPreview(sp))
		if len(w) != 1 {
			t.Errorf("%v: want exactly one warning, got %v", args, w)
			continue
		}
		for _, want := range []string{"인자 #", "글자 그대로", "Claude Code", "OpenCode", "--env", "래퍼"} {
			if !strings.Contains(w[0], want) {
				t.Errorf("%v: warning lacks %q: %s", args, want, w[0])
			}
		}
		if strings.Contains(w[0], "DEFAULTSENTINEL9") || strings.Contains(w[0], "ROOT") {
			t.Errorf("%v: warning echoes the argument: %s", args, w[0])
		}
	}
	// No warning for plain args or for a shell script (the shell expands it itself).
	for _, sp := range []Spec{
		{Transport: TransportStdio, Command: "node", Args: []string{"server.js", "--root", "/srv"}},
		{Transport: TransportStdio, Command: "sh", Args: []string{"-c", "exec node server.js \"${ROOT}\""}},
	} {
		if w := argRefWarnings(BuildPreview(sp)); len(w) != 0 {
			t.Errorf("%v: unexpected warning %v", sp.Args, w)
		}
	}
}

// ---- TASK-0017 AC-3: short flag variants -----------------------------------

func TestCheckArgsShortFlagVariants(t *testing.T) {
	bad := [][]string{
		{"-pHunter2x"}, {"-pS3cr3tValue"}, {"--tokens=abc"}, {"--tokens", "abcdef"}, {"--jwt=xyz"}, {"--jwt", "xyz"},
		{"--pat=xyz"}, {"--github-pat", "xyz"}, {"--access-jwt=xyz"}, {"--secrets=a"}, {"--passwords=a"},
	}
	for _, a := range bad {
		err := CheckArgs(a)
		if err == nil {
			t.Errorf("must refuse: %v", a)
			continue
		}
		for _, p := range a {
			if strings.Contains(p, "Hunter2x") && strings.Contains(err.Error(), "Hunter2x") || strings.Contains(p, "S3cr3tValue") && strings.Contains(err.Error(), "S3cr3tValue") {
				t.Errorf("%v: error leaks the value: %v", a, err)
			}
		}
	}
	// Decisions per flag (docs/security.md "짧은 플래그"): -k and -t are NOT treated
	// as secrets (curl --insecure / docker -t tty / many "tag"), neither are
	// ports, versions and the related non-secret options.
	good := [][]string{
		{"-k"}, {"-t"}, {"-k", "https://example.com/mcp"}, {"-t", "latest"}, {"-t", "name:1.0"},
		{"-p8080"}, {"-p8080:80"}, {"-p3.12"}, {"-p/tmp/x"}, {"-p${DB_PASS}"}, {"-p"},
		{"--tokens=500"}, {"--tokens", "500"}, {"--max-tokens=100"}, {"--tokens-file=/x/y"},
		{"--jwt-issuer=https://issuer.example"}, {"--jwt-algorithm=HS256"}, {"--jwt-secret-file=/x/y"}, {"--jwt"}, {"--pat-file=/x/y"}, {"--path=/srv"},
		{"--keys=a,b"}, {"--secrets-dir=/x/y"}, {"--pat=${GH_PAT}"}, {"--tokens=${T}"},
	}
	for _, a := range good {
		if err := CheckArgs(a); err != nil {
			t.Errorf("false positive %v: %v", a, err)
		}
	}
}

// ---- TASK-0017 AC-4: masking of base64 / URL-escaped echoes ----------------

func TestMaskCoversBase64AndURLEscapedEchoes(t *testing.T) {
	const sec = "Zk9/Wq+7=pa$$word-abc" // base64 and URL escaping both change this value
	secrets := map[string]bool{}
	addSecret(secrets, sec)
	if len(secrets) > 7 {
		t.Fatalf("unbounded forms: %d", len(secrets))
	}
	echoes := []string{
		sec,
		base64.StdEncoding.EncodeToString([]byte(sec)), base64.RawStdEncoding.EncodeToString([]byte(sec)),
		base64.URLEncoding.EncodeToString([]byte(sec)), base64.RawURLEncoding.EncodeToString([]byte(sec)),
		url.QueryEscape(sec), url.PathEscape(sec),
	}
	c := &Conn{secrets: secrets}
	for _, e := range echoes {
		in := "before " + e + " after"
		for name, got := range map[string]string{"Mask": c.Mask(in), "Sanitize": c.Sanitize(in), "maskText": maskText(in, secrets)} {
			if strings.Contains(got, e) || !strings.Contains(got, "***") {
				t.Errorf("%s: echo %.8s... not masked: %q", name, e, got)
			}
		}
	}
	// Short secrets are only masked verbatim (their encodings collide with ordinary text).
	short := map[string]bool{}
	addSecret(short, "abc123")
	if len(short) != 1 {
		t.Errorf("short secret must not expand: %d forms", len(short))
	}
	// Forms of the discovery cache (tool descriptions) are masked as well.
	d := &discovered{srvName: "srv " + base64.StdEncoding.EncodeToString([]byte(sec))}
	maskDiscovered(d, secrets)
	if strings.Contains(d.srvName, base64.StdEncoding.EncodeToString([]byte(sec))) {
		t.Errorf("server info keeps the base64 echo: %q", d.srvName)
	}
}

func TestRefreshMasksEncodedEchoesOfResolvedSecret(t *testing.T) {
	// std base64 has '+' or '/', padded and raw differ, url-safe differs from std.
	const sec = "p/a+ss w?>~~word!"
	b := []byte(sec)
	std, rawStd, rawURL := base64.StdEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b)
	if !strings.ContainsAny(std, "+/") || std == rawStd || rawURL == rawStd || url.QueryEscape(sec) == url.PathEscape(sec) {
		t.Fatalf("fixture no longer exercises every form: %s %s %s", std, rawStd, rawURL)
	}
	reg, _ := newReg(t)
	ctx := context.Background()
	t.Setenv("FORGE_TEST_FORMS_SECRET", sec)
	v, err := ParseValue(SlotEnv, "SECRET_KEY=${FORGE_TEST_FORMS_SECRET}")
	if err != nil {
		t.Fatal(err)
	}
	env := stdioEnv("ok", "echo", "")
	env["MOCK_ECHO_FORMS"] = "SECRET_KEY"
	if err := reg.Add(ctx, stdioSpec("forms", env, v)); err != nil {
		t.Fatal(err)
	}
	res, err := reg.Refresh(ctx, "forms", RefreshOptions{Timeout: 20 * time.Second})
	if err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	info, err := reg.Get(ctx, "forms")
	if err != nil || len(info.Tools) != 1 {
		t.Fatalf("%v %+v", err, info)
	}
	// All six echoes must be replaced as a whole: no partial leftovers such as the
	// dangling "=" of a padded form.
	if want := strings.TrimSuffix(strings.Repeat("*** | ", 6), " | "); info.Tools[0].Description != want {
		t.Errorf("cached description = %q, want %q", info.Tools[0].Description, want)
	}
}

func TestStdioOversizedLineErrorNamesServerAndLimit(t *testing.T) {
	reg, _ := newReg(t)
	ctx := context.Background()
	if err := reg.Add(ctx, stdioSpec("big-srv", stdioEnv("flood", "", ""))); err != nil {
		t.Fatal(err)
	}
	res, err := reg.Refresh(ctx, "big-srv", RefreshOptions{Timeout: 20 * time.Second})
	if err != nil || res.OK || res.ErrorClass != ClassProtocol {
		t.Fatalf("%+v %v", res, err)
	}
	for _, want := range []string{`"big-srv"`, "4 MiB", "tools/list", "줄이거나"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("message lacks %q: %s", want, res.Error)
		}
	}
	if len(res.Error) > 2*maxMessage || strings.Contains(res.Error, "aaaaaaaa") {
		t.Errorf("message must stay short and not echo the output (%d bytes)", len(res.Error))
	}
}

// ---- TASK-0017 AC-5: how-to-pin guidance ----------------------------------

func TestPreviewPinGuidancePerRunner(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		want []string
	}{
		{"npx", stdio("npx", "-y", "@scope/pkg@latest"), []string{"@scope/pkg@<정확한 버전>", "npm view @scope/pkg version"}},
		{"npx range", stdio("npx", "pkg@^1.2.0"), []string{"pkg@<정확한 버전>"}},
		{"bunx", stdio("bunx", "pkg"), []string{"bunx pkg@1.2.3"}},
		{"pnpm dlx", stdio("pnpm", "dlx", "pkg"), []string{"pnpm dlx pkg@1.2.3"}},
		{"uvx", stdio("uvx", "mcp-server-git"), []string{"mcp-server-git==<버전>", "미확인"}},
		{"uvx range", stdio("uvx", "--from", "pkg>=1.0", "cmd"), []string{"pkg==<버전>"}},
		{"pipx", stdio("pipx", "run", "pkg"), []string{"--spec 'pkg==<버전>'", "미확인"}},
		{"docker latest", stdio("docker", "run", "-i", "ghcr.io/org/img:latest"), []string{"ghcr.io/org/img:<버전>", "ghcr.io/org/img@sha256:", "docker inspect"}},
		{"docker registry port", stdio("docker", "run", "localhost:5000/img"), []string{"localhost:5000/img@sha256:"}},
		{"podman", stdio("podman", "run", "img:1.4"), []string{"podman inspect"}},
		{"deno", stdio("deno", "run", "npm:pkg"), []string{"`npm:pkg@<버전>`"}},
		{"deno jsr", stdio("deno", "run", "jsr:@scope/name"), []string{"`jsr:@scope/name@<버전>`"}},
		{"deno url", stdio("deno", "run", "https://deno.land/x/s/mod.ts"), []string{"URL 경로에 정확한 버전"}},
	}
	for _, c := range cases {
		p := BuildPreview(c.spec)
		var got string
		for _, f := range p.Findings {
			if strings.HasPrefix(f.Text, "고정 방법:") {
				got = f.Text
			}
		}
		if got == "" {
			t.Errorf("%s: no pin how-to finding: %+v", c.name, p.Findings)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q missing in %q", c.name, w, got)
			}
		}
		if len(p.Warnings()) == 0 {
			t.Errorf("%s: the warning itself must remain", c.name)
		}
	}
	for _, sp := range []Spec{stdio("npx", "pkg@1.2.3"), stdio("uvx", "pkg==1.0.0"), stdio("npx", "./local"), stdio("node", "s.js")} {
		for _, f := range BuildPreview(sp).Findings {
			if strings.HasPrefix(f.Text, "고정 방법:") {
				t.Errorf("%v: how-to shown for a pinned/local spec", sp.Args)
			}
		}
	}
}

// TASK-0025 AC-NEW (1): arguments that merely contain a dollar sign are not
// ${...} references (no braces, or no closing brace) and must not warn.
func TestNoEnvRefWarningForPlainDollarArguments(t *testing.T) {
	for _, args := range [][]string{
		{"$"}, {"cost=$"}, {"${"}, {"--x=${unterminated"}, {"$1"}, {"--n=$1"}, {"$HOME"}, {"--root=$HOME/work"}, {"$$"}, {"{env:"}, {"{file:/x"}, {"$(pwd)"}, {"{HOME}"},
	} {
		if i := ArgRefIndex("node", args); i != -1 {
			t.Errorf("%q: ArgRefIndex=%d, want -1", args, i)
		}
		sp := Spec{Name: "n", Transport: TransportStdio, Command: "node", Args: args}
		if w := argRefWarnings(BuildPreview(sp)); len(w) != 0 {
			t.Errorf("%q: unexpected warning %v", args, w)
		}
	}
	// a real reference after a lone dollar sign still warns, and points at the right argument
	if i := ArgRefIndex("node", []string{"$", "$HOME", "${X}"}); i != 2 {
		t.Errorf("index=%d, want 2", i)
	}
}
