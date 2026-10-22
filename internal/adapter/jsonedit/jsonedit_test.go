package jsonedit

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustValid(t *testing.T, b []byte) {
	t.Helper()
	if !json.Valid(b) {
		t.Fatalf("result is not valid JSON:\n%s", b)
	}
}

func TestParseRejectsNonStrictJSON(t *testing.T) {
	for name, in := range map[string]string{
		"empty":         "",
		"blank":         " \n",
		"comment":       "{\n // hi\n \"a\": 1}",
		"trailing":      `{"a":1,}`,
		"bom":           "\xef\xbb\xbf{\"a\":1}",
		"single quotes": `{'a':1}`,
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: want parse error", name)
		}
	}
	// The error never echoes the document.
	_, err := Parse([]byte(`{"token": "sk-supersecretvalue1234", oops}`))
	if err == nil || strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("error must not echo content: %v", err)
	}
}

func TestSetPreservesEverythingElse(t *testing.T) {
	src := "{\n    \"z\": 1,\n    \"mcpServers\": {\n        \"user\": {\"type\": \"stdio\", \"command\": \"x\"},\n        \"b\": 2\n    },\n    \"a\": [1, 2]\n}\n"
	out, err := Set([]byte(src), Path{"mcpServers", "forge"}, Obj{{"type", "stdio"}, {"args", []any{"a", "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, out)
	want := "{\n    \"z\": 1,\n    \"mcpServers\": {\n        \"user\": {\"type\": \"stdio\", \"command\": \"x\"},\n        \"b\": 2,\n        \"forge\": {\n            \"type\": \"stdio\",\n            \"args\": [\n                \"a\",\n                \"b\"\n            ]\n        }\n    },\n    \"a\": [1, 2]\n}\n"
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	// Removing it again restores the original bytes exactly.
	back, ok, err := Delete(out, Path{"mcpServers", "forge"})
	if err != nil || !ok || string(back) != src {
		t.Fatalf("delete did not restore original (ok=%v err=%v):\n%s", ok, err, back)
	}
}

func TestSetStylesTabsCRLFEmptyAndCreate(t *testing.T) {
	tab := "{\n\t\"a\": {\n\t\t\"x\": 1\n\t}\n}\n"
	out, err := Set([]byte(tab), Path{"a", "y"}, "v")
	if err != nil || string(out) != "{\n\t\"a\": {\n\t\t\"x\": 1,\n\t\t\"y\": \"v\"\n\t}\n}\n" {
		t.Fatalf("tab: %v\n%q", err, out)
	}
	crlf := "{\r\n  \"a\": 1\r\n}\r\n"
	out, err = Set([]byte(crlf), Path{"b"}, Obj{{"k", "v"}})
	if err != nil || string(out) != "{\r\n  \"a\": 1,\r\n  \"b\": {\r\n    \"k\": \"v\"\r\n  }\r\n}\r\n" {
		t.Fatalf("crlf: %v\n%q", err, out)
	}
	out, err = Set([]byte("{}\n"), Path{"hooks", "PreToolUse"}, []any{Obj{{"matcher", "m"}}})
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, out)
	if !strings.HasPrefix(string(out), "{\n  \"hooks\": {\n    \"PreToolUse\": [\n      {\n        \"matcher\": \"m\"") {
		t.Fatalf("empty object expansion:\n%s", out)
	}
	// single-line object stays single-line
	out, err = Set([]byte(`{"a":1}`), Path{"b"}, "x")
	if err != nil || string(out) != `{"a":1,"b":"x"}` {
		t.Fatalf("compact: %v %s", err, out)
	}
	// replace keeps position
	out, err = Set([]byte("{\n  \"a\": 1,\n  \"b\": 2\n}"), Path{"a"}, "z")
	if err != nil || string(out) != "{\n  \"a\": \"z\",\n  \"b\": 2\n}" {
		t.Fatalf("replace: %v %s", err, out)
	}
}

func TestDeleteFirstMiddleLastOnly(t *testing.T) {
	src := "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}\n"
	for key, want := range map[string]string{
		"a": "{\n  \"b\": 2,\n  \"c\": 3\n}\n",
		"b": "{\n  \"a\": 1,\n  \"c\": 3\n}\n",
		"c": "{\n  \"a\": 1,\n  \"b\": 2\n}\n",
	} {
		out, ok, err := Delete([]byte(src), Path{key})
		if err != nil || !ok || string(out) != want {
			t.Errorf("delete %s: %v\n%s", key, err, out)
		}
	}
	out, _, _ := Delete([]byte("{\n  \"a\": 1\n}\n"), Path{"a"})
	if string(out) != "{}\n" {
		t.Errorf("only member: %q", out)
	}
	if out, ok, _ := Delete([]byte(src), Path{"zzz"}); ok || string(out) != src {
		t.Error("absent key must be a no-op")
	}
}

func TestArrayAppendAndIndexPaths(t *testing.T) {
	src := "{\n  \"h\": [\n    {\"a\": 1}\n  ]\n}\n"
	out, err := Append([]byte(src), Path{"h"}, Obj{{"b", 2}})
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, out)
	if !strings.Contains(string(out), "{\"a\": 1},\n    {\n      \"b\": 2\n    }\n  ]") {
		t.Fatalf("append:\n%s", out)
	}
	out, ok, err := Delete(out, Path{"h", 1})
	if err != nil || !ok || string(out) != src {
		t.Fatalf("remove elem: %v\n%s", err, out)
	}
	out, err = Set([]byte(src), Path{"h", 0}, "x")
	if err != nil || !strings.Contains(string(out), `"h": [`+"\n    \"x\"") {
		t.Fatalf("set idx: %v\n%s", err, out)
	}
}

func TestDuplicateKeysRefused(t *testing.T) {
	if _, err := Set([]byte(`{"a":1,"a":2}`), Path{"a"}, 3); err == nil {
		t.Fatal("duplicate key must be refused")
	}
}

func TestCanonicalIgnoresFormattingAndOrder(t *testing.T) {
	a, _ := Canonical([]byte("{\n \"b\": 1, \"a\": [1,\n2]}"))
	b, _ := Canonical([]byte(`{"a":[1,2],"b":1}`))
	if string(a) != string(b) {
		t.Fatalf("%s != %s", a, b)
	}
}

func TestFormatDoesNotEscapeHTML(t *testing.T) {
	if got := string(Compact(Obj{{"u", "a&b<c>"}})); got != `{"u":"a&b<c>"}` {
		t.Fatal(got)
	}
}
