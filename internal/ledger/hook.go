package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/heonjinjeong/forge/internal/store"
)

// Claude Code hook adapter (docs/clients/claude-code.md §3, §5).
//
// Safety contract: a hook must never disturb the user's client. IngestHook
// reports problems to its caller only; the CLI wrapper turns every outcome into
// exit code 0 with empty stdout. It never creates FORGE_HOME or the database,
// reads at most MaxInput bytes, and gives up after Timeout.

const (
	ClientClaudeCode = "claude-code"

	DefaultHookTimeout = 2 * time.Second
	DefaultMaxInput    = 1 << 20 // 1 MiB

	mcpPrefix = "mcp__"
	// MCP server name under which the Forge gateway is registered in the client; its
	// tools are Forge custom tools (kind=tool), not third-party MCP servers (PLAN §3.1).
	gatewayServer = "forge"
)

// HookOptions configures IngestHook. Zero values use the defaults.
type HookOptions struct {
	Client   string        // required: claude-code
	Event    string        // pre|post fallback when the payload has no hook_event_name
	Timeout  time.Duration // hard limit for read + open + write
	MaxInput int64         // bytes read from stdin
	Home     string        // default store.Home()
}

// ErrIgnored is returned (for diagnostics) when the input is valid but not recorded.
var ErrIgnored = errors.New("기록 대상이 아닙니다")

// claudePayload lists only the confirmed hook fields we use. Prompt/response
// bodies (prompt, last_assistant_message, error, ...) are not declared, so they
// are never decoded; tool_input/tool_response stay raw bytes that are used only
// when record_payload=1 (and then masked).
type claudePayload struct {
	HookEventName string          `json:"hook_event_name"`
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	AgentID       string          `json:"agent_id"`
	ToolName      string          `json:"tool_name"`
	ToolUseID     string          `json:"tool_use_id"`
	CommandName   string          `json:"command_name"` // UserPromptExpansion only; command_args/prompt are never declared
	DurationMS    *float64        `json:"-"`            // decoded leniently by decodePayload
	ToolInput     json.RawMessage `json:"tool_input"`
	ToolResponse  json.RawMessage `json:"tool_response"`
}

const (
	phasePre = iota
	phasePost
	phaseFailure
	// phaseExpansion is a UserPromptExpansion (user-typed /name). It has no result,
	// duration or tool_use_id.
	phaseExpansion
)

// IngestHook reads one Claude Code hook payload from in and records it. The
// whole operation (including a blocked stdin or a locked database) is bounded by
// opts.Timeout; panics are recovered. The returned error is diagnostic only.
func IngestHook(in io.Reader, opts HookOptions) (err error) {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultHookTimeout
	}
	if opts.MaxInput <= 0 {
		opts.MaxInput = DefaultMaxInput
	}
	if opts.Client != ClientClaudeCode {
		return fmt.Errorf("지원하지 않는 client: %q", cut(opts.Client))
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- ingestFn(ctx, in, opts)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("시간 초과 (%s): 기록을 포기합니다", opts.Timeout)
	}
}

// ingestFn is replaced by tests to inject a panic and prove the recover path.
var ingestFn = ingest

func ingest(ctx context.Context, in io.Reader, opts HookOptions) error {
	data, err := io.ReadAll(io.LimitReader(in, opts.MaxInput+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > opts.MaxInput {
		return fmt.Errorf("입력이 %d 바이트를 넘습니다", opts.MaxInput)
	}
	p, err := decodePayload(data)
	if err != nil {
		return err
	}
	ev, phase, err := MapClaudeHook(p, opts.Event)
	if err != nil {
		return err
	}

	home := opts.Home
	if home == "" {
		if home, err = store.Home(); err != nil {
			return err
		}
	}
	// Never create FORGE_HOME or the DB from a hook.
	dbPath := store.DBPath(home)
	if st, err := os.Stat(dbPath); err != nil || st.IsDir() {
		return fmt.Errorf("Forge 가 초기화되지 않았습니다 (forge init 필요)")
	}
	conn, err := store.OpenRaw(dbPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	rec := &SQLite{DB: conn}
	if phase == phaseExpansion {
		// Record only commands that are skills in the Forge registry: built-in and
		// third-party commands are not Forge's business, and recording them would
		// create misses (ADR-F0002) for names Forge never owned. No payload is stored.
		reg, err := skillRegistered(ctx, conn, ev.Name)
		if err != nil {
			return err
		}
		if !reg {
			return fmt.Errorf("%w: Forge 에 등록된 스킬이 아닙니다 %q", ErrIgnored, cut(ev.Name))
		}
		_, err = rec.Record(ctx, ev)
		return err
	}
	if rec.PayloadEnabled(ctx) {
		ev.Input = p.ToolInput
		if phase != phasePre {
			ev.Output = p.ToolResponse
		}
	}
	_, err = rec.Record(ctx, ev)
	return err
}

// skillRegistered reports whether name is a skill known to the Forge registry
// (same lookup as Record).
func skillRegistered(ctx context.Context, q queryer, name string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM skills WHERE name = ?`, name).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// decodePayload decodes a hook payload. duration_ms is decoded on its own and
// leniently: a value that is not a finite JSON number (1e999 overflows float64, a
// string, null, an object) only leaves the duration unknown (nil -> NULL) instead
// of discarding the whole payload.
func decodePayload(data []byte) (*claudePayload, error) {
	var p claudePayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("hook JSON 이 올바르지 않습니다")
	}
	var d struct {
		DurationMS json.RawMessage `json:"duration_ms"`
	}
	if json.Unmarshal(data, &d) == nil && len(d.DurationMS) > 0 && string(d.DurationMS) != "null" {
		var f float64
		if json.Unmarshal(d.DurationMS, &f) == nil {
			p.DurationMS = &f
		}
	}
	return &p, nil
}

// MapClaudeHook converts a hook payload to an Event. Built-in tools and
// unsupported events return ErrIgnored. eventFlag ("pre"|"post") is used only when
// the payload carries no hook_event_name.
func MapClaudeHook(p *claudePayload, eventFlag string) (Event, int, error) {
	phase := -1
	switch p.HookEventName {
	case "PreToolUse":
		phase = phasePre
	case "PostToolUse":
		phase = phasePost
	case "PostToolUseFailure":
		phase = phaseFailure
	case "UserPromptExpansion":
		phase = phaseExpansion
	case "":
		switch eventFlag {
		case "pre":
			phase = phasePre
		case "post":
			phase = phasePost
		}
	}
	if phase < 0 {
		return Event{}, 0, fmt.Errorf("%w: 지원하지 않는 이벤트 %q", ErrIgnored, cut(p.HookEventName))
	}
	ev := Event{
		Client: ClientClaudeCode, Source: SourceHook,
		Project: NormalizeProject(p.Cwd), SessionID: p.SessionID, AgentID: p.AgentID, ToolUseID: p.ToolUseID,
	}
	if phase == phaseExpansion {
		// A user-typed /name (docs/clients/claude-code.md §6, [실측 2.1.311]): no PreToolUse
		// and no tool_use_id. Only the name is kept; command_args and prompt are never
		// decoded or stored. Whether the name is a Forge skill is decided at ingest time
		// (expansionRegistered); command_source/expansion_type values are not relied on.
		name := NormalizeSkillName(p.CommandName)
		if name == "" {
			return Event{}, 0, fmt.Errorf("%w: UserPromptExpansion 에 command_name 이 없습니다", ErrIgnored)
		}
		ev.ToolUseID = ""
		ev.Kind, ev.Name, ev.Invocation = KindSkill, name, InvocationUser
		if err := ev.Validate(); err != nil {
			return Event{}, 0, err
		}
		return ev, phase, nil
	}
	switch {
	case p.ToolName == "Skill":
		var ti struct {
			Skill string `json:"skill"`
		}
		_ = json.Unmarshal(p.ToolInput, &ti)
		name := NormalizeSkillName(ti.Skill)
		if name == "" {
			return Event{}, 0, fmt.Errorf("%w: Skill 호출에 skill 이름이 없습니다", ErrIgnored)
		}
		ev.Kind, ev.Name, ev.Invocation = KindSkill, name, InvocationModel
	case strings.HasPrefix(p.ToolName, mcpPrefix):
		rest := strings.TrimPrefix(p.ToolName, mcpPrefix)
		i := strings.Index(rest, "__")
		if i <= 0 || i+2 >= len(rest) {
			return Event{}, 0, fmt.Errorf("%w: mcp 도구 이름 형식을 알 수 없습니다", ErrIgnored)
		}
		server, name := rest[:i], rest[i+2:]
		if strings.EqualFold(server, gatewayServer) {
			ev.Kind, ev.Name = KindTool, name
		} else {
			ev.Kind, ev.Name, ev.MCPServer = KindMCP, name, server
		}
	default:
		return Event{}, 0, fmt.Errorf("%w: 내장 도구 %q", ErrIgnored, cut(p.ToolName))
	}
	switch phase {
	case phasePost:
		ev.OK = Bool(true)
	case phaseFailure:
		ev.OK = Bool(false)
		ev.ErrorClass = "failure"
	}
	if phase != phasePre && p.DurationMS != nil {
		ev.LatencyMS = latencyFromMillis(*p.DurationMS)
	}
	if err := ev.Validate(); err != nil {
		return Event{}, 0, err
	}
	return ev, phase, nil
}

// maxLatencyMS caps a hook-reported duration (24h). Float-to-int64 conversion of
// out-of-range values is implementation-defined (MinInt64 on amd64), which would
// violate CHECK(latency_ms >= 0), so conversion happens only after range checks.
const maxLatencyMS = 86_400_000

// latencyFromMillis converts a payload duration to whole milliseconds. NaN,
// +/-Inf (non-finite values are untrustworthy, not "very long") and negative
// values yield nil (NULL); values above maxLatencyMS are clamped.
func latencyFromMillis(ms float64) *int64 {
	if math.IsNaN(ms) || math.IsInf(ms, 0) || ms < 0 {
		return nil
	}
	if ms >= maxLatencyMS {
		return Int64(maxLatencyMS)
	}
	return Int64(int64(math.Round(ms)))
}

// ClaudeCodeSnippet returns the settings.json "hooks" fragment to paste by hand.
// bin is the forge command (default "forge"; pass an absolute path for --bin).
// It only renders text; editing settings.json is TASK-0010 (sync).
func ClaudeCodeSnippet(bin string) ([]byte, error) {
	if bin == "" {
		bin = "forge"
	}
	cmd := func(event string) string {
		return shellQuote(bin) + " hook ingest --client claude-code --event " + event
	}
	handlerFor := func(event, matcher string) []map[string]any {
		return []map[string]any{{
			"matcher": matcher,
			"hooks":   []map[string]any{{"type": "command", "command": cmd(event), "timeout": 5}},
		}}
	}
	handler := func(event string) []map[string]any { return handlerFor(event, "Skill|mcp__.*") }
	doc := map[string]any{"hooks": map[string]any{
		"PreToolUse":         handler("pre"),
		"PostToolUse":        handler("post"),
		"PostToolUseFailure": handler("post"),
		// matcher of this event is the command name; ".*" (all commands) is not confirmed
		// against a real client [미확인]. Non-skill names are dropped at ingest.
		"UserPromptExpansion": handlerFor("post", ".*"),
	}}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+:@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
