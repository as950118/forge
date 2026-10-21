package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/heonjinjeong/forge/internal/mcp"
)

// RedactionVersion is the masking rule version stored with every payload
// (ledger_payloads.redaction_version). Bump it when the rules change.
const RedactionVersion = 2

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
	inlineSecretRe = regexp.MustCompile(`(?i)((?:token|secret|passw(?:or)?d|passwd|api[_-]?key|apikey|authorization|credential|cookie|signature)[A-Za-z0-9_-]*\s*[=:]\s*)(?:bearer\s+|basic\s+)?[^\s"'&;,]+`)
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
// names, and homoglyph key names (see foldKey) are not recognized. Key names
// containing secret words (token, password, ...) are always masked regardless
// of the value type.
//
// maskPayload is the only way payload bytes become storable text: it parses raw
// JSON, masks secrets (by key name, by value pattern, inline), caps string and
// document size, and returns valid JSON. There is no unmasked path.
func maskPayload(raw []byte) (string, error) {
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
	out, err := json.Marshal(maskValue(v, 0))
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

func maskValue(v any, depth int) any {
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
			if mcp.LooksSecretValue(k) {
				base = masked
			}
			name := uniqueKey(m, base)
			if mcp.LooksSecret(foldKey(k)) {
				m[name] = masked
				continue
			}
			m[name] = maskValue(val, depth+1)
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
			a[i] = maskValue(val, depth+1)
		}
		return a
	case string:
		return maskString(x)
	default: // numbers, bool, nil
		return v
	}
}

func maskString(s string) string {
	if len(s) > MaxStringBytes {
		s = cutUTF8(s, MaxStringBytes) + truncMark
	}
	if mcp.LooksSecretValue(s) {
		return masked
	}
	s = inlineSecretRe.ReplaceAllString(s, "${1}"+masked)
	s = inlineFlagRe.ReplaceAllString(s, "${1}"+masked)
	s = urlUserinfoRe.ReplaceAllString(s, "${1}"+masked+"${2}")
	return s
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

// foldKey prepares a key name for the secret-name rules: zero-width / bidi
// control characters are removed and compatibility characters are folded to
// ASCII. This is a stdlib approximation of NFKC (golang.org/x/text is not a
// dependency): fullwidth forms, mathematical alphanumerics and circled letters
// are covered. Known limit: homoglyphs from other scripts (e.g. Cyrillic "а"
// for "a") are not folded, so "pаssword" is not recognized by key name; such
// values are still caught by the value-pattern rules when they look like secrets.
func foldKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		switch {
		case r == 0xAD, r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2060 && r <= 0x2064, r == 0xFEFF:
			continue
		case r >= 0xFF01 && r <= 0xFF5E: // fullwidth ASCII
			r -= 0xFEE0
		case r == 0x3000:
			r = ' '
		case r >= 0x24B6 && r <= 0x24CF: // circled capital
			r = 'A' + (r - 0x24B6)
		case r >= 0x24D0 && r <= 0x24E9: // circled small
			r = 'a' + (r - 0x24D0)
		case r >= 0x1D400 && r <= 0x1D6A3: // math alphanumeric letters, 52 per style
			i := (r - 0x1D400) % 52
			if i < 26 {
				r = 'A' + i
			} else {
				r = 'a' + i - 26
			}
		case r >= 0x1D7CE && r <= 0x1D7FF: // math digits, 10 per style
			r = '0' + (r-0x1D7CE)%10
		}
		b.WriteRune(r)
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

// MaskJSON exposes the masking for callers that want to show a payload (tests, history).
func MaskJSON(raw []byte) (string, error) { return maskPayload(raw) }

// MaskText masks secrets inside free text with the same string rules as stored
// payloads (known credential shapes, `token=...`/`password: ...` regardless of the
// value length, `--token X`, URL userinfo) for callers that show text to a human.
func MaskText(s string) string { return maskString(s) }

// MaskInline masks only the inline credential patterns of MaskText (`token=...`,
// `password: ...`, `--token X`, URL userinfo). Unlike MaskText it never replaces a
// whole value for looking random, so names and paths that merely contain long
// identifiers (hash-suffixed gateway names, /var/folders/... temp dirs) stay readable.
func MaskInline(s string) string {
	s = inlineSecretRe.ReplaceAllString(s, "${1}"+masked)
	s = inlineFlagRe.ReplaceAllString(s, "${1}"+masked)
	return urlUserinfoRe.ReplaceAllString(s, "${1}"+masked+"${2}")
}
