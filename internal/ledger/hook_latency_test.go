package ledger

import (
	"math"
	"testing"
)

func TestLatencyFromMillis(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want *int64 // nil = NULL
	}{
		{"round up", 123.7, Int64(124)},
		{"round down", 123.4, Int64(123)},
		{"zero", 0, Int64(0)},
		{"negative", -1, nil},
		{"NaN", math.NaN(), nil},
		// non-finite values are treated as invalid input, not as "very long".
		{"+Inf", math.Inf(1), nil},
		{"-Inf", math.Inf(-1), nil},
		{"huge", 1e300, Int64(maxLatencyMS)},
		{"exact cap", maxLatencyMS, Int64(maxLatencyMS)},
		{"just below cap", maxLatencyMS - 1, Int64(maxLatencyMS - 1)},
	}
	for _, c := range cases {
		got := latencyFromMillis(c.in)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: latencyFromMillis(%v)=%v want %v", c.name, c.in, got, c.want)
		}
		// same result through the hook mapping
		d := c.in
		ev, _, err := MapClaudeHook(&claudePayload{HookEventName: "PostToolUse", ToolName: "Skill",
			ToolInput: []byte(`{"skill":"x"}`), DurationMS: &d}, "")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if (ev.LatencyMS == nil) != (c.want == nil) || (ev.LatencyMS != nil && *ev.LatencyMS != *c.want) {
			t.Errorf("%s: mapped latency=%v want %v", c.name, ev.LatencyMS, c.want)
		}
	}
}
