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
	// Known credential shapes. Anchored on a non-alphanumeric boundary so that
	// e.g. "task-..." does not match "sk-".
	knownSecretRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9])(?:` +
		`sk[-_][A-Za-z0-9_-]{8,}|` +
		`gh[pousr]_[A-Za-z0-9]{8,}|` +
		`github_pat_[A-Za-z0-9_]{8,}|` +
		`xox[abprs]-[A-Za-z0-9-]{8,}|` +
		`A[KS]IA[0-9A-Z]{12,}|` +
		`AIza[0-9A-Za-z_-]{16,}|` +
		`eyJ[A-Za-z0-9_-]{8,}|` +
		`glpat-[A-Za-z0-9_-]{8,}|` +
		`npm_[A-Za-z0-9]{8,}` +
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
// exempt), must not be a UUID and must have high Shannon entropy.
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
	return letter && digit && entropy(tok) >= entropyThreshold
}

// LooksSecretValue reports whether a value (env/header value, URL path segment,
// query value, argument) looks like a credential, independent of any key name.
func LooksSecretValue(v string) bool {
	if v == "" || refVarRe.MatchString(v) {
		return false
	}
	if knownSecretRe.MatchString(v) || bearerRe.MatchString(v) {
		return true
	}
	for _, tok := range longTokRe.FindAllString(v, -1) {
		if highEntropyToken(tok) {
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
		if LooksSecretValue(seg) {
			return fmt.Errorf("URL 경로에 비밀 값처럼 보이는 문자열이 있어 저장할 수 없습니다 (평문 저장 금지). 토큰은 --header 'Authorization=${ENV_NAME}' 처럼 헤더 + 환경변수 참조로 전달하세요")
		}
	}
	for k, vs := range u.Query() {
		for _, v := range vs {
			if LooksSecretValue(v) {
				return fmt.Errorf("URL 쿼리 파라미터(%s)의 값이 비밀처럼 보여 저장할 수 없습니다 (평문 저장 금지). %s", k, "--header 와 ${ENV_NAME} 참조를 사용하세요")
			}
		}
	}
	if LooksSecretValue(u.Fragment) {
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
func checkArg(idx int, a string) error {
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
			if err := checkArgValue(idx, val); err != nil {
				return err
			}
		}
		return nil
	}
	return checkArgValue(idx, a)
}

// checkArgValue checks a non-flag token: KEY=VAL, "Name: value", URL, or a bare value.
func checkArgValue(idx int, a string) error {
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
	if LooksSecretValue(a) {
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
		if err := checkArg(i, a); err != nil {
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
