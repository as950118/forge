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
// to send (claude-code, opencode) but were not captured from a live session in
// this task (docs/gateway.md, "unverified"); --client overrides detection.
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
