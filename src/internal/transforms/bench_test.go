package transforms_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// BenchmarkScrubSynthetic is the portable harness a change can be iterated against;
// BenchmarkScrubRealData measures the truth. Two shapes load different parts of the ladder:
// transcript-1MiB is many short lines, so per-line cost dominates, while bigvalue-8MiB is one
// line whose payload sits in a single string, so the per-value matchers do.
func BenchmarkScrubSynthetic(b *testing.B) {
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
		{"transcript-1MiB", syntheticTranscript(1 << 20)},
		{"bigvalue-8MiB", syntheticBigValue(8 << 20)},
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

// syntheticTranscript builds JSONL lines shaped like a Claude Code transcript, with planted
// secrets at roughly the density real transcripts show.
func syntheticTranscript(size int) []byte {
	rng := rand.New(rand.NewSource(20260816))
	var b strings.Builder
	b.Grow(size + 4096)

	for i := 0; b.Len() < size; i++ {
		var line map[string]any
		switch i % 5 {
		case 0, 2:
			line = map[string]any{
				"parentUuid":  randUUID(rng),
				"isSidechain": false,
				"userType":    "external",
				"cwd":         "/Users/devuser/git/trajectory-shipper",
				"sessionId":   randUUID(rng),
				"version":     "1.0.60",
				"type":        "assistant",
				"message": map[string]any{
					"id":    "msg_01" + randToken(rng, 22),
					"role":  "assistant",
					"model": "claude-opus-4",
					"content": []any{
						map[string]any{"type": "text", "text": randProse(rng, 200+rng.Intn(600))},
					},
					"usage": map[string]any{"input_tokens": 4211, "output_tokens": 118},
				},
				"uuid":      randUUID(rng),
				"timestamp": "2026-08-16T09:12:44.117Z",
			}
		case 1, 3:
			line = map[string]any{
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
							"content":     randCommandOutput(rng, 300+rng.Intn(900)),
						},
					},
				},
				"toolUseResult": map[string]any{
					"stdout":      randCommandOutput(rng, 200+rng.Intn(400)),
					"stderr":      "",
					"tool_use_id": "toolu_01" + randToken(rng, 22),
				},
				"uuid":      randUUID(rng),
				"timestamp": "2026-08-16T09:12:45.002Z",
			}
		default:
			line = map[string]any{
				"parentUuid": randUUID(rng),
				"cwd":        "/Users/devuser/git/trajectory-shipper",
				"sessionId":  randUUID(rng),
				"type":       "user",
				"message": map[string]any{
					"role":    "user",
					"content": randProse(rng, 120+rng.Intn(300)),
				},
				"uuid":      randUUID(rng),
				"timestamp": "2026-08-16T09:12:46.551Z",
			}
			if i%37 == 0 {
				// Enough planted secrets to exercise the re-serialize path, without a file of secrets.
				line["message"].(map[string]any)["content"] =
					"export AWS_SECRET_ACCESS_KEY=" + randToken(rng, 40) + " && ./deploy.sh"
			}
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

// syntheticBigValue is one record whose tool result holds the whole payload: the shape a
// spilled build log or a big file read takes.
func syntheticBigValue(size int) []byte {
	rng := rand.New(rand.NewSource(20260817))
	body := randCommandOutput(rng, size)
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

const hexDigits = "0123456789abcdef"

func randUUID(rng *rand.Rand) string {
	var sb strings.Builder
	for i, n := range []int{8, 4, 4, 4, 12} {
		if i > 0 {
			sb.WriteByte('-')
		}
		for j := 0; j < n; j++ {
			sb.WriteByte(hexDigits[rng.Intn(16)])
		}
	}
	return sb.String()
}

const tokenAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

func randToken(rng *rand.Rand, n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteByte(tokenAlphabet[rng.Intn(len(tokenAlphabet))])
	}
	return sb.String()
}

var proseWords = strings.Fields(`the scrubber walks every decoded string value and applies
the pattern packs before the entropy backstop so a false positive costs a placeholder and a
false negative costs a leak I will read the file first then run the tests and report what
changed the manifest records density per object so a rule that starts eating content shows
up as a delta rather than an absolute number let me check the engine ladder again`)

func randProse(rng *rand.Rand, n int) string {
	var sb strings.Builder
	for sb.Len() < n {
		if sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(proseWords[rng.Intn(len(proseWords))])
	}
	return sb.String()
}

// randCommandOutput imitates tool output: paths, hex digests, quoted fragments and the
// occasional long token, which is what the candidate scanner actually meets.
func randCommandOutput(rng *rand.Rand, n int) string {
	var sb strings.Builder
	sb.Grow(n + 128)
	for sb.Len() < n {
		switch rng.Intn(6) {
		case 0:
			fmt.Fprintf(&sb, "internal/scrub/%s.go:%d:%d: %s\n",
				proseWords[rng.Intn(len(proseWords))], rng.Intn(900)+1, rng.Intn(80)+1,
				randProse(rng, 40))
		case 1:
			fmt.Fprintf(&sb, "%s  refs/heads/%s\n", randHex(rng, 40),
				proseWords[rng.Intn(len(proseWords))])
		case 2:
			fmt.Fprintf(&sb, "  \"%s\": \"%s\",\n", proseWords[rng.Intn(len(proseWords))],
				randToken(rng, 8+rng.Intn(30)))
		case 3:
			fmt.Fprintf(&sb, "ok  \tgithub.com/QuesmaOrg/quesma-shipper/internal/%s\t%d.%03ds\n",
				proseWords[rng.Intn(len(proseWords))], rng.Intn(9), rng.Intn(999))
		default:
			sb.WriteString(randProse(rng, 60+rng.Intn(60)))
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func randHex(rng *rand.Rand, n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteByte(hexDigits[rng.Intn(16)])
	}
	return sb.String()
}

// benchRealDataCap bounds how much of the tree one iteration scrubs: enough to dominate any
// fixed cost, small enough to keep a run coffee-length.
const benchRealDataCap = 256 << 20

// BenchmarkScrubRealData is the measurement that counts: a real transcript tree, whose value
// lengths, secret density and prose no generator reproduces. It skips unless SCRUB_BENCH_DIR
// names a directory of .jsonl files, so CI never depends on private data.
//
//	SCRUB_BENCH_DIR=$HOME/.claude/projects go test ./internal/scrub/ \
//	    -bench BenchmarkScrubRealData -benchmem -run '^$' -benchtime 1x
//
// Run it serially: it is minutes long, and a benchmark sharing the machine moves the number
// more than most changes do.
func BenchmarkScrubRealData(b *testing.B) {
	root := os.Getenv("SCRUB_BENCH_DIR")
	if root == "" {
		b.Skip("set SCRUB_BENCH_DIR to a directory of .jsonl transcripts (e.g. ~/.claude/projects)")
	}

	type file struct {
		name    string
		payload []byte
	}
	var files []file
	total := 0
	// Deterministic order, so two runs over the same tree scrub the same sample.
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if total >= benchRealDataCap {
			return fs.SkipAll
		}
		if d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, file{name: path, payload: raw})
		total += len(raw)
		return nil
	})
	if err != nil {
		b.Fatalf("load %s: %v", root, err)
	}
	if total == 0 {
		b.Fatalf("no .jsonl files under %s", root)
	}
	b.Logf("real corpus: %d files, %.1f MiB", len(files), float64(total)/(1<<20))

	cfg := transforms.DefaultConfig()
	cfg.Username = "devuser"
	s, err := transforms.New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	hint := transforms.Hint{Family: "claude-code", JSONL: true}
	b.SetBytes(int64(total))
	b.ReportAllocs()
	for b.Loop() {
		for _, f := range files {
			if _, err := s.Scrub(f.payload, hint); err != nil {
				b.Fatalf("scrub %s: %v", f.name, err)
			}
		}
	}
}
