package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/as950118/forge/internal/ledger"
	"github.com/as950118/forge/internal/mcp"
	"github.com/as950118/forge/internal/textsafe"
)

// FileDiff is the masked unified diff of one file.
type FileDiff struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
	// SecretChanges counts changed lines whose only difference is a masked secret
	// value: the diff cannot show them (both sides read "***"), so it says so.
	SecretChanges int `json:"secret_changes,omitempty"`
}

// Display limits: diffs are for humans, never a data channel.
const (
	maxDiffLineBytes  = 240
	maxDiffLinesFile  = 300
	maxDiffLinesTotal = 1500
	maxDiffFileBytes  = 256 << 10
	diffContext       = 3
)

// BuildDiffs fills p.Diffs from the operations. Every line is masked for
// secrets and escaped for terminals; nothing here is written anywhere.
func BuildDiffs(p *Plan) {
	p.Diffs = nil
	total := 0
	add := func(path string, before, after []byte, hasBefore, hasAfter bool) {
		if total >= maxDiffLinesTotal {
			return
		}
		d, hidden := unifiedDiff(path, before, after, hasBefore, hasAfter)
		if d == "" {
			return
		}
		total += strings.Count(d, "\n")
		p.Diffs = append(p.Diffs, FileDiff{Path: textsafe.Display(path, 300), Diff: d, SecretChanges: hidden})
	}
	for _, op := range p.Ops {
		switch op.Kind {
		case OpWriteFile:
			add(op.Path, op.BeforeData, op.Data, op.Before.Exists, true)
		case OpDeleteFile:
			add(op.Path, op.BeforeData, nil, true, false)
		case OpReplaceTree, OpDeleteTree:
			before := readTreeFiles(op.Path, op.Before.Exists)
			after := map[string][]byte{}
			if op.Kind == OpReplaceTree {
				after = readTreeFiles(op.Tree.SrcDir, true)
				for n, b := range op.Tree.Extra {
					after[n] = b
				}
			}
			rels := map[string]bool{}
			for r := range before {
				rels[r] = true
			}
			for r := range after {
				rels[r] = true
			}
			names := make([]string, 0, len(rels))
			for r := range rels {
				names = append(names, r)
			}
			sort.Strings(names)
			for _, r := range names {
				b, hb := before[r]
				a, ha := after[r]
				if hb && ha && bytes.Equal(b, a) {
					continue
				}
				add(filepath.Join(op.Path, filepath.FromSlash(r)), b, a, hb, ha)
			}
		}
	}
}

// readTreeFiles reads the regular files of a directory (best effort; sizes capped).
func readTreeFiles(root string, exists bool) map[string][]byte {
	out := map[string][]byte{}
	if !exists {
		return out
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == MarkerName {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if linkCount(fi) > 1 {
			// Another name for the same content (maybe ~/.ssh/...): never read it.
			out[rel] = []byte(hardLinkedMark)
			return nil
		}
		if fi.Size() > maxDiffFileBytes {
			out[rel] = []byte(fmt.Sprintf("\x00large:%d", fi.Size()))
			return nil
		}
		if b, err := os.ReadFile(p); err == nil {
			out[rel] = b
		}
		return nil
	})
	return out
}

// hardLinkedMark stands in for the content of a skill-tree file that has more than
// one hard link (the file is not read; the NUL byte makes it "not text").
const hardLinkedMark = "\x00hardlinked"

func isText(b []byte) bool {
	return len(b) <= maxDiffFileBytes && !bytes.Contains(b, []byte{0}) && utf8.Valid(b)
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
	raw  string // text before display masking (non-JSON files; used to count hidden secret edits)
}

func unifiedDiff(path string, before, after []byte, hasBefore, hasAfter bool) (string, int) {
	var sb strings.Builder
	from, to := path, path
	if !hasBefore {
		from = "/dev/null"
	}
	if !hasAfter {
		to = "/dev/null"
	}
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", textsafe.Display(from, 300), textsafe.Display(to, 300))
	if !isText(before) || !isText(after) {
		if string(before) == hardLinkedMark || string(after) == hardLinkedMark {
			sb.WriteString("(하드링크된 파일이라 내용을 읽지 않고 비교를 생략합니다)\n")
		} else {
			sb.WriteString("(텍스트가 아니거나 너무 커서 내용 비교를 생략합니다)\n")
		}
		return sb.String(), 0
	}
	isJSON := strings.HasSuffix(path, ".json") || filepath.Base(path) == MarkerName
	rawBefore, rawAfter := before, after
	if isJSON {
		// Mask by JSON value before diffing (see MaskJSONDisplay): the display copies
		// keep every line, so context lines of a one-line user file are covered too.
		before, after = []byte(MaskJSONDisplay(string(before))), []byte(MaskJSONDisplay(string(after)))
	}
	ops := diffLines(splitLines(before), splitLines(after))
	hidden := 0
	if isJSON {
		hidden = hiddenSecretChanges(rawBefore, rawAfter, ops)
	}
	same := 0 // changed lines that stay in the diff but read the same on both sides
	secretNote := func() {
		if isJSON && hidden-same > 0 {
			fmt.Fprintf(&sb, "(비밀 값 변경: %d줄 — 값은 가려져 위 diff 에 나타나지 않습니다)\n", hidden-same)
		}
		if same > 0 {
			fmt.Fprintf(&sb, "(비밀 값 변경: %d줄 — 값이 가려져 변경 전후가 같게 보입니다)\n", same)
		}
	}
	masker := newMasker(isJSON)
	for i := range ops {
		ops[i].raw = ops[i].text
		ops[i].text = masker.line(ops[i].text)
	}
	// The changed lines stay in the diff but read the same on both sides: text
	// files, and JSON files whose secret sits inside a string the key-based
	// masking does not cover (`"command": "mysql -p<secret>"`, a URL userinfo).
	same = maskedEqualChanges(ops)
	hidden += same
	// hunks
	n := len(ops)
	keep := make([]bool, n)
	for i, o := range ops {
		if o.kind != ' ' {
			for j := i - diffContext; j <= i+diffContext; j++ {
				if j >= 0 && j < n {
					keep[j] = true
				}
			}
		}
	}
	lines := 0
	oldN, newN := 1, 1
	i := 0
	for i < n {
		if !keep[i] {
			if ops[i].kind != '+' {
				oldN++
			}
			if ops[i].kind != '-' {
				newN++
			}
			i++
			continue
		}
		j := i
		oc, nc := 0, 0
		for j < n && keep[j] {
			if ops[j].kind != '+' {
				oc++
			}
			if ops[j].kind != '-' {
				nc++
			}
			j++
		}
		os0, ns0 := oldN, newN
		if oc == 0 {
			os0--
		}
		if nc == 0 {
			ns0--
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", os0, oc, ns0, nc)
		for k := i; k < j; k++ {
			if lines >= maxDiffLinesFile {
				fmt.Fprintf(&sb, "(… 이후 줄 생략: 파일당 %d줄 제한)\n", maxDiffLinesFile)
				secretNote()
				return sb.String(), hidden
			}
			sb.WriteString(string(ops[k].kind))
			sb.WriteString(textsafe.Display(ops[k].text, maxDiffLineBytes))
			sb.WriteString("\n")
			lines++
		}
		oldN += oc
		newN += nc
		i = j
	}
	secretNote()
	return sb.String(), hidden
}

// maskedEqualChanges counts, per run of changed lines, the removed lines that
// have an added line reading the same after masking although the real text
// differs: a secret value changed in a file that is not JSON (TASK-0027 AC-18).
func maskedEqualChanges(ops []diffOp) int {
	count := 0
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		j := i
		for j < len(ops) && ops[j].kind != ' ' {
			j++
		}
		used := map[int]bool{}
		for a := i; a < j; a++ {
			if ops[a].kind != '-' {
				continue
			}
			for b := i; b < j; b++ {
				if ops[b].kind == '+' && !used[b] && ops[b].text == ops[a].text && ops[b].raw != ops[a].raw {
					used[b] = true
					count++
					break
				}
			}
		}
		i = j
	}
	return count
}

// hiddenSecretChanges counts the changed lines of the real (unmasked) diff that
// the masked diff does not contain: lines that differ only in a secret value
// become identical once masked and so vanish from the diff.
//
// Only the removed lines are counted: both diffs describe the same two files
// (the masked copy has the same line count), so (removed - added) is the same in
// both and the difference of the added lines would always equal the difference of
// the removed ones (TASK-0027 AC-16: the former "maximum of both" was dead code).
func hiddenSecretChanges(rawBefore, rawAfter []byte, maskedOps []diffOp) int {
	count := func(ops []diffOp) (minus int) {
		for _, o := range ops {
			if o.kind == '-' {
				minus++
			}
		}
		return
	}
	h := count(diffLines(splitLines(rawBefore), splitLines(rawAfter))) - count(maskedOps)
	if h < 0 {
		return 0
	}
	return h
}

// diffLines is an LCS line diff with common prefix/suffix trimming. Very large
// inputs fall back to "remove all, add all".
func diffLines(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var out []diffOp
	for _, l := range a[:pre] {
		out = append(out, diffOp{kind: ' ', text: l})
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(ma)*len(mb) > 4_000_000 {
		for _, l := range ma {
			out = append(out, diffOp{kind: '-', text: l})
		}
		for _, l := range mb {
			out = append(out, diffOp{kind: '+', text: l})
		}
	} else {
		w := len(mb) + 1
		dp := make([]int32, (len(ma)+1)*w)
		for i := len(ma) - 1; i >= 0; i-- {
			for j := len(mb) - 1; j >= 0; j-- {
				if ma[i] == mb[j] {
					dp[i*w+j] = dp[(i+1)*w+j+1] + 1
				} else if dp[(i+1)*w+j] >= dp[i*w+j+1] {
					dp[i*w+j] = dp[(i+1)*w+j]
				} else {
					dp[i*w+j] = dp[i*w+j+1]
				}
			}
		}
		i, j := 0, 0
		for i < len(ma) && j < len(mb) {
			switch {
			case ma[i] == mb[j]:
				out = append(out, diffOp{kind: ' ', text: ma[i]})
				i++
				j++
			case dp[(i+1)*w+j] >= dp[i*w+j+1]:
				out = append(out, diffOp{kind: '-', text: ma[i]})
				i++
			default:
				out = append(out, diffOp{kind: '+', text: mb[j]})
				j++
			}
		}
		for ; i < len(ma); i++ {
			out = append(out, diffOp{kind: '-', text: ma[i]})
		}
		for ; j < len(mb); j++ {
			out = append(out, diffOp{kind: '+', text: mb[j]})
		}
	}
	for _, l := range a[len(a)-suf:] {
		out = append(out, diffOp{kind: ' ', text: l})
	}
	return out
}

// ---- secret masking for displayed lines ----

var (
	kvLineRe  = regexp.MustCompile(`^(\s*"((?:[^"\\]|\\.)*)"\s*:\s*)"((?:[^"\\]|\\.)*)"(.*)$`)
	strLitRe  = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	wholeRef  = regexp.MustCompile(`^(?:\$\{[A-Za-z_][A-Za-z0-9_]*\}|\{env:[A-Za-z_][A-Za-z0-9_]*\})$`)
	hashRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	flagKeyRe = regexp.MustCompile(`(?i)^--?[a-z0-9-]*(token|secret|passw|api-?key|auth|cookie|credential|bearer)[a-z0-9-]*$`)
)

const maskedValue = "***"

var (
	camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	plainFlagName = regexp.MustCompile(`^--?[A-Za-z0-9_.-]+$`)
)

// benignDisplayKeys contain a secret-looking substring (AUTH, KEY) but are ordinary
// words: `authors` / `keywords` of a package or skill description must stay readable.
// A real secret value under such a key is still caught by the value-pattern rules.
// Display only; the registry-side rule (mcp.LooksSecret) is not affected.
var benignDisplayKeys = map[string]bool{"author": true, "authors": true, "authority": true, "keyword": true, "keywords": true}

// shortSecretWords are secret key/flag words too short for the substring rule of
// mcp.LooksSecret: they only count as a whole word (`pwd`, `db_pw`, `rootPass`;
// not `bypass`).
//
// "passphrase" and "pin" (TASK-0027 AC-18) are whole-word matches too: `passphrase`
// has no PASSWORD/KEY substring, and `pin` must not hide `pinned` or `mapping`.
var shortSecretWords = map[string]bool{"pw": true, "pwd": true, "pass": true, "passcode": true, "passphrase": true, "pin": true}

func keyWords(s string) []string {
	s = camelBoundary.ReplaceAllString(s, "$1-$2")
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

// secretDisplayKey reports whether the value under this JSON key is masked in a
// diff: the registry's key-name rule, plus short names (`pwd`, `pw`), minus the
// ordinary words of benignDisplayKeys.
func secretDisplayKey(key string) bool {
	if benignDisplayKeys[strings.ToLower(key)] {
		return false
	}
	if mcp.LooksSecret(key) {
		return true
	}
	for _, w := range keyWords(key) {
		if shortSecretWords[w] {
			return true
		}
	}
	return false
}

// secretFlag reports whether the argument after this array element is a secret:
// "--token", "--api-key", "--pwd", and "-p" (docker/mysql style password flag;
// a port after "-p" is masked too - display only).
func secretFlag(s string) bool {
	if !strings.HasPrefix(s, "-") || strings.Contains(s, "=") {
		return false
	}
	if flagKeyRe.MatchString(s) || strings.EqualFold(s, "-p") {
		return true
	}
	for _, w := range keyWords(strings.TrimLeft(s, "-")) {
		if shortSecretWords[w] {
			return true
		}
	}
	return false
}

// gluedShortFlagRe matches a short password flag with its value glued on
// ("-pSECRET", mysql style), in an array element or inside a command string.
// "-pthread" style compiler flags are the one ordinary word kept readable.
// Display only: over-masking is preferred over a leak (as with "-p 22").
var gluedShortFlagRe = regexp.MustCompile(`(^|\s)(-p)([^\s"'=-]\S*)`)

// gluedKeep are ordinary words that follow a single dash "-p" (compiler and find
// options); everything else glued to "-p" is treated as a password.
var gluedKeep = map[string]bool{"-pthread": true, "-print": true, "-print0": true, "-perm": true, "-prune": true}

func maskGluedShortFlag(s string) string {
	return gluedShortFlagRe.ReplaceAllStringFunc(s, func(m string) string {
		sm := gluedShortFlagRe.FindStringSubmatch(m)
		if gluedKeep[sm[2]+sm[3]] {
			return m
		}
		return sm[1] + sm[2] + maskedValue
	})
}

// maskInlineDisplay is the inline credential masking of a displayed string:
// the shared rules (ledger.MaskInline) plus the glued short flag.
func maskInlineDisplay(s string) string {
	return unprotectP(ledger.MaskInline(maskGluedShortFlag(protectP(s))))
}

// harmlessPCmd matches well-known commands whose "-p" is not a password
// (`mkdir -p dir`, `cp -p a b`, `ls -p`, `install -p`): the flag is hidden from
// the credential patterns for the duration of the masking. Any other command
// (`mysql -p X`, an unknown tool) is still masked.
var harmlessPCmd = regexp.MustCompile(`(^|[\s;&|"'(])((?:mkdir|cp|ls|install|touch|rsync|mv)\s+(?:-[A-Za-z]+\s+(?:[0-9][0-9A-Za-z]*\s+)?)*)-p(\s|$)`)

const pGuard = "\x01"

func protectP(s string) string { return harmlessPCmd.ReplaceAllString(s, "${1}${2}-"+pGuard+"p${3}") }

func unprotectP(s string) string { return strings.ReplaceAll(s, pGuard, "") }

// maskFlagValue masks the value of a one-string "--flag=value" argument whose flag
// takes a secret ("--pwd=hunter2"); the references ${VAR}/{env:VAR} stay.
func maskFlagValue(s string) (string, bool) {
	name, val, ok := strings.Cut(s, "=")
	if !ok || val == "" || wholeRef.MatchString(val) || !plainFlagName.MatchString(name) || !secretFlag(name) {
		return "", false
	}
	return name + "=" + maskedValue, true
}

// MaskJSONDisplay returns a display copy of JSON text in which every string VALUE
// whose key name looks secret (TOKEN, SECRET, PASSWORD, KEY, AUTH, COOKIE,
// CREDENTIAL ..., any case, any spelling such as camelCase) is replaced by "***",
// at any nesting depth and also inside arrays below such a key, and in which the
// element after a secret-taking flag ("--token", "x") is masked. It works on the
// token stream, not on lines, so a minified one-line file is covered like a
// pretty-printed one, and it only ever rewrites the inside of a string literal:
// the number of lines is unchanged (the copy lines up 1:1 with the original).
// It is tolerant of invalid JSON and is used for display only; what is written
// to disk is never taken from it.
func MaskJSONDisplay(src string) string {
	type frame struct {
		obj       bool
		expectKey bool
		key       string // object: the key of the current member; array: the key it inherits
		maskNext  bool   // array: the next string follows a secret-taking flag
		secret    bool   // the whole container lies below a secret-looking key: every string/number in it is masked
	}
	stack := []*frame{{}}
	var out strings.Builder
	out.Grow(len(src))
	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		switch c {
		case '"':
			j := i + 1
			for j < n && src[j] != '"' {
				if src[j] == '\\' && j+1 < n {
					j++
				}
				j++
			}
			if j >= n { // unterminated: leave the rest to the line masker
				out.WriteString(src[i:])
				return out.String()
			}
			content := src[i+1 : j]
			top := stack[len(stack)-1]
			if top.obj && top.expectKey {
				top.key, top.expectKey = unescapeJSONString(content), false
				out.WriteString(src[i : j+1])
			} else {
				mask := false
				switch {
				case top.maskNext:
					mask, top.maskNext = true, false
				case top.secret || (top.key != "" && secretDisplayKey(top.key)):
					mask = true
				}
				if !top.obj && !mask {
					u := unescapeJSONString(content)
					if repl, ok := maskFlagValue(u); ok {
						out.WriteString(`"` + repl + `"`)
						i = j + 1
						continue
					}
					if g := maskGluedShortFlag(u); g != u && !strings.ContainsAny(u, " \t") {
						out.WriteString(`"` + g + `"`)
						i = j + 1
						continue
					}
					top.maskNext = secretFlag(u)
				}
				if mask && (wholeRef.MatchString(content) || strings.ContainsAny(content, "\r\n")) {
					mask = false
				}
				if mask {
					out.WriteString(`"` + maskedValue + `"`)
				} else {
					out.WriteString(src[i : j+1])
				}
			}
			i = j + 1
			continue
		case '{':
			top := stack[len(stack)-1]
			stack = append(stack, &frame{obj: true, expectKey: true, secret: top.secret || (top.key != "" && secretDisplayKey(top.key))})
		case '[':
			top := stack[len(stack)-1]
			stack = append(stack, &frame{key: top.key, secret: top.secret || (top.key != "" && secretDisplayKey(top.key))})
		case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			// A number inside a secret subtree (a PIN, a code) is masked as well. The
			// token is replaced by a string literal: display only, one line stays one line.
			// Also a number that is the direct value of a secret-looking key ("pin": 1234).
			if top := stack[len(stack)-1]; !(top.obj && top.expectKey) && (top.secret || (top.obj && top.key != "" && secretDisplayKey(top.key))) {
				j := i + 1
				for j < n && strings.IndexByte("0123456789+-.eE", src[j]) >= 0 {
					j++
				}
				out.WriteString(`"` + maskedValue + `"`)
				i = j
				continue
			}
		case '}', ']':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case ',':
			if top := stack[len(stack)-1]; top.obj {
				top.expectKey, top.key = true, ""
			}
		}
		out.WriteByte(c)
		i++
	}
	if strings.Count(out.String(), "\n") != strings.Count(src, "\n") {
		return src
	}
	return out.String()
}

// isFlagRef reports whether s is "--flag=${VAR}" / "--flag={env:VAR}".
func isFlagRef(s string) bool {
	name, val, ok := strings.Cut(s, "=")
	return ok && plainFlagName.MatchString(name) && wholeRef.MatchString(val)
}

func flagEqMasked(s string) string {
	r, _ := maskFlagValue(s)
	return r
}

func unescapeJSONString(content string) string {
	if !strings.Contains(content, `\`) {
		return content
	}
	var s string
	if json.Unmarshal([]byte(`"`+content+`"`), &s) != nil {
		return content
	}
	return s
}

type masker struct {
	json     bool
	maskNext bool
	prevLit  string // last string literal of the previous line (a flag like --ref or a key)
}

func newMasker(isJSON bool) *masker { return &masker{json: isJSON} }

// MaskLine masks one line of displayed text (exported for the CLI's error paths).
func MaskLine(s string) string { return newMasker(strings.Contains(s, `"`)).line(s) }

func (m *masker) line(s string) string {
	if !m.json {
		return unprotectP(ledger.MaskText(maskGluedShortFlag(protectP(s))))
	}
	if sm := kvLineRe.FindStringSubmatch(s); sm != nil {
		key, val := sm[2], sm[3]
		if secretDisplayKey(key) && !wholeRef.MatchString(val) {
			m.maskNext = false
			return sm[1] + `"` + maskedValue + `"` + sm[4]
		}
	}
	// A string element right after a secret-taking flag ("--token", "xyz").
	trimmed := strings.TrimRight(strings.TrimSpace(s), ",")
	if m.maskNext && len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"' {
		m.maskNext = false
		ind := s[:len(s)-len(strings.TrimLeft(s, " \t"))]
		tail := ""
		if strings.HasSuffix(strings.TrimSpace(s), ",") {
			tail = ","
		}
		return ind + `"` + maskedValue + `"` + tail
	}
	m.maskNext = false
	if len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"' && !strings.Contains(trimmed[1:len(trimmed)-1], `"`) {
		if inner := trimmed[1 : len(trimmed)-1]; secretFlag(inner) {
			m.maskNext = true
		}
	}
	// String literals are checked one by one. A 40/64-hex literal is an identifier
	// when it is the value of a commit/rev/sha/digest key ("commit": "...") or follows
	// a --ref/--rev/--sha256 flag element (the flag may end the previous line).
	var b strings.Builder
	last, prev := 0, m.prevLit
	m.prevLit = ""
	for _, loc := range strLitRe.FindAllStringIndex(s, -1) {
		b.WriteString(s[last:loc[0]])
		lit := s[loc[0]:loc[1]]
		inner := lit[1 : len(lit)-1]
		isValue := strings.HasPrefix(strings.TrimSpace(s[last:loc[0]]), ":")
		last = loc[1]
		switch {
		case wholeRef.MatchString(inner) || hashRe.MatchString(inner): // sha256 digests are not secrets
			b.WriteString(lit)
		case isFlagRef(inner): // "--token=${VAR}": a reference, not a secret
			b.WriteString(lit)
		// A labelled identifier (commit hash) stays as it is, but the inline
		// credential patterns still apply to it: `"url": "https://u:SECRET@h"`,
		// `"command": "mysql -p SECRET"` (TASK-0027 AC-18).
		case isValue && prev != "" && !mcp.LooksSecretKV(prev, inner):
			b.WriteString(`"` + maskInlineDisplay(inner) + `"`)
		case !isValue && prev != "" && !mcp.LooksSecretAfter(prev, inner):
			b.WriteString(`"` + maskInlineDisplay(inner) + `"`)
		case mcp.LooksSecretValue(inner):
			b.WriteString(`"` + maskedValue + `"`)
		case flagEqMasked(inner) != "":
			b.WriteString(`"` + flagEqMasked(inner) + `"`)
		default:
			b.WriteString(`"` + maskInlineDisplay(inner) + `"`)
		}
		prev = inner
	}
	b.WriteString(s[last:])
	if last > 0 {
		m.prevLit = prev
	}
	return b.String()
}
