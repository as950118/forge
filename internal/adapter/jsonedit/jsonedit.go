// Package jsonedit edits JSON documents in place without re-serializing them:
// key order, indentation, line endings and every member the caller does not
// touch stay byte-for-byte identical. The adapters use it for user-owned files
// (.mcp.json, settings.json) where a standard map round trip would reorder keys.
//
// Only strict JSON (RFC 8259) is accepted; comments, trailing commas, a BOM or
// an empty file are parse errors, so callers can refuse to write anything.
package jsonedit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Kind is the JSON value type of a Node.
type Kind int

// Node kinds.
const (
	Object Kind = iota
	Array
	String
	Number
	Bool
	Null
)

// Member is one object member.
type Member struct {
	Key      string
	KeyStart int
	Val      *Node
}

// Node is a parsed value with its byte range [Start, End) in the source.
type Node struct {
	Kind       Kind
	Start, End int
	Members    []Member // Object
	Elems      []*Node  // Array
}

// ErrDuplicateKey is returned when an object that must be edited repeats a key.
var ErrDuplicateKey = errors.New("JSON 객체에 중복 키가 있어 안전하게 편집할 수 없습니다")

const maxDepth = 256

// Parse validates src as strict JSON and returns its root node.
func Parse(src []byte) (*Node, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return nil, errors.New("JSON 이 비어 있습니다")
	}
	if !json.Valid(src) {
		// Never echo the content: the decoder offset is enough to locate the problem.
		var v any
		off := int64(-1)
		if err := json.Unmarshal(src, &v); err != nil {
			var se *json.SyntaxError
			if errors.As(err, &se) {
				off = se.Offset
			}
		}
		if off >= 0 {
			return nil, fmt.Errorf("올바른 JSON 이 아닙니다 (주석·후행 쉼표·BOM 은 허용되지 않음; 바이트 오프셋 %d 부근)", off)
		}
		return nil, errors.New("올바른 JSON 이 아닙니다 (주석·후행 쉼표·BOM 은 허용되지 않음)")
	}
	p := &parser{src: src}
	p.skip()
	n, err := p.value(0)
	if err != nil {
		return nil, err
	}
	return n, nil
}

type parser struct {
	src []byte
	pos int
}

func (p *parser) skip() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (*Node, error) {
	if depth > maxDepth {
		return nil, errors.New("JSON 중첩이 너무 깊습니다")
	}
	start := p.pos
	switch p.src[p.pos] {
	case '{':
		n := &Node{Kind: Object, Start: start}
		p.pos++
		p.skip()
		if p.src[p.pos] == '}' {
			p.pos++
			n.End = p.pos
			return n, nil
		}
		for {
			p.skip()
			ks := p.pos
			key, err := p.str()
			if err != nil {
				return nil, err
			}
			p.skip()
			p.pos++ // ':'
			p.skip()
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			n.Members = append(n.Members, Member{Key: key, KeyStart: ks, Val: v})
			p.skip()
			if p.src[p.pos] == ',' {
				p.pos++
				continue
			}
			p.pos++ // '}'
			n.End = p.pos
			return n, nil
		}
	case '[':
		n := &Node{Kind: Array, Start: start}
		p.pos++
		p.skip()
		if p.src[p.pos] == ']' {
			p.pos++
			n.End = p.pos
			return n, nil
		}
		for {
			p.skip()
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			n.Elems = append(n.Elems, v)
			p.skip()
			if p.src[p.pos] == ',' {
				p.pos++
				continue
			}
			p.pos++ // ']'
			n.End = p.pos
			return n, nil
		}
	case '"':
		if _, err := p.str(); err != nil {
			return nil, err
		}
		return &Node{Kind: String, Start: start, End: p.pos}, nil
	case 't':
		p.pos += 4
		return &Node{Kind: Bool, Start: start, End: p.pos}, nil
	case 'f':
		p.pos += 5
		return &Node{Kind: Bool, Start: start, End: p.pos}, nil
	case 'n':
		p.pos += 4
		return &Node{Kind: Null, Start: start, End: p.pos}, nil
	default:
		for p.pos < len(p.src) && strings.IndexByte("+-0123456789.eE", p.src[p.pos]) >= 0 {
			p.pos++
		}
		return &Node{Kind: Number, Start: start, End: p.pos}, nil
	}
}

// str scans a string literal at pos (the input is already known to be valid).
func (p *parser) str() (string, error) {
	start := p.pos
	p.pos++
	for p.src[p.pos] != '"' {
		if p.src[p.pos] == '\\' {
			p.pos++
		}
		p.pos++
	}
	p.pos++
	var s string
	if err := json.Unmarshal(p.src[start:p.pos], &s); err != nil {
		return "", err
	}
	return s, nil
}

// Get returns the member named key. ok=false when absent; a repeated key is an error.
func (n *Node) Get(key string) (*Node, bool, error) {
	if n.Kind != Object {
		return nil, false, errors.New("객체가 아닙니다")
	}
	var found *Node
	for _, m := range n.Members {
		if m.Key == key {
			if found != nil {
				return nil, false, fmt.Errorf("%w: %q", ErrDuplicateKey, key)
			}
			found = m.Val
		}
	}
	return found, found != nil, nil
}

// Raw returns the source bytes of the node.
func (n *Node) Raw(src []byte) []byte { return src[n.Start:n.End] }

// StringValue decodes a String node.
func (n *Node) StringValue(src []byte) (string, bool) {
	if n.Kind != String {
		return "", false
	}
	var s string
	if err := json.Unmarshal(src[n.Start:n.End], &s); err != nil {
		return "", false
	}
	return s, true
}

// Path addresses a value: string elements are object keys, int elements array indexes.
type Path []any

// Lookup resolves a path. ok=false when a step is missing.
func Lookup(src []byte, root *Node, path Path) (*Node, bool, error) {
	cur := root
	for _, step := range path {
		switch s := step.(type) {
		case string:
			nx, ok, err := cur.Get(s)
			if err != nil || !ok {
				return nil, false, err
			}
			cur = nx
		case int:
			if cur.Kind != Array || s < 0 || s >= len(cur.Elems) {
				return nil, false, nil
			}
			cur = cur.Elems[s]
		default:
			return nil, false, fmt.Errorf("잘못된 경로 요소 %T", step)
		}
	}
	return cur, true, nil
}

// Style holds the detected formatting of a document.
type Style struct {
	Unit string // one indentation level
	EOL  string
}

// DetectStyle finds the indentation unit and line ending of src (default two spaces, "\n").
func DetectStyle(src []byte) Style {
	st := Style{Unit: "  ", EOL: "\n"}
	if bytes.Contains(src, []byte("\r\n")) {
		st.EOL = "\r\n"
	}
	for _, line := range bytes.Split(src, []byte("\n")) {
		t := bytes.TrimLeft(line, " \t")
		if len(t) == len(line) || len(t) == 0 {
			continue
		}
		if t[0] != '"' && t[0] != '{' && t[0] != '[' {
			continue
		}
		ws := string(line[:len(line)-len(t)])
		if strings.Contains(ws, "\t") {
			st.Unit = "\t"
		} else {
			st.Unit = ws
		}
		break
	}
	return st
}

func lineIndent(src []byte, pos int) string {
	ls := bytes.LastIndexByte(src[:pos], '\n') + 1
	i := ls
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return string(src[ls:i])
}

func childStart(n *Node, i int) int {
	if n.Kind == Object {
		return n.Members[i].KeyStart
	}
	return n.Elems[i].Start
}

func childEnd(n *Node, i int) int {
	if n.Kind == Object {
		return n.Members[i].Val.End
	}
	return n.Elems[i].End
}

func childCount(n *Node) int {
	if n.Kind == Object {
		return len(n.Members)
	}
	return len(n.Elems)
}

func splice(src []byte, from, to int, repl string) []byte {
	out := make([]byte, 0, len(src)-(to-from)+len(repl))
	out = append(out, src[:from]...)
	out = append(out, repl...)
	out = append(out, src[to:]...)
	return out
}

// insertEnd appends one child (already formatted by the caller through item) to a container.
// item receives the base indentation for the new child and whether the layout is multi-line.
func insertEnd(src []byte, c *Node, st Style, item func(base string, multiline bool) string) []byte {
	n := childCount(c)
	open, closeCh := string(src[c.Start]), string(src[c.End-1])
	if n == 0 {
		base := lineIndent(src, c.Start)
		child := base + st.Unit
		return splice(src, c.Start, c.End, open+st.EOL+child+item(child, true)+st.EOL+base+closeCh)
	}
	multiline := bytes.IndexByte(src[c.Start:childStart(c, 0)], '\n') >= 0
	if multiline {
		ind := lineIndent(src, childStart(c, n-1))
		return splice(src, childEnd(c, n-1), childEnd(c, n-1), ","+st.EOL+ind+item(ind, true))
	}
	return splice(src, childEnd(c, n-1), childEnd(c, n-1), ","+item("", false))
}

// removeChild deletes child i of a container together with its separating comma.
func removeChild(src []byte, c *Node, i int) []byte {
	n := childCount(c)
	switch {
	case n == 1:
		return splice(src, c.Start, c.End, string(src[c.Start])+string(src[c.End-1]))
	case i < n-1:
		return splice(src, childStart(c, i), childStart(c, i+1), "")
	default:
		return splice(src, childEnd(c, i-1), childEnd(c, i), "")
	}
}

func memberText(key string, v any, st Style, base string, multiline bool) string {
	k, _ := json.Marshal(key)
	if multiline {
		return string(k) + ": " + string(Format(v, st.Unit, base, st.EOL))
	}
	return string(k) + ":" + string(Compact(v))
}

// Set sets (adds or replaces) the value at path, creating missing object members.
func Set(src []byte, path Path, value any) ([]byte, error) {
	root, err := Parse(src)
	if err != nil {
		return nil, err
	}
	if len(path) == 0 {
		return nil, errors.New("빈 경로")
	}
	st := DetectStyle(src)
	cur := root
	for i, step := range path {
		switch s := step.(type) {
		case string:
			if cur.Kind != Object {
				return nil, fmt.Errorf("경로 %v: 객체가 아닙니다", path[:i])
			}
			nx, ok, err := cur.Get(s)
			if err != nil {
				return nil, err
			}
			if !ok {
				// Missing from here on: wrap the value in the remaining keys.
				v := value
				for j := len(path) - 1; j > i; j-- {
					k, isStr := path[j].(string)
					if !isStr {
						return nil, fmt.Errorf("경로 %v: 없는 배열 요소", path[:j])
					}
					v = Obj{{k, v}}
				}
				return insertEnd(src, cur, st, func(base string, ml bool) string {
					return memberText(s, v, st, base, ml)
				}), nil
			}
			if i == len(path)-1 {
				var mem Member
				for _, m := range cur.Members {
					if m.Key == s {
						mem = m
					}
				}
				base := lineIndent(src, mem.KeyStart)
				return splice(src, nx.Start, nx.End, string(Format(value, st.Unit, base, st.EOL))), nil
			}
			cur = nx
		case int:
			if cur.Kind != Array || s < 0 || s >= len(cur.Elems) {
				return nil, fmt.Errorf("경로 %v: 배열 인덱스 %d 가 없습니다", path[:i], s)
			}
			nx := cur.Elems[s]
			if i == len(path)-1 {
				base := lineIndent(src, nx.Start)
				return splice(src, nx.Start, nx.End, string(Format(value, st.Unit, base, st.EOL))), nil
			}
			cur = nx
		default:
			return nil, fmt.Errorf("잘못된 경로 요소 %T", step)
		}
	}
	return nil, errors.New("unreachable")
}

// Append adds value at the end of the array at path.
func Append(src []byte, path Path, value any) ([]byte, error) {
	root, err := Parse(src)
	if err != nil {
		return nil, err
	}
	arr, ok, err := Lookup(src, root, path)
	if err != nil {
		return nil, err
	}
	if !ok || arr.Kind != Array {
		return nil, fmt.Errorf("경로 %v 는 배열이 아닙니다", path)
	}
	st := DetectStyle(src)
	return insertEnd(src, arr, st, func(base string, ml bool) string {
		if ml {
			return string(Format(value, st.Unit, base, st.EOL))
		}
		return string(Compact(value))
	}), nil
}

// Delete removes the member or element at path. It reports false when absent.
func Delete(src []byte, path Path) ([]byte, bool, error) {
	root, err := Parse(src)
	if err != nil {
		return nil, false, err
	}
	if len(path) == 0 {
		return nil, false, errors.New("빈 경로")
	}
	parent, ok, err := Lookup(src, root, path[:len(path)-1])
	if err != nil || !ok {
		return src, false, err
	}
	switch s := path[len(path)-1].(type) {
	case string:
		if parent.Kind != Object {
			return src, false, nil
		}
		idx := -1
		for i, m := range parent.Members {
			if m.Key == s {
				if idx >= 0 {
					return nil, false, fmt.Errorf("%w: %q", ErrDuplicateKey, s)
				}
				idx = i
			}
		}
		if idx < 0 {
			return src, false, nil
		}
		return removeChild(src, parent, idx), true, nil
	case int:
		if parent.Kind != Array || s < 0 || s >= len(parent.Elems) {
			return src, false, nil
		}
		return removeChild(src, parent, s), true, nil
	}
	return nil, false, errors.New("잘못된 경로 요소")
}

// Canonical re-encodes raw JSON with sorted keys and no insignificant whitespace,
// so that formatting differences do not change content hashes.
func Canonical(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // map keys are sorted by encoding/json
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// KV is one ordered object member for Format.
type KV struct {
	K string
	V any
}

// Obj is an object that keeps member order.
type Obj []KV

// Keys returns the sorted member names (test helper).
func (o Obj) Keys() []string {
	ks := make([]string, len(o))
	for i, kv := range o {
		ks[i] = kv.K
	}
	sort.Strings(ks)
	return ks
}
