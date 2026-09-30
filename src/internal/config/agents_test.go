package config_test

import (
	"fmt"
	"path/filepath"
	"strings"
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

func TestCentrallyConfiguredAgentRoots(t *testing.T) {
	for _, id := range []string{"pi-sessions", "opencode-sessions", "hermes-sessions"} {
		t.Run(id, func(t *testing.T) {
			home := fakeHome(t)
			root := filepath.Join(home, "org", id)
			mustMkdir(t, root)
			remote := config.LayeredDocument{Layer: config.LayerRemote, Doc: servedDoc(t, fmt.Sprintf(`
issued_at: 2026-09-30T10:00:00Z
org: acme
sources:
  - id: %s
    roots: ["~/org/%s"]
`, id, id))}
			eff, err := config.Resolve(baseInput(t, home, remote))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, src := range eff.Sources {
				if src.ID == id {
					found = true
					if src.Root != root {
						t.Fatalf("root=%q, want %q", src.Root, root)
					}
				}
			}
			if !found || eff.Provenance["sources."+id+".roots"].Layer != config.LayerRemote {
				t.Fatal("remote roots were not applied")
			}
			denied := config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t, fmt.Sprintf("sources:\n  - id: %s\n    roots: [\"~/.ssh\"]\n", id))}
			if _, err := config.Resolve(baseInput(t, home, denied)); err == nil || !strings.Contains(err.Error(), "deny") {
				t.Fatalf("denied root accepted: %v", err)
			}
		})
	}
}
