package ledger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Fixtures in testdata/claude-code-2.1.311 hold only the FIELD NAMES (and enum-like
// values) of hook payloads observed in real Claude Code 2.1.311 runs (TASK-0029). Provenance per file is in
// testdata/claude-code-2.1.311/README.md: UserPromptExpansion.json was hand-restored from on-screen
// output (original capture lost; field set re-confirmed by the verifier), the others are scrubbed
// copies of original captures whose originals are no longer kept;
// every free-text value is a placeholder. They pin the real shape so the ingester
// is tested against what the client actually sends, not only against hand-written maps.
const realFixtureDir = "testdata/claude-code-2.1.311"

func loadRealFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(realFixtureDir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

func keysOf(m map[string]any) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func TestRealPayloadFieldNames(t *testing.T) {
	want := map[string]string{
		"UserPromptExpansion":                      "command_args,command_name,command_source,cwd,expansion_type,hook_event_name,permission_mode,prompt,prompt_id,session_id,transcript_path",
		"PreToolUse_Skill":                         "cwd,effort,hook_event_name,permission_mode,prompt_id,session_id,tool_input,tool_name,tool_use_id,transcript_path",
		"PostToolUse_Skill":                        "cwd,duration_ms,effort,hook_event_name,permission_mode,prompt_id,session_id,tool_input,tool_name,tool_response,tool_use_id,transcript_path",
		"PreToolUse_mcp_forge_forge_greet":         "cwd,effort,hook_event_name,mcp_server,permission_mode,prompt_id,session_id,tool_input,tool_name,tool_use_id,transcript_path",
		"PostToolUse_mcp_forge_forge_greet":        "cwd,duration_ms,effort,hook_event_name,mcp_server,permission_mode,prompt_id,session_id,tool_input,tool_name,tool_response,tool_use_id,transcript_path",
		"PostToolUseFailure_mcp_forge_forge_asker": "cwd,duration_ms,effort,error,hook_event_name,is_interrupt,mcp_server,permission_mode,prompt_id,session_id,tool_input,tool_name,tool_use_id,transcript_path",
		"SessionStart":                             "cwd,hook_event_name,session_id,source,transcript_path",
		"SessionEnd":                               "cwd,hook_event_name,prompt_id,reason,session_id,transcript_path",
		"Elicitation":                              "cwd,hook_event_name,mcp_server_name,message,mode,prompt_id,requested_schema,session_id,transcript_path",
		"ElicitationResult":                        "action,cwd,hook_event_name,mcp_server_name,mode,prompt_id,session_id,transcript_path",
	}
	for name, keys := range want {
		if got := keysOf(loadRealFixture(t, name)); got != keys {
			t.Errorf("%s: fields\n got  %s\n want %s", name, got, keys)
		}
	}
}

// The real payloads are ingested as before: a Skill call and a gateway MCP tool call
// give one row each (Pre = open row, Post = result), and the failure payload maps to ok=0.
func TestRealPayloadIngest(t *testing.T) {
	rec, home := newRec(t)
	ctx := context.Background()
	for _, n := range []string{"PreToolUse_Skill", "PostToolUse_Skill", "PostToolUse_mcp_forge_forge_greet", "PostToolUseFailure_mcp_forge_forge_asker"} {
		p := loadRealFixture(t, n)
		if strings.HasPrefix(n, "PostToolUse_mcp") {
			p["tool_use_id"] = "toolu_FIXTURE_mcp"
		}
		if strings.HasPrefix(n, "PostToolUseFailure") {
			p["tool_use_id"] = "toolu_FIXTURE_fail"
		}
		if err := IngestHook(payload(p), hookOpts(home)); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	rows, err := rec.Recent(ctx, 10, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Row{}
	for _, r := range rows {
		by[r.Kind+"/"+r.Name] = r
	}
	if s := by["skill/forge-probe-skill"]; s.Invocation != "model" || s.OK == nil || !*s.OK {
		t.Errorf("skill: %+v", s)
	}
	if g := by["tool/forge_greet"]; g.OK == nil || !*g.OK {
		t.Errorf("greet: %+v", g)
	}
	if f := by["tool/forge_asker"]; f.OK == nil || *f.OK || f.ErrorClass != "failure" {
		t.Errorf("asker: %+v", f)
	}
}

// UserPromptExpansion is a known payload but not ingested yet (TASK-0028): it must be ignored
// without error output side effects.
func TestRealUserPromptExpansionIgnoredForNow(t *testing.T) {
	rec, home := newRec(t)
	_ = IngestHook(payload(loadRealFixture(t, "UserPromptExpansion")), hookOpts(home))
	if rows, _ := rec.Recent(context.Background(), 5, Filter{}); len(rows) != 0 {
		t.Fatalf("rows=%+v", rows)
	}
}
