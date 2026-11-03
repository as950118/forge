package gateway

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/as950118/forge/internal/tool"
)

// The approval message keeps file paths readable (macOS TMPDIR) but still masks
// disguised secrets and URL tokens (TASK-0026).
func TestConfirmMessageKeepsPathsAndMasksDisguisedSecrets(t *testing.T) {
	e := newEnv(t)
	e.addTool("pq", tool.ConfirmAsk, "printf", "%s", "{{v}}")
	var msg string
	cs := e.connect(e.gw(), "claude-code", func(_ context.Context, r *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		msg = r.Params.Message
		return &sdk.ElicitResult{Action: "decline"}, nil
	})
	const mac = "/var/folders/gr/q4n6pgs90gx_jny4zh7p58dw0000gn/T/forge-demo.abc123/src/variant"
	invoke(t, cs, "forge_pq", map[string]any{"v": mac})
	if !strings.Contains(msg, mac) {
		t.Errorf("path masked in the approval message: %q", msg)
	}
	for _, v := range []string{
		"/tmp/sk-Zq8Lm2Vb7Nc4Rt6Yh1Jd3Kf5Sg0Wa9Xe2Pu4Io6Qw",
		"https://h.example/v1/q9Zx7Lm2Vb8Nc4Rt6Yh1Jd3Kf5Sg0Wa/mcp",
	} {
		msg = ""
		invoke(t, cs, "forge_pq", map[string]any{"v": v})
		if msg == "" || strings.Contains(msg, "Zq8Lm2") || strings.Contains(msg, "q9Zx7Lm2") {
			t.Errorf("secret shown in the approval message: %q", msg)
		}
	}
}
