// Package mcp implements the MCP server registry: definitions, secret
// references (never plaintext secrets), discovery cache and per-client
// exposure (TASK-0005, ADR-F0001).
package mcp

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// ScopeGlobal is the only scope implemented so far.
// TODO(project-scope): `--project` (mcp_servers.scope='project', projects row)
// is deferred together with skill project scope (ADR-F0001 D2, O-2).
const ScopeGlobal = "global"

// Transports.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
	TransportSSE   = "sse"
)

// Value reference types (schema mcp_server_values.ref_type).
const (
	RefEnv      = "env"
	RefKeychain = "keychain"
	RefLiteral  = "literal"
)

// Value slots.
const (
	SlotEnv    = "env"
	SlotHeader = "header"
)

// Sentinel errors.
var (
	ErrNotFound = errors.New("등록된 MCP 서버가 없습니다")
	ErrExists   = errors.New("이미 등록된 MCP 서버입니다")
)

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`) // == schema CHECK on mcp_servers.name
	envKeyRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerRe  = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	refVarRe  = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
	secretKey = []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "PASSWD", "AUTH", "CREDENTIAL", "COOKIE", "BEARER", "SIGNATURE", "SESSION"}
)

// Value is one env/header entry. Secrets are held only as references.
type Value struct {
	Slot     string `json:"slot"`
	Key      string `json:"key"`
	IsSecret bool   `json:"is_secret"`
	RefType  string `json:"ref_type"`
	// Ref is the environment variable name (ref_type=env) or the keychain locator.
	Ref string `json:"ref,omitempty"`
	// Literal is set only for non-secret literals (ref_type=literal).
	Literal string `json:"literal,omitempty"`
	// Display is the safe rendering: "${NAME}" for references, the literal otherwise.
	Display string `json:"display"`
}

// Spec is a server definition to register.
type Spec struct {
	Name      string
	Transport string
	Command   string
	Args      []string
	Cwd       string
	URL       string
	TimeoutMS int
	Values    []Value
}

// LooksSecret reports whether a key name implies a secret value (case-insensitive
// substring of TOKEN/KEY/SECRET/PASSWORD/AUTH/...). Such keys never accept literals.
func LooksSecret(key string) bool {
	u := strings.ToUpper(key)
	for _, w := range secretKey {
		if strings.Contains(u, w) {
			return true
		}
	}
	return false
}

// ParseValue parses a `KEY=VALUE` CLI argument for the given slot.
//
//	KEY=${NAME}          environment reference (ref_type=env)
//	KEY=keychain:s/a     keychain reference (not supported yet)
//	KEY=literal          plain literal; refused when KEY looks secret
//
// Mixed values such as "Bearer ${TOKEN}" are refused: the schema stores either a
// whole reference or a whole literal.
func ParseValue(slot, kv string) (Value, error) {
	i := strings.IndexByte(kv, '=')
	if i <= 0 {
		return Value{}, fmt.Errorf("%s 형식은 KEY=VALUE 여야 합니다: %q", slotLabel(slot), redactArg(kv))
	}
	key, val := kv[:i], kv[i+1:]
	switch slot {
	case SlotEnv:
		if !envKeyRe.MatchString(key) {
			return Value{}, fmt.Errorf("환경변수 이름이 올바르지 않습니다: %q (영문/숫자/_ 만, 숫자로 시작 불가)", key)
		}
	case SlotHeader:
		if !headerRe.MatchString(key) {
			return Value{}, fmt.Errorf("헤더 이름이 올바르지 않습니다: %q", key)
		}
	default:
		return Value{}, fmt.Errorf("알 수 없는 슬롯: %s", slot)
	}
	if val == "" {
		return Value{}, fmt.Errorf("%s %s 의 값이 비어 있습니다", slotLabel(slot), key)
	}
	if m := refVarRe.FindStringSubmatch(val); m != nil {
		return Value{Slot: slot, Key: key, IsSecret: true, RefType: RefEnv, Ref: m[1], Display: "${" + m[1] + "}"}, nil
	}
	if strings.HasPrefix(val, "keychain:") {
		// TODO(keychain): resolve via macOS `security find-generic-password` (ref = 'service/account').
		return Value{}, fmt.Errorf("%s %s: keychain 참조는 아직 지원하지 않습니다 (TODO: macOS keychain). ${ENV_NAME} 참조를 사용하세요", slotLabel(slot), key)
	}
	if strings.Contains(val, "${") || strings.Contains(val, "{env:") {
		return Value{}, fmt.Errorf("%s %s: 값 전체가 ${ENV_NAME} 이어야 합니다 (접두어 조합 불가). 예: 환경변수에 'Bearer xxx' 전체를 담고 %s=${ENV_NAME} 로 지정하세요", slotLabel(slot), key, key)
	}
	if LooksSecret(key) {
		return Value{}, fmt.Errorf("%s %s: 비밀로 보이는 키에는 literal 값을 저장할 수 없습니다 (평문 저장 금지). %s=${ENV_NAME} 처럼 환경변수 참조를 사용하세요", slotLabel(slot), key, key)
	}
	if LooksSecretKV(key, val) {
		return Value{}, errValueLooksSecret(slot, key)
	}
	return Value{Slot: slot, Key: key, IsSecret: false, RefType: RefLiteral, Literal: val, Display: val}, nil
}

// errValueLooksSecret never includes the value.
func errValueLooksSecret(slot, key string) error {
	return fmt.Errorf("%s %s: 값이 비밀(토큰/키)처럼 보여 literal 로 저장할 수 없습니다 (평문 저장 금지). 값은 환경변수에 넣고 %s=${ENV_NAME} 참조를 사용하세요", slotLabel(slot), key, key)
}

func slotLabel(slot string) string {
	if slot == SlotHeader {
		return "헤더"
	}
	return "환경변수"
}

// redactArg hides everything after '=' so that a malformed secret never reaches an error message.
func redactArg(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return kv[:i+1] + "***"
	}
	return kv
}

// Validate checks a Spec (transport-specific required fields, URL, names).
func (s *Spec) Validate() error {
	if !nameRe.MatchString(s.Name) {
		return fmt.Errorf("MCP 서버 이름이 올바르지 않습니다: %q (영문/숫자로 시작, 영문/숫자/_/- 만, 최대 64자)", s.Name)
	}
	switch s.Transport {
	case TransportStdio:
		if strings.TrimSpace(s.Command) == "" {
			return errors.New("stdio 서버에는 실행 명령(command)이 필요합니다. 예: forge mcp add NAME -- npx -y <패키지>")
		}
		if s.URL != "" {
			return errors.New("stdio 서버에는 --url 을 지정할 수 없습니다")
		}
		for _, v := range s.Values {
			if v.Slot == SlotHeader {
				return errors.New("stdio 서버에는 --header 를 지정할 수 없습니다 (--env 를 사용하세요)")
			}
		}
		if err := CheckArgs(s.Args); err != nil {
			return err
		}
	case TransportHTTP, TransportSSE:
		if s.Command != "" || len(s.Args) > 0 || s.Cwd != "" {
			return fmt.Errorf("%s 서버에는 명령/인자/cwd 를 지정할 수 없습니다 (--url 사용)", s.Transport)
		}
		if err := validateURL(s.URL); err != nil {
			return err
		}
		for _, v := range s.Values {
			if v.Slot == SlotEnv {
				return fmt.Errorf("%s 서버에는 --env 를 지정할 수 없습니다 (--header 를 사용하세요)", s.Transport)
			}
		}
	default:
		return fmt.Errorf("전송 방식(transport)은 stdio|http|sse 중 하나여야 합니다: %q", s.Transport)
	}
	if s.TimeoutMS < 0 {
		return errors.New("timeout 은 0 이상의 밀리초여야 합니다")
	}
	seen := map[string]bool{}
	for _, v := range s.Values {
		k := v.Slot + "\x00" + v.Key
		if seen[k] {
			return fmt.Errorf("%s %s 가 중복 지정되었습니다", slotLabel(v.Slot), v.Key)
		}
		seen[k] = true
		if v.RefType == RefLiteral && (v.IsSecret || LooksSecret(v.Key)) {
			return fmt.Errorf("%s %s: 비밀 값은 참조로만 저장할 수 있습니다", slotLabel(v.Slot), v.Key)
		}
		if v.RefType == RefLiteral && LooksSecretKV(v.Key, v.Literal) {
			return errValueLooksSecret(v.Slot, v.Key)
		}
	}
	return nil
}

func validateURL(raw string) error {
	if raw == "" {
		return errors.New("http/sse 서버에는 --url 이 필요합니다")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("URL 형식이 올바르지 않습니다") // never echo: it may embed secrets
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL 스킴은 http 또는 https 여야 합니다: %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("URL 에 호스트가 없습니다")
	}
	if u.User != nil {
		return errors.New("URL 에 사용자 정보(user:pass@)를 넣을 수 없습니다. --header 'Authorization=${ENV_NAME}' 를 사용하세요")
	}
	for k := range u.Query() {
		if LooksSecret(k) {
			return fmt.Errorf("URL 쿼리스트링에 비밀 값(%s)을 넣을 수 없습니다. --header 와 ${ENV_NAME} 참조를 사용하세요", k)
		}
	}
	return checkURLSecrets(u)
}
