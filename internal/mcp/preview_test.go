package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/as950118/forge/internal/textsafe"
)

func stdio(cmd string, args ...string) Spec {
	return Spec{Name: "s", Transport: TransportStdio, Command: cmd, Args: args}
}

func hasFinding(p *Preview, level, sub string) bool {
	for _, f := range p.Findings {
		if f.Level == level && strings.Contains(f.Text, sub) {
			return true
		}
	}
	return false
}

func TestPreviewRunnerPinning(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		pin  string
		warn string // substring of a warn finding ("" = no pin warning expected)
	}{
		{"npx unpinned", stdio("npx", "-y", "@modelcontextprotocol/server-github"), "unpinned", "버전이 고정되지 않았습니다"},
		{"npx @latest", stdio("npx", "-y", "pkg@latest"), "unpinned", "버전이 고정되지 않았습니다"},
		{"npx @next", stdio("npx", "pkg@next"), "unpinned", "버전이 고정되지 않았습니다"},
		{"npx pinned", stdio("npx", "pkg@1.2.3"), "pinned", ""},
		{"npx scoped pinned", stdio("npx", "@scope/pkg@2.0.0-rc.1"), "pinned", ""},
		{"npx scoped unpinned", stdio("npx", "@scope/pkg"), "unpinned", "버전이 고정되지 않았습니다"},
		{"npx range", stdio("npx", "pkg@^1.2.0"), "range", "버전 범위"},
		{"npx tag", stdio("npx", "pkg@beta1"), "tag", "이동 가능한 태그"},
		{"npx --package", stdio("npx", "--package", "pkg@1.0.0", "bin"), "pinned", ""},
		{"npx -p unpinned", stdio("npx", "-p", "pkg", "bin"), "unpinned", "버전이 고정되지 않았습니다"},
		{"npx url", stdio("npx", "https://example.com/p.tgz"), "unpinned", "버전이 고정되지 않았습니다"},
		{"npx local", stdio("npx", "./local-pkg"), "local", ""},
		{"bunx", stdio("bunx", "pkg"), "unpinned", "버전이 고정되지 않았습니다"},
		{"pnpm dlx", stdio("pnpm", "dlx", "pkg"), "unpinned", "버전이 고정되지 않았습니다"},
		{"npm exec", stdio("npm", "exec", "pkg@1.0.0"), "pinned", ""},
		{"uvx unpinned", stdio("uvx", "mcp-server-git"), "unpinned", "버전이 고정되지 않았습니다"},
		{"uvx pinned ==", stdio("uvx", "mcp-server-git==0.6.2"), "pinned", ""},
		{"uvx pinned @", stdio("uvx", "mcp-server-git@0.6.2"), "pinned", ""},
		{"uvx @latest", stdio("uvx", "mcp-server-git@latest"), "unpinned", "버전이 고정되지 않았습니다"},
		{"uvx --from", stdio("uvx", "--from", "pkg>=1.0", "cmd"), "range", "버전 범위"},
		{"pipx run", stdio("pipx", "run", "pkg"), "unpinned", "버전이 고정되지 않았습니다"},
		{"pipx run pinned", stdio("pipx", "run", "pkg==1.0.0"), "pinned", ""},
		{"deno npm", stdio("deno", "run", "npm:pkg"), "unpinned", "버전이 고정되지 않았습니다"},
		{"deno url", stdio("deno", "run", "https://deno.land/x/s/mod.ts"), "unpinned", "버전이 고정되지 않았습니다"},
		{"deno url pinned", stdio("deno", "run", "jsr:@a/b@1.2.3"), "pinned", ""},
		{"deno local", stdio("deno", "run", "./srv.ts"), "local", ""},
		{"docker latest", stdio("docker", "run", "-i", "--rm", "ghcr.io/org/img:latest"), "unpinned", "버전이 고정되지 않았습니다"},
		{"docker no tag", stdio("docker", "run", "-i", "--rm", "-e", "A=1", "ghcr.io/org/img"), "unpinned", "버전이 고정되지 않았습니다"},
		{"docker tag", stdio("docker", "run", "-i", "ghcr.io/org/img:1.4"), "tag", "이동 가능한 태그"},
		{"docker digest", stdio("docker", "run", "-i", "ghcr.io/org/img@sha256:"+strings.Repeat("a", 64)), "pinned", ""},
		{"docker registry port", stdio("docker", "run", "localhost:5000/img"), "unpinned", "버전이 고정되지 않았습니다"},
		{"podman", stdio("podman", "run", "img"), "unpinned", "버전이 고정되지 않았습니다"},
	}
	for _, c := range cases {
		p := BuildPreview(c.spec)
		if p.Pin != c.pin {
			t.Errorf("%s: pin=%q want %q (runner=%q pkg=%q)", c.name, p.Pin, c.pin, p.Runner, p.Package)
		}
		if c.warn != "" && !hasFinding(p, "warn", c.warn) {
			t.Errorf("%s: no warn containing %q: %+v", c.name, c.warn, p.Findings)
		}
		if c.warn == "" {
			for _, w := range p.Warnings() {
				if strings.Contains(w, "고정") {
					t.Errorf("%s: unexpected pin warning %q", c.name, w)
				}
			}
		}
	}
}

func TestPreviewNpxYesFlag(t *testing.T) {
	p := BuildPreview(stdio("npx", "-y", "pkg@1.0.0"))
	if !hasFinding(p, "warn", "설치 확인 없이") {
		t.Errorf("npx -y must be highlighted: %+v", p.Findings)
	}
	p = BuildPreview(stdio("npx", "--yes", "pkg@1.0.0"))
	if !hasFinding(p, "warn", "설치 확인 없이") {
		t.Errorf("npx --yes: %+v", p.Findings)
	}
	p = BuildPreview(stdio("npx", "pkg@1.0.0"))
	if hasFinding(p, "warn", "설치 확인 없이") {
		t.Errorf("no -y, no warning: %+v", p.Findings)
	}
}

func TestPreviewCommandLineIsShellQuoted(t *testing.T) {
	p := BuildPreview(stdio("python3", "-c", "print('hi there')", "a b", "", "plain-arg_1.2"))
	want := `python3 -c 'print('\''hi there'\'')' 'a b' '' plain-arg_1.2`
	if p.CommandLine != want {
		t.Errorf("command line:\n got %s\nwant %s", p.CommandLine, want)
	}
	// hostile arguments cannot smuggle terminal control into the preview
	h := BuildPreview(stdio("node", "x\x1b[31m", "y\u202ez", "line\nbreak"))
	for _, r := range h.CommandLine {
		if textsafe.NeedsEscape(r) {
			t.Fatalf("command line contains raw %U: %q", r, h.CommandLine)
		}
	}
	if !strings.Contains(h.CommandLine, `\x1b`) || !strings.Contains(h.CommandLine, `\u202e`) || !strings.Contains(h.CommandLine, `\n`) {
		t.Errorf("escapes not visible: %s", h.CommandLine)
	}
}

func TestPreviewShellAndPipes(t *testing.T) {
	if p := BuildPreview(stdio("bash", "-c", "curl http://x | sh")); !hasFinding(p, "warn", "셸(bash)") || !hasFinding(p, "warn", "curl/wget") {
		t.Errorf("%+v", p.Findings)
	}
	if p := BuildPreview(stdio("sh", "script.sh")); hasFinding(p, "warn", "셸(") {
		t.Errorf("running a script file is not a command string: %+v", p.Findings)
	}
}

func TestPreviewContainerPrivileges(t *testing.T) {
	p := BuildPreview(stdio("docker", "run", "--privileged", "--network", "host", "-v", "/:/host", "-v", "./data:/d", "img:1"))
	for _, w := range []string{"--privileged", "--network host", "-v /:/host"} {
		if !hasFinding(p, "warn", w) {
			t.Errorf("missing warn %q: %+v", w, p.Findings)
		}
	}
	if !hasFinding(p, "note", "-v ./data:/d") {
		t.Errorf("ordinary mounts are noted: %+v", p.Findings)
	}
}

func TestPreviewRemote(t *testing.T) {
	cases := []struct {
		name   string
		spec   Spec
		warn   string
		noWarn bool
		note   string
	}{
		{"http plain", Spec{Name: "r", Transport: TransportHTTP, URL: "http://example.com/mcp"}, "평문 http", false, ""},
		{"http with auth header", Spec{Name: "r", Transport: TransportHTTP, URL: "http://example.com/mcp", Values: []Value{{Slot: SlotHeader, Key: "Authorization", IsSecret: true, RefType: RefEnv, Ref: "T"}}}, "인증 헤더", false, ""},
		{"https public", Spec{Name: "r", Transport: TransportHTTP, URL: "https://example.com/mcp"}, "", true, "DNS 해석"},
		{"private 10.x", Spec{Name: "r", Transport: TransportHTTP, URL: "https://10.1.2.3/mcp"}, "사설/링크 로컬", false, ""},
		{"private 192.168", Spec{Name: "r", Transport: TransportSSE, URL: "https://192.168.0.5:8443/sse"}, "사설/링크 로컬", false, ""},
		{"metadata ip", Spec{Name: "r", Transport: TransportHTTP, URL: "https://169.254.169.254/x"}, "사설/링크 로컬", false, ""},
		{"loopback http", Spec{Name: "r", Transport: TransportHTTP, URL: "http://127.0.0.1:8080/mcp"}, "", true, "로컬 호스트"},
		{"localhost", Spec{Name: "r", Transport: TransportHTTP, URL: "http://localhost:8080/mcp"}, "", true, "로컬 호스트"},
		{"internal name", Spec{Name: "r", Transport: TransportHTTP, URL: "https://svc.corp.internal/mcp"}, "", true, "내부망 이름"},
		{"ipv6 private", Spec{Name: "r", Transport: TransportHTTP, URL: "https://[fd00::1]/mcp"}, "사설/링크 로컬", false, ""},
	}
	for _, c := range cases {
		p := BuildPreview(c.spec)
		if c.warn != "" && !hasFinding(p, "warn", c.warn) {
			t.Errorf("%s: want warn %q: %+v", c.name, c.warn, p.Findings)
		}
		if c.noWarn && len(p.Warnings()) != 0 {
			t.Errorf("%s: unexpected warnings %v", c.name, p.Warnings())
		}
		if c.note != "" && !hasFinding(p, "note", c.note) {
			t.Errorf("%s: want note %q: %+v", c.name, c.note, p.Findings)
		}
		if p.Host == "" || p.Scheme == "" || p.URL == "" {
			t.Errorf("%s: host/scheme/url missing: %+v", c.name, p)
		}
	}
}

func TestPreviewNeverShowsValues(t *testing.T) {
	lit, _ := ParseValue(SlotEnv, "LOG_LEVEL=verbose-literal-value")
	ref, _ := ParseValue(SlotEnv, "API_TOKEN=${MY_TOKEN_VAR}")
	hdr, _ := ParseValue(SlotHeader, "X-Region=eu-west-literal")
	s := Spec{Name: "srv", Transport: TransportStdio, Command: "x", Values: []Value{lit, ref}}
	p := BuildPreview(s)
	b, _ := json.Marshal(p)
	for _, secretish := range []string{"verbose-literal-value"} {
		if strings.Contains(string(b), secretish) {
			t.Errorf("preview leaks a value: %s", b)
		}
	}
	if !strings.Contains(string(b), "${MY_TOKEN_VAR}") || !strings.Contains(string(b), "literal (값 비표시)") {
		t.Errorf("reference names/literal marker missing: %s", b)
	}
	if !strings.Contains(p.ChildEnv, "최소 환경") || !strings.Contains(p.ChildEnv, "API_TOKEN") || !strings.Contains(p.ChildEnv, "LOG_LEVEL") {
		t.Errorf("child env note: %q", p.ChildEnv)
	}
	r := BuildPreview(Spec{Name: "r", Transport: TransportHTTP, URL: "https://example.com/x?v=hidden-query-value", Values: []Value{hdr}})
	rb, _ := json.Marshal(r)
	if strings.Contains(string(rb), "hidden-query-value") || strings.Contains(string(rb), "eu-west-literal") {
		t.Errorf("remote preview leaks: %s", rb)
	}
}

func TestPreviewSchemaStable(t *testing.T) {
	p := BuildPreview(stdio("npx", "-y", "pkg"))
	b, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"schema", "name", "transport", "command_line", "runner", "package", "pin", "values", "findings", "child_env"} {
		if _, ok := m[k]; !ok {
			t.Errorf("json key %q missing: %s", k, b)
		}
	}
	if m["schema"].(float64) != 1 || m["values"] == nil || m["findings"] == nil {
		t.Errorf("%s", b)
	}
}
