package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
)

func TestRevocationStopsCollectionAndDoctorWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	paths := enrolledConfigServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "this install is revoked", 403)
	})
	_, _, remote, err := ResolveOnline(context.Background())
	var revoked *controlplane.Revocation
	if err != nil || !errors.As(remote.Err, &revoked) {
		t.Fatalf("resolve: %v, %v", err, remote.Err)
	}
	notifications := 0
	notifyRevocationWith(paths.StateDir, func(context.Context, string) { notifications++ })
	notifyRevocationWith(paths.StateDir, func(context.Context, string) { notifications++ })
	if notifications != 1 {
		t.Fatalf("notifications = %d", notifications)
	}
	// Revocation wins before an absent identity or source tree is opened.
	for i := 0; i < 2; i++ {
		if _, err := New(Build{}); !errors.As(err, &revoked) {
			t.Fatalf("startup: %v", err)
		}
		runtime := &Runtime{eff: &config.Effective{StateDir: paths.StateDir}}
		runtime.OnLocked = func() { t.Fatal("revoked flush opened the engine") }
		if _, err := runtime.Flush(context.Background(), false); !errors.As(err, &revoked) {
			t.Fatalf("flush: %v", err)
		}
		report := Diagnose(context.Background(), Build{}, false)
		if len(report.Sections) != 1 || report.Sections[0].Rows[0].Brief != "install revoked" {
			t.Fatalf("doctor: %+v", report)
		}
	}
	status, err := CurrentStatus(Build{})
	if err != nil || status.Revocation == nil || status.Machine == "" {
		t.Fatalf("status: %+v, %v", status, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("revoked install made %d requests", calls.Load())
	}
}

func TestReenrollmentRetainsOldStateAndRetriesExactRequest(t *testing.T) {
	var enrollRequests [][]byte
	accept := false
	paths := enrolledConfigServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" {
			http.Error(w, "this install is revoked", 403)
			return
		}
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
			return
		}
		enrollRequests = append(enrollRequests, raw)
		if !accept {
			http.Error(w, "response lost", 503)
			return
		}
		_ = json.NewEncoder(w).Encode(controlplane.EnrollResponse{Organization: "new-org"})
	})
	oldUnit, err := identity.Mint(paths.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	oldEnrollment, _ := controlplane.LoadEnrollment(paths.StateDir)
	oldEnrollment.InstallID = oldUnit.InstallID.String()
	if err := oldEnrollment.Save(paths.StateDir); err != nil {
		t.Fatal(err)
	}
	store, err := engine.Open(paths.StateDir, oldEnrollment.InstallID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitAll(map[engine.Key]engine.Fingerprint{{SourceID: "example", ID: "pending"}: {Attempts: 1, LastError: "pending upload"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	oldState, err := os.ReadFile(filepath.Join(paths.StateDir, engine.FileName))
	if err != nil {
		t.Fatal(err)
	}
	oldKey, _ := os.ReadFile(filepath.Join(paths.StateDir, identity.FileName))
	oldRecord, _ := os.ReadFile(filepath.Join(paths.StateDir, controlplane.EnrollmentFile))
	revoked := &controlplane.Revocation{InstallID: oldEnrollment.InstallID, Organization: oldEnrollment.Organization, Endpoint: oldEnrollment.Endpoint, DetectedAt: time.Now(), Notified: true}
	if err := revoked.Save(paths.StateDir); err != nil {
		t.Fatal(err)
	}
	if err := controlplane.SaveCache(paths.StateDir, controlplane.Cached{Config: []byte("org: old-org\n")}); err != nil {
		t.Fatal(err)
	}
	login := func() (LoginResult, error) {
		return loginWithRunCheck(context.Background(), oldEnrollment.Endpoint, "new-token", false, func() error { return nil })
	}
	if _, err := login(); err == nil {
		t.Fatal("expected response loss")
	}
	for file, want := range map[string][]byte{identity.FileName: oldKey, controlplane.EnrollmentFile: oldRecord, engine.FileName: oldState} {
		got, err := os.ReadFile(filepath.Join(paths.StateDir, file))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("failed login changed %s", file)
		}
	}
	accept = true
	result, err := login()
	if err != nil || result.Organization != "new-org" {
		t.Fatalf("reenroll: %+v, %v", result, err)
	}
	if len(enrollRequests) != 2 || !bytes.Equal(enrollRequests[0], enrollRequests[1]) {
		t.Fatal("retry changed single-use request")
	}
	unit, _ := identity.Load(paths.StateDir)
	if unit.InstallID == oldUnit.InstallID || unit.Recipient().String() == oldUnit.Recipient().String() {
		t.Fatal("replacement reused identity")
	}
	if err := EnrollmentRevocation(paths.StateDir); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string][]byte{identity.FileName: oldKey, controlplane.EnrollmentFile: oldRecord, engine.FileName: oldState} {
		got, err := os.ReadFile(filepath.Join(paths.StateDir, "revoked-installs", oldEnrollment.InstallID, file))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("archive lost %s", file)
		}
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir, controlplane.CacheFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old organization cache survived recovery")
	}
	if _, err := login(); !errors.Is(err, ErrAlreadyLoggedIn) {
		t.Fatalf("active enrollment replaced: %v", err)
	}
	if !strings.Contains(revoked.Error(), "detected") {
		t.Fatal("observation time presented as revocation time")
	}
}

func TestRuntimeKeepsRevocationWhenPersistenceFails(t *testing.T) {
	var paths config.Paths
	var calls atomic.Int32
	paths = enrolledConfigServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := os.Mkdir(filepath.Join(paths.StateDir, controlplane.RevocationFile), 0700); err != nil {
			t.Error(err)
		}
		http.Error(w, "this install is revoked", 403)
	})
	client, err := newControlPlaneClient(paths.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AuthorizeUploads(context.Background(), controlplane.AuthorizeRequest{WriterID: "writer", IssuedAt: time.Now(), Objects: []controlplane.UploadObject{{ObjectID: "object"}}})
	var revoked *controlplane.Revocation
	if !errors.As(err, &revoked) {
		t.Fatal(err)
	}
	runtime := &Runtime{eff: &config.Effective{StateDir: paths.StateDir}, client: client}
	for i := 0; i < 2; i++ {
		if _, err := runtime.Flush(context.Background(), false); !errors.As(err, &revoked) {
			t.Fatalf("runtime lost revocation: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("runtime retried a revoked install")
	}
}

func TestInterruptedReenrollmentKeepsOriginalArchive(t *testing.T) {
	var paths config.Paths
	var requests [][]byte
	paths = enrolledConfigServer(t, func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
			return
		}
		requests = append(requests, raw)
		if len(requests) == 1 {
			if err := os.Mkdir(filepath.Join(paths.StateDir, controlplane.CacheFile), 0700); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(filepath.Join(paths.StateDir, controlplane.CacheFile, "cannot-remove"), []byte("x"), 0600); err != nil {
				t.Error(err)
			}
		}
		_ = json.NewEncoder(w).Encode(controlplane.EnrollResponse{Organization: "acme"})
	})
	old, err := identity.Mint(paths.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	enr, _ := controlplane.LoadEnrollment(paths.StateDir)
	enr.InstallID = old.InstallID.String()
	if err := enr.Save(paths.StateDir); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(filepath.Join(paths.StateDir, identity.FileName))
	revoked := &controlplane.Revocation{InstallID: enr.InstallID, Organization: enr.Organization, Endpoint: enr.Endpoint, DetectedAt: time.Now(), Notified: true}
	if err := revoked.Save(paths.StateDir); err != nil {
		t.Fatal(err)
	}
	login := func() (LoginResult, error) {
		return loginWithRunCheck(context.Background(), enr.Endpoint, "token", false, func() error { return nil })
	}
	if _, err := login(); err == nil {
		t.Fatal("cache removal should fail")
	}
	pending, _ := identity.Load(paths.StateDir)
	if pending.InstallID == old.InstallID {
		t.Fatal("test did not reach identity replacement")
	}
	var got *controlplane.Revocation
	if !errors.As(EnrollmentRevocation(paths.StateDir), &got) {
		t.Fatal("interrupted replacement resumed collection")
	}
	if err := os.RemoveAll(filepath.Join(paths.StateDir, controlplane.CacheFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := login(); err != nil {
		t.Fatal(err)
	}
	replacement, _ := identity.Load(paths.StateDir)
	if replacement.InstallID != pending.InstallID || len(requests) != 2 || !bytes.Equal(requests[0], requests[1]) {
		t.Fatal("interrupted recovery minted another identity or request")
	}
	archived, err := os.ReadFile(filepath.Join(paths.StateDir, "revoked-installs", old.InstallID.String(), identity.FileName))
	if err != nil || !bytes.Equal(archived, original) {
		t.Fatal("retry overwrote original archived key")
	}
}
