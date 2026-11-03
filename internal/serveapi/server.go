// Package serveapi is the read-only HTTP API behind `forge serve-api` (TASK-0037,
// ADR-F0008). It serves the aggregate snapshot that `forge export --to-postgres`
// wrote to schema forge. It has GET/HEAD routes only, never writes, and answers
// with a fixed allow list of fields.
//
// Threat model: the service is reachable from the internet. Everything except
// /health and /health/db needs a Bearer token (fail closed: no token, no start).
// Error bodies and logs never carry the database URL, a password, the token or
// the raw text of an internal error.
package serveapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Limits (constants; none of them is configurable from the request).
const (
	MinTokenLen      = 16
	maxTokenLen      = 512
	maxURILen        = 2048
	maxResponseBytes = 4 << 20
	maxDays          = 366
	defaultDays      = 30
	requestTimeout   = 10 * time.Second
	pingTimeout      = 5 * time.Second
)

// ErrTokenTooShort is returned by New for an empty or short token.
var ErrTokenTooShort = fmt.Errorf("FORGE_API_TOKEN 이 비었거나 %d자보다 짧습니다. 토큰 없이는 시작하지 않습니다", MinTokenLen)

// ErrNoSnapshot means no snapshot has been exported yet.
var ErrNoSnapshot = errors.New("no snapshot")

// Response types: the allow list of everything the API can return.
type (
	Meta struct {
		SchemaVersion int    `json:"schema_version"`
		GeneratedAt   string `json:"generated_at"`
		ForgeVersion  string `json:"forge_version"`
		DayBasis      string `json:"day_basis"`
	}
	Item struct {
		Kind        string   `json:"kind"`
		Server      string   `json:"server"`
		Name        string   `json:"name"`
		Calls       int64    `json:"calls"`
		OK          int64    `json:"ok"`
		Failed      int64    `json:"failed"`
		Unknown     int64    `json:"unknown"`
		SuccessRate *float64 `json:"success_rate"`
		LastCall    string   `json:"last_call"`
	}
	Day struct {
		Day     string `json:"day"`
		Kind    string `json:"kind"`
		Calls   int64  `json:"calls"`
		OK      int64  `json:"ok"`
		Failed  int64  `json:"failed"`
		Unknown int64  `json:"unknown"`
	}
	Summary struct {
		Meta  Meta   `json:"meta"`
		Items []Item `json:"items"`
		Daily []Day  `json:"daily"`
	}
)

// Store is the read-only data access. Implementations must not write.
type Store interface {
	Ping(ctx context.Context) error
	Summary(ctx context.Context, days int) (Summary, error)
	Items(ctx context.Context, kind string) ([]Item, error)
	Daily(ctx context.Context, days int) ([]Day, error)
}

// Options configure New.
type Options struct {
	Store  Store
	Token  string
	Redact func(string) string // applied to every logged internal error
	Logf   func(format string, args ...any)
}

// Server is the http.Handler of the API.
type Server struct {
	store     Store
	tokenHash [sha256.Size]byte
	redact    func(string) string
	logf      func(string, ...any)
}

// New validates the token (fail closed) and returns the handler.
func New(o Options) (*Server, error) {
	if len(o.Token) < MinTokenLen || len(o.Token) > maxTokenLen || strings.TrimSpace(o.Token) != o.Token {
		return nil, ErrTokenTooShort
	}
	if o.Store == nil {
		return nil, errors.New("serveapi: Store 가 필요합니다")
	}
	s := &Server{store: o.Store, tokenHash: sha256.Sum256([]byte(o.Token)), redact: o.Redact, logf: o.Logf}
	tok := o.Token
	userRedact := o.Redact
	s.redact = func(t string) string {
		if userRedact != nil {
			t = userRedact(t)
		}
		return strings.ReplaceAll(t, tok, "***")
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) {
	if w.status == 0 {
		w.status = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	start := time.Now()
	w := &statusWriter{ResponseWriter: rw}
	s.serve(w, r)
	// Only method and path are logged: the query string could hold a token a
	// client mistakenly put there.
	s.logf("%s %s %d %s", r.Method, quoteLog(r.URL.Path), w.status, time.Since(start).Round(time.Millisecond))
}

func quoteLog(p string) string {
	if len(p) > 120 {
		p = p[:120]
	}
	return strconv.QuoteToASCII(p)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if len(r.RequestURI) > maxURILen {
		writeErr(w, http.StatusRequestURITooLong, "request too long")
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		writeErr(w, http.StatusBadRequest, "request body not allowed")
		return
	}
	switch r.URL.Path {
	case "/health":
		writeJSON(w, r, http.StatusOK, map[string]any{"status": "ok", "readOnly": true})
		return
	case "/health/db":
		s.healthDB(w, r)
		return
	}
	if !s.authorized(r) {
		h.Set("WWW-Authenticate", "Bearer")
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	switch r.URL.Path {
	case "/v1/summary":
		sum, err := s.store.Summary(ctx, maxDays)
		if err != nil {
			s.storeErr(w, err)
			return
		}
		if sum.Items == nil {
			sum.Items = []Item{}
		}
		if sum.Daily == nil {
			sum.Daily = []Day{}
		}
		writeJSON(w, r, http.StatusOK, sum)
	case "/v1/items":
		kind := r.URL.Query().Get("kind")
		if kind != "" && kind != "skill" && kind != "tool" && kind != "mcp" {
			writeErr(w, http.StatusBadRequest, "invalid kind (skill, tool or mcp)")
			return
		}
		items, err := s.store.Items(ctx, kind)
		if err != nil {
			s.storeErr(w, err)
			return
		}
		if items == nil {
			items = []Item{}
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"items": items})
	case "/v1/daily":
		days := defaultDays
		if v := r.URL.Query().Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxDays {
				writeErr(w, http.StatusBadRequest, "invalid days (1..366)")
				return
			}
			days = n
		}
		daily, err := s.store.Daily(ctx, days)
		if err != nil {
			s.storeErr(w, err)
			return
		}
		if daily == nil {
			daily = []Day{}
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"days": days, "daily": daily})
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

// authorized accepts exactly one "Authorization: Bearer <token>" header. The
// comparison is constant time over SHA-256 digests, so neither content nor length
// of the expected token leaks. A token in the query string is never read.
func (s *Server) authorized(r *http.Request) bool {
	vals := r.Header.Values("Authorization")
	if len(vals) != 1 {
		return false
	}
	scheme, tok, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || len(tok) > maxTokenLen {
		return false
	}
	got := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(got[:], s.tokenHash[:]) == 1
}

func (s *Server) healthDB(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), pingTimeout)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.logf("health/db: %s", s.redact(err.Error()))
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "error": "database unavailable"})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "ok", "db": "ok"})
}

func (s *Server) storeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNoSnapshot) {
		writeErr(w, http.StatusNotFound, "no snapshot exported yet")
		return
	}
	s.logf("store error: %s", s.redact(err.Error()))
	writeErr(w, http.StatusServiceUnavailable, "database unavailable")
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	b, _ := json.Marshal(map[string]string{"error": msg})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}

func writeJSON(w http.ResponseWriter, r *http.Request, code int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil || buf.Len() > maxResponseBytes {
		writeErr(w, http.StatusInternalServerError, "response unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}
