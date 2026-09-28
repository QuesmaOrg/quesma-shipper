package transforms_test

// Copilot's join ids and provider-encrypted reasoning are high-entropy by construction, so the
// family exempts them from the backstop; the pattern rules and the username rewrite still apply.

import (
	"encoding/base64"
	"math/rand"
	"strings"
	"testing"
)

// opaqueBlob is shaped like encryptedContent: base64 of ciphertext, seeded so failures reproduce.
func opaqueBlob(n int) string {
	b := make([]byte, n)
	rand.New(rand.NewSource(7)).Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func TestCopilotJoinIdsAndCiphertextSurvive(t *testing.T) {
	blob := opaqueBlob(600)
	call := "call_r4KeDgjw3zhljpnAV4CdlZT0"
	api := "chatcmpl-CaZ8xQ3vN1rT5yLp0WkEe7Hs2Df9"
	lines := []string{
		`{"type":"assistant.message","data":{"messageId":"m1","apiCallId":"` + api + `","toolRequests":[{"toolCallId":"` + call + `","name":"bash"}],"encryptedContent":"` + blob + `","reasoningOpaque":"` + blob + `","reasoningBlocks":{"provider":"openai","blocks":[{"id":"rs_` + blob[:40] + `","encrypted_content":"` + blob + `"}]}},"id":"a1","parentId":"u1"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"` + call + `","success":true},"id":"t2","parentId":"t1"}`,
		`{"kind":1,"k":["requests",0,"result"],"v":{"metadata":{"toolCallRounds":[{"toolCalls":[{"id":"` + call + `"}],"thinking":{"id":"` + blob[:64] + `","encrypted":"` + blob + `"},"statefulMarker":"` + blob[:120] + `"}]}}}`,
		`{"kind":2,"k":["requests",0,"response"],"v":[{"kind":"toolInvocationSerialized","toolCallId":"` + call + `"}]}`,
		`{"kind":0,"v":{"requests":[{"requestId":"request_` + call + `","result":{"metadata":{"toolCallRounds":[{"thinking":{"encrypted":"` + blob + `"}}]}}}]}}`,
	}
	payload := strings.Join(lines, "\n") + "\n"
	res := scrubJSONL(t, newScrubber(t), "copilot", payload)
	if got := string(res.Out.Bytes()); got != payload {
		t.Errorf("exempt copilot fields changed (hits %v)\n got %s\nwant %s", res.RuleHits, got, payload)
	}
}

func TestCopilotExemptFieldsStillMeetThePatternRules(t *testing.T) {
	pat := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	for _, line := range []string{
		`{"type":"assistant.message","data":{"encryptedContent":"GITHUB_TOKEN=` + pat + `"}}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"` + pat + `"}}`,
		`{"kind":1,"k":["requests",0,"result"],"v":{"metadata":{"toolCallRounds":[{"thinking":{"encrypted":"` + pat + `"}}]}}}`,
	} {
		res := scrubJSONL(t, newScrubber(t), "copilot", line+"\n")
		if strings.Contains(string(res.Out.Bytes()), pat) {
			t.Errorf("token survived in an exempt field: %s", res.Out.Bytes())
		}
	}
}

// VS Code records paths as file URIs, percent-encoded, and in object keys.
func TestCopilotPathsLoseTheUsername(t *testing.T) {
	line := `{"kind":2,"k":["requests",0,"response"],"v":[{"invocationMessage":{"value":"Read [](file:///Users/jane/Library/Application%20Support/Code/x.json)","uris":{"file:///Users/jane/work/api/main.go":{"path":"/Users/jane/work/api/main.go"}}}}]}` + "\n"
	res := scrubJSONL(t, newScrubber(t), "copilot", line)
	if out := string(res.Out.Bytes()); strings.Contains(out, "jane") {
		t.Errorf("username survived: %s", out)
	}
}

// toolCallResults is keyed by the tool call id and uris by the file URI; both keys survive in
// every shape VS Code writes them, while a token used as a key is still removed.
func TestCopilotIDKeyedMapsKeepTheirKeys(t *testing.T) {
	call := "call_r4KeDgjw3zhljpnAV4CdlZT0"
	uri := "file:///Users/__USER__/Library/Application%20Support/Code/User/workspaceStorage/e7f61a270e86b41cc7bc4f2d7d4f3938/GitHub.copilot-chat/debug-logs/models.json"
	results := `{"toolCallResults":{"` + call + `":{"$mid":20,"content":[{"$mid":21,"value":"ok"}]}}}`
	uris := `"uris":{"` + uri + `":{"$mid":1,"scheme":"file"}}`
	lines := []string{
		`{"kind":1,"k":["requests",0,"result"],"v":{"metadata":` + results + `}}`,
		`{"kind":2,"k":["requests"],"v":[{"requestId":"request_1","result":{"metadata":` + results + `},"response":[{"invocationMessage":{"value":"Read",` + uris + `}}]}]}`,
		`{"kind":0,"v":{"requests":[{"result":{"metadata":` + results + `},"response":[{"pastTenseMessage":{"value":"Read",` + uris + `}}]}]}}`,
		`{"kind":2,"k":["requests",0,"response"],"v":[{"invocationMessage":{"value":"Read",` + uris + `},"pastTenseMessage":{"value":"Read",` + uris + `}}]}`,
	}
	payload := strings.Join(lines, "\n") + "\n"
	res := scrubJSONL(t, newScrubber(t), "copilot", payload)
	if got := string(res.Out.Bytes()); got != payload {
		t.Errorf("id-keyed copilot maps changed (hits %v)\n got %s\nwant %s", res.RuleHits, got, payload)
	}

	pat := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	res = scrubJSONL(t, newScrubber(t), "copilot", `{"kind":1,"k":["requests",0,"result"],"v":{"metadata":{"toolCallResults":{"`+pat+`":{}}}}}`+"\n")
	if strings.Contains(string(res.Out.Bytes()), pat) {
		t.Errorf("token survived as a toolCallResults key: %s", res.Out.Bytes())
	}
}
