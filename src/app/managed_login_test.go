package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

	if _, err := login(context.Background(), srv.URL, "managed-grant", true); err == nil {
		t.Fatal("first enrollment should fail locally")
	}
	if _, ok := LoggedIn(); ok {
		t.Fatal("a lost response was persisted as successful enrollment")
	}
	if _, err := login(context.Background(), srv.URL, "managed-grant", true); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0] != requests[1] {
		t.Fatalf("managed retry changed its enrollment identity: %d requests", len(requests))
	}
	if requests[1].Invite != "" || requests[1].Grant != "managed-grant" {
		t.Fatal("managed enrollment did not use the grant request")
	}
	if _, err := login(context.Background(), srv.URL, "replacement-grant", true); err != ErrAlreadyLoggedIn {
		t.Fatalf("profile rotation replaced enrollment: %v", err)
	}
}
