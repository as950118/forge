package skill

import (
	"strings"
	"testing"
)

func TestParseSkillMD(t *testing.T) {
	m, err := ParseSkillMD([]byte(goodSkill))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "demo-skill" || m.Description != "A demo skill." || m.Version != "" {
		t.Errorf("meta: %+v", m)
	}
	if m.Frontmatter["x-custom"] != "keep-me" {
		t.Errorf("unknown field not preserved: %v", m.Frontmatter)
	}
	js, _ := m.FrontmatterJSON()
	if !strings.Contains(js, "x-custom") {
		t.Errorf("json: %s", js)
	}
	// CRLF + BOM + version
	m, err = ParseSkillMD([]byte("\xef\xbb\xbf---\r\nname: a1\r\ndescription: d\r\nversion: \"1.2.3\"\r\n---\r\nbody"))
	if err != nil || m.Version != "1.2.3" {
		t.Errorf("crlf: %v %+v", err, m)
	}
}

func TestParseSkillMDErrors(t *testing.T) {
	long := strings.Repeat("a", 65)
	cases := []struct{ name, in, want string }{
		{"no frontmatter", "# hi\n", "첫 줄"},
		{"unclosed", "---\nname: a\n", "닫는"},
		{"missing name", "---\ndescription: d\n---\n", `"name"`},
		{"missing description", "---\nname: a\n---\n", `"description"`},
		{"empty description", "---\nname: a\ndescription: \"  \"\n---\n", "비어"},
		{"upper", "---\nname: Demo\ndescription: d\n---\n", "규칙"},
		{"underscore", "---\nname: a_b\ndescription: d\n---\n", "규칙"},
		{"leading hyphen", "---\nname: -a\ndescription: d\n---\n", "규칙"},
		{"double hyphen", "---\nname: a--b\ndescription: d\n---\n", "규칙"},
		{"traversal", "---\nname: ../x\ndescription: d\n---\n", "규칙"},
		{"too long", "---\nname: " + long + "\ndescription: d\n---\n", "64"},
		{"name not string", "---\nname: 123\ndescription: d\n---\n", "문자열"},
		{"desc too long", "---\nname: a\ndescription: " + strings.Repeat("x", 1025) + "\n---\n", "1024"},
		{"bad version", "---\nname: a\ndescription: d\nversion: \"1.0\"\n---\n", "semver"},
		{"bad yaml", "---\nname: [a\ndescription: d\n---\n", "YAML"},
	}
	for _, c := range cases {
		_, err := ParseSkillMD([]byte(c.in))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v, want contains %q", c.name, err, c.want)
		}
	}
}

func TestParseSkillMDSizeLimits(t *testing.T) {
	if MaxSkillMDSize != 64<<10 || MaxFrontmatterSize != 32<<10 {
		t.Fatalf("limits changed: %d %d", MaxSkillMDSize, MaxFrontmatterSize)
	}
	head := "---\nname: a1\ndescription: d\n"
	// whole file too large (body only)
	big := head + "---\n" + strings.Repeat("x", MaxSkillMDSize)
	if _, err := ParseSkillMD([]byte(big)); err == nil || !strings.Contains(err.Error(), "SKILL.md 가 크기 제한") {
		t.Errorf("file size: %v", err)
	}
	// frontmatter too large, file under the file limit
	fmBig := head + "x-pad: " + strings.Repeat("y", MaxFrontmatterSize) + "\n---\nbody"
	if len(fmBig) > MaxSkillMDSize {
		t.Fatal("test setup: file limit would trigger first")
	}
	if _, err := ParseSkillMD([]byte(fmBig)); err == nil || !strings.Contains(err.Error(), "frontmatter 가 크기 제한") {
		t.Errorf("frontmatter size: %v", err)
	}
	// just under both limits passes
	ok := head + "x-pad: " + strings.Repeat("y", MaxFrontmatterSize-len("name: a1\ndescription: d\nx-pad: ")-1) + "\n---\nbody"
	if _, err := ParseSkillMD([]byte(ok)); err != nil {
		t.Errorf("under limit: %v", err)
	}
}
