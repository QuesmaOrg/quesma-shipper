package controlplane_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

func TestOnlyExplicitAuthenticatedRevocationLatches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		revoked bool
	}{
		{"revoked", 403, "this install is revoked\n", true},
		{"unknown forbidden", 403, "permission denied", false},
		{"unauthorized", 401, "this install is revoked", false},
		{"unavailable", 503, "this install is revoked", false},
		{"rate limited", 429, "this install is revoked", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, tc.body, tc.status) }))
			defer server.Close()
			_, key, _ := controlplane.NewDeviceKey()
			dir := t.TempDir()
			options := controlplane.Options{Endpoint: server.URL + "/", InstallID: "install", Organization: "acme", DeviceKey: key, StateDir: dir}
			client, _ := controlplane.New(options)
			_, err := client.FetchConfig(context.Background(), controlplane.ConfigRequest{})
			var revoked *controlplane.Revocation
			if errors.As(err, &revoked) != tc.revoked {
				t.Fatalf("revoked = %v, error %v", revoked, err)
			}
			if tc.revoked && !errors.Is(err, formats.ErrCredentialsRefused) {
				t.Fatal("revocation must stop the engine")
			}
			client, _ = controlplane.New(options)
			_, _ = client.FetchConfig(context.Background(), controlplane.ConfigRequest{})
			want := int32(2)
			if tc.revoked {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("requests = %d, want %d", calls.Load(), want)
			}
			if tc.revoked {
				r, err := controlplane.LoadRevocation(dir, &controlplane.Enrollment{InstallID: "install", Organization: "acme", Endpoint: server.URL + "/"})
				if err != nil || r == nil || !r.DetectedAt.Equal(revoked.DetectedAt) {
					t.Fatalf("marker = %+v, %v", r, err)
				}
				r, err = controlplane.LoadRevocation(dir, &controlplane.Enrollment{InstallID: "replacement", Organization: "acme", Endpoint: server.URL})
				if err != nil || r != nil {
					t.Fatalf("replacement inherited revocation: %v, %v", r, err)
				}
			}
		})
	}
}

func TestRevocationFromUploadStopsOtherEndpoints(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "this install is revoked", 403)
	}))
	defer server.Close()
	_, key, _ := controlplane.NewDeviceKey()
	client, _ := controlplane.New(controlplane.Options{Endpoint: server.URL, InstallID: "install", Organization: "acme", DeviceKey: key, StateDir: t.TempDir()})
	_, err := client.AuthorizeUploads(context.Background(), controlplane.AuthorizeRequest{WriterID: "writer", IssuedAt: time.Now(), Objects: []controlplane.UploadObject{{ObjectID: "object"}}})
	var revoked *controlplane.Revocation
	if !errors.As(err, &revoked) {
		t.Fatal(err)
	}
	_, _ = client.FetchConfig(context.Background(), controlplane.ConfigRequest{})
	if calls.Load() != 1 {
		t.Fatal("revoked client retried")
	}
}

func TestRevocationSurvivesPersistenceFailureInMemory(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := os.Mkdir(filepath.Join(dir, controlplane.RevocationFile), 0700); err != nil {
			t.Error(err)
		}
		http.Error(w, "this install is revoked", 403)
	}))
	defer server.Close()
	_, key, _ := controlplane.NewDeviceKey()
	client, _ := controlplane.New(controlplane.Options{Endpoint: server.URL, InstallID: "install", Organization: "acme", DeviceKey: key, StateDir: dir})
	_, err := client.FetchConfig(context.Background(), controlplane.ConfigRequest{})
	var revoked *controlplane.Revocation
	if !errors.As(err, &revoked) || !errors.As(client.Revocation(), &revoked) {
		t.Fatalf("lost revocation after disk failure: %v", err)
	}
	_, _ = client.FetchConfig(context.Background(), controlplane.ConfigRequest{})
	if calls.Load() != 1 {
		t.Fatal("persist failure caused retry")
	}
}

func TestDiskRevocationRemainsLatchedForExistingClient(t *testing.T) {
	dir := t.TempDir()
	_, key, _ := controlplane.NewDeviceKey()
	options := controlplane.Options{Endpoint: "https://fleet.example.com", InstallID: "old", Organization: "acme", DeviceKey: key, StateDir: dir}
	client, err := controlplane.New(options)
	if err != nil {
		t.Fatal(err)
	}
	marker := &controlplane.Revocation{InstallID: options.InstallID, Organization: options.Organization, Endpoint: options.Endpoint, DetectedAt: time.Now()}
	if err := marker.Save(dir); err != nil {
		t.Fatal(err)
	}
	var revoked *controlplane.Revocation
	if !errors.As(client.Revocation(), &revoked) {
		t.Fatal("persisted revocation was not loaded")
	}
	if err := os.Remove(filepath.Join(dir, controlplane.RevocationFile)); err != nil {
		t.Fatal(err)
	}
	if !errors.As(client.Revocation(), &revoked) {
		t.Fatal("existing client lost its loaded revocation")
	}
	options.InstallID = "replacement"
	replacement, err := controlplane.New(options)
	if err != nil || replacement.Revocation() != nil {
		t.Fatalf("replacement client inherited revocation: %v", err)
	}
}
