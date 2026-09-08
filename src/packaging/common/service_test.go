package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testSpec() Spec {
	return Spec{Executable: "/usr/local/bin/quesma-shipper", Args: []string{"run"},
		Home: "/Users/jane", StateDir: "/Users/jane/.local/state/trajectory-shipper",
		LogDir: "/Users/jane/.local/state/trajectory-shipper/logs"}
}

func TestRunMarkerRoundTrips(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 7, 30, 10, 30, 0, 0, time.UTC)
	if err := RecordRun(dir, at); err != nil {
		t.Fatal(err)
	}
	if got := LastRun(dir); !got.Equal(at) {
		t.Errorf("last run = %s, want %s", got, at)
	}
}

func TestACorruptRunMarkerReadsAsNever(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RunMarker), []byte("yesterday\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !LastRun(dir).IsZero() {
		t.Error("a corrupt marker was parsed as a real timestamp")
	}
}

func TestInstallSpecRequiresAnAbsoluteExecutable(t *testing.T) {
	if err := ValidateInstall(Spec{Executable: "shipper"}); err == nil {
		t.Fatal("a relative executable path was accepted")
	}
	if err := ValidateInstall(Spec{}); err == nil {
		t.Fatal("an empty spec was accepted")
	}
}

func TestCronHintUsesOneShotRunAndConfiguredTick(t *testing.T) {
	spec := testSpec()
	spec.Tick = 5 * time.Minute
	got := CronHint(spec)
	if !strings.HasPrefix(got, "*/5 * * * * ") || !strings.Contains(got, " run --once") {
		t.Errorf("unexpected cron hint: %q", got)
	}
}

func TestCronRoundsUpUnsupportedIntervals(t *testing.T) {
	cases := map[time.Duration]string{0: "*/15 * * * *", 30 * time.Second: "* * * * *",
		90 * time.Second: "*/2 * * * *", 25 * time.Hour: "0 0 * * *"}
	for tick, want := range cases {
		if got := cronExpr(tick); got != want {
			t.Errorf("cronExpr(%v) = %q, want %q", tick, got, want)
		}
	}
}
