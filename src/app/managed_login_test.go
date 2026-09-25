package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
)

func TestManagedEnrollmentRetriesTheSameIdentityAfterALostResponse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	var requests []controlplane.EnrollRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req controlplane.EnrollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests = append(requests, req)
		if len(requests) == 1 {
			http.Error(w, "response lost after the server saved enrollment", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(controlplane.EnrollResponse{Organization: "example"})
	}))
	defer srv.Close()
	loginManaged := func(grant string) (LoginResult, error) {
		return loginWithRunCheck(context.Background(), srv.URL, grant, true, func() error { return nil })
	}

	if _, err := loginManaged("managed-grant"); err == nil {
		t.Fatal("first enrollment should fail locally")
	}
	if _, ok := LoggedIn(); ok {
		t.Fatal("a lost response was persisted as successful enrollment")
	}
	if _, err := loginManaged("managed-grant"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0] != requests[1] {
		t.Fatalf("managed retry changed its enrollment identity: %d requests", len(requests))
	}
	if _, err := os.Stat(filepath.Join(home, "state", "trajectory-shipper", "managed-enrollment-key.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending enrollment key survived successful enrollment: %v", err)
	}
	if requests[1].Invite != "" || requests[1].Grant != "managed-grant" {
		t.Fatal("managed enrollment did not use the grant request")
	}
	if _, err := loginManaged("replacement-grant"); err != ErrAlreadyLoggedIn {
		t.Fatalf("profile rotation replaced enrollment: %v", err)
	}
}

func TestManagedEnrollmentErrorKeepsServerBodyOutOfLogs(t *testing.T) {
	err := managedEnrollmentError(&controlplane.HTTPStatusError{Status: 500, Body: "grant=private-secret"})
	if !strings.Contains(err.Error(), "HTTP 500") || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("unsafe managed enrollment diagnostic: %v", err)
	}
}
