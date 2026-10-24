package mcp

import (
	"math/rand"
	"strings"
	"testing"
)

// All values are fake: random strings from a fixed seed with provider-like shapes.
func fakeRand(seed int64) func(set string, n int) string {
	r := rand.New(rand.NewSource(seed))
	return func(set string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = set[r.Intn(len(set))]
		}
		return string(b)
	}
}

const (
	tAlnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	tHex   = "0123456789abcdef"
)

const macTmp = "/var/folders/gr/q4n6pgs90gx_jny4zh7p58dw0000gn/T/forge-demo.abc123/src/variant"

// Regression (TASK-0014/0026): macOS TMPDIR paths were refused as "secret-looking".
func TestMacOSTempDirPathIsNotASecret(t *testing.T) {
	for _, v := range []string{
		macTmp, "/private" + macTmp, "--config=" + macTmp, macTmp + ":/data",
		"/Users/me/.npm/_npx/0123456789abcdef/node_modules/.bin/server",
		"/home/p/node_modules/.pnpm/typescript@5.4.5_p2k3l4m5n6o7q8r9s0t1u2v3w4/node_modules/typescript/bin/tsc",
		`C:\Users\heonjin\AppData\Local\Temp\forge-1a2b3c4d\src\variant.py`,
	} {
		if LooksSecretValue(v) {
			t.Errorf("path refused: %s", v)
		}
		if err := CheckArgs([]string{"s.py", v}); err != nil {
			t.Errorf("CheckArgs refused path %s: %v", v, err)
		}
	}
}

// The path exception removes only the pure entropy rule: known prefixes in any
// component, key=value components and Bearer tokens still count.
func TestPathExceptionKeepsKnownPrefixChecks(t *testing.T) {
	f := fakeRand(1)
	for _, v := range []string{
		"/tmp/sk-" + f(tAlnum, 48),
		"/a/b/ghp_" + f(tAlnum, 36) + "/c",
		"/srv/app/eyJ" + f(tAlnum, 30) + ".eyJ" + f(tAlnum, 30) + "." + f(tAlnum, 30) + "/x",
		"~/.config/AKIA" + f("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", 16),
		"./a/glpat-" + f(tAlnum, 20) + "/b",
		"../x/npm_" + f(tAlnum, 36) + "/y",
		`C:\Users\u\npm_` + f(tAlnum, 36) + `\x`,
		"/var/folders/gr/" + f("abcdefghijklmnopqrstuvwxyz0123456789", 28) + "/T/foo-sk-" + f(tAlnum, 40) + "/x",
		"/opt/app/token=" + f(tAlnum, 20) + "/x",
		"/opt/app/password=" + f(tAlnum, 14) + "/x",
		"--out=/etc/sk-" + f(tAlnum, 40) + "/y",
		"/api/bearer " + f(tAlnum, 30),
		"/tmp/whsec_" + f(tAlnum, 32), "/opt/a/rk_live_" + f(tAlnum, 24), "/tmp/SK" + f(tHex, 32),
		"/tmp/SG." + f(tAlnum, 22) + "." + f(tAlnum, 43), "/tmp/GHP_" + f(tAlnum, 36), "/tmp/Sk-" + f(tAlnum, 48),
		"/tmp/SK-" + f(tAlnum, 48), "/tmp/XOXB-" + f(tAlnum, 12) + "-" + f(tAlnum, 12) + "-" + f(tAlnum, 24),
		"/tmp/EYJ" + f(tAlnum, 34) + ".EYJ" + f(tAlnum, 40) + "." + f(tAlnum, 43), "/tmp/prefixghp_" + f(tAlnum, 36),
		"/tmp/mysk_live_" + f(tAlnum, 24), "/tmp/pk_live_" + f(tAlnum, 24), "/home/u/hf_" + f(tAlnum, 34),
		"/home/u/ya29." + f(tAlnum, 60), "/tmp/xapp-" + f(tAlnum, 30), "/tmp/shpat_" + f(tHex, 32), "/tmp/dop_v1_" + f(tHex, 40),
	} {
		if !LooksSecretValue(v) {
			t.Errorf("disguised secret accepted: %.14s...", v)
		}
		if err := CheckArgs([]string{v}); err == nil {
			t.Errorf("CheckArgs accepted disguised secret: %.14s...", v)
		}
	}
}

// A URL is not a file path: its path, query and fragment stay strict.
func TestURLPartsStayStrictEvenWhenPathShaped(t *testing.T) {
	f := fakeRand(2)
	for _, u := range []string{
		"https://h.example/v1/" + f(tAlnum, 32) + "/mcp",
		"https://h.example/a/b/" + f(tAlnum, 40),
		"http://h.example/var/folders/gr/" + f(tAlnum, 28) + "/T/x",
		"https://h.example/mcp?file=/var/folders/gr/" + f(tAlnum, 28) + "/T/x",
		"https://h.example/mcp#/a/" + f(tAlnum, 32),
		"file:///tmp/x/" + f(tAlnum, 32) + "/a",
	} {
		if !LooksSecretValue(u) {
			t.Errorf("URL value accepted by LooksSecretValue: %.30s...", u)
		}
		if strings.HasPrefix(u, "http") {
			s := Spec{Name: "a", Transport: TransportHTTP, URL: u}
			if s.Validate() == nil {
				t.Errorf("URL accepted by Validate: %.30s...", u)
			}
		}
		if CheckArgs([]string{"--url", u}) == nil {
			t.Errorf("URL accepted by CheckArgs: %.30s...", u)
		}
	}
	// identifier exceptions do not apply inside URLs either
	s := Spec{Name: "a", Transport: TransportHTTP, URL: "https://h.example/commit/" + f(tHex, 40)}
	if s.Validate() == nil {
		t.Error("a 40-hex URL path segment must stay refused")
	}
}

func TestFilePathShape(t *testing.T) {
	for v, want := range map[string]bool{
		"/home/b": true, "/tmp/a/b": true, "~/a/b": true, "./a/b": true, "../a/b": true, `C:\a\b`: true, "D:/a/b": true,
		"/a/b": false, "/x/y/z": false, "/v1/abc/mcp": false, "node_modules/.pnpm/x": true, "node_modules/x": true, "/home": false, "/a": false, "~/a": false, "./a": false, "a/b": false, "/": false, "~": false,
		"https://h/a/b": false, "file:///a/b": false, "/a/b c": false, "/home/$X/b": false, "/home/{x}/b": false, "/home/b?q=1": false,
		"//": false, "/./.": false,
	} {
		if got := isFilePath(v); got != want {
			t.Errorf("isFilePath(%q)=%v want %v", v, got, want)
		}
	}
}

// Identifiers with a clear context are not secrets; the context is required.
func TestIdentifierContextExceptions(t *testing.T) {
	f := fakeRand(3)
	h40, h64, h32 := f(tHex, 40), f(tHex, 64), f(tHex, 32)
	ok := [][]string{
		{"sha256:" + h64}, {"ghcr.io/org/img@sha256:" + h64}, {"--ref", h40}, {"--rev", h40}, {"--sha256", h64},
		{"--commit=" + h40}, {"commit=" + h40}, {"rev=" + h40}, {"sha=" + h40}, {"checksum=" + h64}, {"digest=" + h64},
		{"550e8400-e29b-41d4-a716-446655440000"}, {"my_snake_case_identifier_name_v2_final"}, {"release-candidate-build-for-staging-v2"},
	}
	for _, a := range ok {
		if err := CheckArgs(a); err != nil {
			t.Errorf("identifier refused %.20v: %v", a, err)
		}
	}
	if LooksSecretKV("GIT_COMMIT", h40) || LooksSecretKV("image-digest", h64) || LooksSecretKV("BuildRev", h40) {
		t.Error("hash-named key with 40/64 hex must be an identifier")
	}
	bad := [][]string{
		{h32}, {h40}, {h64}, {"--name", h40}, {"--ref", h32}, {"commit=" + h32}, {"sha256:" + h40}, {"--ref=" + h32},
		{"--token", h40}, {"hash=" + f(tAlnum, 40)}, {"x", h40}, // no label: an API key may look like this
	}
	for _, a := range bad {
		if err := CheckArgs(a); err == nil {
			t.Errorf("unlabelled/odd hex accepted %.20v", a)
		}
	}
	if !LooksSecretKV("SERVICE_ID", h40) || !LooksSecretKV("COMMIT_TOKEN", h40) || !LooksSecretKV("GIT_COMMIT", h32) {
		t.Error("key-name exception must be limited to hash-named, non-secret keys and 40/64 hex")
	}
}

// Known limits of the exceptions (docs/security.md M5): a fake secret that wears
// the label or sits in a path component without a known prefix passes. If one of
// these starts being refused, update the documentation, not just the test.
func TestResidualRisksOfTheExceptions(t *testing.T) {
	f := fakeRand(4)
	for _, a := range [][]string{
		{"/home/u/.config/" + f(tAlnum, 40)},
		{"/tmp/" + f(tAlnum, 40)},
		{"commit=" + f(tHex, 40)},
		{"--ref", f(tHex, 40)},
		{"sha256:" + f(tHex, 64)},
	} {
		if err := CheckArgs(a); err != nil {
			t.Errorf("residual risk no longer accepted (docs stale?): %.18v", a)
		}
	}
}

func TestWordIdentifierShape(t *testing.T) {
	for v, want := range map[string]bool{
		"my_snake_case_identifier_name_v2_final": true, "release-candidate-build-for-staging-v2": true, "weekly-status-report-generator-tool": true,
		"a1b2-c3d4-e5f6-g7h8-i9j0-k1l2": false, "two-parts": false, "abc-def": false, "Upper_case_words_here_too_v2": false,
		"qzxkjhvbnmwp_rtsdfghjklzx_bcdfghjklmn": false,
		// digits: only as a version-like last part (or v<N> before a last word)
		"correct-horse-battery-staple-2": true, "correct-horse-battery-staple-v2": true, "correct-horse-battery-staple-rc1": true,
		"correct-horse-battery-v2-final": true, "correct-horse-7-battery-staple": false, "correct-horse-battery7-staple": false,
		"correct7-horse-battery-staple": false, "correct-horse-battery-staple-7x": false, "correct-horse-v2-battery-staple": false,
		"correct-horse-battery-v12345": false, "correct-horse-battery-12345": false, "correct-horse-v2-v3": false, "correct-horse-battery-v2-v3": false,
		// word count boundary: three words is enough, two words with a version is not
		"alpha-bravo-charlie-2": true, "alpha-bravo-2024-final-x1": false, "alpha-bravo-v2": false, "alpha-bravo-charlie-delta-echo-v2": true, "thisisonelongwordwithoutseparatorsv2": false,
		"averyveryverylongsegment_name_v2_final": false,
	} {
		if got := wordIdentifier(v); got != want {
			t.Errorf("wordIdentifier(%q)=%v want %v", v, got, want)
		}
	}
}

// Property: for random opaque tokens (not path-shaped, no label) the exceptions
// never accept what the legacy strict rules refuse.
func TestExceptionsNeverAcceptWhatStrictRefuses(t *testing.T) {
	f := fakeRand(5)
	r := rand.New(rand.NewSource(55))
	seps := []string{"", "-", "_", "-", "_"}
	n := 0
	for i := 0; i < 60000; i++ {
		var b strings.Builder
		parts := 1 + r.Intn(5)
		for p := 0; p < parts; p++ {
			if p > 0 {
				b.WriteString(seps[r.Intn(len(seps))])
			}
			set := tAlnum
			switch r.Intn(4) {
			case 0:
				set = "abcdefghijklmnopqrstuvwxyz0123456789"
			case 1:
				set = "abcdefghijklmnopqrstuvwxyz"
			case 2:
				set = tHex
			}
			b.WriteString(f(set, 1+r.Intn(14)))
		}
		v := b.String()
		if !looksSecretStrict(v) {
			continue
		}
		n++
		if !LooksSecretValue(v) && !isHexHash(v) { // lone 40/64 hex is only exempt after a label
			t.Fatalf("exception accepted a strict-refused token %q", v)
		}
		if isHexHash(v) && !LooksSecretValue(v) {
			t.Fatalf("unlabelled hex accepted (len %d)", len(v))
		}
	}
	if n < 1000 {
		t.Fatalf("property test too weak: %d refused samples", n)
	}
}

// Property: a known credential prefix anywhere in a path is refused.
func TestPathWithKnownPrefixIsAlwaysRefused(t *testing.T) {
	f := fakeRand(6)
	r := rand.New(rand.NewSource(66))
	prefixes := []string{"sk-", "sk_live_", "ghp_", "gho_", "github_pat_", "xoxb-", "AKIA", "AIza", "glpat-", "npm_", "eyJ"}
	for i := 0; i < 3000; i++ {
		pre := prefixes[r.Intn(len(prefixes))]
		comps := []string{"", "var", f("abcdefghijklmnopqrstuvwxyz0123456789", 28), "T"}
		at := 1 + r.Intn(len(comps)-1)
		comps = append(comps[:at+1], comps[at:]...)
		set := tAlnum
		if pre == "AKIA" {
			set = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		}
		comps[at] = pre + f(set, 36)
		p := strings.Join(comps, "/")
		if !LooksSecretValue(p) || CheckArgs([]string{p}) == nil {
			t.Fatalf("path with prefix %s accepted", pre)
		}
	}
}

// Roots: "/<random>/<random>" is not a path, a known root is required.
func TestRandomAbsoluteComponentsAreNotAPath(t *testing.T) {
	f := fakeRand(7)
	for i := 0; i < 2000; i++ {
		v := "/" + f(tAlnum, 12+i%30) + "/" + f(tAlnum, 12+i%25)
		if isFilePath(v) {
			t.Fatalf("random components taken for a path: %.10s...", v)
		}
	}
	// an AWS-secret-shaped value that begins with "/" is still refused
	for i := 0; i < 200; i++ {
		v := "/" + f(tAlnum+"/+", 39)
		if looksSecretStrict(v) && !LooksSecretValue(v) {
			t.Fatalf("path exception accepted a strict-refused slash value: %.10s...", v)
		}
	}
}

// Known prefixes are one list for every context: case-insensitive, after a
// boundary, and (for distinctive prefixes) anywhere inside a component. Short
// prefixes need a boundary so that ordinary names are not refused.
func TestPrefixPositionAndCaseRules(t *testing.T) {
	f := fakeRand(8)
	must := []string{
		"/tmp/GHP_" + f(tAlnum, 36), "/tmp/prefixghp_" + f(tAlnum, 36), "/tmp/a.ghp_" + f(tAlnum, 36), "/tmp/a=ghp_" + f(tAlnum, 36),
		"/tmp/xgithub_pat_" + f(tAlnum, 30), "/tmp/xsk_live_" + f(tAlnum, 24), "/tmp/Whsec_" + f(tAlnum, 20),
		"/tmp/sk-" + f(tAlnum, 20), "/tmp/a-sk-" + f(tAlnum, 20), "/tmp/npm_" + f(tAlnum, 36), "/tmp/a_npm_" + f(tAlnum, 36),
		"/tmp/SG." + f(tAlnum, 22) + "." + f(tAlnum, 30), "/tmp/SK" + f(tHex, 32), "/home/u/hf_" + f(tAlnum, 30),
	}
	for _, v := range must {
		if !LooksSecretValue(v) {
			t.Errorf("accepted: %.14s...", v)
		}
	}
	mustNot := []string{
		"/home/u/.local/share/pnpm_store/v3/files", "/home/u/.npmrc.d/pnpm_store/x", "/home/u/task-runner/disk-usage/x",
		"/home/u/desk-tools/ask-me/x", "/home/u/projects/risk-model/x", "/home/u/projects/sk/a",
	}
	for _, v := range mustNot {
		if LooksSecretValue(v) {
			t.Errorf("refused an ordinary path: %s", v)
		}
	}
}

// LooksSecretPersist is the strict storage rule: no path or label exception.
func TestPersistRuleHasNoExceptions(t *testing.T) {
	f := fakeRand(9)
	for _, v := range []string{"/tmp/" + f(tAlnum, 40), "/home/u/.config/" + f(tAlnum, 40), macTmp, "commit=" + f(tHex, 40), "sha256:" + f(tHex, 64)} {
		want := looksSecretStrict(v)
		if LooksSecretPersist(v) != want {
			t.Errorf("persist rule differs from strict for %.12s...", v)
		}
	}
	if !LooksSecretPersist("/tmp/" + f(tAlnum, 40)) {
		t.Error("a random path component must be refused for storage")
	}
}
