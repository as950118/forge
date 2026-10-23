package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/heonjinjeong/forge/internal/textsafe"
)

// Install preview (TASK-0013): everything a person should see before a skill
// from outside lands in the registry, computed from the staged (already
// validated and size-limited) tree. Every dynamic string in a Preview has gone
// through textsafe once at construction, so the text and the JSON renderings are
// both safe for a terminal (escape sequences, bidi controls and line breaks in
// names, descriptions, paths or URLs become visible escapes).
//
// The pattern findings are HEURISTICS over SKILL.md only: they show where a
// reader should look first, they are not a detector and a clean result is not a
// safety statement.

// Preview limits (a hostile skill must not make the preview itself explode).
const (
	maxPreviewList  = 30  // entries per list; the rest is counted in *_more
	maxPreviewValue = 120 // runes of one frontmatter value
	maxExample      = 80
	maxURLs         = 10
	probeBytes      = 8192
)

// RiskyFrontmatterKeys are SKILL.md frontmatter fields that change what the
// client does with the skill (lower-case, '_' folded to '-'). `hooks` is the most
// dangerous: a client may run commands from it while the skill is active.
var RiskyFrontmatterKeys = map[string]string{
	"hooks":            "스킬이 활성화되면 클라이언트가 이 명령을 실행할 수 있습니다 (명령 실행 표면)",
	"allowed-tools":    "스킬이 사용자 확인 없이 쓸 수 있는 도구 허용 목록을 넓힙니다",
	"disallowed-tools": "도구 차단 목록을 바꿉니다",
	"context":          "실행 컨텍스트(예: fork)를 바꿉니다",
	"agent":            "하위 에이전트 유형을 지정합니다",
	"model":            "사용 모델을 지정합니다",
	"shell":            "셸 실행 방식을 지정합니다",
}

// FMItem is one frontmatter field.
type FMItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Risky bool   `json:"risky"`
	Why   string `json:"why,omitempty"`
}

// ExecItem is a file a client could execute (exec bit, shebang or script extension).
type ExecItem struct {
	Path        string   `json:"path"`
	Size        int64    `json:"size"`
	SHA256      string   `json:"sha256"`
	Interpreter string   `json:"interpreter,omitempty"`
	Reasons     []string `json:"reasons"`
}

// PatternHit is one heuristic finding in SKILL.md.
type PatternHit struct {
	Kind      string `json:"kind"`
	Count     int    `json:"count"`
	FirstLine int    `json:"first_line"`
	Example   string `json:"example,omitempty"`
}

// Change summarizes the difference to the installed version of the same skill.
type Change struct {
	CurrentVersion string   `json:"current_version"`
	CurrentHash    string   `json:"current_hash"`
	Identical      bool     `json:"identical"`
	Added          []string `json:"added"`
	Removed        []string `json:"removed"`
	Changed        []string `json:"changed"`
	More           int      `json:"more,omitempty"`
	NewExecutables []string `json:"new_executables"`
	NewRisky       []string `json:"new_risky_fields"`
	ChangedRisky   []string `json:"changed_risky_fields"`
	Note           string   `json:"note,omitempty"`
}

// Preview is the stable JSON schema of `forge skill add --preview --json`
// (schema 1).
type Preview struct {
	Schema       int          `json:"schema"`
	SourceType   string       `json:"source_type"`
	SourceURI    string       `json:"source_uri"`
	Commit       string       `json:"commit,omitempty"`
	RequestedRef string       `json:"requested_ref,omitempty"`
	RefStatus    string       `json:"ref_status,omitempty"` // pinned | moving | unpinned (git only)
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	License      string       `json:"license,omitempty"`
	Version      string       `json:"version,omitempty"`
	TreeHash     string       `json:"tree_sha256"`
	FileCount    int          `json:"file_count"`
	TotalBytes   int64        `json:"total_bytes"`
	Frontmatter  []FMItem     `json:"frontmatter"`
	Executables  []ExecItem   `json:"executables"`
	ExecMore     int          `json:"executables_more,omitempty"`
	Hidden       []string     `json:"hidden_files"`
	HiddenMore   int          `json:"hidden_files_more,omitempty"`
	Binary       []string     `json:"binary_files"`
	BinaryMore   int          `json:"binary_files_more,omitempty"`
	Patterns     []PatternHit `json:"patterns"`
	URLs         []string     `json:"urls"`
	IPs          []string     `json:"ips"`
	Existing     *Change      `json:"existing,omitempty"`
	Warnings     []string     `json:"warnings"`
	Notes        []string     `json:"notes"`
}

// Installed describes the installed version a new tree is compared with.
type Installed struct {
	Version string
	Hash    string
	Root    string // snapshot directory
}

// HeuristicNote must accompany every pattern list.
const HeuristicNote = "위험 패턴은 SKILL.md 텍스트에 대한 단순 휴리스틱입니다. 정확한 탐지가 아니며, 아무것도 발견되지 않아도 안전하다는 뜻이 아닙니다. 스크립트 내용은 검사하지 않습니다."

func safe(s string, max int) string {
	return textsafe.Display(strings.Join(strings.Fields(s), " "), max)
}

func safePath(s string) string { return textsafe.Display(s, 200) }

type fileEntry struct {
	Size int64
	Exec bool
	SHA  string
}

// manifest lists the regular files below root with size, exec bit and sha256.
// It refuses symlinks/special files and more than limit files.
func manifest(root string, limit int) (map[string]fileEntry, error) {
	out := map[string]fileEntry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("일반 파일이 아닌 항목: %s", filepath.Base(p))
		}
		if len(out) >= limit {
			return fmt.Errorf("파일이 너무 많습니다(>%d)", limit)
		}
		rel, _ := filepath.Rel(root, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = fileEntry{Size: info.Size(), Exec: info.Mode()&0o111 != 0, SHA: hex.EncodeToString(h.Sum(nil))}
		return nil
	})
	return out, err
}

var scriptExt = map[string]string{
	".sh": "sh", ".bash": "bash", ".zsh": "zsh", ".py": "python", ".js": "node", ".mjs": "node", ".cjs": "node",
	".rb": "ruby", ".pl": "perl", ".ps1": "powershell", ".bat": "cmd", ".cmd": "cmd", ".php": "php", ".lua": "lua",
}

func probe(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, probeBytes)
	n, _ := io.ReadFull(f, b)
	return b[:n]
}

func looksBinary(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return !utf8.Valid(b[:len(b)-trailingPartialRune(b)])
}

// trailingPartialRune is the number of bytes of a rune cut by the probe size.
func trailingPartialRune(b []byte) int {
	for i := 1; i <= 3 && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return i
			}
			return 0
		}
	}
	return 0
}

func shebang(b []byte) string {
	if len(b) < 2 || b[0] != '#' || b[1] != '!' {
		return ""
	}
	line := b[2:]
	if i := strings.IndexByte(string(line), '\n'); i >= 0 {
		line = line[:i]
	}
	if len(line) > 120 {
		line = line[:120]
	}
	return strings.TrimSpace(string(line))
}

func normKey(k string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(k)), "_", "-")
}

func fmValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

type patt struct {
	kind string
	re   *regexp.Regexp
}

var riskPatterns = []patt{
	{"curl", regexp.MustCompile(`(?i)\bcurl\b`)},
	{"wget", regexp.MustCompile(`(?i)\bwget\b`)},
	{"파이프로 셸 실행 (| sh, | bash)", regexp.MustCompile(`\|\s*(?:sudo\s+)?(?:ba|z|da)?sh\b`)},
	{"eval", regexp.MustCompile(`\beval\b`)},
	{"base64 -d", regexp.MustCompile(`base64\s+(?:-d|-D|--decode)\b`)},
	{"chmod +x", regexp.MustCompile(`\bchmod\s+(?:\S*\+x|[0-7]{3,4})\b`)},
	{"rm -rf", regexp.MustCompile(`\brm\s+-[a-zA-Z]*(?:rf|fr)\b|\brm\s+-[a-zA-Z]*r[a-zA-Z]*\s+-[a-zA-Z]*f`)},
	{"sudo", regexp.MustCompile(`\bsudo\b`)},
	{"ssh", regexp.MustCompile(`\bssh\b`)},
	{"nc (netcat)", regexp.MustCompile(`\bnc\s`)},
}

var (
	urlRe  = regexp.MustCompile(`https?://[^\s<>)"'\]]+`)
	ipv4Re = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

func validIPv4(s string) bool {
	for _, p := range strings.Split(s, ".") {
		n := 0
		for _, c := range p {
			n = n*10 + int(c-'0')
		}
		if n > 255 || len(p) > 3 {
			return false
		}
	}
	return true
}

// scanText runs the heuristics over SKILL.md.
func scanText(text string, pv *Preview) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	hits := map[string]*PatternHit{}
	order := []string{}
	urls := map[string]bool{}
	ips := map[string]bool{}
	for i, ln := range lines {
		for _, p := range riskPatterns {
			if p.re.MatchString(ln) {
				h := hits[p.kind]
				if h == nil {
					h = &PatternHit{Kind: p.kind, FirstLine: i + 1, Example: safe(ln, maxExample)}
					hits[p.kind] = h
					order = append(order, p.kind)
				}
				h.Count++
			}
		}
		for _, u := range urlRe.FindAllString(ln, -1) {
			urls[strings.TrimRight(u, ".,;:")] = true
		}
		for _, ip := range ipv4Re.FindAllString(ln, -1) {
			if validIPv4(ip) {
				ips[ip] = true
			}
		}
	}
	for _, k := range order {
		pv.Patterns = append(pv.Patterns, *hits[k])
	}
	pv.URLs = capSorted(urls, maxURLs, func(u string) string { return safe(maskURLText(u), 160) })
	pv.IPs = capSorted(ips, maxURLs, func(s string) string { return safe(s, 40) })
}

func capSorted(set map[string]bool, max int, f func(string) string) []string {
	all := make([]string, 0, len(set))
	for k := range set {
		all = append(all, k)
	}
	sort.Strings(all)
	out := []string{}
	for i, k := range all {
		if i == max {
			out = append(out, fmt.Sprintf("… 외 %d건", len(all)-max))
			break
		}
		out = append(out, f(k))
	}
	return out
}

// MaskURI hides credentials in a URI/URL ("user:pass@host" -> "user:***@host",
// a bare "token@" user of an https URL -> "***@") and drops query strings'
// values. Anything that does not parse as a URL is returned unchanged.
func MaskURI(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		if err == nil && u.RawQuery != "" {
			u.RawQuery = "***"
			return u.String()
		}
		return raw
	}
	user := u.User.Username()
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(user, "***")
	} else if u.Scheme != "ssh" {
		u.User = url.User("***")
	}
	if u.RawQuery != "" {
		u.RawQuery = "***"
	}
	return u.String()
}

func maskURLText(s string) string { return MaskURI(s) }

// BuildPreview computes the preview of a prepared skill. hash is the tree hash
// (HashTree of p.Root); old (optional) is the installed version of the same
// skill to summarize the change against.
func BuildPreview(p *Prepared, hash string, old *Installed) (*Preview, error) {
	files, err := manifest(p.Root, DefaultLimits.MaxFiles+1)
	if err != nil {
		return nil, fmt.Errorf("미리보기 계산 실패: %w", err)
	}
	pv := &Preview{
		Schema: 1, SourceType: p.SourceType, SourceURI: safe(MaskURI(p.SourceURI), 300),
		Commit: safe(p.Commit, 80), RequestedRef: safe(p.RequestedRef, 200),
		Name: safe(p.Meta.Name, 80), Description: safe(p.Meta.Description, 200), Version: safe(p.Meta.Version, 64),
		TreeHash: hash, FileCount: len(files),
		Frontmatter: []FMItem{}, Executables: []ExecItem{}, Hidden: []string{}, Binary: []string{},
		Patterns: []PatternHit{}, URLs: []string{}, IPs: []string{}, Warnings: []string{}, Notes: []string{},
	}
	if p.SourceType == SourceGit {
		switch {
		case p.RequestedRef == "":
			pv.RefStatus = "unpinned"
		case IsCommitHash(p.RequestedRef):
			pv.RefStatus = "pinned"
		default:
			pv.RefStatus = "moving"
		}
	}
	paths := make([]string, 0, len(files))
	for k, f := range files {
		paths = append(paths, k)
		pv.TotalBytes += f.Size
	}
	sort.Strings(paths)

	// frontmatter
	if l, ok := p.Meta.Frontmatter["license"].(string); ok {
		pv.License = safe(l, 80)
	}
	keys := make([]string, 0, len(p.Meta.Frontmatter))
	for k := range p.Meta.Frontmatter {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var riskyKeys []string
	for _, k := range keys {
		it := FMItem{Key: safe(k, 60), Value: safe(fmValue(p.Meta.Frontmatter[k]), maxPreviewValue)}
		if why, ok := RiskyFrontmatterKeys[normKey(k)]; ok {
			it.Risky, it.Why = true, why
			riskyKeys = append(riskyKeys, normKey(k))
		}
		pv.Frontmatter = append(pv.Frontmatter, it)
	}

	// executables, hidden, binary
	for _, rel := range paths {
		f := files[rel]
		full := filepath.Join(p.Root, filepath.FromSlash(rel))
		head := probe(full)
		var reasons []string
		interp := ""
		if f.Exec {
			reasons = append(reasons, "실행 비트")
		}
		if sb := shebang(head); sb != "" {
			reasons = append(reasons, "셔뱅(#!)")
			interp = sb
		}
		if lang, ok := scriptExt[strings.ToLower(filepath.Ext(rel))]; ok {
			reasons = append(reasons, "스크립트 확장자")
			if interp == "" {
				interp = lang + " (확장자 기준)"
			}
		}
		if len(reasons) > 0 {
			if len(pv.Executables) < maxPreviewList {
				pv.Executables = append(pv.Executables, ExecItem{Path: safePath(rel), Size: f.Size, SHA256: f.SHA,
					Interpreter: safe(interp, 120), Reasons: reasons})
			} else {
				pv.ExecMore++
			}
		}
		for _, part := range strings.Split(rel, "/") {
			if strings.HasPrefix(part, ".") {
				if len(pv.Hidden) < maxPreviewList {
					pv.Hidden = append(pv.Hidden, safePath(rel))
				} else {
					pv.HiddenMore++
				}
				break
			}
		}
		if looksBinary(head) {
			if len(pv.Binary) < maxPreviewList {
				pv.Binary = append(pv.Binary, safePath(rel))
			} else {
				pv.BinaryMore++
			}
		}
	}

	// SKILL.md heuristics
	if b, err := os.ReadFile(filepath.Join(p.Root, "SKILL.md")); err == nil && len(b) <= MaxSkillMDSize {
		scanText(string(b), pv)
	}

	pv.Notes = append(pv.Notes,
		"심볼릭 링크·특수 파일·경로 이탈·과도한 파일 수/크기는 가져오기 단계에서 이미 거부됩니다 (이 소스에는 없음).",
		HeuristicNote)

	// existing version
	if old != nil && old.Root != "" {
		pv.Existing = diffInstalled(p, files, hash, old)
	}

	// headline warnings
	for _, k := range riskyKeys {
		if k == "hooks" {
			pv.Warnings = append(pv.Warnings, "frontmatter 에 hooks 가 있습니다: 이 스킬을 설치·동기화하면 클라이언트가 스킬이 활성화될 때 명령을 실행할 수 있습니다. 내용을 직접 읽고 신뢰할 때만 승인하세요.")
		}
	}
	var other []string
	for _, k := range riskyKeys {
		if k != "hooks" {
			other = append(other, k)
		}
	}
	if len(other) > 0 {
		pv.Warnings = append(pv.Warnings, "클라이언트 동작을 바꾸는 frontmatter 필드: "+strings.Join(other, ", "))
	}
	if n := len(pv.Executables) + pv.ExecMore; n > 0 {
		pv.Warnings = append(pv.Warnings, fmt.Sprintf("실행 가능한 파일 %d개 (스크립트 내용은 검사하지 않음 — 직접 읽어 보세요)", n))
	}
	switch pv.RefStatus {
	case "unpinned":
		pv.Warnings = append(pv.Warnings, "고정되지 않음: 기본 브랜치의 현재 HEAD 입니다. 같은 URL 이 나중에 다른 내용을 줄 수 있습니다 (--ref <40자 커밋> 으로 고정).")
	case "moving":
		pv.Warnings = append(pv.Warnings, "고정되지 않음: 브랜치/태그 ref 는 이동할 수 있습니다. 40자 커밋 해시로 고정하세요.")
	}
	if len(pv.Patterns)+len(pv.URLs)+len(pv.IPs) > 0 {
		pv.Warnings = append(pv.Warnings, "SKILL.md 에 URL/IP 또는 위험해 보이는 명령 패턴이 있습니다 (휴리스틱 — 아래 목록 확인)")
	}
	if len(pv.Hidden)+pv.HiddenMore > 0 {
		pv.Warnings = append(pv.Warnings, "숨김 파일이 있습니다")
	}
	if len(pv.Binary)+pv.BinaryMore > 0 {
		pv.Warnings = append(pv.Warnings, "텍스트가 아닌(바이너리) 파일이 있습니다")
	}
	if pv.Existing != nil {
		if len(pv.Existing.NewExecutables) > 0 {
			pv.Warnings = append(pv.Warnings, "기존 버전에 없던 실행 가능한 파일이 새로 생겼습니다")
		}
		if len(pv.Existing.NewRisky) > 0 {
			pv.Warnings = append(pv.Warnings, "기존 버전에 없던 위험 frontmatter 필드가 새로 생겼습니다: "+strings.Join(pv.Existing.NewRisky, ", "))
		}
	}
	return pv, nil
}

func isExec(rel string, f fileEntry, head []byte) bool {
	if f.Exec || shebang(head) != "" {
		return true
	}
	_, ok := scriptExt[strings.ToLower(filepath.Ext(rel))]
	return ok
}

func diffInstalled(p *Prepared, nw map[string]fileEntry, hash string, old *Installed) *Change {
	ch := &Change{CurrentVersion: safe(old.Version, 64), CurrentHash: old.Hash,
		Added: []string{}, Removed: []string{}, Changed: []string{}, NewExecutables: []string{}, NewRisky: []string{}, ChangedRisky: []string{}}
	of, err := manifest(old.Root, DefaultLimits.MaxFiles+1)
	if err != nil {
		ch.Note = "설치된 버전의 파일을 읽을 수 없어 비교하지 못했습니다 (저장소 손상 가능: forge skill check)"
		return ch
	}
	if old.Hash == hash {
		ch.Identical = true
		return ch
	}
	var add, rem, chg []string
	for k, f := range nw {
		o, ok := of[k]
		switch {
		case !ok:
			add = append(add, k)
		case o.SHA != f.SHA || o.Exec != f.Exec:
			chg = append(chg, k)
		}
		if isExec(k, f, probe(filepath.Join(p.Root, filepath.FromSlash(k)))) {
			if !ok || !isExec(k, o, probe(filepath.Join(old.Root, filepath.FromSlash(k)))) {
				ch.NewExecutables = append(ch.NewExecutables, safePath(k))
			}
		}
	}
	for k := range of {
		if _, ok := nw[k]; !ok {
			rem = append(rem, k)
		}
	}
	sort.Strings(add)
	sort.Strings(rem)
	sort.Strings(chg)
	sort.Strings(ch.NewExecutables)
	cp := func(in []string) []string {
		out := []string{}
		for _, s := range in {
			if len(out) < maxPreviewList {
				out = append(out, safePath(s))
			} else {
				ch.More++
			}
		}
		return out
	}
	ch.Added, ch.Removed, ch.Changed = cp(add), cp(rem), cp(chg)
	if len(ch.NewExecutables) > maxPreviewList {
		ch.NewExecutables = append(ch.NewExecutables[:maxPreviewList], "…")
	}
	// risky frontmatter versus the installed SKILL.md
	if b, err := os.ReadFile(filepath.Join(old.Root, "SKILL.md")); err == nil {
		if om, err := ParseSkillMD(b); err == nil {
			oldVals := map[string]string{}
			for k, v := range om.Frontmatter {
				if _, risky := RiskyFrontmatterKeys[normKey(k)]; risky {
					oldVals[normKey(k)] = fmValue(v)
				}
			}
			for k, v := range p.Meta.Frontmatter {
				nk := normKey(k)
				if _, risky := RiskyFrontmatterKeys[nk]; !risky {
					continue
				}
				if ov, had := oldVals[nk]; !had {
					ch.NewRisky = append(ch.NewRisky, nk)
				} else if ov != fmValue(v) {
					ch.ChangedRisky = append(ch.ChangedRisky, nk)
				}
			}
			sort.Strings(ch.NewRisky)
			sort.Strings(ch.ChangedRisky)
		}
	}
	return ch
}
