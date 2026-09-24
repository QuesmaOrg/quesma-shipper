package config_test

import (
	"path/filepath"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
)

func TestAgentStoreRoots(t *testing.T) {
	for _, custom := range []bool{false, true} {
		home := fakeHome(t)
		want := map[string]string{"pi-sessions": filepath.Join(home, ".pi", "agent", "sessions"), "opencode-sessions": filepath.Join(home, ".local", "share", "opencode"), "hermes-sessions": filepath.Join(home, ".hermes")}
		vars := map[string]string{}
		if custom {
			vars["PI_CODING_AGENT_DIR"] = filepath.Join(home, "custom pi")
			vars["XDG_DATA_HOME"] = filepath.Join(home, "custom data")
			vars["HERMES_HOME"] = filepath.Join(home, "custom hermes")
			want["pi-sessions"] = filepath.Join(vars["PI_CODING_AGENT_DIR"], "sessions")
			want["opencode-sessions"] = filepath.Join(vars["XDG_DATA_HOME"], "opencode")
			want["hermes-sessions"] = vars["HERMES_HOME"]
		}
		for _, path := range want {
			mustMkdir(t, path)
		}
		in := baseInput(t, home)
		in.Env = env(home, vars)
		eff, err := config.Resolve(in)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, src := range eff.Sources {
			if path, ok := want[src.ID]; ok {
				seen++
				if src.Root != path || !src.Enabled {
					t.Errorf("%s custom=%v: root=%q enabled=%v, want %q", src.ID, custom, src.Root, src.Enabled, path)
				}
			}
		}
		if seen != 3 {
			t.Fatalf("only %d new sources resolved", seen)
		}
	}
}
