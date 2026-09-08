//go:build perf

// The corpus this tier measures against: deterministic to the byte, so two staged worlds are
// the same backlog and a timing difference is about the pipeline, not about the corpus.
// 10240 small files over 8 projects is the scale at which per-file waste becomes the cost.
package perf

import (
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	corpusFiles    = 10240
	corpusProjects = 8
	corpusMinBytes = 4 << 10
	corpusMaxBytes = 12 << 10

	// A literal, never a clock: the two runs of a comparison must share a corpus.
	corpusSeed = 0x5E3D_1CE5
)

// Seeded secrets at roughly one line in twenty: redaction is a real part of what a sync costs.
const (
	corpusGitHubToken = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	corpusAWSKey      = "AKIAIOSFODNN7EXAMPLE"
	secretEveryNLines = 20
)

// Built once per `go test` process and hardlinked into each world; TestMain removes the tree.
var (
	corpusOnce   sync.Once
	corpusMaster string
	corpusBytes  int
	corpusErr    error
)

func buildCorpusMaster() {
	dir, err := os.MkdirTemp("", "shipper-perf-corpus")
	if err != nil {
		corpusErr = err
		return
	}
	corpusMaster = dir
	corpusBytes, corpusErr = writeCorpus(dir, corpusFiles)
}

// One seeded source across the whole run: file i is the same file wherever it is written.
func writeCorpus(root string, n int) (int, error) {
	rng := rand.New(rand.NewSource(corpusSeed))
	bytes := 0
	for i := range n {
		slug := fmt.Sprintf("-Users-perf-work-project-%d", i%corpusProjects)
		session := corpusSessionID(i)
		body := corpusTranscript(rng, session, slug)

		full := filepath.Join(root, ".claude", "projects", slug, session+".jsonl")
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return bytes, err
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			return bytes, err
		}
		// The pre-filter compares size and mtime, so the stamp cannot be the wall clock.
		if err := os.Chtimes(full, fixtureMTime, fixtureMTime); err != nil {
			return bytes, err
		}
		bytes += len(body)
	}
	return bytes, nil
}

// For tests that only need the client to have something to do, not a backlog to measure.
const smallCorpusFiles = 4

// Written rather than hardlinked: the callers stage one world each, so a shared master buys nothing.
func stageCorpusFiles(t *testing.T, w *world, n int) int {
	t.Helper()
	bytes, err := writeCorpus(w.Home, n)
	if err != nil {
		t.Fatalf("stage %d corpus files into %s: %v", n, w.Home, err)
	}
	return bytes
}

func removeCorpusMaster() {
	if corpusMaster != "" {
		_ = os.RemoveAll(corpusMaster)
	}
}

// stageCorpus hardlinks the whole backlog into w and returns how many files it staged.
func stageCorpus(t *testing.T, w *world) int {
	t.Helper()
	corpusOnce.Do(buildCorpusMaster)
	if corpusErr != nil {
		t.Fatalf("build the corpus master: %v", corpusErr)
	}
	err := filepath.WalkDir(corpusMaster, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(corpusMaster, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(w.Home, rel), 0o700)
		}
		return os.Link(path, filepath.Join(w.Home, rel))
	})
	if err != nil {
		t.Fatalf("link the corpus into the world: %v", err)
	}
	// The denominator of every byte figure this tier reports.
	t.Logf("corpus: %d files, %d bytes staged", corpusFiles, corpusBytes)
	return corpusFiles
}

// A UUID-shaped id with a letter planted in every group: an all-digit id gets its first sixteen
// characters eaten by the card rule, quietly changing the sealed size of those files.
func corpusSessionID(i int) string {
	return fmt.Sprintf("d%07x-a%03x-4b%02x-8c%02x-e%011x", i, i, i%256, i%256, i)
}

// Every fixture opens with this: the jsonl sniff only looks at the first line being a JSON object.
const corpusFirstLine = `{"type":"user","uuid":"u0","sessionId":%q,"cwd":%q,"message":{"role":"user","content":[{"type":"text","text":"start the run"}]}}` + "\n"

// corpusTranscript builds one transcript of a size drawn from the seeded source.
func corpusTranscript(rng *rand.Rand, session, slug string) string {
	target := corpusMinBytes + rng.Intn(corpusMaxBytes-corpusMinBytes+1)
	cwd := "/Users/perf/work/" + strings.TrimPrefix(slug, "-Users-perf-work-")

	var b strings.Builder
	b.Grow(target + 1024)
	fmt.Fprintf(&b, corpusFirstLine, session, cwd)

	for line := 1; b.Len() < target; line++ {
		text := randomText(rng, 200+rng.Intn(600))
		if line%secretEveryNLines == 0 {
			text += " AWS_ACCESS_KEY_ID=" + corpusAWSKey + " token=" + corpusGitHubToken
		}
		fmt.Fprintf(&b, `{"type":"assistant","uuid":"a%d","sessionId":%q,"cwd":%q,"message":{"id":"m%d","model":"claude-opus-5","content":[{"type":"text","text":%q}],"usage":{"input_tokens":120,"output_tokens":340,"cache_read_input_tokens":9000}}}`+"\n",
			line, session, cwd, line, text)
	}
	return b.String()
}

const (
	textAlphabet = "abcdefghijklmnopqrstuvwxyz"

	// Short enough that no token reaches the entropy backstop's length floor: see randomText.
	maxWordLen = 12
)

// Word-shaped fill, not random characters: a run of 24+ base64-alphabet characters trips the
// scrubber's entropy backstop, and the tier would then measure redaction instead of shipping.
func randomText(rng *rand.Rand, n int) string {
	var b strings.Builder
	b.Grow(n + maxWordLen)
	for b.Len() < n {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		for i, word := 0, 3+rng.Intn(maxWordLen-2); i < word; i++ {
			b.WriteByte(textAlphabet[rng.Intn(len(textAlphabet))])
		}
	}
	return b.String()
}
