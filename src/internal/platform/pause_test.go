package platform_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

func TestSetReadClearRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if platform.Read(dir).Paused {
		t.Fatal("a fresh install reads as paused")
	}
	if err := platform.Set(dir, "laptop going to a client site", time.Now(), time.Time{}); err != nil {
		t.Fatal(err)
	}

	got := platform.Read(dir)
	if !got.Paused {
		t.Fatal("not paused after Set")
	}
	if got.Reason != "laptop going to a client site" {
		t.Errorf("reason = %q", got.Reason)
	}
	if got.At == "" {
		t.Error("no timestamp: `status` could not say since when")
	}

	if err := platform.Clear(dir); err != nil {
		t.Fatal(err)
	}
	if platform.Read(dir).Paused {
		t.Fatal("still paused after Clear")
	}
}

func TestSetAndClearAreIdempotent(t *testing.T) {
	dir := t.TempDir()

	// Repeated pause and resume operations must remain script-safe.
	for i := 0; i < 3; i++ {
		if err := platform.Set(dir, "", time.Now(), time.Time{}); err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := platform.Clear(dir); err != nil {
			t.Fatalf("Clear %d: %v", i, err)
		}
	}
}

// The failure directions are not symmetric, so the fail-closed choice is tested explicitly.
func TestAnUnreadableFlagReadsAsPaused(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "not json at all",
			body: "paused\n",
		},
		{
			name: "truncated mid-write",
			body: `{"paused":tr`,
		},
		{
			name: "the flag says paused false",
			// The file's PRESENCE is the switch: honouring the field would let a partial write silently resume collection.
			body: `{"paused":false,"at":"2026-07-30T00:00:00Z"}`,
		},
		{
			name: "empty file",
			body: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, platform.File), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			got := platform.Read(dir)
			if !got.Paused {
				t.Fatal("a flag file that exists but does not parse read as NOT paused: " +
					"that direction resumes collection on a machine whose owner stopped it")
			}
			if tc.body != "" && got.Reason == "" && tc.name != "the flag says paused false" {
				t.Error("no reason given, so `status` could not explain the state")
			}
		})
	}
}

func TestSetCreatesTheStateDirectory(t *testing.T) {
	// Pausing before `init` has to work: switching the tool off must not require initialising it first.
	dir := filepath.Join(t.TempDir(), "not", "created", "yet")
	if err := platform.Set(dir, "", time.Now(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !platform.Read(dir).Paused {
		t.Fatal("not paused")
	}
}

func TestFlagSurvivesReadingItRepeatedly(t *testing.T) {
	dir := t.TempDir()
	if err := platform.Set(dir, "why", time.Now(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	// Read must not consume or rewrite the flag: the daemon reads it every tick.
	for i := 0; i < 5; i++ {
		if !platform.Read(dir).Paused {
			t.Fatalf("read %d cleared the flag", i)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, platform.File)); err != nil {
		t.Fatalf("the flag file is gone: %v", err)
	}
}

// The structural form of "no config can undo it": the guarantee is in what this package cannot see.
func TestPauseCannotSeeConfigOrBackend(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps",
		"github.com/QuesmaOrg/quesma-shipper/internal/platform").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	forbidden := []string{
		"github.com/QuesmaOrg/quesma-shipper/internal/config",
		"github.com/QuesmaOrg/quesma-shipper/internal/controlplane",
		"net/http",
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, f := range forbidden {
			if dep == f {
				t.Errorf("internal/pause links %s: pause state must not be reachable "+
					"from configuration or from a control plane", f)
			}
		}
	}
}
