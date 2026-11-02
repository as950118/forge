package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heonjinjeong/forge/internal/ledger"
	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/skill"
	"github.com/heonjinjeong/forge/internal/tool"
)

// EnvActive is set in the environment of every process the gateway starts. A
// `forge mcp serve` that finds it set refuses to run (recursion guard).
const EnvActive = "FORGE_GATEWAY"

// EnvDebug enables stderr logging (names, timings, errors; never arguments or results).
const EnvDebug = "FORGE_GATEWAY_DEBUG"

// Default limits.
const (
	DefaultCallTimeout   = 60 * time.Second
	DefaultMaxResponse   = 256 << 10
	DefaultMaxConcurrent = 8
	DefaultSkillBytes    = 128 << 10
	DefaultConfirmWait   = 2 * time.Minute
	maxListedSkills      = 20
)

// Options configures a Gateway. Servers, Tools and Skills are required.
type Options struct {
	// Name is the server name the gateway is registered under in the client
	// (default "forge"); it sets the tool-name length budget and the recursion check.
	Name    string
	Version string
	// Client forces the client id ("claude-code"|"opencode"); "" or "auto"
	// identifies the client from initialize.clientInfo.name.
	Client string
	// Project is the working directory used as ledger.project and as the default
	// cwd root of custom tools (default: process cwd).
	Project string

	Servers *mcp.Registry
	Tools   *tool.Registry
	Skills  *skill.Registry

	// Recorder receives one gateway row per call (nil = no ledger). Failures are best-effort.
	Recorder ledger.Recorder
	// Exposure defaults to SettingsExposure{}.
	Exposure Exposure
	// SessionID identifies this gateway process in the ledger (default: random).
	SessionID string
	// Logger receives debug logs (default: stderr when FORGE_GATEWAY_DEBUG=1, else discarded).
	Logger *slog.Logger

	CallTimeout   time.Duration // per proxied call (default 60s)
	MaxResponse   int           // bytes of a proxied response (default 256 KiB)
	MaxConcurrent int           // simultaneous proxied calls (default 8)
	SkillBytes    int           // bytes returned by forge_skill (default 128 KiB)
	ConfirmWait   time.Duration // how long to wait for an elicitation answer (default 2m)
	// DialRetries is the number of connection attempts per call (default 2).
	DialRetries int
	// ProxyEnv is the base environment of stdio children (default mcp.MinimalEnv() + FORGE_GATEWAY=1).
	ProxyEnv []string
}

// Gateway is the MCP server. Create with New, run with Serve / ServeStdio, release with Close.
type Gateway struct {
	o      Options
	log    *slog.Logger
	pool   *pool
	sem    chan struct{}
	server *sdk.Server

	unkMu   sync.Mutex
	unknown map[safeName]struct{} // sanitized unknown names already recorded this session
}

// New validates options and builds the gateway (no I/O yet).
func New(o Options) (*Gateway, error) {
	if o.Servers == nil || o.Tools == nil || o.Skills == nil {
		return nil, errors.New("gateway: Servers, Tools, Skills 레지스트리가 필요합니다")
	}
	if o.Name == "" {
		o.Name = DefaultName
	}
	if o.Client == "auto" {
		o.Client = ""
	}
	if o.Client != "" && !KnownClient(o.Client) {
		return nil, fmt.Errorf("알 수 없는 클라이언트: %q (claude-code|opencode|auto)", o.Client)
	}
	if o.Exposure == nil {
		o.Exposure = SettingsExposure{}
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.MaxResponse <= 0 {
		o.MaxResponse = DefaultMaxResponse
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = DefaultMaxConcurrent
	}
	if o.SkillBytes <= 0 {
		o.SkillBytes = DefaultSkillBytes
	}
	if o.ConfirmWait <= 0 {
		o.ConfirmWait = DefaultConfirmWait
	}
	if o.DialRetries <= 0 {
		o.DialRetries = 2
	}
	var droppedEnv []string
	if o.ProxyEnv == nil {
		droppedEnv = mcp.MinimalEnvDropped()
		o.ProxyEnv = append(mcp.MinimalEnv(), EnvActive+"=1")
	}
	if o.Project == "" {
		o.Project, _ = os.Getwd()
	}
	if o.SessionID == "" {
		o.SessionID = newSessionID()
	}
	g := &Gateway{o: o, sem: make(chan struct{}, o.MaxConcurrent)}
	g.log = o.Logger
	if g.log == nil {
		if os.Getenv(EnvDebug) == "1" {
			g.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		} else {
			g.log = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
	}
	if len(droppedEnv) > 0 {
		g.log.Debug("variables with embedded credentials are not passed to proxied servers; declare them with --env NAME=${NAME}", "names", strings.Join(droppedEnv, ","))
	}
	g.pool = newPool(g)
	g.server = g.buildServer()
	return g, nil
}

func newSessionID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("gw-%d", time.Now().UnixNano())
	}
	return "gw-" + hex.EncodeToString(b)
}

// SessionID is the ledger session id of this gateway process.
func (g *Gateway) SessionID() string { return g.o.SessionID }

// Server exposes the underlying SDK server (tests connect it to in-memory transports).
func (g *Gateway) Server() *sdk.Server { return g.server }

func (g *Gateway) buildServer() *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: g.o.Name, Version: g.o.Version}, &sdk.ServerOptions{
		Instructions: "Forge 게이트웨이: 등록된 커스텀 도구(forge_<이름>), MCP 프록시(<서버>__<도구>), 스킬 조회(forge_skill)를 제공합니다. forge_list 로 현재 노출 목록을 확인하세요.",
		Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}},
	})
	// Tools are resolved per request (exposure depends on the client that sent
	// initialize), so none are registered on the SDK server: tools/list and
	// tools/call are answered by this middleware.
	s.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			ss, _ := req.GetSession().(*sdk.ServerSession)
			if ss == nil || ss.InitializeParams() == nil {
				return next(ctx, method, req)
			}
			switch method {
			case "tools/list":
				client, _ := g.clientOf(ss)
				return g.listTools(g.withSnapshot(ctx, client), ss)
			case "tools/call":
				if r, ok := req.(*sdk.CallToolRequest); ok {
					client, _ := g.clientOf(ss)
					return g.callTool(g.withSnapshot(ctx, client), ss, r.Params)
				}
			}
			return next(ctx, method, req)
		}
	})
	return s
}

// Serve runs on t until the client disconnects (stdin closed) or ctx ends, then
// stops every proxied child process.
func (g *Gateway) Serve(ctx context.Context, t sdk.Transport) error {
	defer g.Close()
	err := g.server.Run(ctx, t)
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, sdk.ErrConnectionClosed) {
		return nil
	}
	g.log.Debug("serve ended", "error", err)
	return nil
}

// ServeStdio serves MCP over this process's stdin/stdout. Stdout is reserved for
// the protocol: after the transport takes it, os.Stdout is pointed at stderr so
// that a stray print cannot corrupt the stream. SIGINT/SIGTERM shut down cleanly.
func (g *Gateway) ServeStdio(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	out := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = out }()
	return g.Serve(ctx, &sdk.IOTransport{Reader: os.Stdin, Writer: out})
}

// Close stops all proxied servers (stdin EOF, SIGTERM, SIGKILL per process group).
func (g *Gateway) Close() { g.pool.closeAll() }

// ---- catalog -------------------------------------------------------------

type entryKind int

const (
	kindMeta entryKind = iota
	kindCustom
	kindProxy
)

type entry struct {
	Name     string // exposed name
	Kind     entryKind
	Identity string
	Desc     string
	Schema   any

	Tool     string // custom tool name / meta name
	Server   string // proxy: registered server name
	Upstream string // proxy: upstream tool name
	// proxy: cached schema for the minimal argument check
	Raw []byte
}

type catalog struct {
	entries  []*entry
	byName   map[string]*entry
	warnings []string
}

func (g *Gateway) clientOf(ss *sdk.ServerSession) (id, version string) {
	p := ss.InitializeParams()
	if p != nil && p.ClientInfo != nil {
		version = p.ClientInfo.Version
		if g.o.Client == "" {
			id = IdentifyClient(p.ClientInfo.Name)
		}
	}
	if g.o.Client != "" {
		id = g.o.Client
	}
	return id, version
}

// build computes the tools visible to client from the registries (always fresh,
// so enable/disable/expose changes apply to the next tools/list or tools/call).
func (g *Gateway) build(ctx context.Context, client string) (*catalog, error) {
	var cands []candidate
	ents := map[string]*entry{}
	add := func(rank int, identity, base string, e *entry) {
		e.Identity = identity
		ents[identity] = e
		cands = append(cands, candidate{rank: rank, identity: identity, base: base})
	}
	var notes []string

	add(rankMeta, "meta:"+MetaSkill, MetaSkill, &entry{Kind: kindMeta, Tool: MetaSkill, Desc: g.skillDescription(ctx, client), Schema: schemaOf(skillSchema)})
	add(rankMeta, "meta:"+MetaList, MetaList, &entry{Kind: kindMeta, Tool: MetaList, Desc: "Forge 게이트웨이가 현재 노출 중인 도구와 스킬의 요약(이름·설명)을 반환합니다.", Schema: schemaOf(listSchema)})

	defs, err := g.o.Tools.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("도구 목록을 읽을 수 없습니다: %w", err)
	}
	for i := range defs {
		d := &defs[i]
		if !d.Enabled || d.Policy.Confirm == tool.ConfirmDeny || !g.exposure(ctx).ToolVisible(d, client) {
			continue
		}
		desc := listText(d.Description, maxDescBytes)
		if effectiveConfirm(d) == tool.ConfirmAsk {
			desc += " (실행 전 사용자 확인 필요)"
		}
		add(rankCustom, "tool:"+d.Name, customBase(d.Name), &entry{Kind: kindCustom, Tool: d.Name, Desc: desc, Schema: schemaOf(d.InputSchema)})
	}

	servers, err := g.o.Servers.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("MCP 서버 목록을 읽을 수 없습니다: %w", err)
	}
	for i := range servers {
		s := &servers[i]
		if !s.Enabled || g.exposure(ctx).ServerMode(s, client) != mcp.ExposureGateway {
			continue
		}
		if self, why := g.isSelfProxy(s); self {
			notes = append(notes, fmt.Sprintf("MCP 서버 %s: 게이트웨이 자신을 가리키므로 프록시하지 않습니다 (%s)", s.Name, why))
			continue
		}
		full, err := g.o.Servers.Get(ctx, s.Name)
		if err != nil {
			return nil, fmt.Errorf("MCP 서버 %s 를 읽을 수 없습니다: %w", s.Name, err)
		}
		n := 0
		var hidChanged, hidNew int
		var hidNames []string
		for _, t := range full.Tools {
			if t.Stale || t.Name == "" {
				continue
			}
			if !t.Exposable(full.Enforce) {
				// Tool definition pinning (forge mcp pin --enforce): only definitions
				// whose hash equals the approved one are exposed; a changed or
				// never-approved tool is hidden (fails closed) and forge_list says why.
				if t.Approval == mcp.ApprovalChanged {
					hidChanged++
				} else {
					hidNew++
				}
				if len(hidNames) < 5 {
					hidNames = append(hidNames, string(cleanName(t.Name)))
				}
				continue
			}
			n++
			add(rankProxy, "mcp:"+s.Name+":"+t.Name, proxyBase(s.Name, t.Name), &entry{
				Kind: kindProxy, Server: s.Name, Upstream: t.Name, Desc: listText(t.Description, maxDescBytes), Schema: schemaOf(t.InputSchema), Raw: t.InputSchema})
		}
		if hidChanged+hidNew > 0 {
			sn := string(cleanName(s.Name))
			notes = append(notes, fmt.Sprintf("MCP 서버 %s: 도구 정의 고정(enforce)으로 도구 %d개를 숨겼습니다 (승인된 정의와 다름 %d, 미승인 %d): %s. 사용자가 설명을 확인한 뒤 `forge mcp approve %s` 로 승인해야 노출됩니다",
				sn, hidChanged+hidNew, hidChanged, hidNew, strings.Join(hidNames, ", "), sn))
		}
		if n == 0 && hidChanged+hidNew == 0 {
			notes = append(notes, fmt.Sprintf("MCP 서버 %s: 도구 캐시가 없어 노출하지 않습니다 (forge mcp refresh %s)", s.Name, s.Name))
		}
	}

	names, warns := assignNames(cands, MaxExposedLen(g.o.Name))
	cat := &catalog{byName: map[string]*entry{}, warnings: append(notes, warns...)}
	for id, n := range names {
		e := ents[id]
		e.Name = n
		cat.entries = append(cat.entries, e)
		cat.byName[n] = e
	}
	sort.Slice(cat.entries, func(i, j int) bool { return cat.entries[i].Name < cat.entries[j].Name })
	return cat, nil
}

func (g *Gateway) listTools(ctx context.Context, ss *sdk.ServerSession) (sdk.Result, error) {
	client, _ := g.clientOf(ss)
	cat, err := g.build(ctx, client)
	if err != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: err.Error()}
	}
	res := &sdk.ListToolsResult{Tools: []*sdk.Tool{}}
	for _, e := range cat.entries {
		res.Tools = append(res.Tools, &sdk.Tool{Name: e.Name, Description: e.Desc, InputSchema: e.Schema})
	}
	for _, w := range cat.warnings {
		g.log.Debug("catalog warning", "warning", w)
	}
	g.log.Debug("tools/list", "client", client, "tools", len(res.Tools))
	return res, nil
}

// isSelfProxy detects a registered server that would start this gateway again
// (name equals the gateway's, or the command is `forge mcp serve`).
func (g *Gateway) isSelfProxy(s *mcp.Info) (bool, string) {
	for _, n := range []string{g.o.Name, DefaultName} {
		if NormalizeServer(s.Name) == NormalizeServer(n) {
			return true, "이름이 게이트웨이 서버명과 같음"
		}
	}
	if s.Transport != mcp.TransportStdio {
		return false, ""
	}
	toks := append([]string{s.Command}, s.Args...)
	var all []string
	for _, a := range toks {
		all = append(all, strings.Fields(a)...)
	}
	has := func(w string) bool {
		for _, t := range all {
			if t == w {
				return true
			}
		}
		return false
	}
	forge := false
	for _, t := range all {
		if b := filepath.Base(t); b == "forge" || b == "forge.exe" {
			forge = true
		}
	}
	if exe, err := os.Executable(); err == nil {
		if r1, e1 := filepath.EvalSymlinks(s.Command); e1 == nil {
			if r2, e2 := filepath.EvalSymlinks(exe); e2 == nil && r1 == r2 {
				forge = true
			}
		}
	}
	if forge && has("mcp") && has("serve") {
		return true, "command 가 forge mcp serve"
	}
	return false, ""
}

// effectiveConfirm is the confirm mode the gateway enforces for a custom tool.
// TODO(TASK-0018): force "ask" for risk=code-exec tools (sqlite3, npm exec, ...)
// even when the stored policy says "none".
func effectiveConfirm(d *tool.Definition) string {
	if d.ForcedAsk() {
		// code-exec program (risk=code-exec) with confirm=none and no explicit
		// acknowledgement (--yes-code-exec): ask anyway, legacy rows included.
		return tool.ConfirmAsk
	}
	if d.Policy.Confirm == "" {
		return tool.ConfirmNone
	}
	return d.Policy.Confirm
}
