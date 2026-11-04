package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func postEnrollment(t *testing.T, server *Server, req enrollRequest, want int) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body)))
	if response.Code != want {
		t.Fatalf("enrollment: HTTP %d, want %d: %s", response.Code, want, response.Body.String())
	}
	if want == http.StatusOK && response.Body.String() != "{\"organization\":\"acme\"}\n" {
		t.Fatalf("unexpected response: %s", response.Body.String())
	}
}

func TestGrantRotationCompletesTheSameInstall(t *testing.T) {
	for _, reachedServer := range []bool{false, true} {
		name := "original never arrived"
		if reachedServer {
			name = "original response lost"
		}
		t.Run(name, func(t *testing.T) {
			server, store := testAdminServer(t)
			manager, _ := server.manager.ForOrganization("acme")
			ctx := context.Background()
			old, err := manager.CreateGrant(ctx, manager.time().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			replacement, err := manager.CreateGrant(ctx, manager.time().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			identity := testInstall(t, uuid.NewString(), "")
			req := enrollRequest{Grant: old, InstallID: identity.InstallID, DevicePublicKey: identity.DevicePublicKey, AgeRecipient: identity.AgeRecipient, Hostname: "original-host", Platform: "darwin/arm64"}
			var before []byte
			var version string
			if reachedServer {
				postEnrollment(t, server, req, http.StatusOK)
				before, version, err = store.Get(ctx, installKey("acme", identity.InstallID))
				if err != nil {
					t.Fatal(err)
				}
			}
			_, id, _ := organizationToken(old)
			if err := manager.RevokeGrant(ctx, id); err != nil {
				t.Fatal(err)
			}
			postEnrollment(t, server, req, http.StatusForbidden)
			req.Grant = replacement
			if reachedServer {
				req.Hostname, req.Platform = "new-host", "linux/amd64"
			}
			for i := 0; i < 3; i++ {
				postEnrollment(t, server, req, http.StatusOK)
			}
			after, afterVersion, err := store.Get(ctx, installKey("acme", identity.InstallID))
			if err != nil {
				t.Fatal(err)
			}
			if reachedServer && (!bytes.Equal(before, after) || version != afterVersion) {
				t.Fatal("grant rotation rewrote the original install")
			}
			if store.versionOf(installKey("acme", identity.InstallID)) != 1 {
				t.Fatal("replays wrote additional install versions")
			}
			installs, err := store.List(ctx, "v1/organization=acme/control/installs/")
			if err != nil || len(installs) != 1 {
				t.Fatalf("install count: %d, %v", len(installs), err)
			}
		})
	}
}

func TestGrantRotationStillValidatesCredentialAndIdentity(t *testing.T) {
	for _, name := range []string{"invalid", "expired", "revoked grant", "device key", "age recipient", "revoked install", "pending install"} {
		t.Run(name, func(t *testing.T) {
			server, manager, _, id := enrolledServer(t)
			ctx := context.Background()
			original, err := manager.LoadActiveInstall(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			token, err := manager.CreateGrant(ctx, manager.time().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			req := enrollRequest{Grant: token, InstallID: id, DevicePublicKey: original.DevicePublicKey, AgeRecipient: original.AgeRecipient}
			want := http.StatusForbidden
			switch name {
			case "invalid":
				req.Grant += "invalid"
			case "expired":
				now := manager.time().Add(2 * time.Hour)
				manager.now = func() time.Time { return now }
			case "revoked grant":
				_, grantID, _ := organizationToken(token)
				if err := manager.RevokeGrant(ctx, grantID); err != nil {
					t.Fatal(err)
				}
			case "device key":
				req.DevicePublicKey = testInstall(t, uuid.NewString(), "").DevicePublicKey
				want = http.StatusConflict
			case "age recipient":
				req.AgeRecipient = testInstall(t, uuid.NewString(), "").AgeRecipient
				want = http.StatusConflict
			case "revoked install":
				if err := manager.RevokeInstall(ctx, id); err != nil {
					t.Fatal(err)
				}
				want = http.StatusConflict
			case "pending install":
				record, version, err := getRecord[InstallRecord](ctx, manager.store, installKey("acme", id))
				if err != nil {
					t.Fatal(err)
				}
				record.Status = InstallPending
				if err := replaceRecord(ctx, manager.store, installKey("acme", id), version, record); err != nil {
					t.Fatal(err)
				}
				want = http.StatusConflict
			}
			before, version, err := manager.store.Get(ctx, installKey("acme", id))
			if err != nil {
				t.Fatal(err)
			}
			postEnrollment(t, server, req, want)
			after, afterVersion, err := manager.store.Get(ctx, installKey("acme", id))
			if err != nil || !bytes.Equal(before, after) || version != afterVersion {
				t.Fatalf("refusal modified install: %v", err)
			}
		})
	}
}

func TestInviteReplayStillRequiresExactRequest(t *testing.T) {
	server, _ := testAdminServer(t)
	manager, _ := server.manager.ForOrganization("acme")
	token, err := manager.CreateInvite(context.Background(), manager.time().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	identity := testInstall(t, uuid.NewString(), "")
	req := enrollRequest{Invite: token, InstallID: identity.InstallID, DevicePublicKey: identity.DevicePublicKey, AgeRecipient: identity.AgeRecipient, Hostname: "original-host"}
	postEnrollment(t, server, req, http.StatusOK)
	postEnrollment(t, server, req, http.StatusOK)
	req.Hostname = "changed-host"
	postEnrollment(t, server, req, http.StatusForbidden)
	replacement, err := manager.CreateInvite(context.Background(), manager.time().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	req.Invite = replacement
	postEnrollment(t, server, req, http.StatusConflict)
}

func TestReplacementGrantMustBelongToEnrollmentOrganization(t *testing.T) {
	manager, store := testManager(t)
	ctx := context.Background()
	old, err := manager.CreateGrant(ctx, manager.time().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	install := testInstall(t, uuid.NewString(), "original request")
	if err := manager.EnrollGrant(ctx, old, install); err != nil {
		t.Fatal(err)
	}
	other, _ := manager.ForOrganization("other")
	token, err := other.CreateGrant(ctx, manager.time().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	install.EnrollmentDigest = enrollmentDigest([]byte("replacement request"))
	if err := manager.EnrollGrant(ctx, token, install); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-organization grant: %v", err)
	}
	if store.versionOf(installKey("acme", install.InstallID)) != 1 {
		t.Fatal("refusal changed the original install")
	}
}
