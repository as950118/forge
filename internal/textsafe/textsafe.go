// Package textsafe makes untrusted text (names, paths, ids read from the
// ledger, proxied servers or the client) safe to print to a terminal or to
// embed in a protocol message: control, bidirectional and zero-width characters
// become visible escapes. It is shared by the gateway and `forge history`.
package textsafe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// NeedsEscape reports whether r must not reach a terminal as-is: C0/C1
// controls (newline, CR, ESC, DEL, CSI), bidi controls, zero-width and other
// format characters, line/paragraph separators, private-use and surrogate code
// points, non-ASCII spaces, invisible fillers, noncharacters and variation
// selectors.
func NeedsEscape(r rune) bool {
	switch {
	case r < 0x20 || r == 0x7f:
		return true
	case unicode.IsControl(r): // C1
		return true
	case unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r),
		unicode.Is(unicode.Co, r), unicode.Is(unicode.Cs, r):
		return true
	case unicode.Is(unicode.Zs, r) && r != ' ':
		return true
	case r == 0x180e, r == 0x061c, r == 0x115f, r == 0x1160, r == 0x3164, r == 0xffa0: // invisible fillers
		return true
	case r >= 0xfdd0 && r <= 0xfdef, r&0xfffe == 0xfffe: // noncharacters
		return true
	case r >= 0xfe00 && r <= 0xfe0f, r >= 0xe0100 && r <= 0xe01ef: // variation selectors
		return true
	}
	return false
}

// EscapeRaw escapes s into at most max bytes (cut at an escape boundary, never
// inside a rune or an escape sequence) and reports whether it cut. Invalid
// UTF-8 bytes become \xNN. quote=true also escapes '"' and '\' so a value placed
// between ASCII quotes cannot leave them; quote=false leaves them as they are
// (free text, paths).
func EscapeRaw(s string, max int, quote bool) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		var piece string
		switch {
		case r == utf8.RuneError && size == 1:
			piece = `\x` + hexN(uint32(s[i]), 2)
		case r == '\\' && quote:
			piece = `\\`
		case r == '"' && quote:
			piece = `\"`
		case r == '\n':
			piece = `\n`
		case r == '\r':
			piece = `\r`
		case r == '\t':
			piece = `\t`
		case NeedsEscape(r):
			piece = escapeRune(r)
		default:
			piece = s[i : i+size]
		}
		if b.Len()+len(piece) > max {
			return b.String(), true
		}
		b.WriteString(piece)
		i += size
	}
	return b.String(), false
}

// Display escapes s for one cell of human-readable output: at most max bytes
// (after escaping), a cut value ends with "…". Backslash and quote stay as they
// are.
func Display(s string, max int) string {
	out, cut := EscapeRaw(s, max, false)
	if cut {
		out, _ = EscapeRaw(s, max-len("…"), false)
		out += "…"
	}
	return out
}

func escapeRune(r rune) string {
	switch {
	case r <= 0xff:
		return `\x` + hexN(uint32(r), 2)
	case r <= 0xffff:
		return `\u` + hexN(uint32(r), 4)
	}
	return `\U` + hexN(uint32(r), 8)
}

func hexN(v uint32, n int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = digits[v&0xf]
		v >>= 4
	}
	return string(out)
}

// EscapeExcept escapes terminal-active characters of s like EscapeRaw but leaves
// the runes listed in keep as they are, and does not touch '\' or '"' or cut the
// value. It is for machine formats (CSV) that need to keep tab and line breaks.
func EscapeExcept(s, keep string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteString(`\x` + hexN(uint32(s[i]), 2))
		case NeedsEscape(r) && !strings.ContainsRune(keep, r):
			b.WriteString(escapeRune(r))
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}
