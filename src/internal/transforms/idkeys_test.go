package transforms_test

// Maps keyed by ids: a "<parent>.*" exemption keeps the keys of that one map away from the
// entropy backstop, and nothing else. Values, other maps and the pattern rules are untouched.

import (
	"strings"
	"testing"
)

const (
	toolUseID = "toolu_01Hq7XkVbN3mRt9LpW2cYs5D"
	pat       = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	// Random enough for the backstop in any field, key or value.
	opaque = "Zk8qW3xR9vLm2TnB7yPc4HsJ6dFg1QaE5uXo0Ai"
)

func TestIDKeyedMapsKeepTheirKeys(t *testing.T) {
	for _, tc := range []struct{ name, family, line string }{
		{"claude-code wireToolInputs", "claude-code",
			`{"type":"assistant","uuid":"a1","message":{"id":"m1","content":[{"type":"tool_use","id":"` + toolUseID + `","name":"Bash","input":{"command":"ls"}}]},"wireToolInputs":{"` + toolUseID + `":{"command":"ls","description":"List files"}}}`},
		{"claude-code wireIngestContext", "claude-code",
			`{"type":"assistant","uuid":"a1","wireIngestContext":{"` + toolUseID + `":{"cwd":"/work/api"}}}`},
		{"codex patch changes keyed by path", "codex",
			`{"timestamp":"2026-09-20T10:00:00.000Z","type":"event_msg","payload":{"type":"patch_apply_end","item":{"changes":{"/work/api/out/` + opaque + `/report.json":{"type":"add"}}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := scrubJSONL(t, newScrubber(t), tc.family, tc.line+"\n")
			if got := string(res.Out.Bytes()); got != tc.line+"\n" {
				t.Errorf("id-keyed map changed (hits %v)\n got %s\nwant %s", res.RuleHits, got, tc.line)
			}
		})
	}
}

// The wildcard names keys only: a value directly under the map, the same key under another
// field, and a known token as a key are all still redacted.
func TestTheKeyWildcardCoversNothingElse(t *testing.T) {
	for _, tc := range []struct{ name, family, line, gone string }{
		{"a value under the map", "claude-code", `{"wireToolInputs":{"` + toolUseID + `":"` + opaque + `"}}`, opaque},
		{"the same key elsewhere", "claude-code", `{"otherInputs":{"` + toolUseID + `":{}}}`, toolUseID},
		{"another family's map", "codex", `{"wireToolInputs":{"` + toolUseID + `":{}}}`, toolUseID},
		{"a token as a key", "claude-code", `{"wireToolInputs":{"` + pat + `":{}}}`, pat},
		{"a token as a codex path key", "codex", `{"payload":{"item":{"changes":{"/work/` + pat + `.txt":{}}}}}`, pat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := scrubJSONL(t, newScrubber(t), tc.family, tc.line+"\n")
			if strings.Contains(string(res.Out.Bytes()), tc.gone) {
				t.Errorf("%q survived: %s (hits %v)", tc.gone, res.Out.Bytes(), res.RuleHits)
			}
		})
	}
}

// The username rewrite is not a detector, so an exempt path key still loses the username.
func TestAnExemptPathKeyLosesTheUsername(t *testing.T) {
	line := `{"payload":{"item":{"changes":{"/Users/jane/work/api/main.go":{"type":"update"}}}}}` + "\n"
	res := scrubJSONL(t, newScrubber(t), "codex", line)
	if out := string(res.Out.Bytes()); strings.Contains(out, "jane") || !strings.Contains(out, "__USER__") {
		t.Errorf("username not rewritten in an exempt key: %s", out)
	}
}
