// Self-update, the parts that answer without a network: the dev-build refusal and the config
// surface. The harness Build carries no release stamp, which is exactly what `update` must refuse:
// every release orders above a dev version, so a bare `update` would replace a developer's build.
package e2e

import (
	"strings"
	"testing"
)

func TestUpdateOnADevBuildRefusesAndNamesTheOverride(t *testing.T) {
	stageBareWorld(t)

	out, err := runExpectingFailure(t, "update")
	if err == nil {
		t.Fatalf("update on a dev build succeeded:\n%s", out)
	}
	for _, want := range []string{"dev build", "make build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestConfigShowReportsTheUpdateSwitch(t *testing.T) {
	stageBareWorld(t)

	out := run(t, "config")
	if !strings.Contains(out, "autoupdate.enabled") {
		t.Errorf("config show does not render autoupdate.enabled — the switch an operator would "+
			"verify before trusting a fleet not to self-update:\n%s", out)
	}
}
