package adapter

import (
	"strings"
	"testing"
)

func TestMaskJSONDisplayMasksWholeSecretSubtrees(t *testing.T) {
	cases := map[string]string{
		"pretty object": "{\n  \"credentials\": {\n    \"github\": \"FAKE-A1\",\n    \"nested\": {\"deep\": \"FAKE-A2\", \"pin\": 123456},\n    \"list\": [\"FAKE-A3\", {\"x\": \"FAKE-A4\"}, 7]\n  }\n}\n",
		"one line":      `{"secrets":{"a":"FAKE-A1","n":{"deep":"FAKE-A2","pin":123456},"list":["FAKE-A3",{"x":"FAKE-A4"},7]}}` + "\n",
		"auth array":    "{\n  \"auth\": [\n    {\"user\": \"FAKE-A1\"},\n    [\"FAKE-A2\", [\"FAKE-A3\"]],\n    {\"k\": {\"z\": \"FAKE-A4\"}, \"c\": 123456}\n  ]\n}\n",
		"apiKeys":       `{"apiKeys":{"a":"FAKE-A1","b":["FAKE-A2",{"c":"FAKE-A3","d":"FAKE-A4"}],"e":123456}}`,
		"camel":         `{"apiCredentials":{"a":"FAKE-A1","b":{"c":"FAKE-A2","d":["FAKE-A3",{"e":"FAKE-A4"}],"f":123456}}}`,
		"upper snake":   `{"MY_SECRETS":{"a":"FAKE-A1","b":{"c":"FAKE-A2","d":["FAKE-A3",{"e":"FAKE-A4"}],"f":123456}}}`,
		"escaped key":   `{"credentials":{"a":"FAKE-A1","b":{"c":"FAKE-A2","d":["FAKE-A3",{"e":"FAKE-A4"}],"f":123456}}}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			out := MaskJSONDisplay(src)
			for _, s := range []string{"FAKE-A1", "FAKE-A2", "FAKE-A3", "FAKE-A4", "123456"} {
				if strings.Contains(out, s) && strings.Contains(src, s) {
					t.Errorf("%s leaked:\n%s", s, out)
				}
			}
			if strings.Count(out, "\n") != strings.Count(src, "\n") {
				t.Error("line count changed")
			}
			if !strings.Contains(out, `"***"`) {
				t.Errorf("nothing masked:\n%s", out)
			}
		})
	}
}

func TestMaskJSONDisplayKeepsReferencesAndOtherKeys(t *testing.T) {
	src := "{\n  \"credentials\": {\"a\": \"{env:MY_VAR}\", \"b\": [\"${OTHER}\", \"FAKE-A1\"]},\n  \"mcp\": {\"srv\": {\"command\": [\"npx\", \"-y\"], \"timeout\": 30, \"environment\": {\"PATH_X\": \"/usr/bin\"}}}\n}\n"
	out := MaskJSONDisplay(src)
	for _, keep := range []string{"{env:MY_VAR}", "${OTHER}", `"npx"`, `"timeout": 30`, "/usr/bin"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%s was masked:\n%s", keep, out)
		}
	}
	if strings.Contains(out, "FAKE-A1") {
		t.Errorf("secret leaked:\n%s", out)
	}
}
