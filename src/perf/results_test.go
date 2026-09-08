//go:build perf

// The machine-readable half of what this tier reports: one JSON object per scenario, appended to a
// file CI keeps. Append-only and one line per record, so a killed run still leaves every scenario
// that finished, and provenance travels with every row.
package perf

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Also the keys in the results file, and append-only: a rename is a silently reset series.
const (
	selfTestScenario         = "harness-selftest"
	smokeBacklogScenario     = "smoke-backlog"
	smokeSteadyScenario      = "smoke-steady"
	acceptanceScenario       = "smoke-big-file-acceptance"
	bigFileScenario          = "smoke-big-file"
	inFlightScenario         = "smoke-in-flight-cap"
	singleLineScenario       = "single-line-file"
	singleLineSecretScenario = "single-line-file-redacted"
	scrubGuardScenario       = "smoke-scrub-guard"
)

// Names the file the rows are appended to; CI points it at the workspace so the upload finds it.
const resultsEnv = "SHIPPER_PERF_RESULTS"

// One scenario's row. Fields a scenario does not measure stay zero, and names are append-only:
// a renamed field silently resets the history for whatever reads this later.
type perfResult struct {
	Scenario   string    `json:"scenario"`
	RecordedAt time.Time `json:"recorded_at"`

	GitSHA    string `json:"git_sha"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	GoVersion string `json:"go_version"`

	CorpusFiles     int   `json:"corpus_files"`
	CorpusBytes     int   `json:"corpus_bytes"`
	ChildGOMAXPROCS int   `json:"child_gomaxprocs"`
	ShapedRTTMillis int64 `json:"shaped_rtt_ms"`

	// Every repetition and the minimum over them: the spread says whether the best is a sample.
	RepSeconds  []float64 `json:"rep_seconds,omitempty"`
	BestSeconds float64   `json:"best_seconds,omitempty"`

	// The reference a scenario subtracted; a bound on added time cannot be read without it.
	BaselineSeconds float64 `json:"baseline_seconds,omitempty"`

	ProxiedBytesUp   int64 `json:"proxied_bytes_up"`
	ProxiedBytesDown int64 `json:"proxied_bytes_down"`
	S3Requests       int64 `json:"s3_requests"`
	Objects          int   `json:"objects"`

	PeakRSSBytes      int64   `json:"peak_rss_bytes"`
	PeakRSSSource     string  `json:"peak_rss_source"`
	MemoryBudgetBytes int64   `json:"memory_budget_bytes,omitempty"`
	CPUSeconds        float64 `json:"cpu_seconds"`

	// Omitted like MemoryBudgetBytes: a scenario that did not gate on CPU reads absent, not zero.
	CPUBudgetSeconds float64 `json:"cpu_budget_seconds,omitempty"`

	ExitStatus string `json:"exit_status"`
}

// Provenance is filled in here: a scenario knows what it measured, not which commit it runs on.
func recordResult(t *testing.T, r perfResult) {
	t.Helper()
	if r.Scenario == "" {
		t.Fatalf("a result row with no scenario name: nothing downstream can key on it")
	}
	if r.RecordedAt.IsZero() {
		r.RecordedAt = time.Now().UTC()
	}
	r.GitSHA = gitSHA(t)
	r.GOOS, r.GOARCH = runtime.GOOS, runtime.GOARCH
	r.GoVersion = runtime.Version()

	line, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal the result for %s: %v", r.Scenario, err)
	}

	path := resultsPath(t)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open the results file %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatalf("append to the results file %s: %v", path, err)
	}

	t.Logf("result %s: best %.3fs over %d reps, %d up / %d down bytes, %d S3 requests, "+
		"%d objects, peak %d bytes (%s), %.2f cpu seconds, %s",
		r.Scenario, r.BestSeconds, len(r.RepSeconds), r.ProxiedBytesUp, r.ProxiedBytesDown,
		r.S3Requests, r.Objects, r.PeakRSSBytes, r.PeakRSSSource, r.CPUSeconds, r.ExitStatus)
}

var (
	resultsOnce sync.Once
	resultsFile string
)

// Where this tier writes what it wants kept: the rows and the forensic files beside them.
func resultsDir() string {
	if v := os.Getenv(resultsEnv); v != "" {
		return filepath.Dir(v)
	}
	return filepath.Join(os.TempDir(), "trajectory-shipper-perf")
}

// Resolves the file once and says where it is: a results file nobody can find is no results file.
func resultsPath(t *testing.T) string {
	t.Helper()
	resultsOnce.Do(func() {
		resultsFile = os.Getenv(resultsEnv)
		if resultsFile == "" {
			resultsFile = filepath.Join(resultsDir(), "results.ndjson")
		}
		if err := os.MkdirAll(filepath.Dir(resultsFile), 0o700); err != nil {
			t.Fatalf("create the results directory for %s: %v", resultsFile, err)
		}
		t.Logf("results: appending to %s (%s overrides it)", resultsFile, resultsEnv)
	})
	return resultsFile
}

var (
	shaOnce sync.Once
	sha     string
)

// Its own variable rather than GITHUB_SHA: on a pull_request that is the ephemeral
// refs/pull/N/merge commit, so every PR row would be keyed to an object no repository keeps.
const shaEnv = "SHIPPER_PERF_GIT_SHA"

// What CI named, else GITHUB_SHA, else git. "unknown" rather than a failed tier, said out loud
// once: such a row is still forensics, but it is worthless as a trend point.
func gitSHA(t *testing.T) string {
	t.Helper()
	shaOnce.Do(func() {
		for _, name := range []string{shaEnv, "GITHUB_SHA"} {
			if env := strings.TrimSpace(os.Getenv(name)); env != "" {
				sha = env
				return
			}
		}
		cmd := exec.Command("git", "rev-parse", "HEAD")
		cmd.Dir = "../.."
		out, err := cmd.Output()
		if err != nil {
			sha = "unknown"
			t.Logf("results: no GITHUB_SHA and git rev-parse failed (%v); "+
				"every row this run is recorded against an unknown commit", err)
			return
		}
		sha = strings.TrimSpace(string(out))
	})
	return sha
}
