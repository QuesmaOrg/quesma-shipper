//go:build darwin

package macos

import (
	"context"
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

func testSpec() Spec {
	return Spec{Executable: "/usr/local/bin/quesma-shipper", Args: []string{"run"},
		Home: "/Users/jane", StateDir: "/Users/jane/.local/state/trajectory-shipper",
		LogDir: "/Users/jane/.local/state/trajectory-shipper/logs"}
}

// A context that is already done makes exec.Cmd.Start fail before it spawns anything, so these
// never reach the developer's real supervisor.
func TestRestartHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RestartService(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("restart with a canceled context = %v, want context canceled", err)
	}
}

func TestServiceStateHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := ServiceState(ctx)
	if got.Loaded {
		t.Fatalf("state with a canceled context = %+v, want not loaded", got)
	}
}

func TestPlistIsWellFormedAndKeepsTheAgentAlive(t *testing.T) {
	got := renderPlist(testSpec())
	var v any
	if err := xml.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("invalid plist XML: %v", err)
	}
	for _, want := range []string{"<key>RunAtLoad</key>\n\t<true/>", "<key>KeepAlive</key>\n\t<true/>",
		"<string>" + bundleIdentifier + "</string>", "<string>/usr/local/bin/quesma-shipper</string>",
		"<key>HOME</key>", "XDG_STATE_HOME", "StandardOutPath", "StandardErrorPath",
		"<key>AssociatedBundleIdentifiers</key>\n\t<array>\n\t\t<string>" + bundleIdentifier + "</string>"} {
		if !strings.Contains(got, want) {
			t.Errorf("plist is missing %q:\n%s", want, got)
		}
	}
}

func TestPlistEscapesPaths(t *testing.T) {
	spec := testSpec()
	spec.Home = "/Users/jane & co"
	spec.Executable = "/opt/<weird>/quesma-shipper"
	got := renderPlist(spec)
	var v any
	if err := xml.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("an unescaped path broke the plist: %v\n%s", err, got)
	}
}
