package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
)

func TestManagedEnrollmentRetriesTheSameIdentityAfterALostResponse(t *testing.T) {
	stateDir := managedEnrollmentTestState(t)
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

	if _, err := testLogin(srv.URL, "managed-grant", true); err == nil {
		t.Fatal("first enrollment should fail locally")
	}
	if _, ok := LoggedIn(); ok {
		t.Fatal("a lost response was persisted as successful enrollment")
	}
	if _, err := testLogin(srv.URL, "managed-grant", true); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || !bytes.Equal(requests[0], requests[1]) {
		t.Fatalf("managed retry changed its enrollment identity: %d requests", len(requests))
	}
	if _, err := os.Stat(filepath.Join(stateDir, "managed-enrollment-attempt.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending enrollment key survived successful enrollment: %v", err)
	}
	var second controlplane.EnrollRequest
	if err := json.Unmarshal(requests[1], &second); err != nil {
		t.Fatal(err)
	}
	if second.Invite != "" || second.Grant != "managed-grant" {
		t.Fatal("managed enrollment did not use the grant request")
	}
	if _, err := testLogin(srv.URL, "replacement-grant", true); err != ErrAlreadyLoggedIn {
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

func TestManagedEnrollmentRotatesGrantAndReplaysSavedReplacement(t *testing.T) {
	for _, firstStatus := range []int{0, http.StatusInternalServerError, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(firstStatus), func(t *testing.T) {
			stateDir := managedEnrollmentTestState(t)
			const oldGrant = "fmi2.example.old.secret"
			const newGrant = "fmi2.example.new.secret"
			var requests [][]byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				requests = append(requests, raw)
				saved, err := os.ReadFile(filepath.Join(stateDir, "managed-enrollment-attempt.json"))
				var attempt struct{ Request []byte }
				if err != nil {
					t.Error(err)
				} else if err := json.Unmarshal(saved, &attempt); err != nil {
					t.Error(err)
				}
				if !bytes.Equal(attempt.Request, raw) {
					t.Error("request was sent before it was saved")
				}
				var req controlplane.EnrollRequest
				if err := json.Unmarshal(raw, &req); err != nil {
					t.Error(err)
					return
				}
				if req.Grant == oldGrant {
					w.WriteHeader(firstStatus)
					return
				}
				if req.Grant != newGrant {
					t.Error("unexpected grant")
				}
				// Lose two responses after rotation, then allow local enrollment to commit.
				replacementCount := len(requests)
				if firstStatus != 0 {
					replacementCount--
				}
				if replacementCount <= 2 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				_ = json.NewEncoder(w).Encode(controlplane.EnrollResponse{Organization: "example"})
			}))
			defer srv.Close()
			if firstStatus == 0 {
				unit, err := loadOrMintIdentity(stateDir)
				if err != nil {
					t.Fatal(err)
				}
				_, _, _, err = controlplane.ManagedEnrollmentAttempt(stateDir, srv.URL, controlplane.EnrollRequest{
					InstallID: unit.InstallID.String(), AgeRecipient: unit.Recipient().String(), Grant: oldGrant, Hostname: "original-host",
				})
				if err != nil {
					t.Fatal(err)
				}
			} else if _, err := testLogin(srv.URL, oldGrant, true); err == nil {
				t.Fatal("initial response should fail locally")
			}
			identityBefore, err := os.ReadFile(filepath.Join(stateDir, identity.FileName))
			if err != nil {
				t.Fatal(err)
			}
			pending, err := os.ReadFile(filepath.Join(stateDir, "managed-enrollment-attempt.json"))
			if err != nil {
				t.Fatal(err)
			}
			var original struct {
				DeviceKey string `json:"device_key"`
				Request   []byte
			}
			if err := json.Unmarshal(pending, &original); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if _, err := testLogin(srv.URL, newGrant, true); err == nil {
					t.Fatal("lost response accepted")
				}
				if _, ok := LoggedIn(); ok {
					t.Fatal("lost response committed enrollment")
				}
			}
			if _, err := testLogin(srv.URL, newGrant, true); err != nil {
				t.Fatal(err)
			}
			rotated := requests[len(requests)-3:]
			if !bytes.Equal(rotated[0], rotated[1]) || !bytes.Equal(rotated[1], rotated[2]) {
				t.Fatal("replacement retries changed bytes")
			}
			var before, after controlplane.EnrollRequest
			if err := json.Unmarshal(original.Request, &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(rotated[0], &after); err != nil {
				t.Fatal(err)
			}
			if after.Grant != newGrant {
				t.Fatal("grant was not replaced")
			}
			after.Grant = before.Grant
			if after != before {
				t.Fatal("rotation changed other request fields")
			}
			identityAfter, err := os.ReadFile(filepath.Join(stateDir, identity.FileName))
			if err != nil || !bytes.Equal(identityBefore, identityAfter) {
				t.Fatalf("identity changed: %v", err)
			}
			enrolled, err := controlplane.LoadEnrollment(stateDir)
			if err != nil || enrolled.Endpoint != srv.URL || enrolled.DeviceKey != original.DeviceKey {
				t.Fatalf("enrollment lost endpoint or key: %v", err)
			}
			if _, err := os.Stat(filepath.Join(stateDir, "managed-enrollment-attempt.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pending state survived success: %v", err)
			}
		})
	}
}

func TestManagedEnrollmentRefusedReplacementRemainsPending(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			stateDir := managedEnrollmentTestState(t)
			var requests [][]byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				requests = append(requests, raw)
				w.WriteHeader(status)
			}))
			defer srv.Close()
			if _, err := testLogin(srv.URL, "fmi2.example.old.secret", true); err == nil {
				t.Fatal("initial refusal accepted")
			}
			for i := 0; i < 2; i++ {
				_, err := testLogin(srv.URL, "fmi2.example.new.secret", true)
				want := formats.ErrCredentialsRefused
				if status == http.StatusConflict {
					want = controlplane.ErrEnrollmentConflict
				}
				if !errors.Is(err, want) {
					t.Fatalf("replacement refusal: %v", err)
				}
				if _, ok := LoggedIn(); ok {
					t.Fatal("refusal committed enrollment")
				}
			}
			if len(requests) != 3 || !bytes.Equal(requests[1], requests[2]) {
				t.Fatal("refusal reverted the replacement grant")
			}
			var replacement controlplane.EnrollRequest
			if err := json.Unmarshal(requests[2], &replacement); err != nil {
				t.Fatal(err)
			}
			if replacement.Grant != "fmi2.example.new.secret" {
				t.Fatal("replacement not sent")
			}
			_, saved, _, err := controlplane.ManagedEnrollmentAttempt(stateDir, srv.URL, replacement)
			if err != nil || !bytes.Equal(saved, requests[2]) {
				t.Fatalf("replacement not retained: %v", err)
			}
		})
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

func testLogin(endpoint, token string, managed bool) (LoginResult, error) {
	return loginWithRunCheck(context.Background(), endpoint, token, managed, func() error { return nil })
}
