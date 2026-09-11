package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

// A restart that outlives the wait is still a successful update: the text has to say so, name
// the window it waited, and hand over the manual restart.
func TestRestartTimeoutKeepsTheSuccessfulUpdateClear(t *testing.T) {
	got := restartTimeoutWarning(5*time.Minute + 30*time.Second)
	for _, want := range []string{"5m30s", "update is installed", "new version"} {
		if !strings.Contains(got, want) {
			t.Errorf("restart timeout warning %q does not contain %q", got, want)
		}
	}
	if cmd := packaging.RestartCommand(); cmd != "" && !strings.Contains(got, cmd) {
		t.Errorf("restart timeout warning %q does not offer %q", got, cmd)
	}
}

// Every branch of the boot-time gate is a promise: dev builds never self-update,
// SHIPPER_NO_SELFUPDATE always wins, and the re-exec guard allows exactly one hop per boot.
func TestTheBootGateKeepsItsPromises(t *testing.T) {
	release := app.Build{Version: "0.0.1-123.abcdef123456", Release: true}
	dev := app.Build{Version: "0.0.0-031a7faa8c16"}

	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}

	for _, tc := range []struct {
		name    string
		build   app.Build
		vars    map[string]string
		hop     string
		wantRun bool
		loud    bool // a skip the operator should see a line about
	}{
		{"a released daemon updates", release, nil, "", true, false},
		{"a dev build never does, silently", dev, nil, "", false, false},
		{"a dev build ignores even a stray re-exec guard", dev,
			map[string]string{app.ReexecGuardEnv: "0.0.1-9.x"}, "", false, false},
		{"the env kill switch wins and says so", release,
			map[string]string{app.NoSelfUpdateEnv: "1"}, "", false, true},
		{"the hop guard stops a second update this boot and says so", release,
			map[string]string{app.ReexecGuardEnv: "0.0.1-124.def456def456"}, "", false, true},
		{"a hop to a version we are not running blocks and says so", release, nil,
			"0.0.1-124.def456def456", false, true},
		{"a hop that landed on this build stops blocking", release, nil,
			release.Version, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, why := selfUpdateGate(tc.build, env(tc.vars), tc.hop)
			if run != tc.wantRun {
				t.Errorf("run = %v, want %v", run, tc.wantRun)
			}
			if (why != "") != tc.loud {
				t.Errorf("why = %q, want loud=%v", why, tc.loud)
			}
		})
	}
}
