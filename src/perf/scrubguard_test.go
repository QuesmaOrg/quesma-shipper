//go:build perf

// The scrub guard: what a sync burns in CPU. Scrubbing is the largest term, and the transforms
// benchmarks only prove the transform in isolation; this proves the shipped binary end to end. The
// gate is CPU seconds, not wall clock: CPU excludes the waiting where a shared runner's noise lives.
package perf

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand/v2"
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

		runScrubGuard(t, w, scrubGuardScenario,
			resourceBudget{memory: scrubGuardBudget, cpu: scrubGuardCPUBudget}, staged)

		assertRuleHits(t, w, corpusSessionID(0), map[string]int{
			"github-pat":        secrets,
			"aws-access-key-id": secrets,
			// The fixture's own guard: a filler that drifted past memguardRun would be redacted
			// wholesale and the scenario would stop measuring what it claims to.
			"generic-entropy": 0,
		})
	})
}

// The CPU gate judges the minimum, since interference on a shared runner only ever adds CPU. The
// memory gate stays per-repetition: a peak in any of them is the finding.
func runScrubGuard(t *testing.T, w *world, scenario string, budget resourceBudget, staged int) {
	t.Helper()
	var best childObservation
	reps := make([]time.Duration, 0, scrubGuardReps)
	for rep := range scrubGuardReps {
		if rep > 0 {
			w.reset(t)
		}
		obs := runUnderBudget(t, w, scenario, budget, 1, staged)
		reps = append(reps, time.Duration(obs.CPUSeconds*float64(time.Second)))
		if rep == 0 || obs.CPUSeconds < best.CPUSeconds {
			best = obs
		}
	}
	t.Logf("%s: cpu across %d cold repetitions: %s, judging the minimum",
		scenario, scrubGuardReps, durationList(reps))
	assertCPUUnderBudget(t, scenario, best, budget.cpu)
}

// Codex stores tool arguments and outputs as JSON documents encoded inside a JSON string, which the
// scrubber walks a second time; the Claude-shaped fixture above never reaches that path.
const codexGuardBytes int64 = 32 << 20

// A planted pair every codexGuardSecretEveryCalls tool calls; output lines of a few filler words.
const (
	codexGuardSecretEveryCalls = 2
	codexGuardOutputBytes      = 6 << 10
	codexGuardOutputLineWords  = 4
)

// PROVISIONAL, by the convention above, and measured on a laptop only (Apple M1 Max, three runs):
// the runner sample has to extend both before they are trusted.
const (
	codexGuardCPUBudget       = 0.86      // seconds: 1.3x the 0.659 s laptop ceiling
	codexGuardBudget    int64 = 226 << 20 // bytes: 1.3x the 182 MB laptop worst
)

func smokeCodexScrubGuard(t *testing.T) {
	t.Run("S5b-codex-scrub-cpu-guard", func(t *testing.T) {
		w := stageWorld(t)
		w.gomaxprocs = smokeGOMAXPROCS

		session := corpusSessionID(0)
		staged, secrets := stageCodexRollout(t, w, session, codexGuardBytes, codexGuardSecretEveryCalls)
		t.Logf("%s: %d logical bytes carrying %d planted secret pairs, one per %d tool calls",
			codexGuardScenario, staged, secrets, codexGuardSecretEveryCalls)

		runScrubGuard(t, w, codexGuardScenario,
			resourceBudget{memory: codexGuardBudget, cpu: codexGuardCPUBudget}, staged)

		assertRuleHits(t, w, session, map[string]int{
			"github-pat":        secrets,
			"aws-access-key-id": secrets,
			"generic-entropy":   0,
		})
	})
}

// A live rollout in Codex's envelope: per tool call a function_call whose arguments are a JSON
// string, a function_call_output holding a JSON document with the text inside, the matching
// exec_command_end, and an assistant message. The text is memguardFill filler broken into lines, so
// the wire floor holds and the entropy backstop stays off it; secrets sit only in the doubly encoded
// output, where the embedded walk has to find them.
func stageCodexRollout(t *testing.T, w *world, session string, target int64, secretEvery int) (int, int) {
	t.Helper()
	cwd := "/Users/perf/work/codex"
	path := filepath.Join(w.Home, ".codex", "sessions", "2026", "07", "01",
		"rollout-2026-07-01T12-00-00-"+session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	encode := func(v any) string {
		t.Helper()
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	line := func(typ string, payload any) int {
		t.Helper()
		n, err := fmt.Fprintf(bw, `{"timestamp":"2026-07-01T12:00:00.000Z","type":%q,"payload":%s}`+"\n",
			typ, encode(payload))
		if err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return n
	}

	written := line("session_meta", map[string]any{"id": session, "timestamp": "2026-07-01T12:00:00Z",
		"cwd": cwd, "originator": "codex_cli_rs", "cli_version": "0.40.0"})
	src := rand.NewChaCha8(memguardStreamSeed(0))
	// Fresh filler per field: a field repeating another's bytes would compress under the wire floor.
	filler := func(n int) string {
		buf := make([]byte, n)
		memguardFill(t, src, buf)
		for i := memguardRun; i < len(buf); i += (memguardRun + 1) * codexGuardOutputLineWords {
			buf[i] = '\n'
		}
		return string(buf)
	}
	secretTail := " " + corpusGitHubToken + " " + corpusAWSKey + " end of line\n"
	secrets := 0
	for call := 1; int64(written) < target; call++ {
		id := fmt.Sprintf("call_%06d", call)
		text := filler(codexGuardOutputBytes)
		if secretEvery > 0 && call%secretEvery == 0 {
			text += secretTail
			secrets++
		}
		stdout := filler(codexGuardOutputBytes / 8)

		written += line("response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": id,
			"arguments": encode(map[string]any{"cmd": fmt.Sprintf("sed -n 1,200p internal/pkg/file%d.go", call),
				"workdir": cwd, "yield_time_ms": 10000})})
		written += line("response_item", map[string]any{"type": "function_call_output", "call_id": id,
			"output": encode(map[string]any{"output": text, "metadata": map[string]any{"exit_code": 0, "duration_seconds": 0.2}})})
		written += line("event_msg", map[string]any{"type": "exec_command_end", "call_id": id,
			"stdout": stdout, "stderr": "", "aggregated_output": stdout, "exit_code": 0,
			"duration": map[string]any{"secs": 0, "nanos": 200000000}})
		written += line("response_item", map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": fmt.Sprintf("Read file%d.go; moving on.", call)}}})
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("flush %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
	// The pre-filter compares size and mtime, so the stamp cannot be the wall clock.
	if err := os.Chtimes(path, fixtureMTime, fixtureMTime); err != nil {
		t.Fatal(err)
	}
	return written, secrets
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
