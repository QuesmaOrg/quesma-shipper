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
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

func TestInteractiveEnrollmentCorrectsServerWithoutRetryingOldHost(t *testing.T) {
	for _, status := range []int{404, 409, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				http.Error(w, "wrong server", status)
			}))
			defer old.Close()
			var received EnrollRequest
			replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Error(err)
				}
				_ = json.NewEncoder(w).Encode(EnrollResponse{Organization: "acme"})
			}))
			defer replacement.Close()
			dir := t.TempDir()
			req := EnrollRequest{InstallID: "one", AgeRecipient: "recipient"}
			if _, _, _, err := EnrollInteractive(context.Background(), dir, old.URL, "old-token", req); err == nil || !strings.Contains(err.Error(), filepath.Join(dir, interactiveAttemptFile)) {
				t.Fatalf("failed attempt did not name its file: %v", err)
			}
			old.Close()
			resp, endpoint, _, err := EnrollInteractive(context.Background(), dir, replacement.URL, "new-token", req)
			if err != nil || resp.Organization != "acme" || endpoint != replacement.URL || calls != 1 || received.Invite != "new-token" {
				t.Fatalf("corrected server: response=%v endpoint=%s calls=%d error=%v", resp, endpoint, calls, err)
			}
		})
	}
}

func TestInteractiveEnrollmentPreservesLostResponseWithReplacementToken(t *testing.T) {
	var requests [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
		}
		requests = append(requests, raw)
		if len(requests) == 1 {
			http.Error(w, "lost response", 503)
			return
		}
		_ = json.NewEncoder(w).Encode(EnrollResponse{Organization: "acme"})
	}))
	defer srv.Close()
	dir := t.TempDir()
	req := EnrollRequest{InstallID: "one", AgeRecipient: "recipient"}
	_, _, _, _ = EnrollInteractive(context.Background(), dir, srv.URL, "old-token", req)
	if _, _, _, err := EnrollInteractive(context.Background(), dir, srv.URL, "replacement-token", req); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || !bytes.Equal(requests[0], requests[1]) {
		t.Fatal("replacement token discarded a possibly consumed invite")
	}
}

func TestEnrollmentAttemptValidatesSavedSecurityState(t *testing.T) {
	for _, mode := range []string{"interactive", "managed"} {
		for _, mutation := range []string{"endpoint", "credential", "both credentials", "identity", "signing key"} {
			t.Run(mode+"/"+mutation, func(t *testing.T) {
				dir := t.TempDir()
				req := EnrollRequest{InstallID: "one", AgeRecipient: "recipient"}
				if mode == "managed" {
					req.Grant = "grant"
				} else {
					req.Invite = "invite"
				}
				path := filepath.Join(dir, "attempt.json")
				if _, _, _, err := enrollmentAttempt(path, "https://example.com", req); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var saved managedAttempt
				if err := json.Unmarshal(raw, &saved); err != nil {
					t.Fatal(err)
				}
				var previous EnrollRequest
				if err := json.Unmarshal(saved.Request, &previous); err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "endpoint":
					saved.Endpoint = ""
				case "credential":
					previous.Grant, previous.Invite = "", ""
				case "both credentials":
					previous.Grant, previous.Invite = "grant", "invite"
				case "identity":
					previous.InstallID = "another"
				case "signing key":
					previous.DevicePublicKey = "wrong"
				}
				saved.Request, _ = json.Marshal(previous)
				if err := platform.WriteJSON(path, saved, 0600); err != nil {
					t.Fatal(err)
				}
				if _, _, _, err := enrollmentAttempt(path, "https://example.com", req); err == nil || !strings.Contains(err.Error(), path) {
					t.Fatalf("accepted corrupt %s: %v", mutation, err)
				}
			})
		}
	}
}

func TestInteractiveEnrollmentConflictRemainsClassifiable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(409) }))
	defer srv.Close()
	_, _, _, err := EnrollInteractive(context.Background(), t.TempDir(), srv.URL, "token", EnrollRequest{InstallID: "one", AgeRecipient: "recipient"})
	if !errors.Is(err, ErrEnrollmentConflict) {
		t.Fatalf("conflict = %v", err)
	}
}
