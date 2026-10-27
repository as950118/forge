package ledger

import (
	"strings"
	"testing"
)

// TASK-0021 AC-4: inline masking of unlabelled credential shapes, quoted values,
// the short -p flag and unquoted env-style values with spaces. Fake credentials only.
func TestMaskInlineFollowups(t *testing.T) {
	const (
		jwt = "eyJhbGciOiJub25lIn0.eyJzdWIiOiJmYWtlIn0.ZmFrZXNpZ25hdHVyZQ"
		aws = "AKIAFAKEFAKEFAKE1234"
	)
	secrets := []struct {
		name, in string
		hidden   []string
		kept     []string
	}{
		{"bare sk-", "call with sk-FAKEFAKEFAKEFAKE98765432 now", []string{"sk-FAKEFAKE"}, []string{"call with", "now"}},
		{"bare ghp_", "push " + fakeGH + " done", []string{"ghp_FAKEFAKE"}, []string{"push", "done"}},
		{"github_pat_", "x github_pat_FAKEFAKEFAKEFAKE12345678 y", []string{"github_pat_FAKE"}, []string{"x", "y"}},
		{"jwt", "token is " + jwt + " ok", []string{"eyJhbGci", "ZmFrZXNpZ"}, []string{"token is", "ok"}},
		{"aws key id", "id " + aws + " end", []string{"AKIAFAKE"}, []string{"id", "end"}},
		{"bearer", "Bearer FAKEFAKEtoken12345 trailing", []string{"FAKEFAKEtoken"}, []string{"trailing"}},
		{"lowercase bearer", "curl -H bearer fakefaketoken9999", []string{"fakefaketoken"}, nil},
		{"double quoted", `api_key = "fake value one" next`, []string{"fake value one", "fake", "one"}, []string{"next", "api_key"}},
		{"single quoted", `password: 'zz fake pw' rest`, []string{"zz fake pw"}, []string{"rest"}},
		{"short -p", "mysql -p hunter2fake db1", []string{"hunter2fake"}, []string{"mysql", "db1"}},
		{"env value with spaces", "TOKEN=zzfake1 zzfake2 run.sh", []string{"zzfake1", "zzfake2"}, []string{"TOKEN="}},
		{"env prefixed name", "export GITHUB_TOKEN=zzfake3 zzfake4", []string{"zzfake3", "zzfake4"}, []string{"export", "GITHUB_TOKEN="}},
	}
	for _, c := range secrets {
		t.Run(c.name, func(t *testing.T) {
			for fn, got := range map[string]string{"MaskInline": MaskInline(c.in), "MaskText": MaskText(c.in), "maskString(persist)": maskString(c.in, true)} {
				for _, h := range c.hidden {
					if strings.Contains(got, h) {
						t.Errorf("%s leaked %q: %q", fn, h, got)
					}
				}
				for _, k := range c.kept {
					// MaskText/maskString also replace a whole value that looks like a secret.
					if fn == "MaskInline" && !strings.Contains(got, k) {
						t.Errorf("%s lost %q: %q", fn, k, got)
					}
				}
				if !strings.Contains(got, "***") {
					t.Errorf("%s: no mask marker in %q", fn, got)
				}
			}
		})
	}
}

// Ordinary text is left alone (the masker is for display strings too).
func TestMaskInlineLeavesOrdinaryText(t *testing.T) {
	for _, s := range []string{
		"task-runner-for-the-whole-pipeline-v2", // contains "sk-" inside a word
		"sk-short",                              // too short to be a key
		"ssh --port 22 host",                    // long flag, not -p
		"/var/folders/ab/cd/T/forge-test-123/forge.db",
		"password policy documentation", // no assignment
		"eyJ not a jwt",
		"AKIA123",
		"TOKEN=", // empty value: nothing to hide
		"the bearer of news",
	} {
		if got := MaskInline(s); got != s {
			t.Errorf("MaskInline changed %q -> %q", s, got)
		}
	}
}

func TestMaskPayloadCoversFollowups(t *testing.T) {
	out, err := maskPayload([]byte(`{"cmd":"mysql -p hunter2fake db","note":"` + fakeSK + `","x":"TOKEN=zzfake1 zzfake2"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2fake", "sk-FAKEFAKE", "zzfake1", "zzfake2"} {
		if strings.Contains(out, leak) {
			t.Errorf("payload leaked %q: %s", leak, out)
		}
	}
	if RedactionVersion < 4 {
		t.Errorf("RedactionVersion=%d: the rules changed, it must be bumped", RedactionVersion)
	}
}
