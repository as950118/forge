// Package skill implements the skill registry: SKILL.md validation, import
// sources (dir/zip/git), immutable content-addressed versions and toggles.
package skill

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Name rules: docs/clients/opencode.md (^[a-z0-9]+(-[a-z0-9]+)*$, 1..64),
// a subset of the CHECK constraint in db/schema.sql. Description: 1..1024.
const (
	MaxNameLen        = 64
	MaxDescriptionLen = 1024
	// MaxSkillMDSize bounds the whole SKILL.md; MaxFrontmatterSize bounds the
	// YAML block before it is handed to the parser.
	MaxSkillMDSize     = 64 << 10
	MaxFrontmatterSize = 32 << 10
)

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?(\+[0-9A-Za-z][0-9A-Za-z.-]*)?$`)
)

// Meta is the parsed SKILL.md frontmatter.
type Meta struct {
	Name        string
	Description string
	// Version is the optional semver from frontmatter ("" if absent).
	Version string
	// Frontmatter holds every field verbatim (unknown fields preserved).
	Frontmatter map[string]any
}

// FrontmatterJSON returns the original fields as JSON for skill_versions.frontmatter.
func (m *Meta) FrontmatterJSON() (string, error) {
	b, err := json.Marshal(m.Frontmatter)
	if err != nil {
		return "", fmt.Errorf("frontmatter 를 JSON 으로 변환할 수 없습니다 (키는 문자열이어야 합니다): %w", err)
	}
	return string(b), nil
}

// ValidateName checks the skill name rule.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("name 이 비어 있습니다")
	}
	if utf8.RuneCountInString(name) > MaxNameLen {
		return fmt.Errorf("name %q 이 %d자를 넘습니다", name, MaxNameLen)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("name %q 이 규칙에 맞지 않습니다: 소문자·숫자·하이픈만 사용하고 하이픈으로 시작/끝나거나 연속될 수 없습니다 (^[a-z0-9]+(-[a-z0-9]+)*$)", name)
	}
	return nil
}

// ParseSkillMD parses and validates SKILL.md content.
func ParseSkillMD(data []byte) (*Meta, error) {
	if len(data) > MaxSkillMDSize {
		return nil, fmt.Errorf("SKILL.md 가 크기 제한(%d바이트)을 초과했습니다 (현재 %d바이트)", MaxSkillMDSize, len(data))
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return nil, fmt.Errorf("SKILL.md 의 첫 줄이 frontmatter 시작 구분자 '---' 가 아닙니다")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, fmt.Errorf("SKILL.md frontmatter 를 닫는 '---' 줄이 없습니다")
	}
	raw := strings.Join(lines[1:end], "\n")
	if len(raw) > MaxFrontmatterSize {
		return nil, fmt.Errorf("SKILL.md frontmatter 가 크기 제한(%d바이트)을 초과했습니다 (현재 %d바이트)", MaxFrontmatterSize, len(raw))
	}
	fm := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &fm); err != nil {
		return nil, fmt.Errorf("SKILL.md frontmatter YAML 을 해석할 수 없습니다: %w", err)
	}
	if fm == nil {
		fm = map[string]any{}
	}
	m := &Meta{Frontmatter: fm}

	name, err := stringField(fm, "name", true)
	if err != nil {
		return nil, err
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	m.Name = name

	desc, err := stringField(fm, "description", true)
	if err != nil {
		return nil, err
	}
	if n := utf8.RuneCountInString(desc); n < 1 || n > MaxDescriptionLen {
		return nil, fmt.Errorf("description 은 1~%d자여야 합니다 (현재 %d자)", MaxDescriptionLen, n)
	}
	m.Description = desc

	ver, err := stringField(fm, "version", false)
	if err != nil {
		return nil, fmt.Errorf("%w (숫자처럼 보이는 값은 따옴표로 감싸세요)", err)
	}
	if ver != "" && !versionRe.MatchString(ver) {
		return nil, fmt.Errorf("version %q 이 semver(MAJOR.MINOR.PATCH[-pre][+build]) 형식이 아닙니다", ver)
	}
	m.Version = ver
	if _, err := m.FrontmatterJSON(); err != nil {
		return nil, err
	}
	return m, nil
}

func stringField(fm map[string]any, key string, required bool) (string, error) {
	v, ok := fm[key]
	if !ok || v == nil {
		if required {
			return "", fmt.Errorf("frontmatter 에 필수 필드 %q 가 없습니다", key)
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("frontmatter 필드 %q 는 문자열이어야 합니다 (현재 %T)", key, v)
	}
	s = strings.TrimSpace(s)
	if s == "" && required {
		return "", fmt.Errorf("frontmatter 필드 %q 가 비어 있습니다", key)
	}
	return s, nil
}
