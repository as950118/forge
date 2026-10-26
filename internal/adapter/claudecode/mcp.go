package claudecode

import (
	"regexp"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/adapter/jsonedit"
	"github.com/heonjinjeong/forge/internal/mcp"
)

// serversKey is the top-level object of .mcp.json that holds the servers.
const serversKey = "mcpServers"

// format is the Claude Code schema of .mcp.json (type always explicit, ${VAR}
// references); the re-validation and refusal rules are adapter.RenderServer's.
var format = adapter.MCPFormat{
	CwdReason: "cwd 지정 서버는 Claude Code 형식(.mcp.json)에 cwd 가 없어 직접 노출할 수 없습니다 (exposure=gateway 사용)",
	Ref:       func(name string) string { return "${" + name + "}" },
	Subst:     regexp.MustCompile(`\$\{`),
	Build: func(s *mcp.Info, kvs func(string) jsonedit.Obj) (val jsonedit.Obj, cmd []string) {
		switch s.Transport {
		case mcp.TransportStdio:
			val = jsonedit.Obj{{K: "type", V: "stdio"}, {K: "command", V: s.Command}}
			if len(s.Args) > 0 {
				val = append(val, jsonedit.KV{K: "args", V: strArray(s.Args)})
			}
			env := kvs(mcp.SlotEnv)
			if len(env) > 0 {
				val = append(val, jsonedit.KV{K: "env", V: env})
			}
			cmd = []string{"claude", "mcp", "add", "--scope", "user", "--transport", "stdio", s.Name}
			for _, kv := range env {
				cmd = append(cmd, "--env", kv.K+"="+kv.V.(string))
			}
			cmd = append(cmd, "--", s.Command)
			cmd = append(cmd, s.Args...)
		case mcp.TransportHTTP, mcp.TransportSSE:
			val = jsonedit.Obj{{K: "type", V: s.Transport}, {K: "url", V: s.URL}}
			h := kvs(mcp.SlotHeader)
			if len(h) > 0 {
				val = append(val, jsonedit.KV{K: "headers", V: h})
			}
			cmd = []string{"claude", "mcp", "add", "--scope", "user", "--transport", s.Transport, s.Name, s.URL}
			for _, kv := range h {
				cmd = append(cmd, "--header", kv.K+": "+kv.V.(string))
			}
		}
		return val, cmd
	},
}

func strArray(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func renderServer(s *mcp.Info) (adapter.MCPEntry, string, bool) {
	return adapter.RenderServer(s, format)
}

func (pl *planner) mcpTarget() adapter.MCPTarget {
	return adapter.MCPTarget{Gateway: pl.gatewayEntry(), GatewayName: GatewayServerName, Render: renderServer}
}

// gatewayEntry is the `forge mcp serve` server (PLAN §3.1).
func (pl *planner) gatewayEntry() adapter.MCPEntry {
	args := []string{"mcp", "serve", "--client", Name}
	val := jsonedit.Obj{{K: "type", V: "stdio"}, {K: "command", V: pl.Bin()}, {K: "args", V: strArray(args)}}
	cmd := append([]string{"claude", "mcp", "add", "--scope", "user", "--transport", "stdio", GatewayServerName, "--", pl.Bin()}, args...)
	return adapter.NewMCPEntry(GatewayServerName, val, cmd)
}

// planMCPFile plans <repo>/.mcp.json (project scope).
func (pl *planner) planMCPFile() error {
	if _, err := pl.PlanMCPFile(adapter.MCPFile{Path: pl.p.mcpJSON, Key: serversKey, Label: ".mcp.json"},
		pl.mcpTarget()); err != nil {
		return err
	}
	pl.Note(".mcp.json 서버는 Claude Code 에서 최초 사용 시 사용자 승인이 필요합니다 (Pending approval).")
	return nil
}

// planMCPCommands prints `claude mcp add` commands for the user scope. Nothing is
// written: the user-level MCP file of Claude Code is neither read nor modified.
func (pl *planner) planMCPCommands() {
	pl.PlanMCPCommands(pl.mcpTarget(), "claude mcp add 명령을 출력합니다 (파일을 쓰지 않음)",
		"user 스코프의 MCP 서버는 Claude Code 사용자 파일을 읽거나 쓰지 않고 위 명령만 출력합니다. 이미 등록된 이름은 add 가 실패하므로 갱신하려면 claude mcp remove --scope user <이름> 후 다시 add 하세요.")
}
