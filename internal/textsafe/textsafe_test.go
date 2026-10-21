package textsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEscapeRaw(t *testing.T) {
	in := "a\x1b[31m\u202e\u200b\u009b\x7f\n\r\t\xff\"\\z"
	got, cut := EscapeRaw(in, 1000, true)
	want := `a\x1b[31m\u202e\u200b\x9b\x7f\n\r\t\xff\"\\z`
	if got != want || cut {
		t.Errorf("got %q want %q", got, want)
	}
	if got, _ := EscapeRaw(`"\`, 100, false); got != `"\` {
		t.Errorf("quote=false: %q", got)
	}
	for _, r := range got {
		if NeedsEscape(r) {
			t.Errorf("raw %U", r)
		}
	}
}

func TestDisplayCutsAtEscapeBoundary(t *testing.T) {
	in := strings.Repeat("\u202e", 50)
	for max := 8; max < 40; max++ {
		out := Display(in, max)
		body := strings.TrimSuffix(out, "…")
		if len(out) > max || !utf8.ValidString(out) || len(body)%6 != 0 || strings.Count(body, `\u202e`)*6 != len(body) {
			t.Errorf("max %d: %q", max, out)
		}
	}
	if Display("short", 100) != "short" {
		t.Error("short value changed")
	}
}

func TestEscapeExcept(t *testing.T) {
	got := EscapeExcept("a\tb\nc\rd\x1be\u202e", "\t\r\n")
	if got != "a\tb\nc\rd\\x1be\\u202e" {
		t.Errorf("%q", got)
	}
}
