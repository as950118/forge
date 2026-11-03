package opencode

import (
	"regexp"
	"strings"

	"github.com/as950118/forge/internal/adapter"
	"github.com/as950118/forge/internal/adapter/jsonedit"
	"github.com/as950118/forge/internal/mcp"
	"github.com/as950118/forge/internal/textsafe"
)

// mcpKey is the top-level object of opencode.json that holds the servers
// (docs/clients/opencode.md §2: [doc] + [bin]).
const mcpKey = "mcp"

// OpenCode substitutes {env:NAME} and {file:path} in the configuration text
// ([bin]); a Forge-rendered value that contains such text by accident (or by
// attack) would read an environment variable or a file, so none may appear
// except the {env:NAME} references the adapter writes itself.
var substRe = regexp.MustCompile(`(?i)\{(env|file):|\$\{`)

// unsafeSubst reports whether s contains substitution syntax.
func unsafeSubst(s string) bool { return substRe.MatchString(s) }

// format is the OpenCode schema of the "mcp" object (local|remote, command as an
// array, {env:VAR} references); the re-validation and refusal rules are
// adapter.RenderServer's.
var format = adapter.MCPFormat{
	CwdReason:      "cwd 지정 서버는 OpenCode 의 cwd 표현이 문서 기준(미교차확인)이라 렌더하지 않습니다 (exposure=gateway 사용)",
	Ref:            func(name string) string { return "{env:" + name + "}" },
	Subst:          substRe,
	SubstInCommand: true,
	Build: func(s *mcp.Info, kvs func(string) jsonedit.Obj) (val jsonedit.Obj, cmd []string) {
		switch s.Transport {
		case mcp.TransportStdio:
			command := append([]string{s.Command}, s.Args...)
			val = jsonedit.Obj{{K: "type", V: "local"}, {K: "command", V: strArray(command)}}
			env := kvs(mcp.SlotEnv)
			if len(env) > 0 {
				val = append(val, jsonedit.KV{K: "environment", V: env})
			}
			val = append(val, jsonedit.KV{K: "enabled", V: true})
			cmd = []string{"opencode", "mcp", "add", s.Name, "--global"}
			for _, kv := range env {
				cmd = append(cmd, "--env", kv.K+"="+kv.V.(string))
			}
			cmd = append(cmd, "--")
			cmd = append(cmd, command...)
		case mcp.TransportHTTP, mcp.TransportSSE:
			// OpenCode has only local|remote: http and sse both become remote
			// (whether the transport is chosen automatically is [미확인]).
			val = jsonedit.Obj{{K: "type", V: "remote"}, {K: "url", V: s.URL}}
			h := kvs(mcp.SlotHeader)
			if len(h) > 0 {
				val = append(val, jsonedit.KV{K: "headers", V: h})
			}
			val = append(val, jsonedit.KV{K: "enabled", V: true})
			cmd = []string{"opencode", "mcp", "add", s.Name, "--global", "--url", s.URL}
			for _, kv := range h {
				cmd = append(cmd, "--header", kv.K+"="+kv.V.(string))
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

// gatewayEntry is the `forge mcp serve` server (PLAN §3.1).
func (pl *planner) gatewayEntry() adapter.MCPEntry {
	command := append([]string{pl.Bin()}, "mcp", "serve", "--client", Name)
	val := jsonedit.Obj{{K: "type", V: "local"}, {K: "command", V: strArray(command)}, {K: "enabled", V: true}}
	return adapter.NewMCPEntry(GatewayServerName, val, append([]string{"opencode", "mcp", "add", GatewayServerName, "--global", "--"}, command...))
}

// mcpTarget renders through the common rules and adds the OpenCode notes about
// what its schema cannot express (sse vs http, timeout).
func (pl *planner) mcpTarget() adapter.MCPTarget {
	var sse bool
	var timeouts []string
	return adapter.MCPTarget{Gateway: pl.gatewayEntry(), GatewayName: GatewayServerName,
		Render: func(s *mcp.Info) (adapter.MCPEntry, string, bool) {
			e, bad, skip := renderServer(s)
			if bad == "" {
				if s.Transport == mcp.TransportSSE {
					sse = true
				}
				if s.TimeoutMS > 0 {
					timeouts = append(timeouts, s.Name)
				}
			}
			return e, bad, skip
		},
		Rendered: func() {
			if sse {
				pl.Note("sse 서버는 type=remote 로 렌더됩니다 (OpenCode 에는 http/sse 구분 키가 없고 전송 방식 자동 선택 여부는 [미확인])")
			}
			if len(timeouts) > 0 {
				pl.Note("timeout_ms 는 OpenCode 쪽 단위가 [미확인]이라 렌더하지 않았습니다: " + textsafe.Display(strings.Join(timeouts, ", "), 200))
			}
		}}
}

// planMCPFile plans <repo>/opencode.json (project scope). Only the member
// "mcp" is edited; every other key of the file keeps its bytes.
func (pl *planner) planMCPFile() error {
	changed, err := pl.PlanMCPFile(adapter.MCPFile{Path: pl.p.configJSON, Key: mcpKey, Label: "opencode.json"}, pl.mcpTarget())
	if err != nil {
		return err
	}
	if changed {
		pl.Note("적용 후 OpenCode 에 반영하려면 직접 `opencode reload` 를 실행하세요 (Forge 는 opencode 를 실행하지 않습니다).")
	}
	return nil
}

// planMCPCommands prints `opencode mcp add --global` commands for the user scope.
// Nothing is written: the user-level OpenCode config (which may hold provider API
// keys) is neither read nor modified.
func (pl *planner) planMCPCommands() {
	pl.PlanMCPCommands(pl.mcpTarget(), "opencode mcp add 명령을 출력합니다 (파일을 쓰지 않음)",
		"user 스코프의 MCP 서버는 전역 opencode.json 을 읽거나 쓰지 않고 위 명령만 출력합니다. 명령은 실행하지 않습니다.",
		"환경변수 값은 출력하지 않고 {env:이름} 참조만 넣었습니다. 비밀 값을 --env 에 직접 쓰지 마세요. add 가 값을 그대로 기록하는지, 옵션 순서(이름을 먼저, 구분자 -- 사용)는 [미확인]입니다 (docs/clients/opencode.md).",
		"opencode 에는 mcp remove 명령이 없습니다. 이미 등록된 이름을 바꾸려면 전역 설정에서 직접 지우세요(Forge 는 그 파일을 열지 않습니다).")
}
