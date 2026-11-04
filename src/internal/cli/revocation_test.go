package cli

import (
	"bytes"
	"context"
	"github.com/QuesmaOrg/quesma-shipper/app"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
)

func TestRevokedSchedulerWaitsForReplacementEnrollment(t *testing.T) {
	dir := t.TempDir()
	enr := controlplane.Enrollment{InstallID: "original", Organization: "acme", Endpoint: "https://example.com"}
	if err := enr.Save(dir); err != nil {
		t.Fatal(err)
	}
	revoked := &controlplane.Revocation{InstallID: enr.InstallID, Organization: enr.Organization, Endpoint: enr.Endpoint, DetectedAt: time.Now()}
	if err := revoked.Save(dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { waitForReenrollment(ctx, dir, revoked, time.Millisecond); close(done) }()
	stillWaiting := func(stage string) {
		t.Helper()
		select {
		case <-done:
			t.Fatalf("resumed %s", stage)
		case <-time.After(20 * time.Millisecond):
		}
	}
	stillWaiting("without replacement")
	if err := os.WriteFile(filepath.Join(dir, controlplane.EnrollmentFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	stillWaiting("on an unreadable enrollment")
	enr.InstallID = "replacement"
	if err := enr.Save(dir); err != nil {
		t.Fatal(err)
	}
	<-done
	if ctx.Err() != nil {
		t.Fatal("replacement did not resume scheduler")
	}
}

func TestRevokedHeaderOverridesRunningAndPausedStates(t *testing.T) {
	for _, paused := range []bool{false, true} {
		var out bytes.Buffer
		printHeader(&out, palette{}, app.Status{Organization: "acme", ServiceOK: true, Paused: paused, Revocation: &controlplane.Revocation{}}, time.Now())
		if !strings.Contains(out.String(), "revoked") || !strings.Contains(out.String(), "nothing is being collected") || strings.Contains(out.String(), "Sending to") || strings.Contains(out.String(), "last sent") {
			t.Fatalf("contradictory header: %s", out.String())
		}
	}
}

func TestLoginWithoutTokenDoesNotClaimRevokedEnrollmentIsActive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv(app.AuthEnv, "")
	_, paths, err := app.ResolveEffective()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	enr := controlplane.Enrollment{InstallID: "original", Organization: "acme", Endpoint: "https://example.com"}
	if err := enr.Save(paths.StateDir); err != nil {
		t.Fatal(err)
	}
	revoked := &controlplane.Revocation{InstallID: enr.InstallID, Organization: enr.Organization, Endpoint: enr.Endpoint, DetectedAt: time.Now()}
	if err := revoked.Save(paths.StateDir); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := loginCmd()
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, []string{}); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("login without token: %v", err)
	}
	if strings.Contains(out.String(), "Already logged in") {
		t.Fatal(out.String())
	}
}
