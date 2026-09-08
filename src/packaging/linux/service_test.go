//go:build linux

package linux

import (
	"strings"
	"testing"
)

func testSpec() Spec {
	return Spec{Executable: "/usr/local/bin/quesma-shipper", Args: []string{"run"},
		Home: "/home/jane", StateDir: "/home/jane/.local/state/trajectory-shipper",
		LogDir: "/home/jane/.local/state/trajectory-shipper/logs"}
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
