package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

func enrolledConfigServer(t *testing.T, serve http.HandlerFunc) config.Paths {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	srv := httptest.NewServer(serve)
	t.Cleanup(srv.Close)
	paths := config.DefaultPaths(home, os.LookupEnv)
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollment := controlplane.Enrollment{
		InstallID: "018e1f80-1234-7000-8000-000000000001", Organization: "acme",
		Endpoint: srv.URL, DeviceKey: base64.StdEncoding.EncodeToString(key),
	}
	if err := enrollment.Save(paths.StateDir); err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestRejectedRemoteKeepsWorkingCacheAndRecovers(t *testing.T) {
	const good = "org: acme\nmax_files_per_run: 8\ntelemetry_endpoint: /v1/telemetry\n"
	for name, bad := range map[string]string{
		"duration":  "drain_deadline: forever\n",
		"rule pack": "scrub:\n  rule_packs: [unknown-pack]\n",
		"source":    "sources:\n  - id: unknown-source\n",
		"enricher":  "sources:\n  - id: cursor-transcripts\n    enrichers:\n      unknown-enricher: true\n",
		"recipient": "encryption:\n  additional_recipients: [not-an-age-key]\n",
		"yaml":      "sources: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			served := good
			calls := 0
			paths := enrolledConfigServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				_ = json.NewEncoder(w).Encode(controlplane.ConfigResponse{
					Config: []byte(served), ExpiresAt: time.Now().Add(time.Hour),
				})
			})
			eff, _, remote, err := ResolveOnline(context.Background())
			if err != nil || remote.Origin != controlplane.OriginFetched || eff.MaxFilesPerRun != 8 {
				t.Fatalf("initial config: effective=%+v remote=%+v err=%v", eff, remote, err)
			}
			cachePath := filepath.Join(paths.StateDir, controlplane.CacheFile)
			before, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			served = "org: changed\nmax_files_per_run: 1\n" + bad
			eff, _, remote, err = ResolveOnline(context.Background())
			if err != nil || remote.Origin != controlplane.OriginCached || !errors.Is(remote.Err, controlplane.ErrConfigRejected) {
				t.Fatalf("rejected candidate: remote=%+v err=%v", remote, err)
			}
			if eff.MaxFilesPerRun != 8 || eff.OrganizationID != "acme" || eff.TelemetryEndpoint != "/v1/telemetry" {
				t.Fatalf("candidate partially applied: %+v", eff)
			}
			after, err := os.ReadFile(cachePath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected candidate changed working cache: %v", err)
			}
			eff, _, err = ResolveEffective()
			if err != nil || eff.MaxFilesPerRun != 8 || calls != 2 {
				t.Fatalf("offline continuation: effective=%+v calls=%d err=%v", eff, calls, err)
			}
			served = "org: acme\nmax_files_per_run: 12\n"
			eff, _, remote, err = ResolveOnline(context.Background())
			if err != nil || remote.Err != nil || remote.Origin != controlplane.OriginFetched || eff.MaxFilesPerRun != 12 {
				t.Fatalf("corrected candidate: effective=%+v remote=%+v err=%v", eff, remote, err)
			}
			eff, _, err = ResolveEffective()
			if err != nil || eff.MaxFilesPerRun != 12 {
				t.Fatalf("corrected candidate was not cached: effective=%+v err=%v", eff, err)
			}
		})
	}
}

func TestUnusableRemoteCacheFallsBackToLocalConfig(t *testing.T) {
	paths := enrolledConfigServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(controlplane.ConfigResponse{Config: []byte("drain_deadline: forever\n")})
	})
	eff, _, remote, err := ResolveOnline(context.Background())
	if err != nil || remote.Origin != controlplane.OriginNone || eff.DrainDeadline != 5*time.Minute {
		t.Fatalf("first invalid config: effective=%+v remote=%+v err=%v", eff, remote, err)
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir, controlplane.CacheFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid first config was cached: %v", err)
	}
	// Older clients could have persisted a document that parses but cannot resolve.
	if err := controlplane.SaveCache(paths.StateDir, controlplane.Cached{Config: []byte("drain_deadline: forever\n")}); err != nil {
		t.Fatal(err)
	}
	eff, _, err = ResolveEffective()
	if err != nil || eff.DrainDeadline != 5*time.Minute {
		t.Fatalf("poisoned legacy cache: effective=%+v err=%v", eff, err)
	}
}

func TestRefreshPreservesCredentialRefusal(t *testing.T) {
	paths := enrolledConfigServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	if err := controlplane.SaveCache(paths.StateDir, controlplane.Cached{Config: []byte("org: acme\n")}); err != nil {
		t.Fatal(err)
	}
	_, _, remote, err := ResolveOnline(context.Background())
	if err != nil || !errors.Is(remote.Err, formats.ErrCredentialsRefused) || errors.Is(remote.Err, controlplane.ErrConfigRejected) {
		t.Fatalf("revocation lost or mistaken for invalid config: remote=%+v err=%v", remote, err)
	}
}
