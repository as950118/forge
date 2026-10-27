package history

import (
	"testing"
	"time"
)

// TASK-0030 AC-4: the text list shows who invoked a skill, as a suffix of the KIND cell
// (no column is added or moved; --json/--csv already carry "invocation").
func TestListReportShowsInvocation(t *testing.T) {
	calls := []Call{
		{Kind: "skill", Name: "a", Invocation: "user"},
		{Kind: "skill", Name: "b", Invocation: "model"},
		{Kind: "tool", Name: "c"},
	}
	r := ListReport(calls, 50, false, time.UTC)
	want := []string{"skill(user)", "skill(model)", "tool"}
	if len(r.Human) != 3 {
		t.Fatalf("rows=%d", len(r.Human))
	}
	for i, w := range want {
		if r.Human[i][1] != w {
			t.Errorf("row %d KIND = %q, want %q", i, r.Human[i][1], w)
		}
	}
	if len(r.Headers) != 9 || r.Headers[1] != "KIND" {
		t.Errorf("headers changed: %v", r.Headers)
	}
}
