package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
)

func TestManagedEnrollmentRetriesTheSameIdentityAfterALostResponse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	var requests [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req controlplane.EnrollRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests = append(requests, raw)
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
	if _, err := loginManaged("replacement-grant"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || !bytes.Equal(requests[0], requests[1]) {
		t.Fatalf("managed retry changed its enrollment identity: %d requests", len(requests))
	}
	if _, err := os.Stat(filepath.Join(home, "state", "trajectory-shipper", "managed-enrollment-attempt.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending enrollment key survived successful enrollment: %v", err)
	}
	var second controlplane.EnrollRequest
	if err := json.Unmarshal(requests[1], &second); err != nil {
		t.Fatal(err)
	}
	if second.Invite != "" || second.Grant != "managed-grant" {
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
	conflict := managedEnrollmentError(errors.Join(controlplane.ErrEnrollmentConflict, errors.New("grant=private-secret")))
	if !errors.Is(conflict, controlplane.ErrEnrollmentConflict) || !strings.Contains(conflict.Error(), "HTTP 409") || !strings.Contains(conflict.Error(), "administrator") ||
		strings.Contains(conflict.Error(), "private-secret") {
		t.Fatalf("unsafe or unhelpful enrollment conflict diagnostic: %v", conflict)
	}
}

func TestManagedEnrollmentAdoptsReplacementAfterCredentialRefusal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			stateDir := managedEnrollmentTestState(t)
			var original, replacement controlplane.EnrollRequest
			oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&original); err != nil {
					t.Error(err)
				}
				if original.Grant != "expired-grant" {
					t.Error("replacement grant leaked to the previous policy endpoint")
				}
				w.WriteHeader(status)
			}))
			defer oldServer.Close()
			newServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&replacement); err != nil {
					t.Error(err)
				}
				_ = json.NewEncoder(w).Encode(controlplane.EnrollResponse{Organization: "example"})
			}))
			defer newServer.Close()
			if _, err := testManagedLogin(oldServer.URL, "expired-grant"); !errors.Is(err, formats.ErrCredentialsRefused) {
				t.Fatalf("initial refusal: %v", err)
			}
			identityBefore, err := os.ReadFile(filepath.Join(stateDir, identity.FileName))
			if err != nil {
				t.Fatal(err)
			}
			_, _, originalKey, err := controlplane.ManagedEnrollmentAttempt(stateDir, oldServer.URL, original)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := testManagedLogin(newServer.URL, "replacement-grant"); err != nil {
				t.Fatal(err)
			}
			if replacement.Grant != "replacement-grant" {
				t.Fatal("replacement grant was not adopted")
			}
			replacement.Grant = original.Grant
			if replacement != original {
				t.Fatal("replacing a refused grant changed enrollment identity or request material")
			}
			identityAfter, err := os.ReadFile(filepath.Join(stateDir, identity.FileName))
			if err != nil || !bytes.Equal(identityBefore, identityAfter) {
				t.Fatalf("replacing a refused grant changed the local identity: %v", err)
			}
			enrolled, err := controlplane.LoadEnrollment(stateDir)
			if err != nil || enrolled.Endpoint != newServer.URL || enrolled.DeviceKey != controlplane.EncodeKey(originalKey) {
				t.Fatalf("replacement enrollment lost its endpoint or device key: %v", err)
			}
		})
	}
}

func TestManagedEnrollmentReplaysReplacementAfterAmbiguousFailure(t *testing.T) {
	for _, lostResponse := range []bool{false, true} {
		name := "HTTP 500"
		if lostResponse {
			name = "lost response"
		}
		t.Run(name, func(t *testing.T) {
			stateDir := managedEnrollmentTestState(t)
			var requestsMu sync.Mutex
			var requests [][]byte
			snapshot := func() [][]byte {
				requestsMu.Lock()
				defer requestsMu.Unlock()
				return append([][]byte(nil), requests...)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				requestsMu.Lock()
				requests = append(requests, raw)
				count := len(requests)
				requestsMu.Unlock()
				switch count {
				case 1, 2:
					w.WriteHeader(http.StatusForbidden)
				case 3:
					if lostResponse {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					w.WriteHeader(http.StatusInternalServerError)
				default:
					_ = json.NewEncoder(w).Encode(controlplane.EnrollResponse{Organization: "example"})
				}
			}))
			defer srv.Close()
			if _, err := testManagedLogin(srv.URL, "expired-grant"); !errors.Is(err, formats.ErrCredentialsRefused) {
				t.Fatalf("initial refusal: %v", err)
			}
			if _, err := testManagedLogin(srv.URL, "replacement-grant"); err == nil {
				t.Fatal("ambiguous replacement response was accepted")
			}
			recorded := snapshot()
			if len(recorded) != 3 {
				t.Fatalf("replacement sent %d requests, want 3", len(recorded))
			}
			var candidate controlplane.EnrollRequest
			if err := json.Unmarshal(recorded[2], &candidate); err != nil {
				t.Fatal(err)
			}
			if candidate.Grant != "replacement-grant" {
				t.Fatal("replacement request retained the refused grant")
			}
			_, persisted, _, err := controlplane.ManagedEnrollmentAttempt(stateDir, srv.URL, candidate)
			if err != nil || !bytes.Equal(recorded[2], persisted) {
				t.Fatalf("ambiguous replacement was not saved for replay: %v", err)
			}
			if _, err := testManagedLogin(srv.URL, "another-policy-grant"); err != nil {
				t.Fatal(err)
			}
			recorded = snapshot()
			if len(recorded) != 4 || !bytes.Equal(recorded[2], recorded[3]) {
				t.Fatal("a further policy change discarded the pending replacement request")
			}
		})
	}
}

func TestManagedEnrollmentConflictPreservesOriginalRecoveryRequest(t *testing.T) {
	stateDir := managedEnrollmentTestState(t)
	var requests [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		requests = append(requests, raw)
		switch len(requests) {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			w.WriteHeader(http.StatusForbidden)
		case 3:
			http.Error(w, "private-grant conflicts", http.StatusConflict)
		default:
			_ = json.NewEncoder(w).Encode(controlplane.EnrollResponse{Organization: "example"})
		}
	}))
	defer srv.Close()
	if _, err := testManagedLogin(srv.URL, "expired-grant"); err == nil {
		t.Fatal("lost enrollment response was accepted")
	}
	pending := filepath.Join(stateDir, "managed-enrollment-attempt.json")
	before, err := os.ReadFile(pending)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testManagedLogin(srv.URL, "replacement-grant"); !errors.Is(err, controlplane.ErrEnrollmentConflict) {
		t.Fatalf("replacement conflict: %v", err)
	}
	after, err := os.ReadFile(pending)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("replacement conflict discarded the original recovery request: %v", err)
	}
	if _, ok := LoggedIn(); ok {
		t.Fatal("conflicting replacement was saved as successful enrollment")
	}
	if _, err := testManagedLogin(srv.URL, "replacement-grant"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 4 || !bytes.Equal(requests[0], requests[3]) {
		t.Fatal("original enrollment could not be recovered after restoring its grant")
	}
}

func managedEnrollmentTestState(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	return filepath.Join(home, "state", "trajectory-shipper")
}

func testManagedLogin(endpoint, grant string) (LoginResult, error) {
	return loginWithRunCheck(context.Background(), endpoint, grant, true, func() error { return nil })
}
