package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
)

// A layer file that exists but cannot be read must refuse the whole resolution, never be silently dropped.
func TestLoadLayersRefusesAnUnreadableLayer(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "config.yaml")
	body := "config_version: 1\n# " + strings.Repeat("x", 1<<20) + "\n"
	if err := os.WriteFile(user, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.LoadLayers(config.Paths{User: user})
	if err == nil {
		t.Fatal("an oversized config layer was silently dropped")
	}
	if !strings.Contains(err.Error(), user) {
		t.Errorf("the refusal does not name the offending file: %v", err)
	}
}

// The strict local parse has to keep loading the `send:` block older builds wrote into config.yaml.
func TestLoadLayersAcceptsTheSendBlockOlderBuildsWrote(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "config.yaml")
	body := "config_version: 1\nsend:\n  sink: file\n  path: /var/tmp/trajectory-archive\n"
	if err := os.WriteFile(user, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	layers, err := config.LoadLayers(config.Paths{User: user})
	if err != nil {
		t.Fatalf("a config.yaml an older build wrote no longer loads: %v", err)
	}
	if len(layers) != 1 {
		t.Fatalf("got %d layers, want the user layer", len(layers))
	}
}

// A missing file stays the clone-and-run case: no layers, no error.
func TestLoadLayersSkipsAMissingFile(t *testing.T) {
	layers, err := config.LoadLayers(config.Paths{
		User: filepath.Join(t.TempDir(), "absent.yaml"),
	})
	if err != nil || len(layers) != 0 {
		t.Fatalf("a missing file returned layers=%d err=%v", len(layers), err)
	}
}
