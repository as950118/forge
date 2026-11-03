package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fakeAPIToken = "fake-api-token-0123456789"
	fakePGURL    = "postgresql://fakeuser:fakepw-S3cret@127.0.0.1:5432/postgres"
)

func serveEnv(t *testing.T, token, url, port string) {
	t.Helper()
	t.Setenv("FORGE_API_TOKEN", token)
	t.Setenv("FORGE_EXPORT_URL", url)
	t.Setenv("FORGE_PG_CA", "")
	t.Setenv("PORT", port)
}

func TestServeAPIRefusesToStart(t *testing.T) {
	cases := map[string]struct{ token, url, port, want string }{
		"no token":         {"", fakePGURL, "", "FORGE_API_TOKEN"},
		"short token":      {"short-token-15c", fakePGURL, "", "FORGE_API_TOKEN"},
		"no url":           {fakeAPIToken, "", "", "FORGE_EXPORT_URL"},
		"transaction pool": {fakeAPIToken, "postgresql://u:fakepw-S3cret@h.example.invalid:6543/postgres", "", "6543"},
		"bad port":         {fakeAPIToken, fakePGURL, "99999", "PORT"},
	}
	for name, c := range cases {
		serveEnv(t, c.token, c.url, c.port)
		args := []string{"serve-api", "--addr", "127.0.0.1:0"}
		if name == "bad port" { // PORT is only read when --addr is not given
			args = []string{"serve-api"}
		}
		out, err := runErr(t, "", args...)
		if err == nil {
			t.Errorf("%s: started", name)
			continue
		}
		msg := out + err.Error()
		if !strings.Contains(msg, c.want) {
			t.Errorf("%s: %q lacks %q", name, msg, c.want)
		}
		if strings.Contains(msg, "fakepw") || strings.Contains(msg, fakeAPIToken) {
			t.Errorf("%s: leaks secret: %s", name, msg)
		}
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestServeAPIStartsAndStops(t *testing.T) {
	serveEnv(t, fakeAPIToken, fakePGURL, "")
	cmd := NewRootCmd(BuildInfo{Version: "v", Commit: "c", Date: "d"})
	var out syncBuf
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"serve-api", "--addr", "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	re := regexp.MustCompile(`(127\.0\.0\.1:\d+) 에서 대기`)
	var addr string
	for i := 0; i < 100 && addr == ""; i++ {
		if m := re.FindStringSubmatch(out.String()); m != nil {
			addr = m[1]
		}
		time.Sleep(50 * time.Millisecond)
	}
	if addr == "" {
		t.Fatalf("no listen line: %s", out.String())
	}
	resp, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"readOnly":true`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if r, err := http.Get("http://" + addr + "/v1/summary"); err != nil || r.StatusCode != 401 {
		t.Fatalf("unauthenticated summary: %v %v", r, err)
	}
	if strings.Contains(out.String(), "fakepw") || strings.Contains(out.String(), fakeAPIToken) {
		t.Fatalf("output leaks secret: %s", out.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no shutdown")
	}
}
