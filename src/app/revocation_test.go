package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
	notifyRevocationWith(paths.StateDir, func(context.Context, string) bool { notifications++; return true })
	notifyRevocationWith(paths.StateDir, func(context.Context, string) bool { notifications++; return true })
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
	oldUnit, oldEnrollment := enrolledIdentity(t, paths.StateDir)
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
		return testLogin(oldEnrollment.Endpoint, "new-token", false)
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
	pending := filepath.Join(paths.StateDir, "re-enrollment", oldEnrollment.InstallID)
	for _, name := range []string{"identity.json.tmp-interrupted", "interactive-enrollment-attempt.json.tmp-interrupted"} {
		if err := os.WriteFile(filepath.Join(pending, name), []byte("synthetic stranded key material"), 0600); err != nil {
			t.Fatal(err)
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
	if _, err := os.Stat(filepath.Join(paths.StateDir, "re-enrollment", oldEnrollment.InstallID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending identity survived successful enrollment: %v", err)
	}
	if _, err := login(); !errors.Is(err, ErrAlreadyLoggedIn) {
		t.Fatalf("active enrollment replaced: %v", err)
	}
	if !strings.Contains(revoked.Error(), "detected") {
		t.Fatal("observation time presented as revocation time")
	}
}

func TestCorruptRevocationCanRecoverWithoutResumingOldEnrollment(t *testing.T) {
	for _, raw := range []string{"{", "{}"} {
		t.Run(raw, func(t *testing.T) {
			calls := 0
			paths := enrolledConfigServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/enroll" {
					t.Error("corrupt record allowed a signed request")
				}
				if calls == 1 {
					http.Error(w, "response lost", 503)
					return
				}
				_ = json.NewEncoder(w).Encode(controlplane.EnrollResponse{Organization: "acme"})
			})
			_, enr := enrolledIdentity(t, paths.StateDir)
			path := filepath.Join(paths.StateDir, controlplane.RevocationFile)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := New(Build{}); !errors.Is(err, controlplane.ErrInvalidRevocation) {
				t.Fatalf("corrupt record did not stop startup: %v", err)
			}
			report := Diagnose(context.Background(), Build{}, false)
			row := report.Sections[0].Rows[0]
			if !strings.Contains(row.Detail, path) || !strings.Contains(row.Fix, "token") {
				t.Fatalf("doctor omitted recovery: %+v", row)
			}
			login := func(managed bool) error {
				_, err := testLogin(enr.Endpoint, "new-token", managed)
				return err
			}
			if err := login(true); !errors.Is(err, controlplane.ErrInvalidRevocation) || calls != 0 {
				t.Fatalf("managed policy silently recovered corrupt enrollment: %v", err)
			}
			if err := login(false); err == nil {
				t.Fatal("expected response loss")
			}
			var revoked *controlplane.Revocation
			if !errors.As(EnrollmentRevocation(paths.StateDir), &revoked) {
				t.Fatal("failed recovery did not retain a stop marker")
			}
			if err := login(false); err != nil {
				t.Fatal(err)
			}
			archived, err := os.ReadFile(filepath.Join(paths.StateDir, "revoked-installs", enr.InstallID, controlplane.RevocationFile))
			if err != nil || string(archived) != raw {
				t.Fatalf("lost damaged original: %q, %v", archived, err)
			}
			if err := EnrollmentRevocation(paths.StateDir); err != nil {
				t.Fatalf("replacement stayed stopped: %v", err)
			}
			current, _ := controlplane.LoadEnrollment(paths.StateDir)
			if current.InstallID == enr.InstallID || calls != 2 {
				t.Fatalf("recovery did not replace enrollment: %+v, calls=%d", current, calls)
			}
		})
	}
}

func TestPendingEnrollmentCleanupDoesNotFollowSymlinks(t *testing.T) {
	pending, outside := t.TempDir(), t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("unrelated state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pending, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := clearPendingEnrollment(pending); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("cleanup followed a symlink: %v", err)
	}
}

func TestReenrollmentDoesNotBypassRevocationFileSafety(t *testing.T) {
	for _, kind := range []string{"symlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			paths := enrolledConfigServer(t, func(http.ResponseWriter, *http.Request) { t.Error("unsafe revocation file allowed enrollment") })
			enr, _ := controlplane.LoadEnrollment(paths.StateDir)
			original, err := os.ReadFile(filepath.Join(paths.StateDir, controlplane.EnrollmentFile))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(paths.StateDir, controlplane.RevocationFile)
			if kind == "symlink" {
				target := filepath.Join(t.TempDir(), "marker")
				if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else {
				if runtime.GOOS == "windows" {
					t.Skip("Windows has no POSIX file mode gate")
				}
				if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			_, err = testLogin(enr.Endpoint, "new-token", false)
			if err == nil || errors.Is(err, controlplane.ErrInvalidRevocation) || !strings.Contains(err.Error(), path) {
				t.Fatalf("unsafe file not refused with its path: %v", err)
			}
			current, err := os.ReadFile(filepath.Join(paths.StateDir, controlplane.EnrollmentFile))
			if err != nil || !bytes.Equal(current, original) {
				t.Fatal("unsafe file recovery changed enrollment")
			}
		})
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
	old, enr := enrolledIdentity(t, paths.StateDir)
	original, _ := os.ReadFile(filepath.Join(paths.StateDir, identity.FileName))
	revoked := &controlplane.Revocation{InstallID: enr.InstallID, Organization: enr.Organization, Endpoint: enr.Endpoint, DetectedAt: time.Now(), Notified: true}
	if err := revoked.Save(paths.StateDir); err != nil {
		t.Fatal(err)
	}
	login := func() (LoginResult, error) {
		return testLogin(enr.Endpoint, "token", false)
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

func TestReenrollmentConflictMintsFreshPendingIdentity(t *testing.T) {
	var requests []controlplane.EnrollRequest
	firstID := ""
	paths := enrolledConfigServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req controlplane.EnrollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		requests = append(requests, req)
		if firstID == "" {
			firstID = req.InstallID
		}
		switch {
		case len(requests) == 1:
			http.Error(w, "grant required", 403)
		case len(requests) == 2:
			http.Error(w, "response lost after grant was consumed", 503)
		case req.Grant == "old-token" || req.Invite == "new-token":
			http.Error(w, "grant required or expired", 403)
		case req.InstallID == firstID:
			http.Error(w, "install already exists with another request digest", 409)
		default:
			_ = json.NewEncoder(w).Encode(controlplane.EnrollResponse{Organization: "new-org"})
		}
	})
	old, enr := enrolledIdentity(t, paths.StateDir)
	revoked := &controlplane.Revocation{InstallID: enr.InstallID, Organization: enr.Organization, Endpoint: enr.Endpoint, DetectedAt: time.Now(), Notified: true}
	if err := revoked.Save(paths.StateDir); err != nil {
		t.Fatal(err)
	}
	login := func(token string) (LoginResult, error) {
		return testLogin(enr.Endpoint, token, false)
	}
	if _, err := login("old-token"); err == nil {
		t.Fatal("expected lost response")
	}
	if _, err := login("new-token"); err != nil {
		t.Fatal(err)
	}
	current, err := identity.Load(paths.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if current.InstallID.String() == firstID || current.InstallID == old.InstallID {
		t.Fatal("conflict reused pending or revoked identity")
	}
	if len(requests) != 7 {
		t.Fatalf("requests = %d, want bounded retry sequence of 7", len(requests))
	}
	if requests[1] != requests[2] {
		t.Fatal("lost grant response was not retried exactly")
	}
	if err := EnrollmentRevocation(paths.StateDir); err != nil {
		t.Fatal(err)
	}
}

func TestFailedNotificationRemainsPending(t *testing.T) {
	paths := enrolledConfigServer(t, func(http.ResponseWriter, *http.Request) { t.Error("notification contacted server") })
	enr, _ := controlplane.LoadEnrollment(paths.StateDir)
	revoked := &controlplane.Revocation{InstallID: enr.InstallID, Organization: enr.Organization, Endpoint: enr.Endpoint, DetectedAt: time.Now()}
	if err := revoked.Save(paths.StateDir); err != nil {
		t.Fatal(err)
	}
	calls := 0
	notify := func(ctx context.Context, message string) bool {
		calls++
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 10*time.Second {
			t.Error("cold notification startup has too little time")
		}
		if !strings.Contains(message, "detected") {
			t.Error("missing revocation detail")
		}
		return calls > 1
	}
	notifyRevocationWith(paths.StateDir, notify)
	saved, err := controlplane.LoadRevocation(paths.StateDir, enr)
	if err != nil || saved.Notified {
		t.Fatalf("failed delivery consumed notification: %v", err)
	}
	notifyRevocationWith(paths.StateDir, notify)
	notifyRevocationWith(paths.StateDir, notify)
	if calls != 2 {
		t.Fatalf("delivery attempts = %d", calls)
	}
}

func TestManagedRevocationGuidanceDoesNotOfferRefusedCommands(t *testing.T) {
	managed := revocationRecovery(true)
	if !strings.Contains(managed, "administrator") || strings.Contains(managed, "quesma-shipper uninstall") {
		t.Fatal(managed)
	}
	personal := revocationRecovery(false)
	if !strings.Contains(personal, "login <token>") || !strings.Contains(personal, "quesma-shipper uninstall") {
		t.Fatal(personal)
	}
	if strings.Contains((&controlplane.Revocation{}).Error(), "quesma-shipper") {
		t.Fatal("control plane error contains installation policy")
	}
}
