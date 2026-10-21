package gateway

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestNameRules(t *testing.T) {
	if got := proxyBase("my-github", "create.issue"); got != "my_github__create_issue" {
		t.Errorf("proxy base %q", got)
	}
	if got := customBase("deploy-check"); got != "forge_deploy-check" {
		t.Errorf("custom base %q", got)
	}
	if NormalizeServer("a-b-c") != "a_b_c" {
		t.Error("normalize")
	}
	if MaxExposedLen("forge") != 52 || MaxExposedLen("") != 52 || MaxExposedLen("my-gateway") != 64-len("mcp__my_gateway__") {
		t.Errorf("max len %d %d", MaxExposedLen("forge"), MaxExposedLen("my-gateway"))
	}
	if MaxExposedLen(strings.Repeat("x", 80)) != minToolBudget {
		t.Error("floor")
	}
}

func TestAssignNamesCollisionsDeterministic(t *testing.T) {
	cands := func() []candidate {
		return []candidate{
			{rankProxy, "mcp:my-srv:x", proxyBase("my-srv", "x")},
			{rankProxy, "mcp:my_srv:x", proxyBase("my_srv", "x")},
			{rankCustom, "tool:skill", customBase("skill")}, // collides with the meta tool forge_skill
			{rankMeta, "meta:forge_skill", MetaSkill},
			{rankProxy, "mcp:a:b__c", proxyBase("a", "b__c")},
			{rankProxy, "mcp:a__b:c", proxyBase("a__b", "c")},
		}
	}
	first, warns := assignNames(cands(), 52)
	if len(warns) != 3 {
		t.Errorf("warnings: %v", warns)
	}
	if first["meta:forge_skill"] != "forge_skill" {
		t.Errorf("meta must keep its name: %v", first)
	}
	if first["tool:skill"] == "forge_skill" || !strings.HasPrefix(first["tool:skill"], "forge_skill_") {
		t.Errorf("custom collision: %q", first["tool:skill"])
	}
	// "my-srv" sorts before "my_srv" ('-' < '_'): the first keeps the plain name.
	if first["mcp:my-srv:x"] != "my_srv__x" || first["mcp:my_srv:x"] == "my_srv__x" {
		t.Errorf("proxy collision: %q %q", first["mcp:my-srv:x"], first["mcp:my_srv:x"])
	}
	seen := map[string]bool{}
	for _, n := range first {
		if seen[strings.ToLower(n)] {
			t.Errorf("duplicate %q", n)
		}
		seen[strings.ToLower(n)] = true
	}
	// Determinism: any input order gives the same assignment.
	for i := 0; i < 20; i++ {
		c := cands()
		rand.Shuffle(len(c), func(a, b int) { c[a], c[b] = c[b], c[a] })
		got, _ := assignNames(c, 52)
		for k, v := range first {
			if got[k] != v {
				t.Fatalf("order dependent: %s %q vs %q", k, got[k], v)
			}
		}
	}
}

func TestAssignNamesShortening(t *testing.T) {
	long := strings.Repeat("verylongtoolname", 6)
	c := []candidate{
		{rankProxy, "mcp:s:" + long + "A", proxyBase("s", long+"A")},
		{rankProxy, "mcp:s:" + long + "B", proxyBase("s", long+"B")},
		{rankProxy, "mcp:s:short", proxyBase("s", "short")},
	}
	got, _ := assignNames(c, 52)
	a, b := got["mcp:s:"+long+"A"], got["mcp:s:"+long+"B"]
	if len(a) > 52 || len(b) > 52 || a == b {
		t.Errorf("shortened names %q %q", a, b)
	}
	if got["mcp:s:short"] != "s__short" {
		t.Errorf("short name changed: %q", got["mcp:s:short"])
	}
	again, _ := assignNames(c, 52)
	if again["mcp:s:"+long+"A"] != a {
		t.Error("not deterministic")
	}
	// the full name seen by Claude Code stays within 64 characters
	if n := len("mcp__forge__" + a); n > 64 {
		t.Errorf("full name %d chars", n)
	}
	for i := 0; i < 200; i++ { // many collisions after truncation still resolve uniquely
		c = append(c, candidate{rankProxy, fmt.Sprintf("mcp:s:%s%d", long, i), proxyBase("s", fmt.Sprintf("%s%d", long, i))})
	}
	all, _ := assignNames(c, 20)
	seen := map[string]bool{}
	for _, n := range all {
		if len(n) > 20 || seen[n] {
			t.Fatalf("bad/dup %q", n)
		}
		seen[n] = true
	}
}

func TestIdentifyClient(t *testing.T) {
	for in, want := range map[string]string{
		"claude-code": ClientClaudeCode, "Claude-Code": ClientClaudeCode, "claude-code/2.1": ClientClaudeCode,
		"opencode": ClientOpenCode, "OpenCode": ClientOpenCode,
		"claude-ai": "", "cursor": "", "": "", "claude": "", "my-opencodes": "",
	} {
		if got := IdentifyClient(in); got != want {
			t.Errorf("IdentifyClient(%q)=%q want %q", in, got, want)
		}
	}
}
