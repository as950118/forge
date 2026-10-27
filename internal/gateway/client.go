package gateway

import "strings"

// Client ids (schema clients.id).
const (
	ClientClaudeCode = "claude-code"
	ClientOpenCode   = "opencode"
)

// IdentifyClient maps MCP initialize.clientInfo.name onto a Forge client id.
// Only exact/prefix matches of known names count; anything else is "" (unknown)
// and gets the conservative exposure. The names are what the clients are known
// to send. claude-code was confirmed in a live Claude Code 2.1.311 session
// (TASK-0029, docs/clients/claude-code.md); the opencode name is still unconfirmed
// from a live session (docs/gateway.md). --client overrides detection.
func IdentifyClient(clientInfoName string) string {
	n := strings.ToLower(strings.TrimSpace(clientInfoName))
	switch {
	case n == "claude-code" || strings.HasPrefix(n, "claude-code/") || strings.HasPrefix(n, "claude-code-"):
		return ClientClaudeCode
	case n == "opencode" || strings.HasPrefix(n, "opencode/") || strings.HasPrefix(n, "opencode-"):
		return ClientOpenCode
	}
	return ""
}

// KnownClient reports whether id is a client Forge has an adapter for.
func KnownClient(id string) bool { return id == ClientClaudeCode || id == ClientOpenCode }
