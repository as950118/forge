package jsonedit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

func encString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimRight(buf.String(), "\n")
}

// normalize converts a json.RawMessage into ordered values.
func normalize(v any) any {
	switch x := v.(type) {
	case json.RawMessage:
		n, err := Parse(x)
		if err != nil {
			return string(x) // cannot happen for values built by the adapters
		}
		return fromNode(x, n)
	case []string:
		a := make([]any, len(x))
		for i, s := range x {
			a[i] = s
		}
		return a
	}
	return v
}

func fromNode(src []byte, n *Node) any {
	switch n.Kind {
	case Object:
		o := make(Obj, 0, len(n.Members))
		for _, m := range n.Members {
			o = append(o, KV{m.Key, fromNode(src, m.Val)})
		}
		return o
	case Array:
		a := make([]any, 0, len(n.Elems))
		for _, e := range n.Elems {
			a = append(a, fromNode(src, e))
		}
		return a
	case String:
		s, _ := n.StringValue(src)
		return s
	case Number:
		return json.Number(src[n.Start:n.End])
	case Bool:
		return string(src[n.Start:n.End]) == "true"
	}
	return nil
}

// Format renders v as indented JSON. The first line carries no indentation (the
// caller places it); nested lines are indented relative to base, and the closing
// bracket sits at base.
func Format(v any, unit, base, eol string) []byte {
	var b bytes.Buffer
	write(&b, normalize(v), unit, base, eol, true)
	return b.Bytes()
}

// Compact renders v without whitespace.
func Compact(v any) []byte {
	var b bytes.Buffer
	write(&b, normalize(v), "", "", "", false)
	return b.Bytes()
}

func write(b *bytes.Buffer, v any, unit, base, eol string, pretty bool) {
	switch x := v.(type) {
	case Obj:
		if len(x) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{")
		for i, kv := range x {
			if i > 0 {
				b.WriteString(",")
			}
			if pretty {
				b.WriteString(eol + base + unit)
			}
			b.WriteString(encString(kv.K))
			b.WriteString(":")
			if pretty {
				b.WriteString(" ")
			}
			write(b, normalize(kv.V), unit, base+unit, eol, pretty)
		}
		if pretty {
			b.WriteString(eol + base)
		}
		b.WriteString("}")
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[")
		for i, e := range x {
			if i > 0 {
				b.WriteString(",")
			}
			if pretty {
				b.WriteString(eol + base + unit)
			}
			write(b, normalize(e), unit, base+unit, eol, pretty)
		}
		if pretty {
			b.WriteString(eol + base)
		}
		b.WriteString("]")
	case string:
		b.WriteString(encString(x))
	case bool:
		fmt.Fprintf(b, "%t", x)
	case nil:
		b.WriteString("null")
	case json.Number:
		b.WriteString(x.String())
	case int:
		fmt.Fprintf(b, "%d", x)
	case int64:
		fmt.Fprintf(b, "%d", x)
	case float64:
		fmt.Fprintf(b, "%v", x)
	default:
		bs, _ := json.Marshal(x)
		b.Write(bs)
	}
}
