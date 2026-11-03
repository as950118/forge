package tool

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/as950118/forge/internal/mcp"
	"github.com/as950118/forge/internal/netsafe"
)

// ssrfError is returned by the dialer when a resolved address is not allowed.
type ssrfError = netsafe.BlockedError

// ipBlocked reports whether connecting to ip is forbidden by the SSRF policy
// (internal/netsafe: shared with the remote MCP transports).
func ipBlocked(ip net.IP, allowPrivate bool) bool { return netsafe.Blocked(ip, allowPrivate) }

type header struct{ key, value, shown string }

// httpPlan is the fully rendered request.
type httpPlan struct {
	method  string
	url     string
	headers []header
	body    string
}

func buildHTTP(def *Definition, args map[string]any, o RunOptions, m *masker, dry bool) (*httpPlan, error) {
	// path/query values are escaped for their position; the host part is never templated.
	qpos := strings.IndexAny(def.URL, "?#")
	raw, err := render(def.URL, args, func(v string, pos int) string {
		if qpos >= 0 && pos > qpos {
			return url.QueryEscape(v)
		}
		return url.PathEscape(v)
	})
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, &runError{ClassInvalidArgs, errors.New("치환된 URL 이 올바르지 않습니다")}
	}
	if err := checkPathSegments(u); err != nil {
		return nil, err
	}
	tu, _ := url.Parse(varRe.ReplaceAllString(def.URL, "x"))
	if tu == nil || u.Host != tu.Host || u.Scheme != tu.Scheme {
		return nil, &runError{ClassInvalidArgs, errors.New("치환 결과가 URL 호스트/스킴을 바꿀 수 없습니다")}
	}
	p := &httpPlan{method: def.Method, url: u.String()}
	if def.BodyTemplate != "" {
		body, err := render(def.BodyTemplate, args, func(v string, _ int) string {
			b, _ := json.Marshal(v) // JSON string escape; strip the quotes
			return string(b[1 : len(b)-1])
		})
		if err != nil {
			return nil, err
		}
		p.body = body
	}
	hasCT := false
	for _, v := range def.Values {
		if v.Slot != mcp.SlotHeader {
			continue
		}
		h := header{key: v.Key}
		if strings.EqualFold(v.Key, "Content-Type") {
			hasCT = true
		}
		switch v.RefType {
		case mcp.RefLiteral:
			h.value, h.shown = v.Literal, v.Literal
		case mcp.RefEnv:
			h.shown = "*** (${" + v.Ref + "})"
			if !dry {
				val, ok := lookupEnvRef(o, v.Ref)
				if !ok {
					return nil, &runError{ClassPolicy, fmt.Errorf("환경변수 %s 가 설정되어 있지 않습니다 (헤더 %s 가 ${%s} 를 참조합니다)", v.Ref, v.Key, v.Ref)}
				}
				if strings.ContainsAny(val, "\r\n\x00") {
					return nil, &runError{ClassPolicy, fmt.Errorf("환경변수 %s 의 값에 개행/널 문자가 있어 헤더로 쓸 수 없습니다", v.Ref)}
				}
				h.value = val
				m.add(val)
			}
		}
		p.headers = append(p.headers, h)
	}
	if p.body != "" && !hasCT {
		p.headers = append(p.headers, header{"Content-Type", "application/json", "application/json"})
	}
	sort.Slice(p.headers, func(i, j int) bool { return p.headers[i].key < p.headers[j].key })
	return p, nil
}

// checkPathSegments rejects path traversal through substituted values: a
// segment that is (or percent-decodes, even repeatedly, to) "." / "..", or that
// contains a slash or backslash once decoded (%2f, %5c), is refused.
func checkPathSegments(u *url.URL) error {
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		cur := seg
		for i := 0; i < 4; i++ {
			if cur == "." || cur == ".." || (i > 0 && strings.ContainsAny(cur, "/\\")) || (i == 0 && strings.Contains(cur, "\\")) {
				return &runError{ClassInvalidArgs, errors.New("경로 변수 값에 '.', '..', 슬래시, 역슬래시(인코딩 포함)는 쓸 수 없습니다 (경로 이동 방지)")}
			}
			dec, err := url.PathUnescape(cur)
			if err != nil || dec == cur {
				break
			}
			cur = dec
		}
	}
	return nil
}

// stripDefinitionHeaders reports whether definition headers (secrets) must not
// follow a redirect: a different host/port OR a different scheme (https->http
// downgrade would send them in clear text).
func stripDefinitionHeaders(orig, next *url.URL) bool {
	return !strings.EqualFold(next.Host, orig.Host) || !strings.EqualFold(next.Scheme, orig.Scheme)
}

// safeDialContext resolves the host itself and validates every address at
// connect time (no DNS rebinding window: the validated IP is the one dialed).
func safeDialContext(allowPrivate bool, lookup func(context.Context, string) ([]net.IP, error)) func(context.Context, string, string) (net.Conn, error) {
	return netsafe.DialContext(allowPrivate, lookup)
}

// testTLSConfig is nil in production (system roots). Tests set it to trust an
// httptest TLS server; it is never reachable from a definition or the CLI.
var testTLSConfig *tls.Config

func runHTTP(ctx context.Context, def *Definition, args map[string]any, o RunOptions, m *masker, res *Result, maxOut int) error {
	plan, err := buildHTTP(def, args, o, m, o.DryRun)
	if err != nil {
		return err
	}
	u, _ := url.Parse(plan.url)
	if u.Scheme != "https" && !(u.Scheme == "http" && def.Policy.AllowHTTP) {
		return policyErr("https 만 허용됩니다 (스킴: %s)", u.Scheme)
	}
	allow := def.Policy.Allow
	if !hostAllowed(u.Hostname(), allow) {
		return policyErr("호스트 %s 가 allowlist 에 없습니다 (허용: %s)", u.Hostname(), strings.Join(allow, ", "))
	}
	if o.DryRun {
		p := &Plan{Method: plan.method, URL: plan.url, Headers: map[string]string{}}
		for _, h := range plan.headers {
			p.Headers[h.key] = h.shown
		}
		p.Body = maskIfSecret(plan.body)
		res.DryRun, res.OK = p, true
		return nil
	}
	origURL := *u
	var body io.Reader
	if plan.body != "" {
		body = strings.NewReader(plan.body)
	}
	req, err := http.NewRequestWithContext(ctx, plan.method, plan.url, body)
	if err != nil {
		return &runError{ClassInvalidArgs, errors.New("요청을 만들 수 없습니다")}
	}
	for _, h := range plan.headers {
		req.Header.Set(h.key, h.value)
	}
	req.Header.Set("User-Agent", "forge-tool/1")
	req.Header.Set("Accept-Encoding", "identity")
	tr := &http.Transport{
		Proxy:                 nil, // environment proxies would bypass the SSRF dialer
		DialContext:           safeDialContext(def.Policy.AllowPrivate, o.Lookup),
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0,
		DisableKeepAlives:     true,
		TLSClientConfig:       testTLSConfig,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		Timeout:   def.Timeout(),
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) > def.Policy.MaxRedirects {
				return policyErr("리다이렉트가 상한(%d회)을 초과했습니다", def.Policy.MaxRedirects)
			}
			if r.URL.Scheme != "https" && !(r.URL.Scheme == "http" && def.Policy.AllowHTTP) {
				return policyErr("리다이렉트가 허용되지 않는 스킴(%s)으로 향합니다", r.URL.Scheme)
			}
			if !hostAllowed(r.URL.Hostname(), allow) {
				return policyErr("리다이렉트 대상 호스트 %s 가 allowlist 에 없습니다", r.URL.Hostname())
			}
			if stripDefinitionHeaders(&origURL, r.URL) {
				// never forward definition headers (secrets) to a different host
				for _, h := range plan.headers {
					r.Header.Del(h.key)
				}
			}
			return nil
		},
	}
	if dl, ok := ctx.Deadline(); ok { // honour a shorter per-call override
		if d := time.Until(dl); d > 0 && d < client.Timeout {
			client.Timeout = d
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return classifyHTTPErr(ctx, res, err)
	}
	defer resp.Body.Close()
	res.HTTPStatus = resp.StatusCode
	lr := io.LimitReader(resp.Body, int64(maxOut)+1)
	b, rerr := io.ReadAll(lr)
	if len(b) > maxOut {
		b, res.StdoutTruncated = b[:maxOut], true
	}
	res.Stdout = strings.ToValidUTF8(string(b), "�")
	if rerr != nil && !res.StdoutTruncated {
		return classifyHTTPErr(ctx, res, rerr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		res.OK = true
		return nil
	}
	res.ErrorClass = ClassHTTPStatus
	res.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	return nil
}

func classifyHTTPErr(ctx context.Context, res *Result, err error) error {
	var se *ssrfError
	var ne net.Error
	var re *runError
	switch {
	case errors.As(err, &re):
		return re
	case errors.As(err, &se):
		return &runError{ClassSSRF, se}
	case ctx.Err() == context.DeadlineExceeded || (errors.As(err, &ne) && ne.Timeout()):
		res.TimedOut = true
		return &runError{ClassTimeout, errors.New("타임아웃으로 요청을 중단했습니다")}
	case ctx.Err() != nil:
		return &runError{ClassCanceled, errors.New("취소되었습니다")}
	}
	// url.Error would echo the URL (may contain template values); keep only the cause.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return &runError{ClassNetwork, fmt.Errorf("HTTP 요청 실패: %w", err)}
}
