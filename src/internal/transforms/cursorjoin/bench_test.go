package cursorjoin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// BenchmarkAlign loads the candidate weighing: argument-less terminal bubbles send every Shell
// block into the look-behind, and the periodic re-runs into the repeat check over all events.
func BenchmarkAlign(b *testing.B) {
	for _, n := range []int{50, 500, 2000} {
		content, events := syntheticConversation(n)
		b.Run(fmt.Sprintf("tools=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				a, err := alignAndRender(content, events)
				if err != nil {
					b.Fatal(err)
				}
				if a.mismatches > 0 {
					b.Fatalf("%d mismatches", a.mismatches)
				}
			}
		})
	}
}

// syntheticConversation is n tool calls in the current store generation's shapes: a user query
// every ten calls, a prose block before each call, and every 40th call a re-run the store deduped.
func syntheticConversation(n int) ([]byte, []*bubble) {
	var lines []string
	var events []*bubble
	mustJSON := func(v any) string {
		out, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return string(out)
	}
	addBubble := func(b *bubble) {
		b.BubbleID = fmt.Sprintf("b%05d", len(events))
		events = append(events, b)
	}
	var lastRead map[string]any
	for i := range n {
		if i%10 == 0 {
			q := fmt.Sprintf("audit package %d and fix what the tests find", i/10)
			lines = append(lines, mustJSON(map[string]any{"role": "user", "message": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "<user_query>\n" + q + "\n</user_query>"}}}}))
			addBubble(&bubble{Type: 1, Text: q})
		}
		prose := fmt.Sprintf("Looking at step %d of the audit before changing anything.", i)
		var name string
		var input map[string]any
		tf := &toolFormerData{ToolCallID: fmt.Sprintf("call_%05d", i), Status: "completed", Result: strings.Repeat("output line\n", 8)}
		switch i % 3 {
		case 0:
			name = "Read"
			path := fmt.Sprintf("/Users/jane/work/api/internal/pkg%03d/handler_%d.go", i/3, i)
			input = map[string]any{"path": path, "offset": 1, "limit": 200}
			tf.Name, tf.RawArgs = "read_file_v2", mustJSON(map[string]any{"targetFile": path, "offset": 1, "limit": 200, "toolCallId": tf.ToolCallID})
			lastRead = input
		case 1:
			name = "Grep"
			input = map[string]any{"pattern": fmt.Sprintf("func handle%dRequest", i), "path": "/Users/jane/work/api/internal", "glob": "*.go"}
			tf.Name, tf.RawArgs = "ripgrep_raw_search", mustJSON(map[string]any{"pattern": input["pattern"], "path": input["path"], "globPattern": "*.go", "caseInsensitive": false})
		default:
			name = "Shell"
			input = map[string]any{"command": fmt.Sprintf("go test ./internal/pkg%03d/... -run TestHandle%d", i/3, i), "description": "Run the package tests"}
			tf.Name, tf.RawArgs = "run_terminal_command_v2", "{}"
		}
		blocks := []any{map[string]any{"type": "text", "text": prose}, map[string]any{"type": "tool_use", "name": name, "input": input}}
		if i%40 == 39 && lastRead != nil {
			blocks = append(blocks, map[string]any{"type": "tool_use", "name": "Read", "input": lastRead})
		}
		lines = append(lines, mustJSON(map[string]any{"role": "assistant", "message": map[string]any{"content": blocks}}))
		addBubble(&bubble{Type: 2, Text: prose})
		addBubble(&bubble{Type: 2, CapabilityType: "15", ToolFormerData: tf})
	}
	lines = append(lines, `{"type":"turn_ended","status":"success"}`)
	return []byte(strings.Join(lines, "\n") + "\n"), events
}
