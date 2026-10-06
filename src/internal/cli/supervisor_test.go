package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/app"
)

func TestSupervisorLogQueryMatchesRunWithCustomState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	configDir := filepath.Join(home, "config", "trajectory-shipper")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(home, "custom state")
	encoded, _ := json.Marshal(custom)
	for _, tc := range []struct{ name, config, state string }{
		{"default", "", filepath.Join(home, "state", "trajectory-shipper")},
		{"custom", "state_dir: " + string(encoded) + "\n", custom},
		{"invalid config", "[not: valid", filepath.Join(home, "state", "trajectory-shipper")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			cmd := Root(app.Build{}, &out, &out)
			cmd.SetArgs([]string{"supervisor-log-dir"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if want := filepath.Join(tc.state, "logs"); got != want {
				t.Fatalf("log directory %q, want %q", got, want)
			}
			_, paths, _ := app.ResolveEffective()
			state, err := runLogStateDir(paths)
			if err != nil || filepath.Join(state, "logs") != got {
				t.Fatalf("run and supervisor disagree: %q, %v", state, err)
			}
		})
	}
}
