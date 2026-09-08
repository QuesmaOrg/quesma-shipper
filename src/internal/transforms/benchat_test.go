package transforms_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// BenchmarkScrubSyntheticAt is BenchmarkScrubSynthetic's corpus with '@' in it: the original has
// none, so the email rule (the most expensive pattern on real data) never fires there. A second
// benchmark rather than an edit to the first, whose numbers are the tracked series.
func BenchmarkScrubSyntheticAt(b *testing.B) {
	cfg := transforms.DefaultConfig()
	cfg.Username = "devuser"
	s, err := transforms.New(cfg)
	if err != nil {
		b.Fatal(err)
	}

	cases := []struct {
		name    string
		payload []byte
	}{
		{"transcript-1MiB", syntheticTranscriptAt(1 << 20)},
		{"bigvalue-8MiB", syntheticBigValueAt(8 << 20)},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := s.Scrub(tc.payload, transforms.Hint{Family: "claude-code", JSONL: true})
				if err != nil {
					b.Fatal(err)
				}
				if len(res.Out) == 0 {
					b.Fatal("empty output")
				}
			}
		})
	}
}

// randCommandOutputAt carries the '@' shapes tool output actually has: scoped package specs,
// decorators, doc tags, ssh targets, git author lines. Most are NOT emails, which is the point:
// the rule's cost is paid on every '@' and recovered only on the few that complete a match.
func randCommandOutputAt(rng *rand.Rand, n int) string {
	var sb strings.Builder
	sb.Grow(n + 128)
	for sb.Len() < n {
		switch rng.Intn(10) {
		case 0:
			fmt.Fprintf(&sb, "npm WARN deprecated @quesma/%s@%d.%d.%d: use @quesma/%s instead\n",
				proseWords[rng.Intn(len(proseWords))], rng.Intn(9), rng.Intn(20), rng.Intn(20),
				proseWords[rng.Intn(len(proseWords))])
		case 1:
			fmt.Fprintf(&sb, "commit %s\nAuthor: Dev User <devuser@example.com>\n", randHex(rng, 40))
		case 2:
			fmt.Fprintf(&sb, "  @param {%s} %s - %s\n", proseWords[rng.Intn(len(proseWords))],
				proseWords[rng.Intn(len(proseWords))], randProse(rng, 40))
		case 3:
			fmt.Fprintf(&sb, "ssh devuser@build-%02d.internal.example: %s\n", rng.Intn(40),
				randProse(rng, 40))
		case 4:
			fmt.Fprintf(&sb, "@decorator(name=\"%s\")\ndef %s(self):\n", proseWords[rng.Intn(len(proseWords))],
				proseWords[rng.Intn(len(proseWords))])
		default:
			sb.WriteString(randCommandOutput(rng, 120+rng.Intn(240)))
		}
	}
	return sb.String()
}

func syntheticTranscriptAt(size int) []byte {
	rng := rand.New(rand.NewSource(20260818))
	var b strings.Builder
	b.Grow(size + 4096)

	for i := 0; b.Len() < size; i++ {
		line := map[string]any{
			"parentUuid": randUUID(rng),
			"cwd":        "/Users/devuser/git/trajectory-shipper",
			"sessionId":  randUUID(rng),
			"type":       "user",
			"message": map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_01" + randToken(rng, 22),
						"content":     randCommandOutputAt(rng, 300+rng.Intn(900)),
					},
				},
			},
			"toolUseResult": map[string]any{
				"stdout":      randCommandOutputAt(rng, 200+rng.Intn(400)),
				"stderr":      "",
				"tool_use_id": "toolu_01" + randToken(rng, 22),
			},
			"uuid":      randUUID(rng),
			"timestamp": "2026-08-16T09:12:45.002Z",
		}
		if i%37 == 0 {
			line["message"].(map[string]any)["content"] =
				"export AWS_SECRET_ACCESS_KEY=" + randToken(rng, 40) + " && ./deploy.sh"
		}
		enc, err := json.Marshal(line)
		if err != nil {
			panic(err)
		}
		b.Write(enc)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func syntheticBigValueAt(size int) []byte {
	rng := rand.New(rand.NewSource(20260819))
	body := randCommandOutputAt(rng, size)
	line, err := json.Marshal(map[string]any{
		"type":      "user",
		"uuid":      randUUID(rng),
		"sessionId": randUUID(rng),
		"cwd":       "/Users/devuser/git/trajectory-shipper",
		"toolUseResult": map[string]any{
			"stdout":      body,
			"stderr":      "",
			"tool_use_id": "toolu_01" + randToken(rng, 22),
		},
		"timestamp": "2026-08-16T09:13:02.900Z",
	})
	if err != nil {
		panic(err)
	}
	return append(line, '\n')
}
