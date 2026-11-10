//go:build perf

package perf

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func smokeCursorForceResend(t *testing.T) {
	t.Run("cursor-force-resend-bounded-history", func(t *testing.T) {
		w := stageWorld(t)
		w.gomaxprocs = smokeGOMAXPROCS
		w.extraEnv = append(w.extraEnv, fmt.Sprintf("%s=%d", envMaxInFlightBytes, inFlightCap))
		const files = 2048
		const budget = 256 << 20
		dir := filepath.Join(w.Home, ".cursor", "projects", "perf", "agent-transcripts")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"` + strings.Repeat("workspace ", (1<<20)/10) + `"}]}}` + "\n")
		master := filepath.Join(t.TempDir(), "transcript.jsonl")
		if err := os.WriteFile(master, body, 0o600); err != nil {
			t.Fatal(err)
		}
		for i := range files {
			if err := os.Link(master, filepath.Join(dir, fmt.Sprintf("%08d.jsonl", i))); err != nil {
				t.Fatal(err)
			}
		}
		for attempt := range 2 {
			obs := w.observedSync(t, "--force-resend")
			if obs.Err != nil {
				t.Fatalf("forced attempt %d: %v\n%s", attempt, obs.Err, obs.Output)
			}
			counts := summary(t, obs.Output)
			if attempt == 0 && counts["shipped"] < files {
				t.Fatalf("forced history incomplete: %s", obs.Output)
			}
			if attempt == 1 && counts["unchanged"] < files {
				t.Fatalf("retry lost progress: %s", obs.Output)
			}
			t.Logf("attempt %d: %d bytes of Cursor history, peak RSS %d (%s), elapsed %v", attempt, files*len(body), obs.PeakRSS, obs.PeakRSSSource, obs.Elapsed)
			assertPeakUnderBudget(t, "cursor-force-resend", obs, budget)
		}
		cursorKeys := 0
		for key, versions := range w.versionCounts(t) {
			if strings.Contains(key, "/source=cursor-transcripts/") {
				cursorKeys++
				if versions != 1 {
					t.Errorf("%s: %d versions after retry", key, versions)
				}
			}
		}
		if cursorKeys != files {
			t.Fatalf("stored %d Cursor transcripts, want %d", cursorKeys, files)
		}
	})
}
