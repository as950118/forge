package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/as950118/forge/internal/ledger"
	"github.com/as950118/forge/internal/mcp"
	"github.com/as950118/forge/internal/mcp/mcptest"
	"github.com/as950118/forge/internal/tool"
)

func approve(v bool) elicitFn {
	return func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		return &sdk.ElicitResult{Action: "accept", Content: map[string]any{"approve": v}}, nil
	}
}

func TestFilteringByExposureAndEnabled(t *testing.T) {
	e := newEnv(t)
	e.addMock("gw-srv", "alpha", mcp.ExposureGateway)
	e.addMock("direct-srv", "beta", mcp.ExposureDirect)
	e.addMock("hidden-srv", "gamma", mcp.ExposureHidden)
	e.addMock("off-srv", "delta", mcp.ExposureGateway)
	e.addMock("only-cc", "eps", "")
	if err := e.servers.SetExposure(context.Background(), "only-cc", "claude-code", mcp.ExposureGateway); err != nil {
		t.Fatal(err)
	}
	if err := e.servers.SetEnabled(context.Background(), "off-srv", false); err != nil {
		t.Fatal(err)
	}
	e.addTool("on", "none", "printf", "%s", "{{v}}")
	e.addTool("off", "none", "printf", "%s", "{{v}}")
	e.addTool("denied", tool.ConfirmDeny, "printf", "%s", "{{v}}")
	if err := e.tools.SetEnabled(context.Background(), "off", false); err != nil {
		t.Fatal(err)
	}

	g := e.gw()
	cc := listNames(t, e.connect(g, "claude-code", nil))
	for _, w := range []string{"gw_srv__alpha", "only_cc__eps", "forge_on", MetaSkill, MetaList} {
		if !has(cc, w) {
			t.Errorf("claude-code missing %s: %v", w, cc)
		}
	}
	for _, bad := range []string{"direct_srv__beta", "hidden_srv__gamma", "off_srv__delta", "forge_off", "forge_denied"} {
		if has(cc, bad) {
			t.Errorf("claude-code must not see %s", bad)
		}
	}
	oc := listNames(t, e.connect(g, "opencode", nil))
	if has(oc, "only_cc__eps") || !has(oc, "gw_srv__alpha") {
		t.Errorf("opencode: %v", oc)
	}
	// unknown client: conservative (gateway for every client only)
	un := listNames(t, e.connect(g, "some-ide", nil))
	if has(un, "only_cc__eps") || !has(un, "gw_srv__alpha") {
		t.Errorf("unknown: %v", un)
	}
	// --client overrides clientInfo
	forced := listNames(t, e.connect(e.gw(func(o *Options) { o.Client = "claude-code" }), "some-ide", nil))
	if !has(forced, "only_cc__eps") {
		t.Errorf("forced client: %v", forced)
	}
	// a server without cache is not exposed
	if err := e.servers.Add(context.Background(), mcp.Spec{Name: "nocache", Transport: mcp.TransportStdio, Command: "x"}); err != nil {
		t.Fatal(err)
	}
	_ = e.servers.SetExposure(context.Background(), "nocache", "claude-code", mcp.ExposureGateway)
	out, _ := invoke(t, e.connect(g, "claude-code", nil), MetaList, nil)
	if !strings.Contains(out, "nocache") || !strings.Contains(out, "forge mcp refresh") {
		t.Errorf("forge_list warnings: %s", out)
	}
}

func TestCustomToolCallAndDeny(t *testing.T) {
	e := newEnv(t)
	e.addTool("say", "none", "printf", "<%s>", "{{v}}")
	e.addTool("nope", tool.ConfirmDeny, "printf", "%s", "{{v}}")
	cs := e.connect(e.gw(), "claude-code", nil)
	if out, isErr := invoke(t, cs, "forge_say", map[string]any{"v": "hi"}); isErr || out != "<hi>" {
		t.Errorf("%q %v", out, isErr)
	}
	if out, isErr := invoke(t, cs, "forge_say", map[string]any{"v": 5}); !isErr || !strings.Contains(out, "스키마") {
		t.Errorf("invalid args: %q %v", out, isErr)
	}
	if _, err := callRaw(cs, "forge_nope", map[string]any{"v": "x"}); err == nil || !strings.Contains(err.Error(), "알 수 없는 도구") {
		t.Errorf("deny must be unknown: %v", err)
	}
	if _, err := callRaw(cs, "nothing_here", nil); err == nil {
		t.Error("unknown tool")
	}
}

func TestConfirmAsk(t *testing.T) {
	e := newEnv(t)
	marker := filepath.Join(e.work, "ran")
	e.addTool("touchit", tool.ConfirmAsk, "touch", "--", "{{v}}")
	g := e.gw()
	ran := func() bool { _, err := os.Stat(marker); return err == nil }

	var msg string
	cs := e.connect(g, "claude-code", func(_ context.Context, r *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		msg = r.Params.Message
		return &sdk.ElicitResult{Action: "accept", Content: map[string]any{"approve": false}}, nil
	})
	out, isErr := invoke(t, cs, "forge_touchit", map[string]any{"v": marker})
	if !isErr || !strings.Contains(out, "거부") || ran() {
		t.Errorf("approve=false must not run: %q ran=%v", out, ran())
	}
	if !strings.Contains(msg, "touchit") || !strings.Contains(msg, "touch") {
		t.Errorf("confirmation message lacks the command: %q", msg)
	}
	for _, action := range []string{"decline", "cancel"} {
		cs := e.connect(g, "claude-code", func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
			return &sdk.ElicitResult{Action: action}, nil
		})
		if _, isErr := invoke(t, cs, "forge_touchit", map[string]any{"v": marker}); !isErr || ran() {
			t.Errorf("%s must not run", action)
		}
	}
	// elicitation error (client failure) => refuse
	cs = e.connect(g, "claude-code", func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		return nil, errors.New("boom")
	})
	if out, isErr := invoke(t, cs, "forge_touchit", map[string]any{"v": marker}); !isErr || ran() || !strings.Contains(out, "확인을 받지 못") {
		t.Errorf("error answer: %q", out)
	}
	// invalid args are rejected without asking
	asked := false
	cs = e.connect(g, "claude-code", func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		asked = true
		return &sdk.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
	})
	if _, isErr := invoke(t, cs, "forge_touchit", map[string]any{"v": 3}); !isErr || asked {
		t.Errorf("invalid args asked=%v", asked)
	}
	// approve=true runs
	cs = e.connect(g, "claude-code", approve(true))
	if out, isErr := invoke(t, cs, "forge_touchit", map[string]any{"v": marker}); isErr || !ran() {
		t.Errorf("approved: %q %v ran=%v", out, isErr, ran())
	}
	_ = os.Remove(marker)
	// client without elicitation => safe refusal with guidance
	cs = e.connect(g, "claude-code", nil)
	out, isErr = invoke(t, cs, "forge_touchit", map[string]any{"v": marker})
	if !isErr || ran() || !strings.Contains(out, "사용자 확인이 필요한 도구입니다") || !strings.Contains(out, "forge tool test") {
		t.Errorf("unsupported: %q ran=%v", out, ran())
	}
	var classes []string
	for _, r := range e.rows() {
		classes = append(classes, r.ErrClass)
	}
	if !strings.Contains(strings.Join(classes, ","), "confirm_unsupported") || !strings.Contains(strings.Join(classes, ","), "confirm_refused") {
		t.Errorf("ledger classes %v", classes)
	}
}

func TestSelfProxyExcluded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, s := range []mcp.Spec{
		{Name: "forge", Transport: mcp.TransportStdio, Command: os.Args[0]},
		{Name: "wrapped", Transport: mcp.TransportStdio, Command: "/usr/local/bin/forge", Args: []string{"mcp", "serve"}},
		{Name: "viash", Transport: mcp.TransportStdio, Command: "sh", Args: []string{"-c", "forge mcp serve --client auto"}},
		{Name: "fine", Transport: mcp.TransportStdio, Command: os.Args[0]},
	} {
		if err := e.servers.Add(ctx, s); err != nil {
			t.Fatal(err)
		}
		_ = e.servers.SetExposure(ctx, s.Name, "claude-code", mcp.ExposureGateway)
		for _, id := range []string{"x"} {
			_ = id
			_, err := e.db.Exec(`INSERT INTO mcp_tools(server_id,name,description,input_schema,schema_hash,stale,discovered_at) SELECT id,'t','d','{}','h',0,1 FROM mcp_servers WHERE name=?`, s.Name)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	cs := e.connect(e.gw(), "claude-code", nil)
	names := listNames(t, cs)
	if !has(names, "fine__t") {
		t.Errorf("fine server missing: %v", names)
	}
	for _, bad := range []string{"forge__t", "wrapped__t", "viash__t"} {
		if has(names, bad) {
			t.Errorf("self proxy %s exposed", bad)
		}
	}
	out, _ := invoke(t, cs, MetaList, nil)
	if !strings.Contains(out, "게이트웨이 자신") {
		t.Errorf("warning missing: %s", out)
	}
	if _, err := callRaw(cs, "forge__t", nil); err == nil {
		t.Error("call to self proxy must fail")
	}
}

func TestSkillLookup(t *testing.T) {
	e := newEnv(t)
	e.addSkill("demo-skill", "A demo skill", "# Body\nhello\n", map[string]string{"references/guide.md": "guide text"})
	e.addSkill("off-skill", "disabled", "x", nil)
	e.addSkill("big-skill", "big", strings.Repeat("A", 5000), nil)
	if err := e.skills.SetEnabled(context.Background(), "off-skill", false); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := e.gw(func(o *Options) { o.SkillBytes = 1000 })
	cs := e.connect(g, "claude-code", nil)

	out, isErr := invoke(t, cs, MetaSkill, map[string]any{"name": "demo-skill"})
	if isErr || !strings.Contains(out, "hello") || !strings.Contains(out, "references/guide.md") {
		t.Errorf("skill body: %q %v", out, isErr)
	}
	if out, _ := invoke(t, cs, MetaSkill, map[string]any{"name": "Demo-Skill", "file": "references/guide.md"}); !strings.Contains(out, "guide text") {
		t.Errorf("file read: %q", out)
	}
	if out, isErr := invoke(t, cs, MetaSkill, map[string]any{"name": "ghost"}); !isErr || !strings.Contains(out, "찾을 수 없습니다") {
		t.Errorf("miss: %q", out)
	}
	if out, isErr := invoke(t, cs, MetaSkill, map[string]any{"name": "off-skill"}); !isErr || !strings.Contains(out, "비활성") {
		t.Errorf("disabled: %q", out)
	}
	for _, f := range []string{"../../forge.db", "/etc/passwd", "references/../../../forge.db", secret, "..", `..\x`} {
		if out, isErr := invoke(t, cs, MetaSkill, map[string]any{"name": "demo-skill", "file": f}); !isErr || strings.Contains(out, "TOPSECRET") || strings.Contains(out, "SQLite") {
			t.Errorf("escape %q: %q %v", f, out, isErr)
		}
	}
	// a symlink inside the skill pointing out is refused (fresh snapshot with a link)
	snap := filepath.Join(e.home, "skills")
	entries, _ := os.ReadDir(snap)
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), "demo-skill@") {
			dir := filepath.Join(snap, en.Name())
			_ = os.Chmod(dir, 0o700)
			if err := os.Symlink(secret, filepath.Join(dir, "link.txt")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if out, isErr := invoke(t, cs, MetaSkill, map[string]any{"name": "demo-skill", "file": "link.txt"}); !isErr || strings.Contains(out, "TOPSECRET") {
		t.Errorf("symlink escape: %q", out)
	}
	if out, _ := invoke(t, cs, MetaSkill, map[string]any{"name": "big-skill"}); !strings.Contains(out, "잘렸습니다") || len(out) > 2500 {
		t.Errorf("size cap: len=%d", len(out))
	}
	if out, isErr := invoke(t, cs, MetaSkill, map[string]any{}); !isErr {
		t.Errorf("missing name: %q", out)
	}
	if out, _ := invoke(t, cs, MetaList, nil); !strings.Contains(out, "demo-skill") || strings.Contains(out, "off-skill:") {
		t.Errorf("forge_list: %s", out)
	}

	rows := e.rows()
	var okRow, miss, dis *ledgerRow
	for i := range rows {
		r := &rows[i]
		switch {
		case r.Kind == "skill" && r.Name == "demo-skill" && okRow == nil:
			okRow = r
		case r.Name == "ghost":
			miss = r
		case r.Name == "off-skill":
			dis = r
		}
	}
	if okRow == nil || okRow.Source != "gateway" || okRow.Invocation != "model" || okRow.Client != "claude-code" || okRow.OK.Int64 != 1 || okRow.Found.Int64 != 1 || okRow.Session != g.SessionID() {
		t.Errorf("skill row: %+v", okRow)
	}
	if miss == nil || !miss.Found.Valid || miss.Found.Int64 != 0 || miss.OK.Int64 != 0 || miss.ErrClass != "not_found" {
		t.Errorf("miss row: %+v", miss)
	}
	if dis == nil || dis.Found.Int64 != 0 || dis.ErrClass != "disabled" {
		t.Errorf("disabled row: %+v", dis)
	}
}

func TestProxyStdioBasics(t *testing.T) {
	e := newEnv(t)
	e.addMock("mock-srv", "echo,fail,big,slow,pid,alpha", mcp.ExposureGateway)
	g := e.gw(func(o *Options) { o.MaxResponse = 64 << 10; o.CallTimeout = 700 * time.Millisecond })
	cs := e.connect(g, "claude-code", nil)

	if out, isErr := invoke(t, cs, "mock_srv__echo", map[string]any{"q": "hello"}); isErr || out != "hello" {
		t.Errorf("echo %q %v", out, isErr)
	}
	if out, isErr := invoke(t, cs, "mock_srv__alpha", nil); isErr || out != "ok" {
		t.Errorf("alpha %q", out)
	}
	if out, isErr := invoke(t, cs, "mock_srv__fail", nil); !isErr || out != "mock failure" {
		t.Errorf("fail %q %v", out, isErr)
	}
	if out, isErr := invoke(t, cs, "mock_srv__echo", map[string]any{"zzz": 1}); !isErr || !strings.Contains(out, "스키마") {
		t.Errorf("invalid args: %q", out)
	}
	out, isErr := invoke(t, cs, "mock_srv__big", nil)
	if isErr || len(out) > 64<<10+300 || !strings.Contains(out, "일부만 전달") {
		t.Errorf("big: len=%d %v", len(out), isErr)
	}
	start := time.Now()
	out, isErr = invoke(t, cs, "mock_srv__slow", map[string]any{"ms": 5000})
	if !isErr || !strings.Contains(out, "응답하지 않아") || time.Since(start) > 4*time.Second {
		t.Errorf("timeout: %q after %v", out, time.Since(start))
	}
	// the wedged connection was dropped; the next call reconnects
	if out, isErr := invoke(t, cs, "mock_srv__echo", map[string]any{"q": "again"}); isErr || out != "again" {
		t.Errorf("after timeout %q", out)
	}
	classes := map[string]bool{}
	for _, r := range e.rows() {
		if r.Kind != "mcp" || r.Server != "mock-srv" || r.Source != "gateway" || r.Client != "claude-code" {
			t.Errorf("row %+v", r)
		}
		classes[r.ErrClass] = true
	}
	for _, c := range []string{"", "upstream_error", "invalid_args", "timeout"} {
		if !classes[c] {
			t.Errorf("ledger class %q missing: %v", c, classes)
		}
	}
}

func TestProxyServerDeathReconnectAndCleanup(t *testing.T) {
	e := newEnv(t)
	e.addMock("mock-srv", "pid,die,echo", mcp.ExposureGateway)
	g := e.gw()
	cs := e.connect(g, "claude-code", nil)
	pidOf := func() int {
		out, _ := invoke(t, cs, "mock_srv__pid", nil)
		n := 0
		for _, c := range out {
			n = n*10 + int(c-'0')
		}
		return n
	}
	p1 := pidOf()
	if p1 == 0 || pidOf() != p1 {
		t.Fatal("connection must be reused")
	}
	out, isErr := invoke(t, cs, "mock_srv__die", nil)
	if !isErr || !strings.Contains(out, "연결이 끊어졌") {
		t.Errorf("die: %q", out)
	}
	p2 := pidOf()
	if p2 == 0 || p2 == p1 {
		t.Errorf("reconnect pids %d %d", p1, p2)
	}
	g.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(p2, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d still alive after Close", p2)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out, isErr := invoke(t, cs, "mock_srv__echo", map[string]any{"q": "x"}); !isErr {
		t.Errorf("closed gateway served a call: %q", out)
	}
}

func TestProxyConnectFailureMessage(t *testing.T) {
	e := newEnv(t)
	e.register(mcp.Spec{Name: "later", Transport: mcp.TransportStdio, Command: os.Args[0], Values: []mcp.Value{
		{Slot: mcp.SlotEnv, Key: mcptest.EnvMode, RefType: mcp.RefLiteral, Literal: "ok", Display: "ok"}}}, mcp.ExposureGateway)
	// break the definition after discovery: the command vanishes
	if _, err := e.db.Exec(`UPDATE mcp_servers SET command='/nonexistent/forge-test-bin' WHERE name='later'`); err != nil {
		t.Fatal(err)
	}
	cs := e.connect(e.gw(func(o *Options) { o.DialRetries = 2 }), "claude-code", nil)
	out, isErr := invoke(t, cs, "later__alpha", nil)
	if !isErr || !strings.Contains(out, "연결할 수 없습니다") || !strings.Contains(out, "실행 파일을 찾을 수 없습니다") {
		t.Errorf("%q", out)
	}
	rows := e.rows()
	if len(rows) == 0 || rows[len(rows)-1].ErrClass != "command_not_found" {
		t.Errorf("rows %+v", rows)
	}
}

func TestProxyHTTP(t *testing.T) {
	e := newEnv(t)
	hs := mcptest.HTTP([]string{"h1", "echo"}, "http tool")
	defer hs.Close()
	e.register(mcp.Spec{Name: "web", Transport: mcp.TransportHTTP, URL: hs.URL}, mcp.ExposureGateway)
	cs := e.connect(e.gw(), "claude-code", nil)
	if out, isErr := invoke(t, cs, "web__h1", nil); isErr || out != "ok" {
		t.Errorf("http %q %v", out, isErr)
	}
	if out, _ := invoke(t, cs, "web__echo", map[string]any{"q": "via http"}); out != "via http" {
		t.Errorf("echo %q", out)
	}
	hs.Close()
	if out, isErr := invoke(t, cs, "web__h1", nil); !isErr || out == "" {
		t.Errorf("dead http server: %q", out)
	}
}

func TestSecretsNotLeaked(t *testing.T) {
	e := newEnv(t)
	const secret = "ghp_SUPERSECRETTOKENVALUE0123456789"
	t.Setenv("FORGE_TEST_UPSTREAM_TOKEN", secret)
	t.Setenv("FORGE_PARENT_ONLY", "parent-env-value")
	e.addMock("leaky", "leak,pid", mcp.ExposureGateway,
		mcp.Value{Slot: mcp.SlotEnv, Key: "UPSTREAM_TOKEN", IsSecret: true, RefType: mcp.RefEnv, Ref: "FORGE_TEST_UPSTREAM_TOKEN", Display: "${FORGE_TEST_UPSTREAM_TOKEN}"},
		mcp.Value{Slot: mcp.SlotEnv, Key: "MOCK_LEAK_ENV", RefType: mcp.RefLiteral, Literal: "UPSTREAM_TOKEN", Display: "UPSTREAM_TOKEN"})
	cs := e.connect(e.gw(), "claude-code", nil)
	out, _ := invoke(t, cs, "leaky__leak", nil)
	if strings.Contains(out, secret) || !strings.Contains(out, "***") {
		t.Errorf("secret in tool result: %q", out)
	}
	// the secret reached the child (value=*** means the child saw it) but nothing else did
	listed, _ := invoke(t, cs, MetaList, nil)
	if strings.Contains(listed, secret) {
		t.Error("secret in forge_list")
	}
	for _, r := range e.rows() {
		if strings.Contains(r.Name+r.Server+r.ErrClass, secret) {
			t.Errorf("secret in ledger: %+v", r)
		}
	}
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM ledger_payloads`).Scan(&n); err != nil || n != 0 {
		t.Errorf("payload stored by default: %d %v", n, err)
	}
}

func TestParentEnvNotInherited(t *testing.T) {
	t.Setenv("FORGE_PARENT_ONLY", "parent-env-value")
	env := mcp.MinimalEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "FORGE_PARENT_ONLY=") {
			t.Fatal("parent variable leaked into minimal env")
		}
	}
	if len(env) == 0 {
		t.Error("minimal env is empty (PATH missing)")
	}
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			found = true
		}
	}
	if !found {
		t.Error("PATH must be kept")
	}
}

func TestProxyConcurrency(t *testing.T) {
	e := newEnv(t)
	e.addMock("mock-srv", "slow,echo", mcp.ExposureGateway)
	g := e.gw(func(o *Options) { o.MaxConcurrent = 2 })
	cs := e.connect(g, "claude-code", nil)
	if out, _ := invoke(t, cs, "mock_srv__echo", map[string]any{"q": "warm"}); out != "warm" {
		t.Fatal(out)
	}
	var wg sync.WaitGroup
	start := time.Now()
	errs := make(chan string, 8)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := callRaw(cs, "mock_srv__slow", map[string]any{"ms": 300})
			if err != nil || res.IsError {
				errs <- "call failed"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for m := range errs {
		t.Error(m)
	}
	// 6 calls x 300ms with at most 2 in flight need >= 3 rounds
	if d := time.Since(start); d < 850*time.Millisecond {
		t.Errorf("concurrency limit not enforced: %v", d)
	}
	var okRows int
	for _, r := range e.rows() {
		if r.Name == "slow" && r.OK.Int64 == 1 {
			okRows++
		}
	}
	if okRows != 6 {
		t.Errorf("ledger rows for slow: %d", okRows)
	}
}

type failingRecorder struct{ n int }

func (f *failingRecorder) Record(context.Context, ledger.Event) (int64, error) {
	f.n++
	return 0, errors.New("disk on fire")
}

type panicRecorder struct{}

func (panicRecorder) Record(context.Context, ledger.Event) (int64, error) { panic("recorder bug") }

func TestLedgerFailureDoesNotChangeResult(t *testing.T) {
	e := newEnv(t)
	e.addTool("say", "none", "printf", "<%s>", "{{v}}")
	e.addMock("mock-srv", "echo", mcp.ExposureGateway)
	e.addSkill("demo-skill", "d", "body", nil)
	for _, rec := range []ledger.Recorder{&failingRecorder{}, panicRecorder{}} {
		cs := e.connect(e.gw(func(o *Options) { o.Recorder = rec }), "claude-code", nil)
		if out, isErr := invoke(t, cs, "forge_say", map[string]any{"v": "x"}); isErr || out != "<x>" {
			t.Errorf("tool: %q", out)
		}
		if out, isErr := invoke(t, cs, "mock_srv__echo", map[string]any{"q": "y"}); isErr || out != "y" {
			t.Errorf("proxy: %q", out)
		}
		if out, isErr := invoke(t, cs, MetaSkill, map[string]any{"name": "demo-skill"}); isErr || !strings.Contains(out, "body") {
			t.Errorf("skill: %q", out)
		}
	}
	// no recorder at all
	cs := e.connect(e.gw(func(o *Options) { o.Recorder = nil }), "claude-code", nil)
	if out, _ := invoke(t, cs, "forge_say", map[string]any{"v": "z"}); out != "<z>" {
		t.Errorf("nil recorder: %q", out)
	}
}

func TestLedgerRowsPerKindAndMisses(t *testing.T) {
	e := newEnv(t)
	e.addTool("say", "none", "printf", "<%s>", "{{v}}")
	e.addMock("mock-srv", "echo", mcp.ExposureGateway)
	g := e.gw()
	cs := e.connect(g, "claude-code", nil)
	invoke(t, cs, "forge_say", map[string]any{"v": "x"})
	invoke(t, cs, "forge_say", map[string]any{"v": 1}) // failed call
	invoke(t, cs, "mock_srv__echo", map[string]any{"q": "y"})
	_, _ = callRaw(cs, "forge_ghost", nil)    // tool miss
	_, _ = callRaw(cs, "mock_srv__nope", nil) // mcp miss
	rows := e.rows()
	if len(rows) != 5 {
		t.Fatalf("rows=%d %+v", len(rows), rows)
	}
	want := []struct {
		kind, name, server string
		ok, found          int64
		class              string
	}{
		{"tool", "say", "", 1, 1, ""},
		{"tool", "say", "", 0, 1, "invalid_args"},
		{"mcp", "echo", "mock-srv", 1, 1, ""},
		{"tool", "forge_ghost", "", 0, 0, "not_found"},
		{"mcp", "nope", "mock-srv", 0, 0, "not_found"},
	}
	for i, w := range want {
		r := rows[i]
		if r.Kind != w.kind || r.Name != w.name || r.OK.Int64 != w.ok || r.Found.Int64 != w.found || r.ErrClass != w.class ||
			r.Source != "gateway" || r.Client != "claude-code" || r.Session != g.SessionID() {
			t.Errorf("row %d: %+v want %+v", i, r, w)
		}
	}
	// a client we cannot identify is stored with client NULL
	cs2 := e.connect(g, "weird-client", nil)
	invoke(t, cs2, "forge_say", map[string]any{"v": "x"})
	rows = e.rows()
	if last := rows[len(rows)-1]; last.Client != "" || last.Kind != "tool" {
		t.Errorf("unknown client row: %+v", last)
	}
}

func TestPayloadOptInMasked(t *testing.T) {
	e := newEnv(t)
	e.addMock("mock-srv", "echo", mcp.ExposureGateway)
	cs := e.connect(e.gw(), "claude-code", nil)
	const secret = "ghp_SUPERSECRETTOKENVALUE0123456789"
	if _, err := e.db.Exec(`INSERT INTO settings(key,value,updated_at) VALUES ('record_payload','1',1)`); err != nil {
		t.Fatal(err)
	}
	invoke(t, cs, "mock_srv__echo", map[string]any{"q": "token " + secret})
	var in, out sql.NullString
	if err := e.db.QueryRow(`SELECT input_json, output_json FROM ledger_payloads`).Scan(&in, &out); err != nil {
		t.Fatal(err)
	}
	if !in.Valid || strings.Contains(in.String+out.String, secret) {
		t.Errorf("payload not masked: %q %q", in.String, out.String)
	}
}

func TestConfirmShowsShellArguments(t *testing.T) {
	e := newEnv(t)
	d := tool.Definition{Name: "shq", Kind: tool.KindCommand, Description: "shell tool", Shell: true,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"},"token":{"type":"string"}},"required":["q"],"additionalProperties":false}`),
		Command:     "sh", Args: []string{"-c", `echo "$FORGE_ARG_q"`}, Policy: tool.Policy{Confirm: tool.ConfirmAsk, WorkingDir: e.work}}
	if _, err := e.tools.Add(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	var msg string
	cs := e.connect(e.gw(), "claude-code", func(_ context.Context, r *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		msg = r.Params.Message
		return &sdk.ElicitResult{Action: "decline"}, nil
	})
	long := strings.Repeat("z", 5000)
	invoke(t, cs, "forge_shq", map[string]any{"q": "a", "token": "sk-live-FAKEFAKEFAKE1234567890"})
	if !strings.Contains(msg, "인자:") || !strings.Contains(msg, `q="a"`) {
		t.Errorf("shell argument values missing from confirmation: %q", msg)
	}
	if strings.Contains(msg, "FAKEFAKEFAKE") {
		t.Errorf("secret leaked into confirmation: %q", msg)
	}
	invoke(t, cs, "forge_shq", map[string]any{"q": long})
	if len(msg) > 2000 || strings.Contains(msg, strings.Repeat("z", 400)) {
		t.Errorf("argument display not bounded: %d bytes", len(msg))
	}
}

func TestInvalidArgsCallsAreRecorded(t *testing.T) {
	e := newEnv(t)
	e.addTool("plain", "none", "printf", "%s", "{{v}}")
	e.addMock("gw-srv", "alpha", mcp.ExposureGateway)
	cs := e.connect(e.gw(), "claude-code", nil)
	invoke(t, cs, MetaSkill, map[string]any{})                     // no name
	invoke(t, cs, MetaSkill, map[string]any{"name": 5})            // wrong type
	invoke(t, cs, MetaSkill, map[string]any{"name": "x", "z": 1})  // extra property
	invoke(t, cs, "forge_plain", map[string]any{"v": 3})           // custom tool
	invoke(t, cs, "gw_srv__alpha", map[string]any{"bad": "extra"}) // proxy (cached schema)
	rows := e.rows()
	want := []struct{ kind, name string }{{"skill", MetaSkill}, {"skill", MetaSkill}, {"skill", MetaSkill}, {"tool", "plain"}}
	if len(rows) < len(want) {
		t.Fatalf("rows=%+v", rows)
	}
	for i, w := range want {
		r := rows[i]
		if r.Kind != w.kind || r.Name != w.name || r.ErrClass != classInvalidArgs || !r.OK.Valid || r.OK.Int64 != 0 {
			t.Errorf("row %d = %+v, want %v invalid_args ok=0", i, r, w)
		}
	}
}
