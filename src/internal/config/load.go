package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// maxConfigBytes bounds a config file; anything larger is a mistake or an attempt to exhaust memory.
const maxConfigBytes = 1 << 20

// Paths locates the one config file and the state directory. The remote layer is absent on purpose: it needs enrollment.
type Paths struct {
	User     string
	StateDir string
}

// DefaultPaths returns the standard locations, honouring XDG where it applies.
func DefaultPaths(home string, lookup func(string) (string, bool)) Paths {
	configHome := filepath.Join(home, ".config")
	if v, ok := lookup("XDG_CONFIG_HOME"); ok && v != "" {
		configHome = v
	}
	stateHome := filepath.Join(home, ".local", "state")
	if v, ok := lookup("XDG_STATE_HOME"); ok && v != "" {
		stateHome = v
	}
	return Paths{
		User:     filepath.Join(configHome, "trajectory-shipper", "config.yaml"),
		StateDir: filepath.Join(stateHome, "trajectory-shipper"),
	}
}

// LoadLayers reads the user's config file. Missing is not an error (clone-and-run); present but unparseable is.
func LoadLayers(p Paths) ([]LayeredDocument, error) {
	if p.User == "" {
		return nil, nil
	}
	raw, _, err := platform.ReadWhole(p.User, maxConfigBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if errors.Is(err, platform.ErrNotRegular) {
			return nil, fmt.Errorf("config: %s is not a regular file", p.User)
		}
		// Every other failure on a file that exists is refused: skipping would silently drop the whole layer.
		return nil, fmt.Errorf("config: cannot read %s: %w", p.User, err)
	}
	doc, err := ParseDocument(raw)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", p.User, err)
	}
	return []LayeredDocument{{Layer: LayerUser, Doc: doc}}, nil
}

// UserConfigFound reports whether the user's config file exists, for `config show` and the startup log line.
func UserConfigFound(p Paths) (string, bool) {
	if p.User == "" {
		return "", false
	}
	if _, err := os.Stat(p.User); err != nil {
		return "", false
	}
	return p.User, true
}
