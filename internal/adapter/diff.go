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

	"github.com/heonjinjeong/forge/internal/ledger"
	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

// FileDiff is the masked unified diff of one file.
type FileDiff struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
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
		d := unifiedDiff(path, before, after, hasBefore, hasAfter)
		if d == "" {
			return
		}
		total += strings.Count(d, "\n")
		p.Diffs = append(p.Diffs, FileDiff{Path: textsafe.Display(path, 300), Diff: d})
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
}

func unifiedDiff(path string, before, after []byte, hasBefore, hasAfter bool) string {
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
		sb.WriteString("(텍스트가 아니거나 너무 커서 내용 비교를 생략합니다)\n")
		return sb.String()
	}
	isJSON := strings.HasSuffix(path, ".json") || filepath.Base(path) == MarkerName
	if isJSON {
		// Mask by JSON value before diffing (see MaskJSONDisplay): the display copies
		// keep every line, so context lines of a one-line user file are covered too.
		before, after = []byte(MaskJSONDisplay(string(before))), []byte(MaskJSONDisplay(string(after)))
	}
	ops := diffLines(splitLines(before), splitLines(after))
	masker := newMasker(isJSON)
	for i := range ops {
		ops[i].text = masker.line(ops[i].text)
	}
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
				return sb.String()
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
	return sb.String()
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
		out = append(out, diffOp{' ', l})
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(ma)*len(mb) > 4_000_000 {
		for _, l := range ma {
			out = append(out, diffOp{'-', l})
		}
		for _, l := range mb {
			out = append(out, diffOp{'+', l})
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
				out = append(out, diffOp{' ', ma[i]})
				i++
				j++
			case dp[(i+1)*w+j] >= dp[i*w+j+1]:
				out = append(out, diffOp{'-', ma[i]})
				i++
			default:
				out = append(out, diffOp{'+', mb[j]})
				j++
			}
		}
		for ; i < len(ma); i++ {
			out = append(out, diffOp{'-', ma[i]})
		}
		for ; j < len(mb); j++ {
			out = append(out, diffOp{'+', mb[j]})
		}
	}
	for _, l := range a[len(a)-suf:] {
		out = append(out, diffOp{' ', l})
	}
	return out
}

// ---- secret masking for displayed lines ----

var (
	kvLineRe  = regexp.MustCompile(`^(\s*"((?:[^"\\]|\\.)*)"\s*:\s*)"((?:[^"\\]|\\.)*)"(.*)$`)
	strLitRe  = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	wholeRef  = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)
	hashRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	flagKeyRe = regexp.MustCompile(`(?i)^--?[a-z0-9-]*(token|secret|passw|api-?key|auth|cookie|credential|bearer)[a-z0-9-]*$`)
)

const maskedValue = "***"

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
				case top.key != "" && mcp.LooksSecret(top.key):
					mask = true
				}
				if !top.obj && !mask {
					u := unescapeJSONString(content)
					top.maskNext = flagKeyRe.MatchString(u) && !strings.Contains(u, "=")
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
			stack = append(stack, &frame{obj: true, expectKey: true})
		case '[':
			top := stack[len(stack)-1]
			stack = append(stack, &frame{key: top.key})
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
}

func newMasker(isJSON bool) *masker { return &masker{json: isJSON} }

// MaskLine masks one line of displayed text (exported for the CLI's error paths).
func MaskLine(s string) string { return newMasker(strings.Contains(s, `"`)).line(s) }

func (m *masker) line(s string) string {
	if !m.json {
		return ledger.MaskText(s)
	}
	if sm := kvLineRe.FindStringSubmatch(s); sm != nil {
		key, val := sm[2], sm[3]
		if mcp.LooksSecret(key) && !wholeRef.MatchString(val) {
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
		if inner := trimmed[1 : len(trimmed)-1]; flagKeyRe.MatchString(inner) && !strings.Contains(inner, "=") {
			m.maskNext = true
		}
	}
	return strLitRe.ReplaceAllStringFunc(s, func(lit string) string {
		inner := lit[1 : len(lit)-1]
		if wholeRef.MatchString(inner) || hashRe.MatchString(inner) { // sha256 digests are not secrets
			return lit
		}
		if mcp.LooksSecretValue(inner) {
			return `"` + maskedValue + `"`
		}
		return `"` + ledger.MaskInline(inner) + `"`
	})
}
