package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEnrollmentLockRejectsConcurrentLoginAndReleases(t *testing.T) {
	dir := t.TempDir()
	unlock, err := LockEnrollment(dir)
	if err != nil {
		t.Fatal(err)
	}
	if otherUnlock, err := LockEnrollment(dir); err == nil {
		otherUnlock()
		t.Fatal("concurrent enrollment acquired the lock")
	}
	unlock()
	unlock, err = LockEnrollment(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestManagedEnrollmentAttemptPreservesTheExactRequest(t *testing.T) {
	dir := t.TempDir()
	req := EnrollRequest{InstallID: "one", AgeRecipient: "recipient", Hostname: "old-host", Grant: "old-grant", Platform: "darwin/arm64"}
	endpoint, body, priv, err := ManagedEnrollmentAttempt(dir, "https://old.example", req)
	if err != nil {
		t.Fatal(err)
	}
	req.Hostname = "new-host"
	againEndpoint, againBody, againPriv, err := ManagedEnrollmentAttempt(dir, "https://old.example", req)
	if err != nil || endpoint != againEndpoint || !bytes.Equal(body, againBody) || !bytes.Equal(priv, againPriv) {
		t.Fatalf("pending enrollment changed on retry: %v", err)
	}
	var saved EnrollRequest
	if err := json.Unmarshal(againBody, &saved); err != nil || saved.Grant != "old-grant" || saved.Hostname != "old-host" || saved.DevicePublicKey == "" {
		t.Fatalf("pending request was not preserved: %+v, %v", saved, err)
	}
	req.InstallID = "two"
	if _, _, _, err := ManagedEnrollmentAttempt(dir, "https://old.example", req); err == nil {
		t.Fatal("pending request accepted another identity")
	}
	info, err := os.Stat(filepath.Join(dir, managedAttemptFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 && runtime.GOOS != "windows" {
		t.Fatal("pending enrollment request is not private")
	}
	if err := ClearManagedEnrollmentAttempt(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, managedAttemptFile)); !os.IsNotExist(err) {
		t.Fatalf("pending request survived cleanup: %v", err)
	}
}

func TestConflictErrorsDistinguishEnrollmentFromConfiguration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "private-grant", http.StatusConflict)
	}))
	defer srv.Close()
	client, err := New(Options{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want error
	}{
		{"/v1/enroll", ErrEnrollmentConflict},
		{"/v1/config", ErrUnsupportedVersion},
	} {
		err := client.postJSON(context.Background(), tc.path, []byte(`{}`), nil, false)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if errors.Is(err, ErrEnrollmentConflict) && errors.Is(err, ErrUnsupportedVersion) {
			t.Fatal("conflict meanings overlap")
		}
		if tc.path == "/v1/enroll" && err.Error() != ErrEnrollmentConflict.Error() {
			t.Fatal("enrollment conflict leaked server body")
		}
	}
}

func TestManagedEnrollmentRejectsDestinationChangesBeforeSending(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, grant string }{
		{"server", "https://other.example", "fmi2.acme.new.secret"},
		{"organization", "", "fmi2.other.new.secret"},
		{"dotted organization", "", "fmi2.acme.corp.new.secret"},
		{"unroutable grant", "", "invalid-grant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("changed destination sent a request") }))
			defer srv.Close()
			req := EnrollRequest{InstallID: "one", AgeRecipient: "recipient", Grant: "fmi2.acme.old.secret"}
			_, _, _, err := ManagedEnrollmentAttempt(dir, srv.URL, req)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, managedAttemptFile))
			if err != nil {
				t.Fatal(err)
			}
			endpoint := tc.endpoint
			if endpoint == "" {
				endpoint = srv.URL
			}
			req.Grant = tc.grant
			if _, _, _, err := EnrollManaged(context.Background(), dir, endpoint, req); !errors.Is(err, ErrManagedEnrollmentTargetChanged) {
				t.Fatalf("destination change: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(dir, managedAttemptFile))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("destination change replaced pending state: %v", err)
			}
		})
	}
}

func TestManagedEnrollmentDoesNotSendUnsavedRotation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not prevent writes on Windows")
	}
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unsaved rotation was sent") }))
	defer srv.Close()
	req := EnrollRequest{InstallID: "one", AgeRecipient: "recipient", Grant: "fmi2.acme.old.secret"}
	_, original, _, err := ManagedEnrollmentAttempt(dir, srv.URL, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	req.Grant = "fmi2.acme.new.secret"
	if _, _, _, err := EnrollManaged(context.Background(), dir, srv.URL, req); err == nil {
		t.Fatal("failed persistence accepted")
	}
	req.Grant = "fmi2.acme.old.secret"
	_, saved, _, err := ManagedEnrollmentAttempt(dir, srv.URL, req)
	if err != nil || !bytes.Equal(original, saved) {
		t.Fatalf("failed rotation replaced saved request: %v", err)
	}
}

func TestManagedEnrollmentCanRotateAgainAfterResponseLoss(t *testing.T) {
	dir := t.TempDir()
	req := EnrollRequest{InstallID: "one", AgeRecipient: "recipient", Grant: "fmi2.acme.first.secret", Hostname: "original"}
	_, _, originalKey, err := ManagedEnrollmentAttempt(dir, "https://fleet.example", req)
	if err != nil {
		t.Fatal(err)
	}
	for _, grant := range []string{"fmi2.acme.second.secret", "fmi2.acme.third.secret"} {
		req.Grant, req.Hostname = grant, "changed"
		_, body, key, err := ManagedEnrollmentAttempt(dir, "https://fleet.example", req)
		if err != nil {
			t.Fatal(err)
		}
		var saved EnrollRequest
		if err := json.Unmarshal(body, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.Grant != grant || saved.Hostname != "original" || !bytes.Equal(key, originalKey) {
			t.Fatal("further rotation lost grant or original identity")
		}
		_, retry, _, err := ManagedEnrollmentAttempt(dir, "https://fleet.example", req)
		if err != nil || !bytes.Equal(body, retry) {
			t.Fatalf("retry changed bytes: %v", err)
		}
	}
}

func TestManagedEnrollmentRotatesDottedOrganizationAcrossServerSpellings(t *testing.T) {
	dir := t.TempDir()
	req := EnrollRequest{InstallID: "one", AgeRecipient: "recipient", Grant: "fmi2.acme.corp.first.secret"}
	if _, _, _, err := ManagedEnrollmentAttempt(dir, "https://fleet.example/", req); err != nil {
		t.Fatal(err)
	}
	req.Grant = "fmi2.acme.corp.second.secret"
	endpoint, body, _, err := ManagedEnrollmentAttempt(dir, "https://fleet.example", req)
	if err != nil {
		t.Fatal(err)
	}
	var saved EnrollRequest
	if err := json.Unmarshal(body, &saved); err != nil || saved.Grant != req.Grant || endpoint != "https://fleet.example" {
		t.Fatalf("rotation was not saved: %v", err)
	}
}
