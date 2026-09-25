package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
)

func testMultiTenantManager(t *testing.T) (*Manager, *memoryStore) {
	t.Helper()
	store := newMemoryStore()
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) }
	return manager, store
}

func testManager(t *testing.T) (*Manager, *memoryStore) {
	t.Helper()
	store := newMemoryStore()
	root, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := root.ForOrganization("acme")
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) }
	return manager, store
}

func testInstall(t *testing.T, id, body string) InstallRecord {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return InstallRecord{InstallID: id, DevicePublicKey: base64.StdEncoding.EncodeToString(public), AgeRecipient: identity.Recipient().String(), EnrollmentDigest: enrollmentDigest([]byte(body))}
}

// ptr addresses a literal, for the configuration fields whose nil is a third state.
func ptr[T any](v T) *T { return &v }

func testAgeRecipients(t *testing.T, count int) []string {
	t.Helper()
	recipients := make([]string, count)
	for i := range recipients {
		identity, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		recipients[i] = identity.Recipient().String()
	}
	return recipients
}

func TestInviteEnrollmentRecoversEveryTransition(t *testing.T) {
	for _, transition := range []string{"reserved", "install-created", "spent"} {
		t.Run(transition, func(t *testing.T) {
			manager, _ := testManager(t)
			ctx := context.Background()
			token, err := manager.CreateInvite(ctx, manager.time().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			install := testInstall(t, uuid.NewString(), transition)
			injected := errors.New("injected interruption")
			manager.transition = func(name string) error {
				if name == transition {
					return injected
				}
				return nil
			}
			if err := manager.EnrollInvite(ctx, token, install); !errors.Is(err, injected) {
				t.Fatalf("first attempt: %v", err)
			}
			manager.transition = nil
			if err := manager.EnrollInvite(ctx, token, install); err != nil {
				t.Fatalf("retry: %v", err)
			}
			record, err := manager.LoadActiveInstall(ctx, install.InstallID)
			if err != nil || record.Status != InstallActive {
				t.Fatalf("active record: %#v %v", record, err)
			}
			if err := manager.EnrollInvite(ctx, token, install); err != nil {
				t.Fatalf("completed replay: %v", err)
			}
		})
	}
}

func TestGrantEnrollmentReplayRequiresTheSameIdentity(t *testing.T) {
	manager, _ := testManager(t)
	ctx := context.Background()
	token, _ := manager.CreateGrant(ctx, manager.time().Add(time.Hour))
	install := testInstall(t, uuid.NewString(), "same body")
	if err := manager.EnrollGrant(ctx, token, install); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnrollGrant(ctx, token, install); err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	changed := install
	changed.DevicePublicKey = "another key"
	if err := manager.EnrollGrant(ctx, token, changed); !errors.Is(err, ErrEnrollmentConflict) {
		t.Fatalf("changed identity replay: %v", err)
	}
}

func TestInviteReservationCannotMoveToAnotherInstall(t *testing.T) {
	manager, _ := testManager(t)
	ctx := context.Background()
	token, _ := manager.CreateInvite(ctx, manager.time().Add(time.Hour))
	first := testInstall(t, uuid.NewString(), "first")
	manager.transition = func(name string) error {
		if name == "reserved" {
			return errors.New("stop")
		}
		return nil
	}
	if err := manager.EnrollInvite(ctx, token, first); err == nil {
		t.Fatal("expected interruption")
	}
	manager.transition = nil
	second := testInstall(t, uuid.NewString(), "second")
	if err := manager.EnrollInvite(ctx, token, second); !errors.Is(err, ErrForbidden) {
		t.Fatalf("second install: %v", err)
	}
}

func TestReservedInviteCanResumeAfterExpiry(t *testing.T) {
	manager, _ := testManager(t)
	ctx := context.Background()
	token, _ := manager.CreateInvite(ctx, manager.time().Add(time.Minute))
	install := testInstall(t, uuid.NewString(), "body")
	manager.transition = func(name string) error {
		if name == "reserved" {
			return errors.New("stop")
		}
		return nil
	}
	_ = manager.EnrollInvite(ctx, token, install)
	manager.transition = nil
	manager.now = func() time.Time { return time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC) }
	if err := manager.EnrollInvite(ctx, token, install); err != nil {
		t.Fatalf("resume after expiry: %v", err)
	}
}

func TestGrantExpiryAndRevocation(t *testing.T) {
	manager, _ := testManager(t)
	ctx := context.Background()
	token, _ := manager.CreateGrant(ctx, manager.time().Add(time.Hour))
	_, id, _ := organizationToken(token)
	if err := manager.RevokeGrant(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnrollGrant(ctx, token, testInstall(t, uuid.NewString(), "revoked")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked grant: %v", err)
	}
	short, _ := manager.CreateGrant(ctx, manager.time().Add(time.Minute))
	manager.now = func() time.Time { return time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC) }
	if err := manager.EnrollGrant(ctx, short, testInstall(t, uuid.NewString(), "expired")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expired grant: %v", err)
	}
}

func TestInviteReleaseRequiresAbsentOrRevokedInstall(t *testing.T) {
	manager, _ := testManager(t)
	ctx := context.Background()
	token, _ := manager.CreateInvite(ctx, manager.time().Add(time.Hour))
	_, id, _ := organizationToken(token)
	install := testInstall(t, uuid.NewString(), "body")
	manager.transition = func(name string) error {
		if name == "install-created" {
			return errors.New("stop")
		}
		return nil
	}
	_ = manager.EnrollInvite(ctx, token, install)
	manager.transition = nil
	if err := manager.ReleaseInvite(ctx, id); err == nil {
		t.Fatal("released reservation for pending install")
	}
	if err := manager.RevokeInstall(ctx, install.InstallID); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseInvite(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestInitRequiresVersioningAndValidRecipients(t *testing.T) {
	manager, store := testManager(t)
	store.versioning = false
	if err := manager.Init(context.Background(), FleetConfig{AgeRecipients: testAgeRecipients(t, 2)}); err == nil {
		t.Fatal("initialized without versioning")
	}
	store.versioning = true
	if err := manager.Init(context.Background(), FleetConfig{AgeRecipients: []string{"AGE-SECRET-KEY-PRIVATE"}, IncludeInstallRecipient: false}); err == nil {
		t.Fatal("accepted private recipient")
	}
	if err := manager.Init(context.Background(), FleetConfig{AgeRecipients: testAgeRecipients(t, 1)}); err == nil {
		t.Fatal("initialized with fewer than two organization recipients")
	}
	if err := manager.Init(context.Background(), FleetConfig{AgeRecipients: []string{quesmaETLAgeRecipient, testAgeRecipients(t, 1)[0]}}); err == nil {
		t.Fatal("accepted the managed Quesma ETL recipient as an organization recipient")
	}
}

func TestExistingSingleRecipientConfigCanBeReadButNotReapplied(t *testing.T) {
	manager, store := testManager(t)
	cfg := FleetConfig{Schema: schemaVersion, Organization: "acme", AgeRecipients: testAgeRecipients(t, 1), IncludeInstallRecipient: true, UpdatedAt: manager.time()}
	if err := createRecord(context.Background(), store, configKey("acme"), cfg); err != nil {
		t.Fatal(err)
	}
	loaded, version, err := manager.LoadConfig(context.Background())
	if err != nil {
		t.Fatalf("load existing config: %v", err)
	}
	if err := manager.ApplyConfig(context.Background(), loaded, version); err == nil {
		t.Fatal("reapplied config with fewer than two organization recipients")
	}
}

func TestSelfRoutingTokensKeepOrganizationsIsolated(t *testing.T) {
	manager, _ := testMultiTenantManager(t)
	ctx := context.Background()
	acme, _ := manager.ForOrganization("acme.prod")
	other, _ := manager.ForOrganization("other")
	for _, scoped := range []*Manager{acme, other} {
		if err := scoped.Init(ctx, FleetConfig{AgeRecipients: testAgeRecipients(t, 2), IncludeInstallRecipient: true}); err != nil {
			t.Fatal(err)
		}
	}
	token, err := acme.CreateInvite(ctx, manager.time().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "fmi2.acme.prod.") {
		t.Fatalf("token = %q", token)
	}
	org, err := enrollmentOrganization(token)
	if err != nil || org != "acme.prod" {
		t.Fatalf("route = %q, %v", org, err)
	}
	install := testInstall(t, uuid.NewString(), "body")
	if err := acme.EnrollInvite(ctx, token, install); err != nil {
		t.Fatal(err)
	}
	if _, err := other.LoadActiveInstall(ctx, install.InstallID); !errors.Is(err, ErrUnknownInstall) {
		t.Fatalf("other organization loaded install: %v", err)
	}
	tampered := strings.Replace(token, "fmi2.acme.prod.", "fmi2.other.", 1)
	if err := other.EnrollInvite(ctx, tampered, testInstall(t, uuid.NewString(), "tampered")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tampered organization: %v", err)
	}
}

func TestRoutingRequiresAnOrganization(t *testing.T) {
	if _, err := enrollmentOrganization("fmi1.3f2504e0-4f89-41d3-9a0c-0305e82c3301.secret"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("legacy token: %v", err)
	}
	if _, err := deviceOrganization(""); !errors.Is(err, ErrUnknownInstall) {
		t.Fatalf("missing device organization: %v", err)
	}
}
