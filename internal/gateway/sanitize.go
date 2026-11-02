package gateway

import (
	"regexp"
	"strings"
	"unicode/utf8"

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
//
// Notation (TASK-0020 AC-11): the escapes written here are \xNN, \uNNNN and
// \UNNNNNNNN. A `"` or `\` is NOT escaped in general (this is not a quoted value, and
// doubling every backslash would garble paths and regular expressions), but a
// literal backslash that would read as one of those escapes (`\x1b`, `\u202e`,
// `\U000e0041` typed in the text) is written as two backslashes. So every `\x1b`,
// `\u202e` in the output is a real escape of a hidden character and a literal one
// shows as `\\x1b`. A backslash directly before a character that is escaped is
// doubled too, so the output can be read back unambiguously.
func listText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	s = doubleAmbiguousBackslashes(s)
	out, cut := escapeRaw(s, max, false)
	if cut {
		out, _ = escapeRaw(s, max-len("…"), false)
		out += "…"
	}
	return out
}

// partialEscapeRe matches an escape sequence cut short at the end of a string.
var partialEscapeRe = regexp.MustCompile(`\\(?:x[0-9A-Fa-f]?|u[0-9A-Fa-f]{0,3}|U[0-9A-Fa-f]{0,7})?$`)

// cutClean shortens text that already went through listText to at most max bytes
// (ends with "…"). It never sanitizes again (that would double the escapes) and
// does not leave half an escape sequence at the cut.
func cutClean(s string, max int) string {
	if len(s) <= max {
		return s
	}
	out := cutBytes(s, max-len("…"))
	if m := partialEscapeRe.FindString(out); m != "" {
		out = out[:len(out)-len(m)]
	}
	return out + "…"
}

// lookalikeEscapeRe matches the text after a backslash that spells one of the
// escapes escapeRaw writes (\xNN, \uNNNN, \UNNNNNNNN).
var lookalikeEscapeRe = regexp.MustCompile(`^(?:x[0-9A-Fa-f]{2}|u[0-9A-Fa-f]{4}|U[0-9A-Fa-f]{8})`)

// doubleAmbiguousBackslashes doubles a backslash that would otherwise be read as
// part of an escape written by escapeRaw: one followed by text that spells such an
// escape, or one directly before a character that escapeRaw is going to escape.
func doubleAmbiguousBackslashes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			rest := s[i+1:]
			next, _ := utf8.DecodeRuneInString(rest)
			if rest != "" && (lookalikeEscapeRe.MatchString(rest) || next == utf8.RuneError || needsEscape(next)) {
				b.WriteByte('\\')
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// escapeRaw and needsEscape are the shared rules of internal/textsafe.
func escapeRaw(s string, max int, quote bool) (string, bool) {
	return textsafe.EscapeRaw(s, max, quote)
}

func needsEscape(r rune) bool { return textsafe.NeedsEscape(r) }

const truncMarker = "…(잘림)"

// quoteValue renders a dynamic value as one ASCII-quoted token.
func quoteValue(s string, max int) string { return `"` + escapeText(s, max) + `"` }
