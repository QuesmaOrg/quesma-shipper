package transforms_test

import (
	"encoding/base64"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// BenchmarkScrubSyntheticMedia A/Bs the compiled drops on signatures, images and Fernet tokens.
func BenchmarkScrubSyntheticMedia(b *testing.B) {
	cases := []struct {
		name, family string
		payload      []byte
	}{
		{"claude-4MiB", "claude-code", syntheticClaudeMedia(4 << 20)},
		{"codex-4MiB", "codex", syntheticCodexMedia(4 << 20)},
	}
	for _, variant := range []struct {
		name  string
		drops map[string]map[string]string
	}{{"drop", transforms.CompiledDrops()}, {"nodrop", nil}} {
		s := scrubberWith(b, func(cfg *transforms.Config) {
			cfg.Username = "devuser"
			cfg.Drops = variant.drops
		})
		for _, tc := range cases {
			b.Run(tc.name+"/"+variant.name, func(b *testing.B) {
				b.SetBytes(int64(len(tc.payload)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					res, err := s.Scrub(tc.payload, transforms.Hint{Family: tc.family, JSONL: true})
					if err != nil {
						b.Fatal(err)
					}
					if len(res.Out.Bytes()) == 0 {
						b.Fatal("empty output")
					}
				}
			})
		}
	}
}

func randBase64(rng *rand.Rand, n int, enc *base64.Encoding) string {
	raw := make([]byte, n)
	rng.Read(raw)
	return enc.EncodeToString(raw)
}

// randFernet has a Fernet token's layout (version 0x80, then a timestamp), so it reads "gAAAAA...".
func randFernet(rng *rand.Rand, n int) string {
	raw := make([]byte, n)
	rng.Read(raw)
	copy(raw, []byte{0x80, 0, 0, 0, 0})
	return base64.URLEncoding.EncodeToString(raw)
}

func syntheticClaudeMedia(size int) []byte {
	rng := rand.New(rand.NewSource(20260929))
	var sb strings.Builder
	for i := 0; sb.Len() < size; i++ {
		var line map[string]any
		switch {
		case i%150 == 0:
			image := randBase64(rng, 24<<10, base64.StdEncoding)
			id := "toolu_01" + randToken(rng, 22)
			line = map[string]any{
				"type": "user", "uuid": randUUID(rng), "sessionId": randUUID(rng),
				"message": map[string]any{"role": "user", "content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": id,
					"content": []any{map[string]any{"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "image/png", "data": image}}},
				}}},
				"toolUseResult": map[string]any{"type": "image", "file": map[string]any{
					"base64": image, "type": "image/png", "originalSize": 36000}},
			}
		case i%8 == 0:
			line = map[string]any{
				"type": "assistant", "uuid": randUUID(rng), "sessionId": randUUID(rng),
				"message": map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "thinking", "thinking": "", "signature": randBase64(rng, 1200, base64.StdEncoding)},
					map[string]any{"type": "text", "text": randProse(rng, 200+rng.Intn(600))},
				}},
			}
		default:
			line = map[string]any{
				"type": "user", "uuid": randUUID(rng), "sessionId": randUUID(rng),
				"cwd": "/Users/devuser/git/trajectory-shipper",
				"message": map[string]any{"role": "user", "content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": "toolu_01" + randToken(rng, 22),
					"content": randCommandOutput(rng, 300+rng.Intn(900)),
				}}},
			}
		}
		appendJSONLine(&sb, line)
	}
	return []byte(sb.String())
}

func syntheticCodexMedia(size int) []byte {
	rng := rand.New(rand.NewSource(20260930))
	var sb strings.Builder
	for i := 0; sb.Len() < size; i++ {
		var payload map[string]any
		switch {
		case i%300 == 0:
			payload = map[string]any{"type": "function_call_output", "call_id": "call_" + randToken(rng, 24),
				"output": []any{map[string]any{"type": "input_image",
					"image_url": "data:image/png;base64," + randBase64(rng, 24<<10, base64.StdEncoding)}}}
		case i%10 == 0:
			payload = map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": randFernet(rng, 1200)}
		default:
			payload = map[string]any{"type": "function_call_output", "call_id": "call_" + randToken(rng, 24),
				"output": randCommandOutput(rng, 300+rng.Intn(900))}
		}
		appendJSONLine(&sb, map[string]any{"timestamp": "2026-09-29T10:00:00.000Z", "type": "response_item", "payload": payload})
	}
	return []byte(sb.String())
}

func appendJSONLine(sb *strings.Builder, line map[string]any) {
	enc, err := json.Marshal(line)
	if err != nil {
		panic(err)
	}
	sb.Write(enc)
	sb.WriteByte('\n')
}
