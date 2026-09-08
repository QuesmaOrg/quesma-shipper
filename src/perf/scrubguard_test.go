//go:build perf

// The scrub guard: what a sync burns in CPU. Scrubbing is the largest term, and the transforms
// benchmarks only prove the transform in isolation; this proves the shipped binary end to end. The
// gate is CPU seconds, not wall clock: CPU excludes the waiting where a shared runner's noise lives.
package perf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Big enough that scrubbing dominates the CPU; the same size as the memory guard's fixture.
const scrubGuardBytes int64 = 32 << 20

// Density chosen in bytes, not lines: at 8 KiB a line, the corpus's every-twentieth cadence would
// leave this fixture twenty times thinner in secrets. Half the values stay clean, so one run
// exercises both the gated verification path and the prefilter's fast path.
const scrubGuardSecretEveryLines = 2

// Cold repetitions the CPU minimum is taken over; see smokeScrubGuard for why the minimum.
const scrubGuardReps = 3

// Both budgets are PROVISIONAL. 1.3x over the observed ceiling is only safe because the judged CPU
// number is a minimum of cold repetitions, and the memory budget is measured for this scenario, which
// runs under neither GOMEMLIMIT nor a cgroup, rather than borrowed from the memory guard. A machine
// slower than the whole sample extends the sample; loosening either budget to pass is not an option.
const (
	scrubGuardCPUBudget       = 0.95      // seconds: 1.3x the 0.699 s runner ceiling
	scrubGuardBudget    int64 = 272 << 20 // bytes: 1.3x the 218 MB worst
)

// One secret-dense payload and a ceiling on what the child burned. No cgroup and no GOMEMLIMIT,
// deliberately: a soft memory limit buys collector CPU, which is the number this scenario reads,
// and a capped run's rusage would describe the sudo and systemd-run wrapper chain.
func smokeScrubGuard(t *testing.T) {
	t.Run("S5-scrub-cpu-guard", func(t *testing.T) {
		w := stageWorld(t)
		w.gomaxprocs = smokeGOMAXPROCS

		staged, secrets := stageIncompressibleValue(t, w, 0, scrubGuardBytes, scrubGuardSecretEveryLines)
		t.Logf("%s: %d logical bytes carrying %d planted secret pairs, one per %d bytes of fill",
			scrubGuardScenario, staged, secrets, scrubGuardSecretEveryLines*memguardLineFill)

		// The CPU gate judges the minimum, since interference on a shared runner only ever adds
		// CPU. The memory gate stays per-repetition: a peak in any of them is the finding.
		var best childObservation
		reps := make([]time.Duration, 0, scrubGuardReps)
		for rep := range scrubGuardReps {
			if rep > 0 {
				w.reset(t)
			}
			obs := runUnderBudget(t, w, scrubGuardScenario,
				resourceBudget{memory: scrubGuardBudget, cpu: scrubGuardCPUBudget}, 1, staged)
			reps = append(reps, time.Duration(obs.CPUSeconds*float64(time.Second)))
			if rep == 0 || obs.CPUSeconds < best.CPUSeconds {
				best = obs
			}
		}
		t.Logf("%s: cpu across %d cold repetitions: %s, judging the minimum",
			scrubGuardScenario, scrubGuardReps, durationList(reps))
		assertCPUUnderBudget(t, scrubGuardScenario, best, scrubGuardCPUBudget)

		assertRuleHits(t, w, corpusSessionID(0), map[string]int{
			"github-pat":        secrets,
			"aws-access-key-id": secrets,
			// The fixture's own guard: a filler that drifted past memguardRun would be redacted
			// wholesale and the scenario would stop measuring what it claims to.
			"generic-entropy": 0,
		})
	})
}

// Scoped to the shipped entries for this transcript: a sync also scrubs derived objects, and a sum
// over every entry would fold their hits into a count the fixture is supposed to predict exactly.
func assertRuleHits(t *testing.T, w *world, session string, want map[string]int) {
	t.Helper()
	// This tier is its own module and cannot import the shipper's internal auditlog package, so it
	// decodes the two fields it judges directly.
	raw, err := os.ReadFile(filepath.Join(w.State, "trajectory-shipper", "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Decision string         `json:"decision"`
			File     string         `json:"file"`
			RuleHits map[string]int `json:"rule_hits"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("audit log line is not JSON: %v", err)
		}
		if entry.Decision != "shipped" || !strings.HasSuffix(entry.File, session+".jsonl") {
			continue
		}
		found = true
		for rule, n := range entry.RuleHits {
			got[rule] += n
		}
	}
	if !found {
		t.Fatalf("no shipped audit entry for %s.jsonl: the ledger this gate reads is not the run's",
			session)
	}
	for rule, n := range want {
		if got[rule] != n {
			t.Errorf("rule %q recorded %d hits, want %d (whole ledger for this file: %v)",
				rule, got[rule], n, got)
		}
	}
}
