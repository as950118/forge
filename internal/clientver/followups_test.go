package clientver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TASK-0024 follow-ups: version normalization, shim hint, inexact path note.

func TestLeadingZerosAreNormalized(t *testing.T) {
	cases := map[string]string{
		"02.1.0305":           "2.1.305",
		"v0002.01.0305 x":     "2.1.305",
		"opencode 00.0.00":    "0.0.0",
		"claude 2.1.305":      "2.1.305",
		"2.10.0":              "2.10.0",
		"Claude Code 2.0.024": "2.0.24",
	}
	for in, want := range cases {
		got, ok := ParseVersion([]byte(in + "\n"))
		if !ok || got != want {
			t.Errorf("%q -> %q (%v), want %q", in, got, ok, want)
		}
	}
	if st, code, _ := Classify("2.1.305", "2.1.305"); st != StatusOK || code != CodeVerified {
		t.Fatal("exact version is ok")
	}
	// the zero-padded spelling is the verified version, not "unverified"
	dir := fake(t, "claude", "echo 02.1.0305")
	if r := check(t, dir); r.Status != StatusOK || r.Version != "2.1.305" {
		t.Fatalf("%+v", r)
	}
}

func TestFailedRunExplainsHomeIsolationForShims(t *testing.T) {
	skipWindows(t)
	dir := fake(t, "claude", "echo 'node: not found' >&2\nexit 127")
	r := check(t, dir)
	if r.Code != CodeRunFailed || !strings.Contains(r.Message, "종료 코드 127") || !strings.Contains(r.Message, "HOME") || !strings.Contains(r.Message, "claude --version") {
		t.Fatalf("%+v", r)
	}
	if strings.Contains(r.Message, "버전 관리자") {
		t.Fatalf("a plain directory is not a version manager shim: %s", r.Message)
	}
	// a path inside a version manager directory gets the specific explanation
	base, _ := filepath.EvalSymlinks(t.TempDir())
	nvm := filepath.Join(base, ".nvm", "shims")
	if err := os.MkdirAll(nvm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nvm, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r = check(t, nvm)
	if r.Code != CodeRunFailed || !strings.Contains(r.Message, "버전 관리자") || !strings.Contains(r.Message, "--allow-untested-version") {
		t.Fatalf("%+v", r)
	}
	// success and timeouts carry no such hint
	if r := check(t, fake(t, "claude", "echo 2.1.305")); strings.Contains(r.Message, "HOME") {
		t.Fatalf("%+v", r)
	}
}

func TestInexactPathIsFlaggedOnlyWhenShownWithEscapes(t *testing.T) {
	skipWindows(t)
	withGroups(t, func(uint32) bool { return false })
	// plain path: exact
	plain := mkdirMode(t, 0o777)
	writeBin(t, plain, "claude", markerScript(filepath.Join(t.TempDir(), "m")), 0o755)
	if r := check(t, plain); r.Code != CodeUnsafeLoc || r.Inexact || strings.Contains(r.Message, "이스케이프") {
		t.Fatalf("%+v", r)
	}
	// a directory name with an ESC and a right-to-left override
	base, _ := filepath.EvalSymlinks(t.TempDir())
	odd := filepath.Join(base, "b\x1b[31m\u202ein")
	if err := os.Mkdir(odd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(odd, 0o777); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	writeBin(t, odd, "claude", markerScript(marker), 0o755)
	r := check(t, odd)
	if r.Code != CodeUnsafeLoc || !r.Inexact || ranMarker(t, marker) {
		t.Fatalf("%+v", r)
	}
	for _, s := range []string{r.Message, r.Path, r.UnsafePath} {
		if strings.ContainsRune(s, 0x1b) || strings.ContainsRune(s, 0x202e) {
			t.Fatalf("raw control characters in %q", s)
		}
	}
	if !strings.Contains(r.Message, "붙여 넣으면 실제 경로와 다릅니다") {
		t.Fatalf("the message must say the shown path is not the real one: %s", r.Message)
	}
}
