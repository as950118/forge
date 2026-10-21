package gateway

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// escapeText is the single sanitizer for every dynamic value that reaches a
// human (the confirm=ask approval message) or the ledger (names of unknown
// tools). It makes the text one visible line: control characters (newline, CR,
// ESC, C1, DEL), bidirectional controls (U+202A-202E, U+2066-2069, U+200E/200F,
// U+061C), zero-width and other format characters, line/paragraph separators,
// private-use code points, non-ASCII spaces and invalid UTF-8 become visible
// escapes (\n, \x1b, \u202e, ...), and backslash and double quote are escaped
// so a value placed between ASCII quotes cannot leave them.
//
// max bounds the escaped length in bytes (the cap applies after escaping so a
// value cannot grow past it); a cut value ends with truncMarker.
func escapeText(s string, max int) string {
	out, cut := escapeRaw(s, max, true)
	if cut {
		out += truncMarker
	}
	return out
}

// safeName is a name that already went through cleanName: control, bidi and
// zero-width characters are escaped and it is at most maxCleanName bytes. The
// type exists so the ledger bookkeeping cannot receive (or sanitize) a name
// twice: only cleanName produces one.
type safeName string

// maxCleanName is the byte bound of a safeName, below the ledger's 128-byte name
// limit. A longer name is cut after escaping (at an escape boundary) and ends
// with "…".
const maxCleanName = 120

// cleanName is the single sanitizer of names shown to a human or stored in the
// ledger. It escapes exactly once; apply it only to raw text, never to a
// safeName.
func cleanName(s string) safeName {
	out, cut := escapeRaw(s, maxCleanName, true)
	if cut {
		out, _ = escapeRaw(s, maxCleanName-len("…"), true)
		out += "…"
	}
	return safeName(out)
}

// maxDescBytes bounds a tool description sent in tools/list.
const maxDescBytes = 1024

// listText makes untrusted free text (tool descriptions from proxied servers,
// skill descriptions) safe for tools/list and forge_list: whitespace runs
// (newline, CR, tab, U+2028/2029, NBSP...) fold to one space, remaining control,
// bidi, zero-width and private-use characters become visible escapes, and the
// result is at most max bytes (cut after escaping, ends with "…").
func listText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	out, cut := escapeRaw(s, max, false)
	if cut {
		out, _ = escapeRaw(s, max-len("…"), false)
		out += "…"
	}
	return out
}

// escapeRaw escapes s into at most max bytes (cut at an escape boundary, never
// inside a rune or an escape sequence) and reports whether it cut. quote=false
// leaves '"' and '\' unescaped (free text not placed between quotes).
func escapeRaw(s string, max int, quote bool) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		var piece string
		switch {
		case r == utf8.RuneError && size == 1:
			piece = `\x` + hex2(s[i])
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
		case needsEscape(r):
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

const truncMarker = "…(잘림)"

// quoteValue renders a dynamic value as one ASCII-quoted token.
func quoteValue(s string, max int) string { return `"` + escapeText(s, max) + `"` }

func needsEscape(r rune) bool {
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

func escapeRune(r rune) string {
	const digits = "0123456789abcdef"
	switch {
	case r <= 0xff:
		return `\x` + string([]byte{digits[r>>4&0xf], digits[r&0xf]})
	case r <= 0xffff:
		return `\u` + hexN(uint32(r), 4)
	}
	return `\U` + hexN(uint32(r), 8)
}

func hex2(b byte) string { return hexN(uint32(b), 2) }

func hexN(v uint32, n int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = digits[v&0xf]
		v >>= 4
	}
	return string(out)
}
