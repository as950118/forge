package ledger

import (
	"strings"
	"testing"
)

// TASK-0020 AC-2: quoted values with escaped quotes, JSON text inside a string, and
// labels spelled with look-alike characters. Fake credentials only.
func TestMaskInlineTask20Limits(t *testing.T) {
	cases := []struct {
		name, in string
		hidden   []string
		kept     []string
	}{
		{"plain quoted", `api_key="fakeval1"`, []string{"fakeval1"}, nil},
		{"escaped quote inside value", `api_key="fake\"tail1" after`, []string{"fake", "tail1"}, []string{"after"}},
		{"backslash-escaped quotes", `api_key=\"fakeval2\" after`, []string{"fakeval2"}, []string{"after"}},
		{"upper-case env with escaped quotes", `bash -c "export API_KEY=\"fakeval3 x\"" run`, []string{"fakeval3"}, []string{"run"}},
		{"json text in a string", `{\"api_key\":\"fakeval4\",\"n\":1}`, []string{"fakeval4"}, []string{`\"n\":1`}},
		{"quoted key, plain quotes", `{"password": "fakeval5 more", "n": 1}`, []string{"fakeval5", "more"}, []string{`"n": 1`}},
		{"single quoted key and value", `{'token': 'fakeval6'}`, []string{"fakeval6"}, nil},
		{"fullwidth label", "\uff54oken=fakeval7 after", []string{"fakeval7"}, []string{"after"}},
		{"fullwidth upper label", "\uff21PI_KEY=fakeval8 after", []string{"fakeval8", "after"}, nil},
		{"fullwidth label quoted value", "\uff54oken=\"fakeval9 x\" after", []string{"fakeval9", "x\""}, []string{"after"}},
		{"cyrillic a in password", "p\u0430ssword: fakeval10 after", []string{"fakeval10"}, []string{"after"}},
		{"zero-width inside label", "to\u200bken=fakeval11 after", []string{"fakeval11"}, []string{"after"}},
		{"combining mark in label", "pa\u0301ssword=fakeval12 after", []string{"fakeval12"}, []string{"after"}},
		{"fullwidth flag", "--\uff54oken fakeval13 after", []string{"fakeval13"}, []string{"after"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for fn, got := range map[string]string{"MaskInline": MaskInline(c.in), "MaskText": MaskText(c.in), "maskString(persist)": maskString(c.in, true)} {
				for _, h := range c.hidden {
					if strings.Contains(got, h) {
						t.Errorf("%s leaked %q: %q", fn, h, got)
					}
				}
				if fn == "MaskInline" {
					for _, k := range c.kept {
						if !strings.Contains(got, k) {
							t.Errorf("%s lost %q: %q", fn, k, got)
						}
					}
				}
			}
		})
	}
}

// Text that merely contains foldable characters is not rewritten when nothing is masked,
// and the non-secret part of a masked text keeps its original characters.
func TestMaskInlineKeepsOriginalCharacters(t *testing.T) {
	in := "\uff08설명\uff09 \u0440\u0443\u0441\u0441\u043a\u0438\u0439 text"
	if got := MaskInline(in); got != in {
		t.Errorf("unmasked text changed: %q", got)
	}
	got := MaskInline("\u0440\u0443\u0441\u0441\u043a\u0438\u0439 \uff54oken=fakeval14 \uff08설명\uff09")
	if !strings.HasPrefix(got, "\u0440\u0443\u0441\u0441\u043a\u0438\u0439 \uff54oken=***") || !strings.HasSuffix(got, "\uff08설명\uff09") || strings.Contains(got, "fakeval14") {
		t.Errorf("got %q", got)
	}
}
