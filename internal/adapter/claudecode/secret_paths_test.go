package claudecode

import (
	"testing"

	"github.com/as950118/forge/internal/adapter"
	"github.com/as950118/forge/internal/mcp"
)

// A server whose literal env value is a commit id under a commit-named key is
// registered by the registry (TASK-0026), so the adapter must render it too: the
// render check has to use the same key-aware rule, not the value-only rule.
func TestRenderAcceptsLabelledHashLiteral(t *testing.T) {
	e := newEnv(t)
	const h40 = "0123456789abcdef0123456789abcdef01234567"
	e.addServer(mcp.Spec{Name: "pinned", Transport: "stdio", Command: "node", Args: []string{"server.js"},
		Values: []mcp.Value{{Slot: mcp.SlotEnv, Key: "GIT_COMMIT", RefType: mcp.RefLiteral, Literal: h40, Display: h40}}})
	p := e.mustPlan(adapter.ScopeProject)
	wantAction(t, p, adapter.KindMCP, "pinned", adapter.ActionAdd)
}
