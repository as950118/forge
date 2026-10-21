package gateway

import (
	"strings"

	"github.com/heonjinjeong/forge/internal/textsafe"
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

// escapeRaw and needsEscape are the shared rules of internal/textsafe.
func escapeRaw(s string, max int, quote bool) (string, bool) {
	return textsafe.EscapeRaw(s, max, quote)
}

func needsEscape(r rune) bool { return textsafe.NeedsEscape(r) }

const truncMarker = "…(잘림)"

// quoteValue renders a dynamic value as one ASCII-quoted token.
func quoteValue(s string, max int) string { return `"` + escapeText(s, max) + `"` }
