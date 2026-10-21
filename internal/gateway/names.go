// Package gateway implements `forge mcp serve`: Forge as a stdio MCP server that
// exposes registered custom tools, a proxy for registered MCP servers and the
// forge_skill meta tool through one endpoint (TASK-0007, PLAN §3.1).
//
// Naming rules (see docs/gateway.md for the normative text):
//
//	proxy tool   <server with '-' -> '_'>__<tool>      e.g. my_github__create_issue
//	custom tool  forge_<name>                          e.g. forge_deploy_check
//	meta tools   forge_skill, forge_list
//
// The client shows each tool as mcp__<gateway name>__<tool> (Claude Code) and the
// Anthropic API limits a tool name to 64 characters, so the exposed name is
// shortened deterministically when it would not fit.
package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Name building blocks.
const (
	ProxySep     = "__"
	CustomPrefix = "forge_"
	MetaSkill    = "forge_skill"
	MetaList     = "forge_list"

	// DefaultName is the name the gateway is expected to be registered under in
	// a client ("forge": mcp__forge__<tool> in Claude Code).
	DefaultName = "forge"

	// MaxFullToolName is the longest fully qualified tool name that clients can
	// send to the model (Anthropic API tool name: ^[a-zA-Z0-9_-]{1,64}$).
	MaxFullToolName = 64
	minToolBudget   = 16
)

// NormalizeServer is the server-name rule used in proxy tool names: '-' becomes
// '_' (Claude Code does the same inside mcp__<server>__<tool>). Server names are
// restricted to [A-Za-z0-9_-] by the registry, so nothing else needs mapping, and
// "my-srv" and "my_srv" intentionally collapse onto one prefix (see collisions).
func NormalizeServer(s string) string { return strings.ReplaceAll(s, "-", "_") }

// sanitizeTool maps every character outside [A-Za-z0-9_-] of an upstream tool
// name to '_' (MCP allows '.', clients do not accept it in tool names).
func sanitizeTool(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// MaxExposedLen is the longest tool name (as exposed by this gateway) that still
// fits "mcp__<gateway name>__<tool>" into MaxFullToolName characters.
func MaxExposedLen(gatewayName string) int {
	if gatewayName == "" {
		gatewayName = DefaultName
	}
	n := MaxFullToolName - len("mcp__"+NormalizeServer(gatewayName)+"__")
	if n < minToolBudget {
		n = minToolBudget
	}
	return n
}

func shortHash(s string, n int) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:n]
}

// fitName shortens base to max characters: first max-9 characters + "_" + 8 hex
// characters of the hash of the identity. Names that fit are returned unchanged.
func fitName(base, identity string, max int) string {
	if len(base) <= max {
		return base
	}
	keep := max - 9
	if keep < 1 {
		keep = 1
	}
	return base[:keep] + "_" + shortHash(identity, 8)
}

func withSuffix(n, suffix string, max int) string {
	if len(n)+len(suffix) > max {
		n = n[:max-len(suffix)]
	}
	return n + suffix
}

// candidate is one tool awaiting an exposed name.
type candidate struct {
	rank     int    // 0 meta, 1 custom, 2 proxy: lower ranks win collisions
	identity string // unique, stable: "meta:forge_skill", "tool:<name>", "mcp:<server>:<tool>"
	base     string // normalized name before shortening
}

const (
	rankMeta = iota
	rankCustom
	rankProxy
)

func proxyBase(server, tool string) string {
	return NormalizeServer(server) + ProxySep + sanitizeTool(tool)
}
func customBase(name string) string { return CustomPrefix + sanitizeTool(name) }

// assignNames gives every candidate a unique exposed name, deterministically
// (independent of discovery order): candidates are processed in (rank, identity)
// order; the first keeps its name, later ones get "_<6 hex of identity hash>"
// appended (shortened to fit). Every renaming is reported as a warning.
// Comparison is case-insensitive because some clients fold tool names.
func assignNames(cands []candidate, max int) (map[string]string, []string) {
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].rank != cands[j].rank {
			return cands[i].rank < cands[j].rank
		}
		return cands[i].identity < cands[j].identity
	})
	out := make(map[string]string, len(cands))
	owner := map[string]string{} // lower-case name -> identity
	var warns []string
	for _, c := range cands {
		n := fitName(c.base, c.identity, max)
		if prev, taken := owner[strings.ToLower(n)]; taken {
			suffix := "_" + shortHash(c.identity, 6)
			alt := withSuffix(n, suffix, max)
			for i := 2; ; i++ {
				if _, t := owner[strings.ToLower(alt)]; !t {
					break
				}
				alt = withSuffix(n, suffix+strconv.Itoa(i), max)
			}
			warns = append(warns, fmt.Sprintf("이름 충돌: %s 와 %s 가 모두 %q 로 노출되어, 뒤의 것을 %q 로 노출합니다", prev, c.identity, n, alt))
			n = alt
		}
		owner[strings.ToLower(n)] = c.identity
		out[c.identity] = n
	}
	return out, warns
}
