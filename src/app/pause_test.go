package app_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// homeWithConfig points HOME and the XDG dirs into home and writes body as its config.yaml.
func homeWithConfig(t *testing.T, home, body string) {
	t.Helper()
	configDir := filepath.Join(home, ".config", "trajectory-shipper")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Pausing must not depend on configuration parsing: a user must be able to resume even when
// the configuration needs repair.
func TestPauseWorksUnderABrokenConfig(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, ".state", "trajectory-shipper")

	// A config file that does not parse.
	homeWithConfig(t, home, "config_version: 1\n bad: indent: here\n")
	if _, _, err := app.ResolveEffective(); err == nil {
		t.Fatal("the fixture config parses; this test proves nothing")
	}

	warning, err := app.Pause(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("pause failed under broken configuration: %v", err)
	}
	if !strings.Contains(warning, "does not resolve") {
		t.Errorf("the fallback was silent: %q", warning)
	}
	if !platform.Read(stateDir).Paused {
		t.Fatal("pausing under broken configuration did not take effect")
	}

	wasPaused, warning, err := app.Resume()
	if err != nil {
		t.Fatalf("resume failed under broken configuration: %v", err)
	}
	if !wasPaused {
		t.Fatal("resume did not report the existing pause")
	}
	if !strings.Contains(warning, "does not resolve") {
		t.Errorf("the fallback was silent: %q", warning)
	}
	if platform.Read(stateDir).Paused {
		t.Fatal("resuming under broken configuration did not take effect")
	}
}

// With valid configuration, pause and resume use its state directory rather than the default.
func TestPauseHonoursAResolvedStateDir(t *testing.T) {
	home := t.TempDir()
	moved := filepath.Join(home, "elsewhere")

	homeWithConfig(t, home, "config_version: 1\nstate_dir: "+moved+"\n")

	warning, err := app.Pause(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Errorf("warned on a configuration that resolves cleanly: %q", warning)
	}
	if !platform.Read(moved).Paused {
		t.Fatal("pause was not written to the configured state directory")
	}

	wasPaused, warning, err := app.Resume()
	if err != nil {
		t.Fatal(err)
	}
	if !wasPaused {
		t.Fatal("resume did not report the existing pause")
	}
	if warning != "" {
		t.Errorf("warned on a configuration that resolves cleanly: %q", warning)
	}
	if platform.Read(moved).Paused {
		t.Fatal("resume did not clear the configured pause state")
	}
}

// The identity unit and the fingerprint document live in ONE state directory: they persist
// together or not at all, which is what makes a rebuilt ephemeral host the SAME install.
func TestOneResolvedStateDirectoryForEveryArtifact(t *testing.T) {
	home := t.TempDir()
	moved := filepath.Join(home, "moved-state")

	homeWithConfig(t, home, "config_version: 1\nstate_dir: "+moved+"\n")

	eff, paths, err := app.ResolveEffective()
	if err != nil {
		t.Fatal(err)
	}
	// The two values have to agree: half the CLI reads one and the engine reads the other.
	if paths.StateDir != moved {
		t.Errorf("paths.StateDir = %q, want %q", paths.StateDir, moved)
	}
	if eff.StateDir != moved {
		t.Errorf("eff.StateDir = %q, want %q", eff.StateDir, moved)
	}
}
