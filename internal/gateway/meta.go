package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heonjinjeong/forge/internal/ledger"
	"github.com/heonjinjeong/forge/internal/skill"
	"github.com/heonjinjeong/forge/internal/tool"
)

var (
	skillSchema = json.RawMessage(`{"type":"object","properties":{` +
		`"name":{"type":"string","description":"조회할 스킬 이름 (forge_list 로 확인)","minLength":1,"maxLength":128},` +
		`"file":{"type":"string","description":"스킬 디렉터리 안의 다른 파일 상대 경로(선택, 예: references/guide.md). 생략하면 SKILL.md","maxLength":512}` +
		`},"required":["name"],"additionalProperties":false}`)
	listSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

	skillNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,127}$`)
)

const maxSkillFiles = 200

// schemaOf turns stored JSON schema text into a value for tools/list; anything
// that is not an object schema becomes the empty object schema.
func schemaOf(raw []byte) any {
	var m map[string]any
	if len(raw) > 0 && json.Unmarshal(raw, &m) == nil && m["type"] == "object" {
		return m
	}
	return map[string]any{"type": "object"}
}

func (g *Gateway) skillDescription(ctx context.Context, client string) string {
	d := "Forge 레지스트리의 스킬 본문(SKILL.md)과 파일 목록을 이름으로 조회합니다. 작업에 맞는 스킬이 있으면 먼저 조회해 지침을 따르세요."
	infos, err := g.o.Skills.List(ctx)
	if err != nil {
		return d
	}
	var parts []string
	for _, s := range infos {
		if !s.Enabled || !g.exposure(ctx).SkillVisible(&s, client) {
			continue
		}
		if len(parts) == maxListedSkills {
			parts = append(parts, "…(전체는 forge_list)")
			break
		}
		parts = append(parts, fmt.Sprintf("- %s: %s", s.Name, listText(s.Description, 80)))
	}
	if len(parts) > 0 {
		d += "\n사용 가능한 스킬:\n" + strings.Join(parts, "\n")
	}
	return d
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func (g *Gateway) callMeta(ctx context.Context, e *entry, args map[string]any, client, version string) *sdk.CallToolResult {
	switch e.Tool {
	case MetaSkill:
		return g.callSkill(ctx, args, client, version)
	case MetaList:
		return g.callList(ctx, client)
	}
	return errResult("알 수 없는 메타 도구입니다")
}

// callList summarizes what this client currently sees (no ledger row: it is not a
// skill/tool/MCP invocation).
func (g *Gateway) callList(ctx context.Context, client string) *sdk.CallToolResult {
	cat, err := g.build(ctx, client)
	if err != nil {
		return errResult(err.Error())
	}
	var b strings.Builder
	b.WriteString("## 도구\n")
	for _, e := range cat.entries {
		fmt.Fprintf(&b, "- %s: %s\n", e.Name, cutClean(strings.SplitN(e.Desc, "\n", 2)[0], 160))
	}
	b.WriteString("\n## 스킬 (forge_skill 로 조회)\n")
	infos, _ := g.o.Skills.List(ctx)
	n := 0
	for _, s := range infos {
		if s.Enabled && g.exposure(ctx).SkillVisible(&s, client) {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, listText(s.Description, 160))
			n++
		}
	}
	if n == 0 {
		b.WriteString("(없음)\n")
	}
	if len(cat.warnings) > 0 {
		b.WriteString("\n## 경고\n")
		for _, w := range cat.warnings {
			b.WriteString("- " + w + "\n")
		}
	}
	return textResult(b.String())
}

// callSkill serves one enabled skill: SKILL.md (or another file inside the skill
// directory) plus the file list, bounded by Options.SkillBytes. Every call is
// recorded as kind=skill invocation=model; unknown or disabled skills are misses.
func (g *Gateway) callSkill(ctx context.Context, args map[string]any, client, version string) *sdk.CallToolResult {
	rawName, _ := args["name"].(string)
	name := ledger.NormalizeSkillName(rawName)
	// Arguments that do not match the schema are attributed to the meta tool
	// itself (name=forge_skill); the skill-name field is not kept for them.
	if _, err := tool.ValidateArgs(&tool.Definition{InputSchema: skillSchema}, args); err != nil {
		c := g.begin(client, version, ledger.KindSkill, cleanName(MetaSkill), "", ledger.InvocationModel, nil)
		c.endClass(classInvalidArgs, nil)
		var ae *tool.ArgsError
		if errors.As(err, &ae) {
			return errResult(ae.Summary)
		}
		return errResult(err.Error())
	}
	logName := name
	if logName == "" { // e.g. "/" normalizes to nothing
		logName = MetaSkill
	}
	c := g.begin(client, version, ledger.KindSkill, cleanName(logName), "", ledger.InvocationModel, nil)
	miss := func(class string) *sdk.CallToolResult {
		c.endClass(class, ledger.Bool(false))
		if class == classDisabled {
			return errResult(fmt.Sprintf("스킬 \"%s\" 은 비활성화되어 있습니다 (forge skill enable %s)", cleanName(rawName), name))
		}
		return errResult(fmt.Sprintf("스킬을 찾을 수 없습니다: \"%s\". forge_list 로 사용 가능한 스킬을 확인하세요", cleanName(rawName)))
	}
	if !skillNameRe.MatchString(name) {
		return miss(classNotFound)
	}
	info, err := g.o.Skills.Get(ctx, name)
	if errors.Is(err, skill.ErrNotFound) {
		return miss(classNotFound)
	}
	if err != nil {
		c.endClass(classInternal, nil)
		return errResult("스킬 조회 중 오류가 발생했습니다: " + err.Error())
	}
	if !info.Enabled || !g.exposure(ctx).SkillVisible(info, client) {
		return miss(classDisabled)
	}
	// A profile may pin the skill to one immutable version; otherwise the
	// current version is served.
	want := ""
	if pv, ok := g.exposure(ctx).(PinnedSkills); ok {
		want = pv.PinnedVersion(info, client)
	}
	var store, shown, wantHash string
	for _, v := range info.Versions {
		if (want == "" && v.Current) || (want != "" && v.Version == want) {
			store, shown, wantHash = v.StorePath, v.Version, v.Hash
		}
	}
	root, err := g.skillRoot(store)
	if err != nil {
		c.endClass(classInternal, nil)
		return errResult(err.Error())
	}
	// Tamper detection: the files about to be served must still hash to the
	// content hash recorded when the version was added (same scheme as sync).
	if st, _, _ := skill.VerifyStore(root, wantHash); st != skill.StoreOK {
		c.endClass(classInternal, nil)
		return errResult(fmt.Sprintf("스킬 \"%s\" (버전 %s) 의 저장된 파일이 등록된 해시와 다릅니다 (변조 또는 손상 의심: %s). 제공하지 않습니다. 사용자에게 `forge skill check` 실행을 알려 주세요", cleanName(info.Name), cleanName(shown), st))
	}
	rel := "SKILL.md"
	if f, ok := args["file"].(string); ok && f != "" {
		rel = f
	}
	body, trunc, err := readInside(root, rel, g.o.SkillBytes)
	if err != nil {
		class := classNotFound
		if errors.Is(err, errEscape) {
			class = classDenied
		}
		c.endClass(class, nil)
		return errResult(err.Error())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 스킬: %s (버전 %s)\n\n", info.Name, shown)
	if rel != "SKILL.md" {
		fmt.Fprintf(&b, "파일: %s\n\n", filepath.ToSlash(filepath.Clean(rel)))
	}
	b.WriteString(body)
	if trunc {
		fmt.Fprintf(&b, "\n\n[내용이 상한(%d바이트)을 넘어 잘렸습니다]", g.o.SkillBytes)
	}
	files, more := listFiles(root)
	b.WriteString("\n\n## 파일 목록\n")
	for _, f := range files {
		b.WriteString("- " + f + "\n")
	}
	if more {
		fmt.Fprintf(&b, "- … (%d개까지만 표시)\n", maxSkillFiles)
	}
	out := b.String()
	c.end(&tool.Result{OK: true}, out)
	return textResult(out)
}

func (g *Gateway) skillRoot(storePath string) (string, error) {
	if storePath == "" {
		return "", errors.New("스킬에 현재 버전이 없습니다")
	}
	base := filepath.Join(g.o.Skills.Home, "skills")
	root := filepath.Join(g.o.Skills.Home, filepath.FromSlash(storePath))
	if r, err := filepath.Rel(base, root); err != nil || r == "." || strings.HasPrefix(r, "..") {
		return "", errors.New("스킬 저장 경로가 저장소 밖을 가리킵니다")
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return "", errors.New("스킬 파일을 찾을 수 없습니다 (저장소가 손상되었을 수 있습니다)")
	}
	return root, nil
}

var errEscape = errors.New("스킬 디렉터리 밖의 경로는 읽을 수 없습니다")

// readInside reads a regular text file under root. The path must be relative and
// must stay inside root after symlink resolution.
func readInside(root, rel string, max int) (string, bool, error) {
	if rel == "" || strings.ContainsRune(rel, 0) || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", false, errEscape
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false, errEscape
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false, errors.New("스킬 디렉터리를 열 수 없습니다")
	}
	full, err := filepath.EvalSymlinks(filepath.Join(realRoot, clean))
	if err != nil {
		return "", false, fmt.Errorf("스킬 안에 해당 파일이 없습니다: %s", cleanName(filepath.ToSlash(clean)))
	}
	if r, err := filepath.Rel(realRoot, full); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false, errEscape
	}
	f, err := os.Open(full)
	if err != nil {
		return "", false, errors.New("파일을 열 수 없습니다")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", false, errors.New("일반 파일이 아닙니다")
	}
	buf := make([]byte, max+1)
	n, _ := readFull(f, buf)
	data := buf[:n]
	trunc := n > max
	if trunc {
		data = data[:max]
	}
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if strings.ContainsRune(string(probe), 0) {
		return "", false, errors.New("텍스트 파일이 아니어서 반환하지 않습니다")
	}
	s := strings.ToValidUTF8(string(data), "�")
	if trunc {
		s = cutBytes(s, max)
	}
	return s, trunc, nil
}

func readFull(f *os.File, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := f.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// listFiles lists regular files below root (slash-separated, sorted, symlinks
// not followed), at most maxSkillFiles.
func listFiles(root string) ([]string, bool) {
	var out []string
	more := false
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if len(out) >= maxSkillFiles {
			more = true
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(root, p)
		sz := int64(-1)
		if info, err := d.Info(); err == nil {
			sz = info.Size()
		}
		out = append(out, fmt.Sprintf("%s (%d바이트)", filepath.ToSlash(rel), sz))
		return nil
	})
	sort.Strings(out)
	return out, more
}
