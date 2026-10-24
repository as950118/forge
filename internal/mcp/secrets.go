package mcp

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
)

// Value-pattern based secret detection (ADR-F0001: plaintext secrets are never
// stored; there is deliberately no bypass flag). The detectors are heuristics:
// they err towards refusing, but exempt plain words, paths, UUIDs and
// hyphenated names to keep false positives low.

var (
	// Known credential shapes, one list for every context (URL parts, arguments,
	// every component of a file path, display). Matching is case-insensitive.
	//
	// knownSecretRe: anchored on a non-alphanumeric boundary (start or after a
	// separator such as . _ - / : = @) so that e.g. "task-..." does not match "sk-"
	// and "pnpm_store" does not match "npm_".
	knownSecretRe = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(?:` +
		`sk[-_][A-Za-z0-9_-]{8,}|` +
		`gh[pousr]_[A-Za-z0-9]{8,}|` +
		`A[KS]IA[0-9A-Z]{12,}|` +
		`AIza[0-9A-Za-z_-]{16,}|` +
		`eyJ[A-Za-z0-9_-]{8,}|` +
		`npm_[A-Za-z0-9]{8,}|` +
		`hf_[A-Za-z0-9]{20,}|` +
		`ya29\.[A-Za-z0-9_-]{16,}|` +
		`SG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}|` +
		`SK[0-9a-f]{32}` + // Twilio API key SID
		`)`)
	// knownSecretMidRe: distinctive prefixes (6+ characters or a long fixed tail)
	// are recognized anywhere inside a component, e.g. "prefixghp_...".
	knownSecretMidRe = regexp.MustCompile(`(?i)(?:` +
		`github_pat_[A-Za-z0-9_]{8,}|` +
		`gh[pousr]_[A-Za-z0-9]{30,}|` +
		`xox[abprs]-[A-Za-z0-9-]{8,}|` +
		`xapp-[A-Za-z0-9-]{8,}|` +
		`glpat-[A-Za-z0-9_-]{8,}|` +
		`glsa_[A-Za-z0-9_]{16,}|` +
		`sk-(?:ant|proj)-[A-Za-z0-9_-]{8,}|` +
		`[sr]k_(?:live|test)_[A-Za-z0-9]{8,}|` +
		`pk_(?:live|test)_[A-Za-z0-9]{8,}|` +
		`whsec_[A-Za-z0-9]{8,}|` +
		`shp(?:at|ss|ca|pa)_[a-f0-9]{16,}|` +
		`dop_v1_[a-f0-9]{16,}` +
		`)`)
	bearerRe  = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{16,}`)
	longTokRe = regexp.MustCompile(`[A-Za-z0-9_-]{24,}`)
	uuidRe    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	camelRe   = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	headerKV  = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*):\s*(\S.*)$`)
	portRe    = regexp.MustCompile(`^\d{1,5}(:\d{1,5})?$`)
)

// entropyThreshold (bits/char) for long opaque tokens. English-like hyphenated
// names and paths score well below it; random keys score above it.
const entropyThreshold = 3.5

func entropy(s string) float64 {
	if s == "" {
		return 0
	}
	cnt := map[rune]int{}
	n := 0
	for _, r := range s {
		cnt[r]++
		n++
	}
	h := 0.0
	for _, c := range cnt {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}

// highEntropyToken reports whether tok (>=24 chars of [A-Za-z0-9_-]) looks like
// a random credential: it must mix letters and digits (pure words/numbers are
// exempt), must not be a UUID and must have high Shannon entropy. A hex string of
// 32+ characters that mixes letters and digits is treated as a credential
// regardless of its entropy (a random hex key of 32 characters often scores
// below the threshold).
func highEntropyToken(tok string) bool {
	if len(tok) < 24 || uuidRe.MatchString(tok) {
		return false
	}
	var letter, digit bool
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letter = true
		}
	}
	if !(letter && digit) {
		return false
	}
	if len(tok) >= 32 && hexOnlyRe.MatchString(tok) {
		return true
	}
	return entropy(tok) >= entropyThreshold
}

// LooksSecretValue reports whether a value (env/header value, argument, free
// text shown to a human) looks like a credential, independent of any key name.
//
// It is used in both registration (refuse to store) and display (mask) contexts
// with the same rules (TASK-0026, docs/security.md M5):
//   - known credential prefixes (sk-, ghp_, eyJ, AKIA, ...) and Bearer/Basic
//     tokens are checked everywhere, including every component of a file path;
//   - the pure high-entropy rule is NOT applied to a value shaped like a file path
//     (/a/b, ~/a/b, ./a/b, ../a/b, C:\a\b with 2+ components, no "://"), because
//     temp directories, npx/pnpm caches and hash-named directories look random;
//   - clearly labelled identifiers are not secrets: sha256:<64 hex>,
//     name@sha256:<64 hex>, a 40/64-hex value after --ref/--rev/--sha256/... or in
//     commit=/rev=/sha=/hash=/digest=/checksum=, and long snake/kebab-case names
//     made of short lowercase words;
//   - everything with "://" (URLs) keeps the strict legacy rules.
func LooksSecretValue(v string) bool { return looksSecret(v, false) }

// LooksSecretPersist is the strict rule for data that is written to disk and
// stays there (ledger payloads): no path or identifier exception, so a random
// path component is masked too. Reading comfort is traded for safety, because a
// miss in stored data is permanent.
func LooksSecretPersist(v string) bool { return looksSecretStrict(v) }

// LooksSecretKV is LooksSecretValue for a value with a known key name: a 40/64-hex
// value under a key named commit/rev/ref/sha/hash/digest/checksum is an identifier.
func LooksSecretKV(key, val string) bool {
	if hashName(key) && isHexHash(val) {
		return false
	}
	return LooksSecretValue(val)
}

// LooksSecretAfter is LooksSecretValue for a value that follows another argument
// (prev, "" when there is none): after --ref/--rev/--sha256/... a 40/64-hex value
// is an identifier.
func LooksSecretAfter(prev, val string) bool {
	if strings.HasPrefix(prev, "-") && !strings.Contains(prev, "=") && hashName(prev) && isHexHash(val) {
		return false
	}
	return LooksSecretValue(val)
}

// looksSecretStrict is the legacy rule set: no path or identifier exception. It
// is used for URL parts (path segments, query values, fragments), which are not
// file paths.
func looksSecretStrict(v string) bool {
	if v == "" || refVarRe.MatchString(v) {
		return false
	}
	if knownSecret(v) || bearerRe.MatchString(v) {
		return true
	}
	return strictTokens(v)
}

func knownSecret(v string) bool {
	return knownSecretRe.MatchString(v) || knownSecretMidRe.MatchString(v)
}

func strictTokens(v string) bool {
	for _, f := range strings.Fields(v) {
		if awsSecretShaped(f) {
			return true
		}
	}
	for _, tok := range longTokRe.FindAllString(v, -1) {
		if highEntropyToken(tok) {
			return true
		}
	}
	return false
}

var (
	awsSecretRe = regexp.MustCompile(`^[A-Za-z0-9/+]{40}$`)
	hexOnlyRe   = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	// sha256:<64 hex> (also name@sha256:<64 hex>), delimited on both sides.
	digestRe        = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])sha256:[0-9a-f]{64}($|[^A-Za-z0-9_-])`)
	pathCharsRe     = regexp.MustCompile(`^[A-Za-z0-9._~@%+=,:/-]+$`)
	winPathRe       = regexp.MustCompile(`^[A-Za-z]:[\\/][A-Za-z0-9._~@%+=,:\\/ -]*$`)
	winStartRe      = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	idAlphaRe       = regexp.MustCompile(`^[a-z]{2,12}$`)
	idVersionLastRe = regexp.MustCompile(`^(?:v[0-9]{1,3}|rc[0-9]{1,2}|[0-9]{1,4})$`)
	idVersionMidRe  = regexp.MustCompile(`^v[0-9]{1,2}$`)
	vowelRe         = regexp.MustCompile(`[aeiouy]`)
	consRunRe       = regexp.MustCompile(`[bcdfghjklmnpqrstvwxz]{4,}`)
	hashWords       = map[string]bool{"commit": true, "rev": true, "ref": true, "sha": true, "sha1": true, "sha256": true, "hash": true, "digest": true, "checksum": true}
)

// hashName reports whether a key or flag name (commit, GIT_COMMIT, --ref,
// --sha256, IMAGE_DIGEST, ...) says its value is a commit id or digest.
func hashName(name string) bool {
	if LooksSecret(name) {
		return false
	}
	w := flagWords(name)
	return len(w) > 0 && hashWords[w[len(w)-1]]
}

func isHexHash(v string) bool { return (len(v) == 40 || len(v) == 64) && hexOnlyRe.MatchString(v) }

func looksSecret(v string, afterHashFlag bool) bool {
	if v == "" || refVarRe.MatchString(v) {
		return false
	}
	if knownSecret(v) || bearerRe.MatchString(v) {
		return true
	}
	v = digestRe.ReplaceAllString(v, "$1 $2")
	for _, f := range strings.Fields(v) {
		f = strings.TrimLeft(f, "([{<\"'`")
		f = strings.TrimRight(f, ".,;:)]}>\"'`")
		if f == "" {
			continue
		}
		hashFlag := !strings.Contains(f, "=") && strings.HasPrefix(f, "-") && hashName(f)
		if fieldSecret(f, afterHashFlag) {
			return true
		}
		afterHashFlag = hashFlag
	}
	return false
}

// fieldSecret applies the value rules to one whitespace-free field.
func fieldSecret(f string, afterHashFlag bool) bool {
	if strings.Contains(f, "://") {
		return strictTokens(f)
	}
	if afterHashFlag && isHexHash(f) {
		return false
	}
	// key=value / --flag=value (but not a path that merely contains "=")
	if i := strings.IndexByte(f, '='); i > 0 && !strings.ContainsAny(f[:i], `/\`) {
		k, val := f[:i], f[i+1:]
		if hashName(k) && isHexHash(val) {
			return false
		}
		return identTokens(k) || fieldSecret(val, false)
	}
	if isFilePath(f) {
		return pathHasKeyValueSecret(f)
	}
	return identTokens(f)
}

// awsSecretShaped: a 40-character base64 string (AWS secret access key shape)
// that mixes cases, digits and +// with high entropy. Its "/" and "+" split it
// into short pieces for the token rule.
func awsSecretShaped(f string) bool {
	return len(f) == 40 && awsSecretRe.MatchString(f) && strings.ContainsAny(f, "+/") && strings.ContainsAny(f, digits) &&
		strings.ContainsAny(f, "abcdefghijklmnopqrstuvwxyz") && strings.ContainsAny(f, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") && entropy(f) >= 4.0
}

const digits = "0123456789"

// identTokens is the entropy rule with the identifier exemptions.
func identTokens(f string) bool {
	if awsSecretShaped(f) {
		return true
	}
	for _, tok := range longTokRe.FindAllString(f, -1) {
		if highEntropyToken(tok) && !wordIdentifier(tok) {
			return true
		}
	}
	return false
}

// wordIdentifier reports whether tok is a long snake/kebab-case identifier made
// of short lowercase words (my_snake_case_identifier_name_v2_final): 3+ parts of
// at most 12 characters; at least three are lowercase words (2-12 letters, a
// vowel, no run of four consonants). Digits are allowed only as a version-like
// LAST part (v2, 2024, rc1) or as v<N> right before a last word (v2_final);
// a digit anywhere else means it is not an identifier (dictionary passphrases
// with digits mixed in are refused). Random tokens do not take this shape.
func wordIdentifier(tok string) bool {
	parts := strings.FieldsFunc(tok, func(r rune) bool { return r == '_' || r == '-' })
	if len(parts) < 3 {
		return false
	}
	isWord := func(p string) bool {
		return idAlphaRe.MatchString(p) && vowelRe.MatchString(p) && !consRunRe.MatchString(p)
	}
	last := len(parts) - 1
	words := 0
	for i, p := range parts {
		switch {
		case isWord(p):
			words++
		case i == last && idVersionLastRe.MatchString(p): // ..._v2, ..._2024, ..._rc1
		case i == last-1 && idVersionMidRe.MatchString(p) && isWord(parts[last]): // ..._v2_final
		default:
			return false // digits anywhere else: not an identifier
		}
	}
	return words >= 3
}

// isFilePath reports whether f has the shape of a file path: it starts with a
// known root directory (/Users, /var, /tmp, ...), ~/, ./, ../, node_modules/ or a
// Windows drive, has 2+ real components, contains only characters a
// path normally has and is not a URL. A URL path is not a file path.
func isFilePath(f string) bool {
	if strings.Contains(f, "://") {
		return false
	}
	win := winStartRe.MatchString(f)
	switch {
	case win:
		if !winPathRe.MatchString(f) {
			return false
		}
	case strings.HasPrefix(f, "/"):
		// An absolute path must start at a root directory name that exists on real
		// systems: "/<random>/<random>" is not a path. (Residual risk: a secret below a
		// known root, e.g. /tmp/<random>, is not recognized.)
		if !pathCharsRe.MatchString(f) || !pathRoots[firstComponent(f)] {
			return false
		}
	case strings.HasPrefix(f, "~/"), strings.HasPrefix(f, "./"), strings.HasPrefix(f, "../"), strings.HasPrefix(f, "node_modules/"):
		if !pathCharsRe.MatchString(f) {
			return false
		}
	default:
		return false
	}
	n := 0
	for _, c := range pathComponents(f) {
		if c != "." && c != ".." && c != "~" && !(win && len(c) == 2 && c[1] == ':') {
			n++
		}
	}
	return n >= 2
}

// pathRoots are the common first components of absolute paths.
var pathRoots = map[string]bool{
	"Users": true, "home": true, "var": true, "private": true, "tmp": true, "opt": true, "usr": true, "etc": true,
	"Library": true, "Applications": true, "System": true, "Volumes": true, "mnt": true, "srv": true, "root": true,
	"proc": true, "dev": true, "run": true, "nix": true, "snap": true, "bin": true, "sbin": true, "lib": true, "lib64": true,
	"cache": true, "data": true, "workspace": true, "app": true, "src": true, "build": true, "work": true,
}

func firstComponent(f string) string {
	for _, c := range strings.Split(f, "/") {
		if c != "" {
			return c
		}
	}
	return ""
}

func pathComponents(f string) []string {
	return strings.FieldsFunc(f, func(r rune) bool { return r == '/' || r == '\\' })
}

// pathHasKeyValueSecret: a path component like token=abc or password=abc still
// counts as a credential. (Known credential prefixes were checked on the whole
// value before.)
func pathHasKeyValueSecret(f string) bool {
	for _, c := range pathComponents(f) {
		if k, v, ok := strings.Cut(c, "="); ok && v != "" && !isRefOnly(v) && LooksSecret(k) {
			return true
		}
	}
	return false
}

const refAdvice = "비밀 값은 저장하지 않으므로 환경변수에 넣고 ${ENV_NAME} 참조를 사용하세요"

// checkURLSecrets inspects path segments, query values and the fragment by value
// pattern (not by key name). The URL itself is never echoed.
func checkURLSecrets(u *url.URL) error {
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		if s, err := url.PathUnescape(seg); err == nil {
			seg = s
		}
		if looksSecretStrict(seg) {
			return fmt.Errorf("URL 경로에 비밀 값처럼 보이는 문자열이 있어 저장할 수 없습니다 (평문 저장 금지). 토큰은 --header 'Authorization=${ENV_NAME}' 처럼 헤더 + 환경변수 참조로 전달하세요")
		}
	}
	for k, vs := range u.Query() {
		for _, v := range vs {
			if looksSecretStrict(v) {
				return fmt.Errorf("URL 쿼리 파라미터(%s)의 값이 비밀처럼 보여 저장할 수 없습니다 (평문 저장 금지). %s", k, "--header 와 ${ENV_NAME} 참조를 사용하세요")
			}
		}
	}
	if looksSecretStrict(u.Fragment) {
		return fmt.Errorf("URL 프래그먼트가 비밀 값처럼 보여 저장할 수 없습니다. --header 와 ${ENV_NAME} 참조를 사용하세요")
	}
	return nil
}

// flag-name classification -------------------------------------------------

var (
	secretWords = map[string]bool{"token": true, "secret": true, "password": true, "passwd": true, "pass": true, "pwd": true,
		"passphrase": true, "apikey": true, "auth": true, "authorization": true, "credential": true, "credentials": true,
		"cookie": true, "bearer": true, "signature": true}
	// Suffix / prefix words meaning the flag does not carry the secret itself.
	notValueLast = map[string]bool{"file": true, "path": true, "dir": true, "url": true, "type": true, "mode": true, "method": true, "scheme": true, "name": true, "enabled": true, "disabled": true, "required": true, "provider": true, "endpoint": true, "env": true, "var": true, "stdin": true, "header": true,
		"limit": true, "count": true, "size": true, "ttl": true, "length": true, "expiry": true, "lifetime": true}
	notValueFirst = map[string]bool{"no": true, "disable": true, "skip": true, "without": true, "enable": true, "use": true, "public": true}
)

func flagWords(name string) []string {
	name = camelRe.ReplaceAllString(strings.TrimLeft(name, "-"), "$1-$2")
	return strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '-' || r == '_' || r == '.' })
}

// secretFlagName reports whether a long flag name (e.g. "--api-key") normally
// carries a credential.
func secretFlagName(name string) bool {
	w := flagWords(name)
	if len(w) == 0 || notValueLast[w[len(w)-1]] || notValueFirst[w[0]] {
		return false
	}
	for _, x := range w {
		if secretWords[x] {
			return true
		}
	}
	return w[len(w)-1] == "key"
}

func argErr(idx int, what string) error {
	return fmt.Errorf("인자 #%d: %s. 인자는 평문으로 저장되므로 거부합니다 (평문 저장 금지). 비밀은 --env KEY=${ENV_NAME} 로 환경변수 참조에 담아 전달하세요. 서버가 환경변수를 읽지 못하고 인자로만 받는다면 환경변수를 읽어 실행하는 래퍼 스크립트를 command 로 등록하세요", idx+1, what)
}

func isRefOnly(s string) bool { return refVarRe.MatchString(s) }

// checkArg inspects one argument (without looking at its neighbours).
func checkArg(idx int, a string, afterHashFlag bool) error {
	if a == "" {
		return nil
	}
	if strings.HasPrefix(a, "-") {
		name, val, hasVal := strings.Cut(a, "=")
		if hasVal && val != "" && !isRefOnly(val) {
			if secretFlagName(name) || name == "-p" {
				return argErr(idx, fmt.Sprintf("%s 에 비밀 값이 지정되어 있습니다", name))
			}
			// --env=KEY=VAL, --header=Name: value, --url=https://..., ...
			if err := checkArgValue(idx, val, hashName(name)); err != nil {
				return err
			}
		}
		return nil
	}
	return checkArgValue(idx, a, afterHashFlag)
}

// checkArgValue checks a non-flag token: KEY=VAL, "Name: value", URL, or a bare value.
func checkArgValue(idx int, a string, afterHashFlag bool) error {
	if k, v, ok := strings.Cut(a, "="); ok && envKeyRe.MatchString(k) && v != "" && !isRefOnly(v) && LooksSecret(k) {
		return argErr(idx, fmt.Sprintf("%s=*** 가 비밀 값처럼 보입니다", k))
	}
	if m := headerKV.FindStringSubmatch(a); m != nil && !strings.Contains(a, "://") && LooksSecret(m[1]) && !isRefOnly(strings.TrimSpace(m[2])) {
		return argErr(idx, fmt.Sprintf("헤더 %s 에 비밀 값이 지정되어 있습니다", m[1]))
	}
	if strings.Contains(a, "://") {
		if u, err := url.Parse(a); err == nil && u.Host != "" {
			if _, hasPw := u.User.Password(); hasPw {
				return argErr(idx, "URL 에 user:password 가 포함되어 있습니다")
			}
			for k := range u.Query() {
				if LooksSecret(k) && u.Query().Get(k) != "" {
					return argErr(idx, fmt.Sprintf("URL 쿼리 파라미터 %s 에 비밀 값이 지정되어 있습니다", k))
				}
			}
			if err := checkURLSecrets(u); err != nil {
				return argErr(idx, "URL 에 비밀 값처럼 보이는 문자열이 있습니다")
			}
			return nil
		}
	}
	if looksSecret(a, afterHashFlag) {
		return argErr(idx, "비밀 값처럼 보이는 문자열이 있습니다")
	}
	return nil
}

// CheckArgs refuses command arguments that carry credentials: `--token=V`,
// `--token V`, `--api-key`, `--password`, `--secret`, `--auth`, `-p V`, KEY=V
// with a secret-like key, "Name: value" headers, URLs with embedded credentials
// and any argument matching a credential value pattern. Plain flags (--port 8080,
// --verbose, paths, package names) pass.
func CheckArgs(args []string) error {
	for i, a := range args {
		// `--ref <40hex>`, `--sha256 <64hex>`: the flag names the value a commit/digest.
		afterHash := i > 0 && strings.HasPrefix(args[i-1], "-") && !strings.Contains(args[i-1], "=") && hashName(args[i-1])
		if err := checkArg(i, a, afterHash); err != nil {
			return err
		}
		// space-separated form: `--token VALUE`, `-p VALUE`
		if strings.HasPrefix(a, "-") && !strings.Contains(a, "=") && i+1 < len(args) {
			next := args[i+1]
			if next == "" || strings.HasPrefix(next, "-") || isRefOnly(next) {
				continue
			}
			if secretFlagName(a) {
				return argErr(i, fmt.Sprintf("%s 다음 인자에 비밀 값이 지정되어 있습니다", a))
			}
			// `-p VALUE`: ports (-p 8080) and paths pass, anything else is treated as a password.
			if a == "-p" && !portRe.MatchString(next) && !strings.ContainsAny(next, "/\\") {
				return argErr(i, "-p 다음 인자가 비밀번호처럼 보입니다")
			}
		}
	}
	return nil
}
