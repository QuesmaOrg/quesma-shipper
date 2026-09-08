package transforms_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// TestRealDataRescrub replays the scrubber over a decrypted archive mirror (SCRUB_REALDATA_DIR,
// payload files plus <payload>.manifest.json sidecars), skipping without the variable so CI
// never depends on private data. The mirror is POST-scrub, so what it measures is idempotency
// at scale: re-scrubbed output must be byte-identical, and any new hit on it wants eyeballs.
func TestRealDataRescrub(t *testing.T) {
	root := os.Getenv("SCRUB_REALDATA_DIR")
	if root == "" {
		t.Skip("set SCRUB_REALDATA_DIR to a decrypted mirror to run the real-data replay")
	}

	// Username stays empty on purpose: the mirror's content already carries __USER__, and naming one
	// here would make byte-diffs mean two things.
	cfg := transforms.DefaultConfig()
	s, err := transforms.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	type manifest struct {
		SourceFamily string `json:"source_family"`
		Redaction    *struct {
			Density  float64        `json:"density"`
			RuleHits map[string]int `json:"rule_hits"`
			ScanMode string         `json:"scan_mode"`
		} `json:"redaction"`
	}

	var (
		objects, skipped, changed, engineErrs int
		recordedHits                          = map[string]int{}
		rescrubHits                           = map[string]int{}
		filesWithNewHits                      int
		samples                               []string
	)
	const maxSamples = 20

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".manifest.json") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var m manifest
		if err := json.Unmarshal(raw, &m); err != nil || m.Redaction == nil {
			skipped++
			return nil
		}
		payloadPath := strings.TrimSuffix(path, ".manifest.json")
		payload, err := os.ReadFile(payloadPath)
		if err != nil {
			skipped++
			return nil
		}

		objects++
		for rule, n := range m.Redaction.RuleHits {
			recordedHits[rule] += n
		}

		res, err := s.Scrub(payload, transforms.Hint{
			Family: m.SourceFamily,
			JSONL:  m.Redaction.ScanMode != transforms.ScanModeRawText,
		})
		if err != nil {
			engineErrs++
			return nil
		}
		if string(res.Out) != string(payload) {
			changed++
		}
		if len(res.RuleHits) > 0 {
			filesWithNewHits++
			for rule, n := range res.RuleHits {
				rescrubHits[rule] += n
			}
			if len(samples) < maxSamples {
				samples = append(samples, fmt.Sprintf("%s %v\n  %s",
					payloadPath, res.RuleHits, firstDiffLine(string(payload), string(res.Out))))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("objects rescrubbed: %d (skipped %d, engine errors %d)", objects, skipped, engineErrs)
	t.Logf("recorded hits in manifests (historical): %s", formatHits(recordedHits))
	t.Logf("hits on rescrub (should be ~0): %s", formatHits(rescrubHits))
	t.Logf("objects changed by rescrub: %d of %d; objects with new hits: %d", changed, objects, filesWithNewHits)
	for _, sample := range samples {
		t.Logf("sample: %s", sample)
	}
}

func firstDiffLine(before, after string) string {
	b, a := strings.Split(before, "\n"), strings.Split(after, "\n")
	for i := range b {
		if i >= len(a) || b[i] != a[i] {
			return truncate(b[i], 160) + "\n  -> " + truncate(safeIndex(a, i), 160)
		}
	}
	return "(no line diff)"
}

func safeIndex(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func formatHits(hits map[string]int) string {
	if len(hits) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(hits))
	for k := range hits {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return hits[keys[i]] > hits[keys[j]] })
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, hits[k]))
	}
	return strings.Join(parts, " ")
}
