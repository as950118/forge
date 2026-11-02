package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heonjinjeong/forge/internal/netsafe"
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
		tb.dt = &diagTransport{base: newRemoteRoundTripper(in), headers: vals[SlotHeader], host: urlHost(in.URL)}
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

// originOf is the normalized origin of a URL: lower-case scheme://host:port with the
// default port (80 for http, 443 for https) made explicit, so "https://h/x" and
// "https://h:443/x" are the same origin. "" when it cannot be determined.
func originOf(u *url.URL) string {
	if u == nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// urlHost is the origin of a raw URL string (see originOf).
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return originOf(u)
}

// remoteRoundTripper is the HTTP transport of a remote (http/sse) MCP server. Its
// dialer resolves the host itself and refuses link-local/metadata addresses always
// and loopback/private ones unless the person allowed them (netsafe): a public name
// that resolves to an internal address, and a redirect to one, are refused at
// connect time. Allowed without a flag: a URL whose host is itself a loopback name
// or a literal private address (the person wrote it, and the registration preview
// warned); `forge mcp allow-private <server>` allows internal addresses reached by
// name (an intranet host). When the environment configures a proxy for the request
// the proxy resolves and connects to the destination, which Forge cannot check: that
// path keeps the previous behaviour (documented limit in docs/security.md).
type remoteRoundTripper struct {
	direct, proxied *http.Transport
}

// remoteLookup resolves host names for remote MCP connections. Tests replace it; no
// definition, flag or environment variable can.
var remoteLookup netsafe.Lookup = netsafe.DefaultLookup

func newRemoteRoundTripper(in *Info) *remoteRoundTripper {
	return newRemoteRoundTripperWith(in, remoteLookup)
}

func newRemoteRoundTripperWith(in *Info, lookup netsafe.Lookup) *remoteRoundTripper {
	base := http.DefaultTransport.(*http.Transport)
	allow := in.AllowPrivate
	if h := hostnameOf(in.URL); netsafe.IsLocalName(h) || netsafe.IsLiteralPrivate(h) {
		allow = true
	}
	direct := base.Clone()
	direct.Proxy = nil
	direct.DialContext = netsafe.DialContext(allow, lookup)
	proxied := base.Clone()
	proxied.Proxy = http.ProxyFromEnvironment
	return &remoteRoundTripper{direct: direct, proxied: proxied}
}

func hostnameOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (t *remoteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if pu, err := http.ProxyFromEnvironment(req); err == nil && pu != nil {
		return t.proxied.RoundTrip(req)
	}
	return t.direct.RoundTrip(req)
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

// schemelessCredentialRe matches "userinfo@host" at the start of a value that has
// no scheme (HTTPS_PROXY=u:pw@proxy:1): the part before the first '/', '?' or '#'
// contains '@'.
var schemelessCredentialRe = regexp.MustCompile(`^[^/?#\s@]*@`)

// embedsCredentials reports whether the value of an allow-listed variable carries
// credentials: "scheme://userinfo@host" in any variable, or a scheme-less
// "userinfo@host:port" in a proxy/registry variable (those are URL-or-host:port
// valued; for other variables such as paths a bare '@' proves nothing, and
// NO_PROXY is a host list, not an endpoint).
func embedsCredentials(k, v string) bool {
	if credentialURLRe.MatchString(v) {
		return true
	}
	lk := strings.ToLower(k)
	if lk == "no_proxy" || !(strings.Contains(lk, "proxy") || strings.Contains(lk, "registry")) {
		return false
	}
	v = strings.TrimSpace(v)
	if i := strings.Index(v, "://"); i >= 0 {
		v = v[i+3:]
	}
	return schemelessCredentialRe.MatchString(v)
}

// MinimalEnv returns the allow-listed subset of the current environment
// (PATH/HOME/locale/proxy/CA settings, LC_*, XDG_*). It is the single base
// environment of every stdio child: both discovery (forge mcp refresh) and the
// gateway proxy use it, so a server behaves the same in both.
//
// A variable whose value contains "://userinfo@", or a proxy/registry variable
// whose scheme-less value is "userinfo@host:port" (a proxy or registry URL with
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
		if embedsCredentials(k, v) {
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
