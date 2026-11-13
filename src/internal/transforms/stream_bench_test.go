package transforms_test

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// Both paths include input reads, scrubbing, hashing, sealing and draining the prepared object.
func BenchmarkLargeJSONLPipeline(b *testing.B) {
	id, _ := age.GenerateX25519Identity()
	scrubber, err := transforms.New(transforms.DefaultConfig())
	if err != nil {
		b.Fatal(err)
	}
	cases := []struct{ name, line string }{
		{"short-lines", `{"message":{"content":[{"type":"text","text":"Contact alice@example.com after completing the task."}]}}` + "\n"},
		{"inline-media", `{"message":{"content":[{"type":"image","source":{"data":"` + strings.Repeat("A", 2<<20) + `"}}]}}` + "\n"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			input, err := os.CreateTemp(b.TempDir(), "input.jsonl")
			if err != nil {
				b.Fatal(err)
			}
			size := int64(0)
			for size < 32<<20 {
				n, err := input.WriteString(tc.line)
				if err != nil {
					b.Fatal(err)
				}
				size += int64(n)
			}
			input.Close()
			for _, stream := range []bool{false, true} {
				b.Run(map[bool]string{false: "whole", true: "stream"}[stream], func(b *testing.B) {
					b.SetBytes(size)
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						m := manifest()
						if !stream {
							raw, _, err := platform.ReadWhole(input.Name(), size)
							if err != nil {
								b.Fatal(err)
							}
							m.SourceHash = transforms.Hash(raw)
							result, err := scrubber.Scrub(raw, transforms.Hint{Family: "claude-code", JSONL: true})
							if err != nil {
								b.Fatal(err)
							}
							body, _, err := transforms.Seal(m, result.Out, []age.Recipient{id.Recipient()})
							if err != nil {
								b.Fatal(err)
							}
							if len(body) == 0 {
								b.Fatal("empty object")
							}
						} else {
							source, err := os.Open(input.Name())
							if err != nil {
								b.Fatal(err)
							}
							plain, err := platform.NewSpool("")
							if err != nil {
								b.Fatal(err)
							}
							cipher, err := platform.NewSpool("")
							if err != nil {
								b.Fatal(err)
							}
							result, hash, err := scrubber.ScrubTo(context.Background(), plain, source, transforms.Hint{Family: "claude-code", JSONL: true}, size)
							source.Close()
							if err != nil {
								b.Fatal(err)
							}
							m.SourceHash = hash
							_, err = transforms.SealTo(cipher, m, result.Out, []age.Recipient{id.Recipient()})
							if err != nil {
								b.Fatal(err)
							}
							if _, err = cipher.Seek(0, io.SeekStart); err != nil {
								b.Fatal(err)
							}
							if _, err = io.Copy(io.Discard, cipher); err != nil {
								b.Fatal(err)
							}
							plain.Close()
							cipher.Close()
						}
					}
				})
			}
		})
	}
}
