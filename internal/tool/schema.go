package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// DefaultInputSchema accepts no arguments.
const DefaultInputSchema = `{"type":"object","properties":{},"additionalProperties":false}`

// normalizeSchema validates an input schema, forces the root to be an object
// schema and defaults additionalProperties to false. Remote $ref is not
// resolvable (no loader), so a schema can never trigger network access.
func normalizeSchema(raw json.RawMessage) (json.RawMessage, *schemaInfo, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(DefaultInputSchema)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("입력 스키마가 올바른 JSON 객체가 아닙니다: %w", err)
	}
	if t, _ := m["type"].(string); t != "object" {
		return nil, nil, errors.New(`입력 스키마의 최상위 type 은 "object" 여야 합니다`)
	}
	if _, ok := m["additionalProperties"]; !ok {
		m["additionalProperties"] = false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	if _, err := compile(out); err != nil {
		return nil, nil, err
	}
	var info schemaInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, nil, fmt.Errorf("입력 스키마(properties)를 해석할 수 없습니다: %w", err)
	}
	return out, &info, nil
}

func compile(raw []byte) (*jsonschema.Resolved, error) {
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("입력 스키마가 올바르지 않습니다: %w", err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("입력 스키마를 해석할 수 없습니다: %w", err)
	}
	return rs, nil
}

// ValidateArgs validates call arguments against the tool's input schema
// (required / types / enum / additionalProperties) and applies schema defaults.
// The returned map is a copy.
func ValidateArgs(def *Definition, args map[string]any) (map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("입력 인자를 JSON 으로 변환할 수 없습니다: %w", err)
	}
	if len(b) > maxArgsBytes {
		return nil, fmt.Errorf("입력 인자가 너무 큽니다 (최대 %d바이트)", maxArgsBytes)
	}
	var cp map[string]any
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, err
	}
	sch := def.InputSchema
	if len(sch) == 0 {
		sch = json.RawMessage(DefaultInputSchema)
	}
	rs, err := compile(sch)
	if err != nil {
		return nil, err
	}
	if err := rs.ApplyDefaults(&cp); err != nil {
		return nil, fmt.Errorf("입력 인자 기본값 적용 실패: %w", err)
	}
	if err := rs.Validate(cp); err != nil {
		return nil, newArgsError(err)
	}
	return cp, nil
}

// ArgsError is a schema violation of call arguments. Error() is Korean guidance;
// the validator's raw (English) text is in Detail (shown with --verbose).
type ArgsError struct {
	Summary string
	Detail  string
}

func (e *ArgsError) Error() string { return e.Summary }

var (
	propRe    = regexp.MustCompile(`/properties/([A-Za-z0-9_.-]+)`)
	listedRe  = regexp.MustCompile(`\[([^\]]*)\]`)
	kindWords = []struct{ key, msg string }{
		{"required", "필수 속성이 없습니다"},
		{"additional", "스키마에 정의되지 않은 속성입니다"},
		{"enum", "허용된 값(enum)이 아닙니다"},
		{"minimum", "최솟값(minimum) 미만입니다"},
		{"maximum", "최댓값(maximum) 초과입니다"},
		{"minLength", "최소 길이(minLength) 미만입니다"},
		{"maxLength", "최대 길이(maxLength) 초과입니다"},
		{"pattern", "패턴(pattern)과 맞지 않습니다"},
		{"type", "타입(type)이 맞지 않습니다"},
	}
)

func newArgsError(err error) *ArgsError {
	raw := err.Error()
	var parts []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			parts = append(parts, p)
		}
	}
	low := strings.ToLower(raw)
	for _, k := range kindWords {
		if strings.Contains(low, strings.ToLower(k.key)) {
			add(k.msg)
		}
	}
	for _, m := range propRe.FindAllStringSubmatch(raw, -1) {
		add("속성 " + m[1])
	}
	for _, m := range listedRe.FindAllStringSubmatch(raw, -1) {
		add("대상 [" + m[1] + "]")
	}
	sum := "입력 인자가 입력 스키마와 맞지 않습니다"
	if len(parts) > 0 {
		sum += " (" + strings.Join(parts, "; ") + ")"
	}
	return &ArgsError{Summary: sum + ". 자세한 검증 메시지는 --verbose 또는 error_detail 로 확인하세요", Detail: raw}
}
