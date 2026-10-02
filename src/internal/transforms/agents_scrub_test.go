package transforms_test

import (
	"strings"
	"testing"
)

const agentCall = "toolu_bdrk_015BTZL2HUsWU5Ta9njsHLe4"

func TestAgentJoinIdsSurvive(t *testing.T) {
	for family, lines := range map[string][]string{
		"pi": {
			`{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":[{"type":"toolCall","id":"` + agentCall + `","name":"read","arguments":{"path":"calc.py"}}]}}`,
			`{"type":"message","id":"r1","parentId":"a1","message":{"role":"toolResult","toolCallId":"` + agentCall + `","toolName":"read","content":[{"type":"text","text":"ok"}]}}`,
		},
		"opencode": {
			`{"type":"session","id":"ses_f041cb039ffebeDrMjfIBP7C83","parent_id":"ses_e9b2c71a4ffeXq8RtLmN3vKd5w","session_id":"ses_f041cb039ffebeDrMjfIBP7C83"}`,
			`{"type":"message","id":"msg_0fbe350ac001FqtpJHCRuFI7xt","session_id":"ses_f041cb039ffebeDrMjfIBP7C83","data":{"role":"assistant","parentID":"msg_0fbe34fdf001Z6a2yMHKixDAkN"}}`,
			`{"type":"part","id":"prt_0fbe35a2c0011NbcDjFmzrK4wr","message_id":"msg_0fbe350ac001FqtpJHCRuFI7xt","session_id":"ses_f041cb039ffebeDrMjfIBP7C83","data":{"type":"tool","tool":"glob","callID":"` + agentCall + `"}}`,
		},
		"hermes": {
			`{"type":"message","id":2,"role":"assistant","tool_calls":"[{\"id\": \"` + agentCall + `\", \"call_id\": \"` + agentCall + `\", \"response_item_id\": \"fc_` + agentCall + `\", \"type\": \"function\"}]"}`,
			`{"type":"message","id":3,"role":"tool","tool_call_id":"` + agentCall + `","tool_name":"read_file"}`,
		},
	} {
		payload := strings.Join(lines, "\n") + "\n"
		res := scrubJSONL(t, newScrubber(t), family, payload)
		if got := string(res.Out.Bytes()); got != payload {
			t.Errorf("%s: exempt fields changed (hits %v)\n got %s\nwant %s", family, res.RuleHits, got, payload)
		}
	}
}

func TestAgentExemptFieldsStillMeetThePatternRules(t *testing.T) {
	for family, line := range map[string]string{
		"pi":       `{"message":{"role":"toolResult","toolCallId":"` + pat + `"}}`,
		"opencode": `{"type":"part","data":{"callID":"` + pat + `"}}`,
		"hermes":   `{"tool_calls":"[{\"id\": \"` + pat + `\"}]"}`,
	} {
		res := scrubJSONL(t, newScrubber(t), family, line+"\n")
		if strings.Contains(string(res.Out.Bytes()), pat) {
			t.Errorf("%s: token survived in an exempt field: %s", family, res.Out.Bytes())
		}
	}
}

func TestPiDropsThinkingSignaturesAndImages(t *testing.T) {
	blob := strings.Repeat(opaque, 5)
	line := `{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hm","thinkingSignature":"` + blob + `"},{"type":"image","data":"` + blob + `","mimeType":"image/png"}]}}` + "\n"
	res := scrubJSONL(t, newScrubber(t), "pi", line)
	if out := string(res.Out.Bytes()); strings.Contains(out, opaque[:12]) || res.RuleHits["dropped"] != 2 {
		t.Errorf("not dropped (hits %v): %s", res.RuleHits, out)
	}
}
