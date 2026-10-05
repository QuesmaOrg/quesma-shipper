package platform

import "path/filepath"

// DefaultStateDir is shared by configuration and startup diagnostics when configuration cannot load.
func DefaultStateDir(home string, lookup func(string) (string, bool)) string {
	stateHome := filepath.Join(home, ".local", "state")
	if v, ok := lookup("XDG_STATE_HOME"); ok && v != "" {
		stateHome = v
	}
	return filepath.Join(stateHome, "trajectory-shipper")
}
