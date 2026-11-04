package transforms_test

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestClaudeRequestHashesSurviveScrubbing(t *testing.T) {
	shape := fmt.Sprintf("%x", sha256.Sum256([]byte("synthetic request shape")))
	blob := fmt.Sprintf("%x", sha256.Sum256([]byte("synthetic message blob")))
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("synthetic request digest")))
	in := fmt.Sprintf(`{"type":"api-request-shape","shapeHash":"%[1]s","shape":{"model":"auto"}}
{"type":"api-request-blob","hash":"%[2]s","message":{"role":"user","content":"hello"}}
{"type":"api-request","shapeHash":"%[1]s","keep":0,"tail":["%[2]s"],"requestDigest":"%[3]s"}
`, shape, blob, digest)
	s := newScrubber(t)
	if got := string(scrubJSONL(t, s, "claude-code", in).Out.Bytes()); got != in {
		t.Errorf("request reconstruction hashes changed:\n%s", got)
	}
	control := `{"message":{"content":"` + blob + `"}}` + "\n"
	if got := string(scrubJSONL(t, s, "claude-code", control).Out.Bytes()); strings.Contains(got, blob) {
		t.Errorf("an unexempted field bypassed entropy detection: %s", got)
	}
}

func TestCodexUsageResponseIDSurvivesScrubbing(t *testing.T) {
	digest := sha256.Sum256([]byte("synthetic provider response"))
	id := "resp_" + strings.Repeat(base64.RawURLEncoding.EncodeToString(digest[:]), 4)
	in := codexLine(t, "token_usage_record", map[string]any{
		"response_id": id,
		"usage":       map[string]any{"input_tokens": 123, "output_tokens": 7},
	})
	s := newScrubber(t)
	if got := string(scrubJSONL(t, s, "codex", in).Out.Bytes()); got != in {
		t.Errorf("usage response ID changed:\n%s", got)
	}
	control := codexLine(t, "event_msg", map[string]any{"message": id})
	if got := string(scrubJSONL(t, s, "codex", control).Out.Bytes()); strings.Contains(got, id) {
		t.Errorf("an unexempted field bypassed entropy detection: %s", got)
	}
}
