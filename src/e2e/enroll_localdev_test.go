// First contact: `local-dev` and `login` on a machine that has nothing, so these start from a
// bare world rather than the seeded identity the rest of the tier uses.
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	backend "github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
)

func statePath(w *world) string {
	return filepath.Join(w.State, "trajectory-shipper")
}

func userConfigPath(w *world) string {
	return filepath.Join(w.Config, "trajectory-shipper", "config.yaml")
}

func TestLocalDevMintsIdentityAndWritesNoConfig(t *testing.T) {
	w := stageBareWorld(t)

	out := run(t, "local-dev")

	unit, err := identity.Load(statePath(w))
	if err != nil {
		t.Fatalf("no loadable identity after local-dev: %v", err)
	}
	if !strings.Contains(out, unit.InstallID.String()) {
		t.Errorf("output does not name the identity:\n%s", out)
	}
	if _, err := os.Stat(userConfigPath(w)); !os.IsNotExist(err) {
		t.Errorf("local-dev must not write a config file, stat: %v", err)
	}
	if _, err := os.Stat(filepath.Join(statePath(w), backend.EnrollmentFile)); !os.IsNotExist(err) {
		t.Errorf("local-dev must not write an enrollment record, stat: %v", err)
	}
}

func TestLocalDevRerunKeepsIdentityAndConfig(t *testing.T) {
	w := stageBareWorld(t)

	run(t, "local-dev")
	first, err := identity.Load(statePath(w))
	if err != nil {
		t.Fatal(err)
	}
	own := []byte("config_version: 1\n# mine\n")
	if err := os.MkdirAll(filepath.Dir(userConfigPath(w)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userConfigPath(w), own, 0o600); err != nil {
		t.Fatal(err)
	}

	run(t, "local-dev")

	second, err := identity.Load(statePath(w))
	if err != nil {
		t.Fatal(err)
	}
	if first.InstallID != second.InstallID {
		t.Errorf("re-run changed install_id: %s -> %s", first.InstallID, second.InstallID)
	}
	cfgAfter, err := os.ReadFile(userConfigPath(w))
	if err != nil {
		t.Fatal(err)
	}
	if string(cfgAfter) != string(own) {
		t.Errorf("re-run rewrote the user's own config file")
	}
}

func TestLoginWithoutTokenOrServerFails(t *testing.T) {
	w := stageBareWorld(t)

	out, err := runExpectingFailure(t, "login")
	if err == nil {
		t.Fatalf("login with nothing succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "login <token>") {
		t.Errorf("error does not show how to pass the token: %v", err)
	}

	out, err = runExpectingFailure(t, "login", "tsg1.payload.sig")
	if err == nil {
		t.Fatalf("login with a token and no server succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--server") {
		t.Errorf("error does not name the missing server: %v", err)
	}
	if _, err := os.Stat(filepath.Join(statePath(w), identity.FileName)); !os.IsNotExist(err) {
		t.Errorf("a refused login minted an identity anyway, stat: %v", err)
	}
}

func TestLocalDevRefusesLoggedInInstall(t *testing.T) {
	w := stageBareWorld(t)

	rec := backend.Enrollment{
		InstallID:    testInstallID,
		Organization: "acme",
		Endpoint:     "https://cp.example",
	}
	if err := os.MkdirAll(statePath(w), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rec.Save(statePath(w)); err != nil {
		t.Fatal(err)
	}

	out, err := runExpectingFailure(t, "local-dev")
	if err == nil {
		t.Fatalf("local-dev on a logged-in install succeeded:\n%s", out)
	}
	for _, want := range []string{"already logged in", "acme"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(statePath(w), identity.FileName)); !os.IsNotExist(err) {
		t.Errorf("the refused local-dev minted an identity, stat: %v", err)
	}
}

func TestLocalDevSurfacesUnreadableIdentity(t *testing.T) {
	w := stageBareWorld(t)
	if err := os.MkdirAll(statePath(w), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath(w), identity.FileName), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runExpectingFailure(t, "local-dev")
	if err == nil {
		t.Fatalf("local-dev over a corrupt identity succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error does not surface the read failure: %v", err)
	}
	if strings.Contains(err.Error(), "refusing to mint") {
		t.Errorf("error is Mint's overwrite refusal, not the unit's own failure: %v", err)
	}
}

func TestRunOnceOnVirginMachineWaitsForLogin(t *testing.T) {
	stageBareWorld(t)

	out, err := runUntilCancelled(t, "run", "--once")
	if err != nil {
		t.Fatalf("cancelled wait failed: %v", err)
	}
	if !strings.Contains(out, "waiting for enrollment") || !strings.Contains(out, "shipper login") {
		t.Errorf("the enrollment wait was not logged:\n%s", out)
	}
}

func TestStatusBeforeAndAfterLocalDevSetup(t *testing.T) {
	w := stageBareWorld(t)

	before, err := runExpectingFailure(t, "status")
	if err == nil || !strings.Contains(before, "not logged in") || !strings.Contains(before, "shipper login") {
		t.Errorf("status on a virgin machine does not point at login (err %v):\n%s", err, before)
	}

	run(t, "local-dev")

	after := run(t, "status")
	if !strings.Contains(after, "shipper local") || !strings.Contains(after, "nothing is sent") {
		t.Errorf("status after local-dev does not describe the local install and its destination:\n%s", after)
	}
	if _, err := identity.Load(statePath(w)); err != nil {
		t.Fatal(err)
	}
}

func TestLocalDevPreviewsButRunStillWaitsForEnrollment(t *testing.T) {
	w := stageBareWorld(t)
	run(t, "local-dev")
	stageClaude(t, w, realUsername(t))

	out := run(t, "preview")
	if !strings.Contains(out, claudeSource) {
		t.Fatalf("preview on a standalone install decided nothing:\n%s", out)
	}

	out, err := runUntilCancelled(t, "run", "--once")
	if err != nil {
		t.Fatalf("cancelled wait failed: %v", err)
	}
	if !strings.Contains(out, "waiting for enrollment") || !strings.Contains(out, "shipper login") {
		t.Errorf("the enrollment wait was not logged:\n%s", out)
	}
}
