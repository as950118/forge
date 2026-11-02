package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heonjinjeong/forge/internal/ledger"
	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/tool"
)

// Refusal texts (Korean). The unsupported text is the guidance required by TASK-0007.
const (
	msgNoElicit         = "사용자 확인이 필요한 도구입니다 (confirm=ask). 이 클라이언트는 확인 요청(elicitation)을 지원하지 않아 실행하지 않았습니다: forge tool test 로 직접 실행하거나 정책을 변경하세요 (forge tool add --replace ... --confirm none)"
	msgNoElicitCodeExec = "사용자 확인이 필요한 도구입니다 (코드 실행형 프로그램, risk=code-exec 는 confirm=none 이어도 확인을 강제합니다). 이 클라이언트는 확인 요청(elicitation)을 지원하지 않아 실행하지 않았습니다: forge tool test 로 직접 실행하거나, 위험을 이해한 경우에만 forge tool add --replace ... --confirm none --yes-code-exec 로 다시 등록하세요"
	msgDeclined         = "사용자가 실행을 거부했습니다. 도구를 실행하지 않았습니다"
	// msgCancelled is the answer "cancel": the client dismissed the request without a
	// decision. Claude Code answers cancel by itself when there is no UI to ask (claude -p,
	// TASK-0029 measurement), so the text tells the user how to approve or change the policy.
	msgCancelled = "사용자 확인이 취소되어 도구를 실행하지 않았습니다 (확인 창이 없는 환경 - 예: Claude Code 의 claude -p - 에서는 요청이 자동으로 취소됩니다). 사용자가 승인하려면: Claude Code 는 Elicitation hook 으로 승인하거나(docs/clients/claude-code.md §6), 신뢰하는 도구라면 forge tool add --replace ... --confirm none 으로 정책을 바꾸세요 (코드 실행형 프로그램은 --yes-code-exec 도 필요)"
	msgNoAnswer  = "사용자 확인을 받지 못해 도구를 실행하지 않았습니다 (확인 요청 실패 또는 시간 초과)"
)

var approveSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"approve": map[string]any{
			"type": "boolean", "title": "실행 승인",
			"description": "체크하면 이 도구를 위 인자로 한 번 실행합니다", "default": false,
		},
	},
	"required": []string{"approve"},
}

// confirm enforces confirm=ask: the user must approve through MCP elicitation.
// It returns ("", "") when approved, otherwise the refusal text and the ledger
// error class. Safe default: anything but an explicit approve=true (decline,
// cancel, timeout, error, client without elicitation) means the tool is not run.
func (g *Gateway) confirm(ctx context.Context, ss *sdk.ServerSession, def *tool.Definition, args map[string]any) (string, string) {
	p := ss.InitializeParams()
	if p == nil || p.Capabilities == nil || p.Capabilities.Elicitation == nil {
		if def.CodeExecBinary() != "" {
			return msgNoElicitCodeExec, classNoElicit
		}
		return msgNoElicit, classNoElicit
	}
	// DryRun validates the arguments (invalid ones are reported without bothering
	// the user) and renders the masked command/request the user is approving.
	plan, err := tool.Run(ctx, def, args, tool.RunOptions{ProjectDir: g.o.Project, DryRun: true})
	if err != nil {
		return plan.Error, classInvalidArgs
	}
	ectx, cancel := context.WithTimeout(ctx, g.o.ConfirmWait)
	defer cancel()
	res, err := ss.Elicit(ectx, &sdk.ElicitParams{Message: confirmMessage(def, plan), RequestedSchema: approveSchema})
	if err != nil {
		g.log.Debug("elicit failed", "tool", def.Name, "error", err)
		return msgNoAnswer, classConfirm
	}
	if res.Action == "cancel" {
		return msgCancelled, classConfirm
	}
	if res.Action != "accept" {
		return msgDeclined, classConfirm
	}
	if v, ok := res.Content["approve"].(bool); !ok || !v {
		return msgDeclined, classConfirm
	}
	return "", ""
}

// Approval message layout. Every line starts with one static label; every
// dynamic value (tool name, description, argv elements, URL, header, argument
// values, script input, paths) is masked, escaped by escapeText (no raw control,
// bidi, zero-width or separator character survives) and shown between ASCII
// double quotes on that same line, so a value can neither start a new line nor
// fake a label such as "승인됨:".
//
//	Forge 도구 "name" (command) 를 실행할까요?
//	설명: "..."
//	실행: "cmd" "arg1" "arg2"          요청: GET "https://..."
//	헤더: "Name"="value", ...           인자: q="a", token="***"
//	입력: "{...}"                       인터프리터: "/bin/sh" (스크립트 해시 "..")
//	작업 디렉터리: "/path"
const (
	maxDescShown   = 300
	maxArgvShown   = 800
	maxURLShown    = 400
	maxArgShown    = 200
	maxArgsShown   = 800
	maxInputShown  = 800
	maxHeadersSize = 600
)

// shown masks secrets in a dynamic value and quotes the escaped result.
func shown(s string, max int) string { return quoteValue(ledger.MaskText(s), max) }

func confirmMessage(def *tool.Definition, plan *tool.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Forge 도구 %s (%s) 를 실행할까요?\n", quoteValue(def.Name, 120), escapeText(def.Kind, 20))
	if d := strings.TrimSpace(def.Description); d != "" {
		b.WriteString("설명: " + quoteValue(d, maxDescShown) + "\n")
	}
	if bin := def.CodeExecBinary(); bin != "" {
		b.WriteString("위험: " + quoteValue("코드 실행형 프로그램 "+bin+" (risk=code-exec): 인자가 코드·명령으로 해석될 수 있습니다", 200) + "\n")
	}
	if pl := plan.DryRun; pl != nil {
		switch {
		case len(pl.Argv) > 0:
			parts := make([]string, len(pl.Argv))
			for i, a := range pl.Argv {
				parts[i] = shown(a, maxArgvShown)
			}
			b.WriteString("실행: " + joinCapped(parts, " ", maxArgvShown) + "\n")
		case pl.URL != "":
			b.WriteString("요청: " + escapeText(pl.Method, 16) + " " + shown(pl.URL, maxURLShown) + "\n")
		}
		if len(pl.Headers) > 0 {
			var hs []string
			for k, v := range pl.Headers {
				hs = append(hs, quoteValue(k, 60)+"="+shown(headerValue(k, v), maxArgShown))
			}
			sort.Strings(hs)
			b.WriteString("헤더: " + joinCapped(hs, ", ", maxHeadersSize) + "\n")
		}
		if def.Shell {
			// --shell tools receive arguments as FORGE_ARG_<name> environment
			// data only, so show them next to the script.
			var kv []string
			for k, v := range pl.Env {
				if n, ok := strings.CutPrefix(k, "FORGE_ARG_"); ok {
					if mcp.LooksSecret(n) { // key-name based: any length
						v = "***"
					}
					kv = append(kv, escapeText(n, 64)+"="+shown(v, maxArgShown))
				}
			}
			sort.Strings(kv)
			if len(kv) > 0 {
				b.WriteString("인자: " + joinCapped(kv, ", ", maxArgsShown) + "\n")
			}
		}
		if def.Kind == tool.KindScript && pl.Body != "" {
			b.WriteString("입력: " + shown(maskedInput(pl.Body), maxInputShown) + "\n")
		}
		if pl.Interpreter != "" {
			b.WriteString("인터프리터: " + quoteValue(pl.Interpreter, 200) + " (스크립트 해시 " + quoteValue(pl.ScriptHash, 12) + ")\n")
		}
		if pl.Cwd != "" {
			b.WriteString("작업 디렉터리: " + quoteValue(pl.Cwd, 300) + "\n")
		}
	}
	return b.String()
}

// joinCapped joins already quoted values and stops at a value boundary once the
// line fragment would pass max bytes (the first value is always kept), so a
// quote is never left open.
func joinCapped(parts []string, sep string, max int) string {
	var b strings.Builder
	for i, p := range parts {
		if i > 0 && b.Len()+len(sep)+len(p) > max {
			b.WriteString(sep + truncMarker)
			break
		}
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(p)
	}
	return b.String()
}

// headerValue hides the value of a header whose name implies a credential.
func headerValue(name, v string) string {
	if mcp.LooksSecret(name) {
		return "***"
	}
	return v
}

// maskedInput prepares the stdin JSON of a script tool for display: valid JSON
// is masked by key name (token/password/... keys are hidden whatever their value
// length), anything else by the text rules.
func maskedInput(body string) string {
	body = strings.TrimPrefix(body, "stdin(JSON 인자): ")
	if json.Valid([]byte(body)) {
		if m, err := ledger.MaskJSON([]byte(body)); err == nil {
			return m
		}
	}
	return body
}
