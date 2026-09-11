//go:build linux

package linux

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func testSpec() Spec {
	return Spec{Executable: "/usr/local/bin/quesma-shipper", Args: []string{"run"},
		Home: "/home/jane", StateDir: "/home/jane/.local/state/trajectory-shipper",
		LogDir: "/home/jane/.local/state/trajectory-shipper/logs"}
}

func TestRestartHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RestartService(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("restart with a canceled context = %v, want context canceled", err)
	}
}

func TestUnitCarriesRestartEnvironmentAndLoginStart(t *testing.T) {
	got := renderUnit(testSpec())
	for _, want := range []string{"ExecStart=/usr/local/bin/quesma-shipper run", "Restart=always",
		"WantedBy=default.target", "Environment=HOME=/home/jane"} {
		if !strings.Contains(got, want) {
			t.Errorf("unit is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "User=") || strings.Contains(got, "[Timer]") {
		t.Errorf("user service contains system-level or timer configuration:\n%s", got)
	}
}
