package adapter

import (
	"errors"
	"regexp"

	"github.com/heonjinjeong/forge/internal/mcp"
)

var (
	envKeyRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	headerKeyRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,128}$") // RFC 9110 token
	keySubstRe  = regexp.MustCompile(`(?i)\{(env|file):|\$\{`)
)

// CheckValueKey validates the NAME of an env/header slot right before it is
// rendered as a JSON key, whatever its reference type (the registry validates
// names on input, but the database can be edited directly). Substitution syntax
// ({env:, {file:, ${), control characters, spaces, quotes, backslashes, non-ASCII
// and over-long names are all refused: an env name must be an identifier, a
// header name an HTTP token.
func CheckValueKey(slot, key string) error {
	if keySubstRe.MatchString(key) {
		return errors.New("이름에 치환 문법({env:}/{file:}/${})이 있습니다")
	}
	if key == "__proto__" {
		// A valid identifier / HTTP token, but a JavaScript client that merges the
		// parsed config into an object (spread, Object.assign) would set the
		// prototype instead of an entry.
		return errors.New("이름 __proto__ 는 사용할 수 없습니다")
	}
	switch slot {
	case mcp.SlotEnv:
		if !envKeyRe.MatchString(key) {
			return errors.New("환경변수 이름이 올바르지 않습니다")
		}
	case mcp.SlotHeader:
		if !headerKeyRe.MatchString(key) {
			return errors.New("헤더 이름이 올바르지 않습니다")
		}
	default:
		return errors.New("알 수 없는 슬롯입니다")
	}
	return nil
}
