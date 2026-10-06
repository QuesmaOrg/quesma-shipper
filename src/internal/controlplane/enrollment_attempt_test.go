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
	req.Hostname, req.Grant = "new-host", "new-grant"
	againEndpoint, againBody, againPriv, err := ManagedEnrollmentAttempt(dir, "https://new.example", req)
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
