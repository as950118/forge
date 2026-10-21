// Package ledger records skill / tool / MCP invocations (PLAN §3.1, ADR-F0001).
//
// Metadata only by default. Arguments/results are stored only when
// settings.record_payload=1, and only after masking (see maskPayload); there is
// no code path that writes an unmasked payload. Error text is never stored,
// only a short classification.
package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Kinds, sources and invocation values (match the ledger CHECK constraints).
const (
	KindSkill = "skill"
	KindTool  = "tool"
	KindMCP   = "mcp"

	SourceHook       = "hook"
	SourceGateway    = "gateway"
	SourceTranscript = "transcript"

	InvocationModel = "model"
	InvocationUser  = "user"
)

const (
	maxNameLen = 128
	maxIDLen   = 256
	maxMetaLen = 64
	maxPathLen = 4096
)

var (
	clientRe     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	errorClassRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// Event is one invocation. Pointers distinguish "unknown" (nil) from false/0.
type Event struct {
	TS            time.Time // zero = now
	Kind          string
	Name          string
	MCPServer     string // kind=mcp only
	Client        string // e.g. claude-code; unknown clients are stored as NULL
	ClientVersion string
	Project       string // normalized absolute path (see NormalizeProject)
	SessionID     string
	AgentID       string
	ToolUseID     string
	Source        string
	Invocation    string // skill only: model|user
	OK            *bool  // nil = result not observed (PreToolUse only)
	ErrorClass    string // classification only, never message text
	LatencyMS     *int64
	Found         *bool // nil = derive from the registry
	SkillVersion  string

	// Input/Output are stored only when settings.record_payload=1 and always masked.
	Input  json.RawMessage
	Output json.RawMessage
}

// Recorder is what hook adapters and the gateway call.
type Recorder interface {
	// Record stores ev and returns its ledger id. Re-sending the same
	// (client, session_id, tool_use_id, source) updates the existing row.
	Record(ctx context.Context, ev Event) (int64, error)
}

// Bool and Int64 build the optional pointers of Event.
func Bool(b bool) *bool    { return &b }
func Int64(n int64) *int64 { return &n }

func okStr(s string, max int, what string) error {
	if len(s) > max {
		return fmt.Errorf("%s 가 너무 깁니다 (최대 %d 바이트)", what, max)
	}
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return fmt.Errorf("%s 에 유효하지 않은 문자가 있습니다", what)
	}
	return nil
}

// Validate checks the event against the schema constraints and size limits.
func (e *Event) Validate() error {
	switch e.Kind {
	case KindSkill, KindTool, KindMCP:
	default:
		return fmt.Errorf("kind 는 skill|tool|mcp 여야 합니다: %q", cut(e.Kind))
	}
	switch e.Source {
	case SourceHook, SourceGateway, SourceTranscript:
	default:
		return fmt.Errorf("source 는 hook|gateway|transcript 여야 합니다: %q", cut(e.Source))
	}
	if e.Name == "" {
		return fmt.Errorf("name 이 비어 있습니다")
	}
	if err := okStr(e.Name, maxNameLen, "name"); err != nil {
		return err
	}
	for _, r := range e.Name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("name 에 제어 문자가 있습니다")
		}
	}
	if e.Kind == KindMCP && e.MCPServer == "" {
		return fmt.Errorf("kind=mcp 는 mcp_server 가 필요합니다")
	}
	if e.Kind != KindMCP && e.MCPServer != "" {
		return fmt.Errorf("mcp_server 는 kind=mcp 에서만 쓸 수 있습니다")
	}
	if err := okStr(e.MCPServer, maxNameLen, "mcp_server"); err != nil {
		return err
	}
	switch e.Invocation {
	case "":
	case InvocationModel, InvocationUser:
		if e.Kind != KindSkill {
			return fmt.Errorf("invocation 은 kind=skill 에서만 쓸 수 있습니다")
		}
	default:
		return fmt.Errorf("invocation 은 model|user 여야 합니다: %q", cut(e.Invocation))
	}
	if e.Client != "" && !clientRe.MatchString(e.Client) {
		return fmt.Errorf("client 형식이 올바르지 않습니다: %q", cut(e.Client))
	}
	for _, f := range []struct {
		v, what string
		max     int
	}{
		{e.ClientVersion, "client_version", maxMetaLen}, {e.SkillVersion, "skill_version", maxMetaLen},
		{e.Project, "project", maxPathLen}, {e.SessionID, "session_id", maxIDLen},
		{e.AgentID, "agent_id", maxIDLen}, {e.ToolUseID, "tool_use_id", maxIDLen},
	} {
		if err := okStr(f.v, f.max, f.what); err != nil {
			return err
		}
	}
	if e.ErrorClass != "" && !errorClassRe.MatchString(e.ErrorClass) {
		return fmt.Errorf("error_class 는 짧은 분류 코드(소문자/숫자/_)여야 합니다 (오류 원문 금지)")
	}
	if e.LatencyMS != nil && *e.LatencyMS < 0 {
		return fmt.Errorf("latency_ms 는 0 이상이어야 합니다")
	}
	return nil
}

func cut(s string) string {
	if len(s) > 24 {
		return cutUTF8(s, 24) + "..."
	}
	return s
}
