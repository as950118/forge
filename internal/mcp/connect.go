package mcp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// transportBundle is a ready-to-connect transport plus the diagnostics that
// classify its failures.
type transportBundle struct {
	transport sdk.Transport
	cmd       *exec.Cmd
	errBuf    *tailBuf
	dt        *diagTransport
	lim       *lineLimit
}

// newTransport builds the stdio (own process group, stdout line cap, stderr
// captured, never inherited) or HTTP/SSE transport for a server. env is the base
// environment of a stdio child; resolved env values are appended to it.
func newTransport(in *Info, vals map[string]map[string]string, env []string) (*transportBundle, *failure) {
	tb := &transportBundle{errBuf: &tailBuf{}}
	switch in.Transport {
	case TransportStdio:
		cmd := exec.Command(in.Command, in.Args...)
		if cmd.Err != nil {
			return nil, &failure{class: ClassCommandNotFound, msg: fmt.Sprintf("실행 파일을 찾을 수 없습니다: %s", in.Command)}
		}
		cmd.Dir = in.Cwd
		cmd.Env = append([]string{}, env...)
		for k, v := range vals[SlotEnv] {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stderr = tb.errBuf
		cmd.WaitDelay = 2 * time.Second
		setProcGroup(cmd)
		tb.cmd = cmd
		tb.lim = &lineLimit{max: maxStdoutLine}
		tb.transport = &limitedCommandTransport{cmd: cmd, lim: tb.lim}
	case TransportHTTP, TransportSSE:
		tb.dt = &diagTransport{base: http.DefaultTransport, headers: vals[SlotHeader]}
		hc := &http.Client{Transport: tb.dt}
		if in.Transport == TransportHTTP {
			tb.transport = &sdk.StreamableClientTransport{Endpoint: in.URL, HTTPClient: hc, MaxRetries: -1, DisableStandaloneSSE: true}
		} else {
			tb.transport = &sdk.SSEClientTransport{Endpoint: in.URL, HTTPClient: hc}
		}
	default:
		return nil, &failure{class: ClassProtocol, msg: "알 수 없는 transport: " + in.Transport}
	}
	return tb, nil
}

// minimalEnvKeys are the only variables a gateway-spawned stdio child inherits
// from the gateway process (besides its own declared env values). Everything
// else (API keys, tokens, ...) must be declared on the server as ${ENV}.
var minimalEnvKeys = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true, "TZ": true,
	"LANG": true, "LANGUAGE": true, "TERM": true, "SYSTEMROOT": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
	// CA bundles of python/curl based servers (paths only, no secrets).
	"REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true,
	// Version-manager roots: shims of nvm/volta/pyenv/asdf stop resolving a
	// runtime for npx/uvx/node/python without them (directory paths only).
	"NVM_DIR": true, "VOLTA_HOME": true, "PYENV_ROOT": true, "ASDF_DIR": true, "ASDF_DATA_DIR": true,
	// Package cache/registry location of npx/uvx. Deliberately NOT inherited:
	// NODE_OPTIONS (code injection), NPM_TOKEN/npm_config_*authToken and
	// UV_INDEX_URL/PIP_INDEX_URL (may embed credentials), PYTHONPATH.
	"npm_config_cache": true, "NPM_CONFIG_CACHE": true, "npm_config_registry": true, "NPM_CONFIG_REGISTRY": true,
	"UV_CACHE_DIR": true,
	// Windows process basics.
	"COMSPEC": true, "PATHEXT": true, "WINDIR": true,
}

// credentialURLRe matches "scheme://userinfo@host": a URL that embeds credentials.
var credentialURLRe = regexp.MustCompile(`://[^/?#\s]*@`)

// MinimalEnv returns the allow-listed subset of the current environment
// (PATH/HOME/locale/proxy/CA settings, LC_*, XDG_*). It is the single base
// environment of every stdio child: both discovery (forge mcp refresh) and the
// gateway proxy use it, so a server behaves the same in both.
//
// A variable whose value contains "://userinfo@" (a proxy or registry URL with
// embedded credentials, e.g. HTTPS_PROXY=http://user:pw@host) is dropped as a
// whole, never rewritten: the child must not receive credentials it was not
// given explicitly, and a modified URL could silently route elsewhere. A server
// that needs it declares it: forge mcp add --env HTTPS_PROXY='${HTTPS_PROXY}'.
func MinimalEnv() []string {
	kept, _ := minimalEnv()
	return kept
}

// MinimalEnvDropped returns the names (never the values) of allow-listed
// variables MinimalEnv leaves out because their value embeds credentials.
func MinimalEnvDropped() []string {
	_, dropped := minimalEnv()
	return dropped
}

func minimalEnv() (kept, dropped []string) {
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if !(minimalEnvKeys[k] || strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "XDG_")) {
			continue
		}
		if credentialURLRe.MatchString(v) {
			dropped = append(dropped, k)
			continue
		}
		kept = append(kept, kv)
	}
	return kept, dropped
}

// DialOptions tunes Dial.
type DialOptions struct {
	// Timeout bounds connect + initialize (server timeout_ms, else DefaultTimeout).
	Timeout time.Duration
	// Env is the base environment of a stdio child (default MinimalEnv()).
	Env []string
	// ClientName/Version are sent in initialize.clientInfo (default forge-gateway).
	ClientName, ClientVersion string
}

// DialError is a failed connection with a class (Class* constants) and a
// sanitized Korean message that never contains resolved secrets.
type DialError struct {
	Class, Message, Hint string
}

func (e *DialError) Error() string { return e.Message }

// Conn is a live connection to a registered MCP server, held by the gateway.
type Conn struct {
	Session *sdk.ClientSession
	// Info is the definition the connection was made from.
	Info *Info

	cmd     *exec.Cmd
	secrets map[string]bool
	errBuf  *tailBuf
	lim     *lineLimit
	done    chan struct{}
}

// Dial connects to a registered server (full definition with Values, as from
// Registry.Get). Value references are resolved here, in memory only, and handed
// to the child process / HTTP request.
func Dial(parent context.Context, in *Info, opt DialOptions) (*Conn, *DialError) {
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
		if in.TimeoutMS > 0 {
			timeout = time.Duration(in.TimeoutMS) * time.Millisecond
		}
	}
	secrets := map[string]bool{}
	resolved, fail := resolveValues(in.Values, secrets)
	toErr := func(f *failure) *DialError {
		return &DialError{Class: f.class, Message: f.msg, Hint: Hint(f.class, f.msg)}
	}
	if fail != nil {
		return nil, toErr(fail)
	}
	env := opt.Env
	if env == nil {
		env = MinimalEnv()
	}
	tb, fail := newTransport(in, resolved, env)
	if fail != nil {
		return nil, toErr(fail)
	}
	name, ver := opt.ClientName, opt.ClientVersion
	if name == "" {
		name, ver = "forge-gateway", "0"
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	client := sdk.NewClient(&sdk.Implementation{Name: name, Version: ver}, nil)
	cs, err := client.Connect(ctx, tb.transport, nil)
	if err != nil {
		if tb.cmd != nil {
			killGroup(tb.cmd)
		}
		return nil, toErr(classifyLimited(tb.lim, ctx, err, in, tb.dt, tb.errBuf.String(), secrets, timeout))
	}
	c := &Conn{Session: cs, Info: in, cmd: tb.cmd, secrets: secrets, errBuf: tb.errBuf, lim: tb.lim, done: make(chan struct{})}
	go func() { _ = cs.Wait(); close(c.done) }()
	return c, nil
}

// Done is closed when the connection has ended (server exit, transport error, Close).
func (c *Conn) Done() <-chan struct{} { return c.done }

// Alive reports whether the connection is still open.
func (c *Conn) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Sanitize masks resolved secret values of this server and bounds the length.
func (c *Conn) Sanitize(s string) string { return sanitize(s, c.secrets) }

// Mask masks resolved secret values without shortening or flattening the text.
func (c *Conn) Mask(s string) string { return maskText(s, c.secrets) }

// Stderr returns the sanitized tail of the child's stderr ("" for http/sse).
func (c *Conn) Stderr() string {
	return lastRunes(oneLine(c.Mask(c.errBuf.String())), stderrTail)
}

// LineLimitExceeded reports whether the stdio server was cut for an over-long line.
func (c *Conn) LineLimitExceeded() bool { return c.lim != nil && c.lim.exceeded() }

// Close ends the session (stdin EOF, then SIGTERM, then SIGKILL of the whole
// process group) and makes sure no process of the group is left.
func (c *Conn) Close() {
	_ = c.Session.Close()
	if c.cmd != nil {
		killGroup(c.cmd)
	}
	<-c.done
}
