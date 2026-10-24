package adapter

import (
	"strings"
	"testing"
)

// Preview/diff display (MaskJSONDisplay + MaskLine) keeps paths and labelled
// identifiers readable and masks disguised secrets (TASK-0026).
func TestDisplayMaskKeepsPathsMasksDisguised(t *testing.T) {
	const mac = "/var/folders/gr/q4n6pgs90gx_jny4zh7p58dw0000gn/T/forge-demo.abc123/src/variant"
	const h40 = "0123456789abcdef0123456789abcdef01234567"
	src := "{\n  \"mcpServers\": {\n    \"x\": {\"command\": \"python3\", \"args\": [\"s.py\", \"" + mac + "\", \"--ref\", \"" + h40 + "\", \"/tmp/sk-Zq8Lm2Vb7Nc4Rt6Yh1Jd3Kf5Sg0Wa9Xe2Pu4Io6Qw\"]}\n  }\n}\n"
	out := MaskLine(MaskJSONDisplay(src)) // key-based pass, then the per-line value rules
	if !strings.Contains(out, mac) || !strings.Contains(out, h40) {
		t.Errorf("readable values masked:\n%s", out)
	}
	if strings.Contains(out, "Zq8Lm2") {
		t.Errorf("disguised secret shown:\n%s", out)
	}
	if got := MaskLine("cwd " + mac); !strings.Contains(got, mac) {
		t.Errorf("MaskLine masked a path: %q", got)
	}
}

// Display masking of JSON lines uses the key (isValue) and the previous flag
// element as context for a 40/64-hex string; without a label it stays masked.
func TestMaskerHexContext(t *testing.T) {
	const h40 = "0123456789abcdef0123456789abcdef01234567"
	run := func(lines ...string) string {
		m := newMasker(true)
		var out []string
		for _, l := range lines {
			out = append(out, m.line(l))
		}
		return strings.Join(out, "\n")
	}
	keep := map[string][]string{
		"key commit":          {`  "commit": "` + h40 + `",`},
		"key image digest":    {`  "image_digest": "` + h40 + `"`},
		"flag on same line":   {`  "args": ["--ref", "` + h40 + `"]`},
		"flag on prev line":   {`    "--rev",`, `    "` + h40 + `"`},
		"flag then ref= line": {`  "args": ["--sha256", "` + h40 + `", "x"]`},
	}
	for name, lines := range keep {
		if got := run(lines...); !strings.Contains(got, h40) {
			t.Errorf("%s: labelled identifier masked: %s", name, got)
		}
	}
	mask := map[string][]string{
		"unrelated key":                  {`  "note": "` + h40 + `",`},
		"secret-ish key":                 {`  "commit_token": "` + h40 + `"`},
		"plain element":                  {`  "args": ["x", "` + h40 + `"]`},
		"flag not hash":                  {`  "args": ["--name", "` + h40 + `"]`},
		"label two away":                 {`  "args": ["--ref", "x", "` + h40 + `"]`},
		"stale flag, new line":           {`    "--rev",`, `    "x",`, `    "` + h40 + `"`},
		"flag, literal-free line, value": {`    "--rev",`, `  ],`, `    "` + h40 + `"`},
		"bare":                           {`  "` + h40 + `",`},
	}
	for name, lines := range mask {
		if got := run(lines...); strings.Contains(got, h40) {
			t.Errorf("%s: unlabelled hex shown: %s", name, got)
		}
	}
}
