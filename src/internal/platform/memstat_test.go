package platform_test

import (
	"os"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// The default must actually reach the runtime: a limit computed, logged and never applied is a missing safeguard.
func TestTheDefaultLimitIsApplied(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")
	previous := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(previous) })

	applied, fromEnv := platform.SetSoftLimit(platform.DefaultSoftLimit)
	if fromEnv {
		t.Fatal("reported GOMEMLIMIT with none set")
	}
	if applied != platform.DefaultSoftLimit {
		t.Errorf("applied %d, want %d", applied, platform.DefaultSoftLimit)
	}
	if got := debug.SetMemoryLimit(-1); got != platform.DefaultSoftLimit {
		t.Errorf("the runtime has %d, want %d — the limit was never set", got, platform.DefaultSoftLimit)
	}
}

// An operator who set GOMEMLIMIT has decided what this process may use; overriding them silently would be worse.
func TestGOMEMLIMITWins(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "700MiB")
	previous := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(previous) })

	const operatorChoice = 700 << 20
	debug.SetMemoryLimit(operatorChoice) // what the runtime does with the variable at startup

	applied, fromEnv := platform.SetSoftLimit(platform.DefaultSoftLimit)
	if !fromEnv {
		t.Error("GOMEMLIMIT was set and the default was applied anyway")
	}
	if applied != operatorChoice {
		t.Errorf("reported %d, want the operator's %d", applied, operatorChoice)
	}
}

// The cap is process-wide, so a test that moves it puts it back, registered before the t.Setenv that follows.
func capFromEnv(t *testing.T, value string) {
	t.Helper()
	t.Cleanup(func() {
		if err := platform.ApplyMaxInFlightBytesFromEnv(); err != nil {
			t.Fatalf("restoring the cap: %v", err)
		}
	})
	t.Setenv(platform.EnvMaxInFlightBytes, value)
}

func TestTheInFlightCapTakesTheEnvironmentOverride(t *testing.T) {
	capFromEnv(t, "4194304")

	if err := platform.ApplyMaxInFlightBytesFromEnv(); err != nil {
		t.Fatalf("applying a valid cap: %v", err)
	}
	if got := platform.MaxInFlightBytes(); got != 4<<20 {
		t.Errorf("cap = %d, want %d", got, 4<<20)
	}
}

// Unset is the production path; blank is what a cleared shell variable leaves behind and must read the same.
func TestTheInFlightCapDefaultsWithoutTheEnvironment(t *testing.T) {
	// Each case leaves the variable in a state that has to read as "no cap was asked for".
	for name, clearIt := range map[string]func(*testing.T){
		"unset": func(*testing.T) { os.Unsetenv(platform.EnvMaxInFlightBytes) },
		"blank": func(t *testing.T) { t.Setenv(platform.EnvMaxInFlightBytes, "   ") },
	} {
		t.Run(name, func(t *testing.T) {
			// Moved off the default first, so the call below reports a cap it restored rather than one it never touched.
			capFromEnv(t, "4194304")
			if err := platform.ApplyMaxInFlightBytesFromEnv(); err != nil {
				t.Fatalf("applying a valid cap: %v", err)
			}

			clearIt(t)
			if err := platform.ApplyMaxInFlightBytesFromEnv(); err != nil {
				t.Fatalf("applying %s: %v", name, err)
			}
			if got := platform.MaxInFlightBytes(); got != platform.DefaultMaxInFlightBytes {
				t.Errorf("cap = %d, want the default %d", got, platform.DefaultMaxInFlightBytes)
			}
		})
	}
}

// A cap that cannot be honoured is refused: falling back would run at 512 MiB while the operator believed otherwise.
func TestAnUnusableInFlightCapIsRefused(t *testing.T) {
	for _, value := range []string{"banana", "0", "-1", "512MiB", "4.5"} {
		t.Run(value, func(t *testing.T) {
			before := platform.MaxInFlightBytes()
			capFromEnv(t, value)

			err := platform.ApplyMaxInFlightBytesFromEnv()
			if err == nil {
				t.Fatalf("%s=%q was accepted", platform.EnvMaxInFlightBytes, value)
			}
			if !strings.Contains(err.Error(), platform.EnvMaxInFlightBytes) || !strings.Contains(err.Error(), value) {
				t.Errorf("diagnostic %q names neither the variable nor the value", err)
			}
			if got := platform.MaxInFlightBytes(); got != before {
				t.Errorf("cap moved to %d on a refused value; it must stay at %d", got, before)
			}
		})
	}
}

func TestAReadingDescribesTheRun(t *testing.T) {
	before := platform.ReadMemStats()
	junk := make([]byte, 32<<20)
	for i := range junk {
		junk[i] = byte(i)
	}
	d := platform.Delta{Before: before, After: platform.ReadMemStats()}
	if d.Growth() <= 0 {
		t.Errorf("allocating 32 MB showed growth of %d", d.Growth())
	}
	if d.String() == "" {
		t.Error("empty summary")
	}
	runtimeKeepAlive(junk)
}

func runtimeKeepAlive(b []byte) { _ = b[0] }
