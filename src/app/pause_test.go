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

// Pausing must not depend on configuration parsing: a user must be able to resume even when
// the configuration needs repair.
func TestPauseWorksUnderABrokenConfig(t *testing.T) {
	home := t.TempDir()
	configHome := filepath.Join(home, ".config")
	stateHome := filepath.Join(home, ".state")
	stateDir := filepath.Join(stateHome, "trajectory-shipper")

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", stateHome)

	// A config file that does not parse.
	if err := os.MkdirAll(filepath.Join(configHome, "trajectory-shipper"), 0o700); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(configHome, "trajectory-shipper", "config.yaml")
	if err := os.WriteFile(broken, []byte("config_version: 1\n bad: indent: here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	configHome := filepath.Join(home, ".config")
	moved := filepath.Join(home, "elsewhere")

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))

	if err := os.MkdirAll(filepath.Join(configHome, "trajectory-shipper"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "config_version: 1\nstate_dir: " + moved + "\n"
	if err := os.WriteFile(filepath.Join(configHome, "trajectory-shipper", "config.yaml"),
		[]byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

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
	configHome := filepath.Join(home, ".config")
	moved := filepath.Join(home, "moved-state")

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))

	if err := os.MkdirAll(filepath.Join(configHome, "trajectory-shipper"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "config_version: 1\nstate_dir: " + moved + "\n"
	if err := os.WriteFile(filepath.Join(configHome, "trajectory-shipper", "config.yaml"),
		[]byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

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
	if paths.StateDir != eff.StateDir {
		t.Errorf("the CLI and the engine would use different state directories: %q vs %q",
			paths.StateDir, eff.StateDir)
	}
}
