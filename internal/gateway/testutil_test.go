package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/as950118/forge/internal/ledger"
	"github.com/as950118/forge/internal/mcp"
	"github.com/as950118/forge/internal/mcp/mcptest"
	"github.com/as950118/forge/internal/skill"
	"github.com/as950118/forge/internal/store"
	"github.com/as950118/forge/internal/testenv"
	"github.com/as950118/forge/internal/tool"
)

func TestMain(m *testing.M) {
	mcptest.MaybeRunStdio()
	os.Exit(testenv.Run(m))
}

type env struct {
	t       *testing.T
	home    string
	db      *sql.DB
	servers *mcp.Registry
	tools   *tool.Registry
	skills  *skill.Registry
	rec     *ledger.SQLite
	work    string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	t.Cleanup(func() { // skill snapshots are read-only
		_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
			if err == nil {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	if err := store.EnsureLayout(home); err != nil {
		t.Fatal(err)
	}
	conn, err := store.Open(context.Background(), store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &env{t: t, home: home, db: conn,
		servers: &mcp.Registry{DB: conn}, tools: &tool.Registry{DB: conn, Home: home},
		skills: &skill.Registry{DB: conn, Home: home}, rec: &ledger.SQLite{DB: conn}, work: t.TempDir()}
}

// gw builds a gateway over the env registries and closes it at test end.
func (e *env) gw(mod ...func(*Options)) *Gateway {
	e.t.Helper()
	o := Options{Servers: e.servers, Tools: e.tools, Skills: e.skills, Recorder: e.rec, Project: e.work, Version: "test"}
	for _, m := range mod {
		m(&o)
	}
	g, err := New(o)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(g.Close)
	return g
}

type elicitFn = func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error)

// connect attaches an in-process MCP client (clientInfo.name = clientName). A nil
// elicit handler means the client does not advertise elicitation.
func (e *env) connect(g *Gateway, clientName string, elicit elicitFn) *sdk.ClientSession {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t1, t2 := sdk.NewInMemoryTransports()
	ss, err := g.Server().Connect(ctx, t1, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	var opts *sdk.ClientOptions
	if elicit != nil {
		opts = &sdk.ClientOptions{ElicitationHandler: elicit}
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: "9.9"}, opts).Connect(ctx, t2, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = cs.Close(); _ = ss.Close(); cancel() })
	return cs
}

func listNames(t *testing.T, cs *sdk.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tl := range res.Tools {
		out = append(out, tl.Name)
	}
	return out
}

func has(names []string, n string) bool {
	for _, x := range names {
		if x == n {
			return true
		}
	}
	return false
}

func callRaw(cs *sdk.ClientSession, name string, args map[string]any) (*sdk.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
}

func invoke(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := callRaw(cs, name, args)
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func (e *env) addTool(name string, confirm string, argv ...string) {
	e.t.Helper()
	d := tool.Definition{Name: name, Kind: tool.KindCommand, Description: "test tool " + name,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"v":{"type":"string"}},"required":["v"],"additionalProperties":false}`),
		Command:     argv[0], Args: argv[1:], Policy: tool.Policy{Confirm: confirm, WorkingDir: e.work}}
	if _, err := e.tools.Add(context.Background(), d, false); err != nil {
		e.t.Fatal(err)
	}
}

// addMock registers a stdio mock server (the test binary re-executed), caches its
// tools, and sets exposure for both clients.
func (e *env) addMock(name, tools string, expose string, extra ...mcp.Value) {
	e.t.Helper()
	vals := []mcp.Value{
		{Slot: mcp.SlotEnv, Key: mcptest.EnvMode, RefType: mcp.RefLiteral, Literal: "ok", Display: "ok"},
		{Slot: mcp.SlotEnv, Key: "MOCK_TOOLS", RefType: mcp.RefLiteral, Literal: tools, Display: tools},
	}
	vals = append(vals, extra...)
	e.register(mcp.Spec{Name: name, Transport: mcp.TransportStdio, Command: os.Args[0], Values: vals}, expose)
}

func (e *env) register(s mcp.Spec, expose string) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.servers.Add(ctx, s); err != nil {
		e.t.Fatal(err)
	}
	r, err := e.servers.Refresh(ctx, s.Name, mcp.RefreshOptions{Timeout: 30 * time.Second})
	if err != nil || !r.OK {
		e.t.Fatalf("refresh %s: %v %+v", s.Name, err, r)
	}
	for _, c := range []string{"claude-code", "opencode"} {
		if expose != "" {
			if err := e.servers.SetExposure(ctx, s.Name, c, expose); err != nil {
				e.t.Fatal(err)
			}
		}
	}
}

func (e *env) addSkill(name, desc, body string, extra map[string]string) {
	e.t.Helper()
	d := filepath.Join(e.t.TempDir(), name)
	write := func(rel, content string) {
		p := filepath.Join(d, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	write("SKILL.md", "---\nname: "+name+"\ndescription: "+desc+"\n---\n"+body)
	for k, v := range extra {
		write(k, v)
	}
	p, err := skill.Prepare(context.Background(), d, skill.PrepareOptions{TmpDir: filepath.Join(e.home, "tmp")})
	if err != nil {
		e.t.Fatal(err)
	}
	defer p.Cleanup()
	if _, err := e.skills.Add(context.Background(), p); err != nil {
		e.t.Fatal(err)
	}
}

type ledgerRow struct {
	Kind, Name, Server, Client, Session, ErrClass, Source, Invocation string
	OK, Found                                                         sql.NullInt64
}

func (e *env) rows() []ledgerRow {
	e.t.Helper()
	rs, err := e.db.Query(`SELECT kind,name,COALESCE(mcp_server,''),COALESCE(client,''),COALESCE(session_id,''),COALESCE(error_class,''),source,COALESCE(invocation,''),ok,found FROM ledger ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	var out []ledgerRow
	for rs.Next() {
		var r ledgerRow
		if err := rs.Scan(&r.Kind, &r.Name, &r.Server, &r.Client, &r.Session, &r.ErrClass, &r.Source, &r.Invocation, &r.OK, &r.Found); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}
