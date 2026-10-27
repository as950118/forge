package ledger

import (
	"context"
	"strings"
	"testing"
)

// UserPromptExpansion (TASK-0028): a user-typed /name is recorded as invocation=user
// only when the name is a skill in the Forge registry. Fixture: real 2.1.311 field set.

func upsPayload(t *testing.T, over map[string]any) map[string]any {
	p := loadRealFixture(t, "UserPromptExpansion")
	for k, v := range over {
		if v == nil {
			delete(p, k)
		} else {
			p[k] = v
		}
	}
	return p
}

func seedSkill(t *testing.T, rec *SQLite, name string) {
	exec(t, rec.DB, `INSERT INTO skills(scope,name,description) VALUES ('global',?,'d')`, name)
}

func skillRows(t *testing.T, rec *SQLite) []Row {
	rows, err := rec.Recent(context.Background(), 20, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestUserPromptExpansionRegisteredSkill(t *testing.T) {
	rec, home := newRec(t)
	seedSkill(t, rec, "forge-probe-skill")
	if err := IngestHook(payload(loadRealFixture(t, "UserPromptExpansion")), hookOpts(home)); err != nil {
		t.Fatal(err)
	}
	rows := skillRows(t, rec)
	if len(rows) != 1 {
		t.Fatalf("rows=%+v", rows)
	}
	r := rows[0]
	if r.Kind != KindSkill || r.Name != "forge-probe-skill" || r.Invocation != InvocationUser || r.Source != SourceHook ||
		r.Client != ClientClaudeCode || r.SessionID != "00000000-0000-0000-0000-000000000000" || r.ToolUseID != "" ||
		r.Found == nil || !*r.Found || r.OK != nil || r.LatencyMS != nil {
		t.Fatalf("row=%+v", r)
	}
	ms, err := rec.Misses(context.Background())
	if err != nil || len(ms) != 0 {
		t.Fatalf("misses=%v err=%v", ms, err)
	}
}

func TestUserPromptExpansionUnregisteredNotRecorded(t *testing.T) {
	rec, home := newRec(t)
	seedSkill(t, rec, "other-skill")
	for _, name := range []string{"forge-probe-skill", "context", "plugin:thing", "/Other-Skill-x"} {
		err := IngestHook(payload(upsPayload(t, map[string]any{"command_name": name})), hookOpts(home))
		if err == nil || !strings.Contains(err.Error(), ErrIgnored.Error()) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if rows := skillRows(t, rec); len(rows) != 0 {
		t.Fatalf("rows=%+v", rows)
	}
	if ms, _ := rec.Misses(context.Background()); len(ms) != 0 {
		t.Fatalf("misses=%v", ms)
	}
}

// Names are normalized like Skill tool names; command_source / expansion_type values
// are not relied on (only projectSettings is confirmed).
func TestUserPromptExpansionIgnoresSourceAndType(t *testing.T) {
	rec, home := newRec(t)
	seedSkill(t, rec, "forge-probe-skill")
	over := map[string]any{"command_name": "/Forge-Probe-Skill", "command_source": "userSettings", "expansion_type": "mystery"}
	if err := IngestHook(payload(upsPayload(t, over)), hookOpts(home)); err != nil {
		t.Fatal(err)
	}
	if rows := skillRows(t, rec); len(rows) != 1 || rows[0].Name != "forge-probe-skill" {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestUserPromptExpansionLenientFields(t *testing.T) {
	rec, home := newRec(t)
	seedSkill(t, rec, "forge-probe-skill")
	bad := []map[string]any{
		{"command_name": nil},
		{"command_name": ""},
		{"command_name": "   "},
		{"command_name": 42},
		{"command_name": []any{"x"}},
		{"command_name": "a\nb"},
		{"command_name": strings.Repeat("x", 500)},
		{"session_id": 7},
	}
	for i, o := range bad {
		if err := IngestHook(payload(upsPayload(t, o)), hookOpts(home)); err == nil {
			t.Errorf("case %d: expected a diagnostic error", i)
		}
	}
	if rows := skillRows(t, rec); len(rows) != 0 {
		t.Fatalf("rows=%+v", rows)
	}
	// Missing optional fields still record (session_id, cwd, command_args, prompt).
	for _, k := range []string{"session_id", "cwd", "command_args", "prompt", "prompt_id", "command_source"} {
		o := map[string]any{k: nil}
		if err := IngestHook(payload(upsPayload(t, o)), hookOpts(home)); err != nil {
			t.Errorf("missing %s: %v", k, err)
		}
	}
	if rows := skillRows(t, rec); len(rows) != 6 {
		t.Fatalf("rows=%d", len(rows))
	}
}

// Prompt text and command_args are never stored, even with record_payload=1.
func TestUserPromptExpansionNoPromptOrArgs(t *testing.T) {
	rec, home := newRec(t)
	seedSkill(t, rec, "forge-probe-skill")
	exec(t, rec.DB, `INSERT INTO settings(key,value) VALUES ('record_payload','1') ON CONFLICT(key) DO UPDATE SET value='1'`)
	const secret = "FAKE-SECRET-sk-abcdef0123456789"
	p := upsPayload(t, map[string]any{"command_args": "--token " + secret, "prompt": "/forge-probe-skill " + secret,
		"tool_input": map[string]any{"x": secret}, "tool_response": map[string]any{"y": secret}})
	if err := IngestHook(payload(p), hookOpts(home)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := rec.DB.QueryRow(`SELECT count(*) FROM ledger_payloads`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("payload rows=%d err=%v", n, err)
	}
	var dump string
	if err := rec.DB.QueryRow(`SELECT group_concat(coalesce(name,'')||'|'||coalesce(project,'')||'|'||coalesce(session_id,'')||'|'||coalesce(skill_version,'')||'|'||coalesce(error_class,''),';') FROM ledger`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, "FAKE-SECRET") || strings.Contains(dump, "token") {
		t.Fatalf("leak: %s", dump)
	}
}

// A user /skill arrives as UserPromptExpansion only (no PreToolUse, no tool_use_id,
// [실측 2.1.311]); a later model Skill call is a separate invocation. The two never merge
// or double count one call, and a Pre/Post pair of the model call still merges.
func TestUserPromptExpansionNoDoubleCount(t *testing.T) {
	rec, home := newRec(t)
	seedSkill(t, rec, "forge-probe-skill")
	opts := hookOpts(home)
	ingest := func(p map[string]any) {
		t.Helper()
		if err := IngestHook(payload(p), opts); err != nil {
			t.Fatal(err)
		}
	}
	ingest(loadRealFixture(t, "UserPromptExpansion"))
	rows := skillRows(t, rec)
	if len(rows) != 1 || rows[0].Invocation != InvocationUser {
		t.Fatalf("user only: %+v", rows)
	}
	ingest(loadRealFixture(t, "PreToolUse_Skill"))
	ingest(loadRealFixture(t, "PostToolUse_Skill"))
	by := map[string]int{}
	for _, r := range skillRows(t, rec) {
		by[r.Invocation]++
	}
	if by[InvocationUser] != 1 || by[InvocationModel] != 1 || len(by) != 2 {
		t.Fatalf("invocations=%v", by)
	}
}

func TestUserPromptExpansionSnippetAndMapping(t *testing.T) {
	b, err := ClaudeCodeSnippet("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"UserPromptExpansion"`) {
		t.Fatalf("snippet lacks UserPromptExpansion:\n%s", b)
	}
	p, err := decodePayload([]byte(`{"hook_event_name":"UserPromptExpansion","command_name":"Demo","command_args":"x","session_id":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	ev, phase, err := MapClaudeHook(p, "")
	if err != nil || phase != phaseExpansion || ev.Name != "demo" || ev.Invocation != InvocationUser || ev.ToolUseID != "" || ev.Input != nil {
		t.Fatalf("ev=%+v phase=%d err=%v", ev, phase, err)
	}
}
