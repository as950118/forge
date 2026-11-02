package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/heonjinjeong/forge/internal/mcp"
)

// RedactionVersion is the masking rule version stored with every payload
// (ledger_payloads.redaction_version). Bump it when the rules change.
const RedactionVersion = 5

const (
	// MaxStringBytes caps every string value inside a stored payload.
	MaxStringBytes = 4 << 10
	// MaxPayloadBytes caps one stored payload document after masking.
	MaxPayloadBytes = 64 << 10
	maskDepthLimit  = 32
	masked          = "***"
	truncMark       = "...[잘림]"
)

var (
	// "password=hunter2", "Authorization: Bearer xyz", "api_key: abc" embedded in free text.
	// The value may be quoted ("x" or 'x', spaces allowed inside the quotes).
	inlineSecretRe = regexp.MustCompile(`(?i)((?:token|secret|passw(?:or)?d|passwd|api[_-]?key|apikey|authorization|credential|cookie|signature)[A-Za-z0-9_-]*(?:\\?["'])?\s*[=:]\s*)(?:bearer\s+|basic\s+)?(?:"(?:\\.|[^"\\\n])*"|'(?:\\.|[^'\\\n])*'|[^\s"'&;,]+)`)
	// The same labels followed by a value in backslash-escaped quotes: JSON text that
	// was itself put inside a string ({\"api_key\":\"x\"}) or a shell line
	// (export API_KEY=\"x\"). Runs before inlineEnvRe, which would stop at the quote.
	inlineEscQuotedRe = regexp.MustCompile(`(?i)((?:token|secret|passw(?:or)?d|passwd|api[_-]?key|apikey|authorization|credential|cookie|signature)[A-Za-z0-9_-]*(?:\\?["'])?\s*[=:]\s*)\\"(?:[^"\\\n]|\\[^"\n])*\\"`)
	// TOKEN=ab cd: an upper-case environment-variable style name takes the whole
	// unquoted value up to the next quote, '&', ';', ',' or the end of the line (the
	// value may contain spaces). Runs before inlineSecretRe.
	inlineEnvRe = regexp.MustCompile(`\b((?:[A-Z][A-Z0-9_]*)?(?:TOKEN|SECRET|PASSW(?:OR)?D|PASSWD|API_?KEY|APIKEY|AUTHORIZATION|CREDENTIAL|COOKIE|SIGNATURE)[A-Z0-9_]*=)[^\n"'&;,]*[^\s"'&;,]`)
	// "-p x": the short password flag of many CLIs. Also matches "-p 22"; over-masking
	// a display string is preferred over leaking a password.
	inlineShortFlagRe = regexp.MustCompile(`(^|\s)(-p\s+)[^\s"'&;,-][^\s"'&;,]*`)
	// Credential shapes that need no label: sk-..., GitHub tokens, JWT, AWS access key
	// ids, "Bearer <token>".
	inlineTokenRe = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}|\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*|\b(?:AKIA|ASIA)[A-Z0-9]{16}\b|(?i:\bbearer)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// "--token X", "--api-key X", "--password X" inside one string (space-separated form;
	// the "=" form is covered by inlineSecretRe). The value must not itself look like a flag.
	inlineFlagRe = regexp.MustCompile(`(?i)(--?[a-z0-9-]*(?:token|secret|passw(?:or)?d|passwd|api-?key|apikey|auth|cookie|credential|bearer)[a-z0-9-]*\s+)(?:bearer\s+|basic\s+)?[^\s"'&;,-][^\s"'&;,]*`)
	// scheme://user:pass@host
	urlUserinfoRe = regexp.MustCompile(`(://[^/\s:@]*:)[^@\s/]+(@)`)
	// "--token", "--api-key", "-p" style flags whose next array element is the value.
	secretFlagRe = regexp.MustCompile(`(?i)^--?[a-z0-9-]*(token|secret|passw|api-?key|auth|cookie|credential|bearer)[a-z0-9-]*$`)
)

// Known limits (documented, not bugs): secrets shorter than the value-pattern
// thresholds, secrets stored as JSON numbers or booleans under non-secret key
// names, and look-alike characters outside the confusables table (see normKey) are
// not recognized. Array flag elements ("--token" followed by its value) use foldKey only. Key names
// containing secret words (token, password, ...) are always masked regardless
// of the value type.
//
// maskPayload is the only way payload bytes become storable text: it parses raw
// JSON, masks secrets (by key name, by value pattern, inline), caps string and
// document size, and returns valid JSON. There is no unmasked path.
func maskPayload(raw []byte) (string, error) { return maskPayloadMode(raw, true) }

// maskPayloadMode: persist=true is the storage rule (strict: paths and identifiers
// get no exception, see mcp.LooksSecretPersist); persist=false is for transient
// display (paths and labelled identifiers stay readable).
func maskPayloadMode(raw []byte, persist bool) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		// Not JSON: never store the text; keep only a marker.
		b, _ := json.Marshal(map[string]any{"_unparsed": true, "_bytes": len(raw)})
		return string(b), nil
	}
	out, err := json.Marshal(maskValue(v, 0, persist))
	if err != nil {
		return "", err
	}
	if len(out) <= MaxPayloadBytes {
		return string(out), nil
	}
	prev := cutUTF8(string(out), MaxPayloadBytes/2)
	b, err := json.Marshal(map[string]any{"_truncated": true, "_original_bytes": len(out), "preview": prev})
	return string(b), err
}

func maskValue(v any, depth int, persist bool) any {
	if depth > maskDepthLimit {
		return "[깊이 제한]"
	}
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic "***#N" suffixes
		for _, k := range keys {
			val := x[k]
			// A key that is itself a secret value (sk-..., ghp_...) is replaced; "***",
			// "***#2", ... keep colliding keys apart.
			base := k
			if looksSecret(k, persist) {
				base = masked
			}
			name := uniqueKey(m, base)
			if keyLooksSecret(k) {
				m[name] = masked
				continue
			}
			// A 40/64-hex value under a commit/rev/sha/digest key is an identifier.
			if sv, ok := val.(string); ok && !persist && !mcp.LooksSecretKV(k, sv) && mcp.LooksSecretValue(sv) {
				m[name] = sv
				continue
			}
			m[name] = maskValue(val, depth+1, persist)
		}
		return m
	case []any:
		a := make([]any, len(x))
		maskNext := false
		for i, val := range x {
			if maskNext {
				a[i] = masked
				maskNext = false
				continue
			}
			if s, ok := val.(string); ok && secretFlagRe.MatchString(foldKey(s)) && !strings.Contains(s, "=") {
				maskNext = true
			}
			a[i] = maskValue(val, depth+1, persist)
		}
		return a
	case string:
		return maskString(x, persist)
	default: // numbers, bool, nil
		return v
	}
}

func looksSecret(s string, persist bool) bool {
	if persist {
		return mcp.LooksSecretPersist(s)
	}
	return mcp.LooksSecretValue(s)
}

func maskString(s string, persist bool) string {
	if len(s) > MaxStringBytes {
		s = cutUTF8(s, MaxStringBytes) + truncMark
	}
	if looksSecret(s, persist) {
		return masked
	}
	return maskInlineText(s)
}

// maskInlineText applies every inline credential pattern (labelled and unlabelled).
// A label spelled with look-alike characters (fullwidth "\uff54oken=...", a Cyrillic
// "p\u0430ssword: ...", zero-width characters inside the word) is recognized too: the
// labelled patterns also run on the folded text (foldInline) and the values they find
// are masked in the original text, which is otherwise left as it was.
func maskInlineText(s string) string {
	s = maskFoldedLabels(s)
	return maskInlinePlain(s)
}

func maskInlinePlain(s string) string {
	s = inlineTokenRe.ReplaceAllString(s, masked)
	s = inlineEscQuotedRe.ReplaceAllString(s, "${1}"+masked)
	s = inlineEnvRe.ReplaceAllString(s, "${1}"+masked)
	s = inlineSecretRe.ReplaceAllString(s, "${1}"+masked)
	s = inlineFlagRe.ReplaceAllString(s, "${1}"+masked)
	s = inlineShortFlagRe.ReplaceAllString(s, "${1}${2}"+masked)
	return urlUserinfoRe.ReplaceAllString(s, "${1}"+masked+"${2}")
}

// foldRune folds one rune the way foldKey does; drop reports a rune that vanishes.
func foldRune(r rune) (out rune, drop bool) {
	switch {
	case r == 0xAD, r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2060 && r <= 0x2064, r == 0xFEFF:
		return 0, true
	case r >= 0xFF01 && r <= 0xFF5E: // fullwidth ASCII
		return r - 0xFEE0, false
	case r == 0x3000:
		return ' ', false
	case r >= 0x24B6 && r <= 0x24CF: // circled capital
		return 'A' + (r - 0x24B6), false
	case r >= 0x24D0 && r <= 0x24E9: // circled small
		return 'a' + (r - 0x24D0), false
	case r >= 0x1D400 && r <= 0x1D6A3: // math alphanumeric letters, 52 per style
		i := (r - 0x1D400) % 52
		if i < 26 {
			return 'A' + i, false
		}
		return 'a' + i - 26, false
	case r >= 0x1D7CE && r <= 0x1D7FF: // math digits, 10 per style
		return '0' + (r-0x1D7CE)%10, false
	}
	return r, false
}

// foldInline folds free text for label matching: foldRune plus dropped combining
// marks (Mn) and the Cyrillic/Greek look-alike table (normKey's confusables). It
// returns the folded text and, for every byte of it, the byte range of the
// original rune it came from.
func foldInline(s string) (folded string, from, to []int) {
	var b strings.Builder
	for i, r := range s {
		n := utf8.RuneLen(r)
		if r == utf8.RuneError {
			n = 1
		}
		fr, drop := foldRune(r)
		if drop || unicode.Is(unicode.Mn, fr) {
			continue
		}
		if l, ok := confusables[unicode.ToLower(fr)]; ok {
			fr = l
		}
		w := utf8.RuneLen(fr)
		if w < 0 {
			fr, w = utf8.RuneError, 3
		}
		b.WriteRune(fr)
		for k := 0; k < w; k++ {
			from = append(from, i)
			to = append(to, i+n)
		}
	}
	return b.String(), from, to
}

// maskFoldedLabels masks the values of labelled secrets whose label only matches
// after folding. Text that folding does not change is returned as is.
func maskFoldedLabels(s string) string {
	f, from, to := foldInline(s)
	if f == s {
		return s
	}
	type span struct{ a, b int }
	var spans []span
	add := func(re *regexp.Regexp, prefixGroup int) {
		for _, m := range re.FindAllStringSubmatchIndex(f, -1) {
			vs, ve := m[2*prefixGroup+1], m[1]
			if vs >= ve {
				continue
			}
			spans = append(spans, span{from[vs], to[ve-1]})
		}
	}
	add(inlineEscQuotedRe, 1)
	add(inlineEnvRe, 1)
	add(inlineSecretRe, 1)
	add(inlineFlagRe, 1)
	add(inlineShortFlagRe, 2)
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].a < spans[j].a })
	var out strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp.b <= pos {
			continue
		}
		a := max(sp.a, pos)
		out.WriteString(s[pos:a])
		out.WriteString(masked)
		pos = sp.b
	}
	out.WriteString(s[pos:])
	return out.String()
}

func uniqueKey(m map[string]any, base string) string {
	if _, ok := m[base]; !ok {
		return base
	}
	for i := 2; ; i++ {
		k := fmt.Sprintf("%s#%d", base, i)
		if _, ok := m[k]; !ok {
			return k
		}
	}
}

// keyLooksSecret reports whether a JSON key name implies a secret value. It checks
// the folded name and the normalized name (normKey), so "pass-word", "api.key",
// "p\u0430ssword" (Cyrillic a) and "pa\u0301ssword" (combining accent) are recognized.
func keyLooksSecret(k string) bool {
	f := foldKey(k)
	return mcp.LooksSecret(f) || mcp.LooksSecret(normKey(f))
}

// normKey normalizes a key name for secret-word matching: combining marks (Mn) are
// dropped, separators (Unicode Pd dashes, Pc connectors, Zs spaces, Cf format characters, plus '.') are removed, letters are lower-cased,
// and a small table of Cyrillic/Greek look-alikes is folded to Latin.
func normKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		switch {
		case unicode.In(r, unicode.Mn, unicode.Pd, unicode.Pc, unicode.Zs, unicode.Cf), r == 0x2212, r == '-', r == '_', r == ' ', r == '.':
			continue
		}
		r = unicode.ToLower(r)
		if l, ok := confusables[r]; ok {
			r = l
		}
		b.WriteRune(r)
	}
	return b.String()
}

// confusables maps representative Cyrillic and Greek look-alikes (lower case) to Latin.
// Known limit: characters absent from this table are not folded; docs/security.md.
var confusables = map[rune]rune{
	0x0430: 'a', 0x0435: 'e', 0x043E: 'o', 0x0440: 'p', 0x0441: 'c', 0x0443: 'y', 0x0445: 'x',
	0x0455: 's', 0x0456: 'i', 0x0458: 'j', 0x04BB: 'h', 0x0501: 'd', 0x051B: 'q', 0x051D: 'w',
	0x043A: 'k', 0x0442: 't', 0x043C: 'm', 0x043D: 'h', 0x0432: 'b', 0x0433: 'r',
	0x03B1: 'a', 0x03B5: 'e', 0x03BF: 'o', 0x03C1: 'p', 0x03BA: 'k', 0x03B9: 'i', 0x03BD: 'v',
	0x03C5: 'u', 0x03C7: 'x', 0x03C4: 't', 0x03B7: 'n', 0x03BC: 'u', 0x03C9: 'w', 0x03B2: 'b',
}

// foldKey prepares a key name for the secret-name rules: zero-width / bidi
// control characters are removed and compatibility characters are folded to
// ASCII. This is a stdlib approximation of NFKC (golang.org/x/text is not a
// dependency): fullwidth forms, mathematical alphanumerics and circled letters
// are covered. Homoglyphs, combining marks and separators are handled by normKey.
func foldKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		if fr, drop := foldRune(r); !drop {
			b.WriteRune(fr)
		}
	}
	return b.String()
}

func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// MaskJSON exposes the display masking for callers that show a payload (the
// gateway approval message): paths and labelled identifiers stay readable. Stored
// payloads always use the strict rule (maskPayload).
func MaskJSON(raw []byte) (string, error) { return maskPayloadMode(raw, false) }

// MaskText masks secrets inside free text with the same string rules as stored
// payloads (known credential shapes, `token=...`/`password: ...` regardless of the
// value length, `--token X`, URL userinfo) for callers that show text to a human.
func MaskText(s string) string { return maskString(s, false) }

// MaskInline masks only the inline credential patterns of MaskText (`token=...`,
// `password: ...`, `--token X`, URL userinfo). Unlike MaskText it never replaces a
// whole value for looking random, so names and paths that merely contain long
// identifiers (hash-suffixed gateway names, /var/folders/... temp dirs) stay readable.
func MaskInline(s string) string { return maskInlineText(s) }
