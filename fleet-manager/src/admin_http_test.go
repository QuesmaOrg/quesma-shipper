package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

const testAdminCredential = "fma1.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func storeAdminCredential(t *testing.T, store *memoryStore, credential string) {
	t.Helper()
	digest := sha256.Sum256([]byte(credential))
	rec := AdminCredentialRecord{Schema: schemaVersion, SecretDigest: hex.EncodeToString(digest[:])}
	raw, err := encodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), deploymentAdminCredentialKey(), raw); err != nil {
		t.Fatal(err)
	}
}

func testAdminServer(t *testing.T) (*Server, *memoryStore) {
	t.Helper()
	store := newMemoryStore()
	storeAdminCredential(t, store, testAdminCredential)
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) }
	acme, _ := manager.ForOrganization("acme")
	if err := acme.Init(context.Background(), FleetConfig{AgeRecipients: twoRecipients(t), IncludeInstallRecipient: true}); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(manager, fakeSigner{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	attachTelemetryProxy(t, server)
	return server, store
}

func testMultiTenantAdminServer(t *testing.T) (*Server, *memoryStore) {
	t.Helper()
	store := newMemoryStore()
	digest := sha256.Sum256([]byte(testAdminCredential))
	rec, _ := encodeRecord(AdminCredentialRecord{Schema: schemaVersion, SecretDigest: hex.EncodeToString(digest[:])})
	if err := store.Create(context.Background(), deploymentAdminCredentialKey(), rec); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) }
	server, err := NewServer(manager, fakeSigner{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return server, store
}

func adminRequest(method, target, body, credential string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	return req
}

func serveAdmin(t *testing.T, server *Server, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)
	return w
}

func TestAdminCredentialVerificationAndRotation(t *testing.T) {
	server, store := testAdminServer(t)
	verified, err := server.manager.VerifyAdminCredential(context.Background(), testAdminCredential)
	if err != nil || !verified {
		t.Fatalf("verify configured credential: %v, %v", verified, err)
	}
	verified, err = server.manager.VerifyAdminCredential(context.Background(), "fma1.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
	if err != nil || verified {
		t.Fatalf("verify incorrect credential: %v, %v", verified, err)
	}

	rotated := "fma1.CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	digest := sha256.Sum256([]byte(rotated))
	rec, _ := encodeRecord(AdminCredentialRecord{Schema: schemaVersion, SecretDigest: hex.EncodeToString(digest[:])})
	_, version, _ := store.Get(context.Background(), deploymentAdminCredentialKey())
	if err := store.Replace(context.Background(), deploymentAdminCredentialKey(), version, rec); err != nil {
		t.Fatal(err)
	}
	oldVerified, _ := server.manager.VerifyAdminCredential(context.Background(), testAdminCredential)
	newVerified, _ := server.manager.VerifyAdminCredential(context.Background(), rotated)
	if oldVerified || !newVerified {
		t.Fatalf("rotation result: old=%v new=%v", oldVerified, newVerified)
	}
}

func TestAdminCredentialRecordFailsClosed(t *testing.T) {
	tests := []AdminCredentialRecord{
		{Schema: 2, SecretDigest: strings.Repeat("0", 64)},
		{Schema: 1, SecretDigest: "not-hex"},
		{Schema: 1, SecretDigest: strings.Repeat("A", 64)},
	}
	for _, rec := range tests {
		store := newMemoryStore()
		raw, _ := encodeRecord(rec)
		_ = store.Create(context.Background(), deploymentAdminCredentialKey(), raw)
		manager, _ := NewManager(store)
		if ok, err := manager.VerifyAdminCredential(context.Background(), testAdminCredential); err == nil || ok {
			t.Fatalf("record %#v did not fail closed: ok=%v err=%v", rec, ok, err)
		}
	}
}

func TestEveryAdminRouteRequiresAuthentication(t *testing.T) {
	server, _ := testAdminServer(t)
	routes := []struct{ method, path string }{
		{"GET", "/v1/admin/orgs"}, {"POST", "/v1/admin/orgs"},
		{"GET", "/v1/admin/orgs/acme/config"}, {"PUT", "/v1/admin/orgs/acme/config"},
		{"GET", "/v1/admin/orgs/acme/grants"}, {"POST", "/v1/admin/orgs/acme/grants"}, {"POST", "/v1/admin/orgs/acme/grants/id/revoke"},
		{"GET", "/v1/admin/orgs/acme/invites"}, {"POST", "/v1/admin/orgs/acme/invites"}, {"POST", "/v1/admin/orgs/acme/invites/id/revoke"},
		{"POST", "/v1/admin/orgs/acme/invites/id/release"}, {"GET", "/v1/admin/orgs/acme/installs"}, {"POST", "/v1/admin/orgs/acme/installs/id/revoke"},
		{"GET", "/v1/admin/orgs/acme/installs/seen"}, {"GET", "/v1/admin/orgs/acme/installs/tags"}, {"PUT", "/v1/admin/orgs/acme/installs/id/tags"},
		{"GET", "/v1/admin/not-a-route"},
	}
	for _, route := range routes {
		for _, credential := range []string{"", "malformed", "fma1.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"} {
			w := serveAdmin(t, server, adminRequest(route.method, route.path, `{}`, credential))
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %q: status %d", route.method, route.path, credential, w.Code)
			}
			if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
				t.Errorf("%s %s error is not JSON: %q", route.method, route.path, w.Header().Get("Content-Type"))
			}
		}
	}
}

type countingStore struct {
	ObjectStore
	credentialKey string
	otherGets     int
}

type failingGetStore struct {
	ObjectStore
	key string
	err error
}

func (s *failingGetStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if key == s.key {
		return nil, "", s.err
	}
	return s.ObjectStore.Get(ctx, key)
}

func (s *countingStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if key != s.credentialKey && key != deploymentAdminCredentialKey() {
		s.otherGets++
	}
	return s.ObjectStore.Get(ctx, key)
}

func TestAdminAuthenticatesBeforeBodyOrOrganizationState(t *testing.T) {
	base := newMemoryStore()
	storeAdminCredential(t, base, testAdminCredential)
	store := &countingStore{ObjectStore: base, credentialKey: deploymentAdminCredentialKey()}
	manager, _ := NewManager(store)
	server, _ := NewServer(manager, fakeSigner{}, log.New(io.Discard, "", 0))

	body := strings.Repeat("x", requestLimit+1)
	w := serveAdmin(t, server, adminRequest("PUT", "/v1/admin/orgs/acme/config", body, "fma1.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", w.Code)
	}
	if store.otherGets != 0 {
		t.Fatalf("read %d non-credential objects before rejecting authentication", store.otherGets)
	}
}

func TestAdminDoesNotExposeBackendErrors(t *testing.T) {
	base := newMemoryStore()
	storeAdminCredential(t, base, testAdminCredential)
	store := &failingGetStore{ObjectStore: base, key: configKey("acme"), err: errors.New("configuration backend secret detail")}
	manager, _ := NewManager(store)
	server, _ := NewServer(manager, fakeSigner{}, log.New(io.Discard, "", 0))
	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/config", "", testAdminCredential))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "backend secret") {
		t.Fatalf("backend error response = %d: %s", w.Code, w.Body.String())
	}
	var response errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Error != "load config unavailable" {
		t.Fatalf("backend error response = %#v, %v", response, err)
	}
}

func twoRecipients(t *testing.T) []string {
	t.Helper()
	one, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	two, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return []string{one.Recipient().String(), two.Recipient().String()}
}

func configJSON(t *testing.T, recipients []string) string {
	t.Helper()
	raw, err := json.Marshal(adminConfigRequest{AgeRecipients: recipients, IncludeInstallRecipient: true, AuthoredYAML: "mode: once"})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAdminConfigLifecycleAndConcurrency(t *testing.T) {
	server, _ := testAdminServer(t)
	body := configJSON(t, twoRecipients(t))
	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/config", "", testAdminCredential))
	if w.Code != http.StatusOK || w.Header().Get("ETag") == "" {
		t.Fatalf("config GET = %d ETag %q: %s", w.Code, w.Header().Get("ETag"), w.Body.String())
	}
	etag := w.Header().Get("ETag")
	var cfg FleetConfig
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil || cfg.Organization != "acme" || cfg.Schema != schemaVersion {
		t.Fatalf("config response = %#v, %v", cfg, err)
	}

	w = serveAdmin(t, server, adminRequest("PUT", "/v1/admin/orgs/acme/config", body, testAdminCredential))
	if w.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match status = %d", w.Code)
	}
	req := adminRequest("PUT", "/v1/admin/orgs/acme/config", body, testAdminCredential)
	req.Header.Set("If-Match", etag)
	w = serveAdmin(t, server, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("apply status = %d: %s", w.Code, w.Body.String())
	}
	req = adminRequest("PUT", "/v1/admin/orgs/acme/config", body, testAdminCredential)
	req.Header.Set("If-Match", etag)
	w = serveAdmin(t, server, req)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale apply status = %d: %s", w.Code, w.Body.String())
	}
}

// What an organization gets when it is created three ways: silent, off, and on. Silent and on must
// be indistinguishable -- the recipient is there unless someone says otherwise, so history stays
// open to analytics asked for later -- and the explicit false must remove it.
func TestQuesmaETLIsOnUnlessAnOrganizationOptsOut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow *bool
		want  bool
	}{
		{"absent", nil, true},
		{"false", ptr(false), false},
		{"true", ptr(true), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := testMultiTenantAdminServer(t)
			body, err := json.Marshal(adminCreateOrganizationRequest{
				Slug: "private", DisplayName: "Private", AgeRecipients: twoRecipients(t),
				IncludeInstallRecipient: true, AllowQuesmaETL: tc.allow,
			})
			if err != nil {
				t.Fatal(err)
			}
			w := serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs", string(body), testAdminCredential))
			if w.Code != http.StatusCreated {
				t.Fatalf("create organization = %d: %s", w.Code, w.Body.String())
			}
			manager, _ := server.manager.ForOrganization("private")
			cfg, _, err := manager.LoadConfig(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			served := strings.Contains(renderConfig(cfg), quesmaETLAgeRecipient)
			if cfg.quesmaETLEnabled() != tc.want || served != tc.want {
				t.Fatalf("allow_quesma_etl %s: enabled=%v served=%v, want %v", tc.name, cfg.quesmaETLEnabled(), served, tc.want)
			}
		})
	}
}

func TestAdminStrictJSONAndRequestLimit(t *testing.T) {
	server, _ := testAdminServer(t)
	w := serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs/acme/grants", `{"expires_at":"2030-01-01T00:00:00Z","extra":true}`, testAdminCredential))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown JSON field status = %d", w.Code)
	}
	tooLarge := `{"expires_at":"2030-01-01T00:00:00Z","padding":"` + strings.Repeat("x", requestLimit) + `"}`
	w = serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs/acme/grants", tooLarge, testAdminCredential))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "exceeds 1 MiB") {
		t.Fatalf("oversize response = %d %q", w.Code, w.Body.String())
	}
}

func TestAdminCreatesListsAndRevokesCredentials(t *testing.T) {
	server, _ := testAdminServer(t)
	expiry := `{"expires_at":"2030-01-01T00:00:00Z"}`
	for _, resource := range []string{"grants", "invites"} {
		w := serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs/acme/"+resource, expiry, testAdminCredential))
		if w.Code != http.StatusCreated || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("create %s = %d cache %q: %s", resource, w.Code, w.Header().Get("Cache-Control"), w.Body.String())
		}
		var response adminSecretResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Secret == "" {
			t.Fatalf("create %s response: %#v, %v", resource, response, err)
		}
		org, id, err := organizationToken(response.Secret)
		if err != nil || org != "acme" {
			t.Fatalf("unexpected secret %q", response.Secret)
		}
		w = serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/"+resource, "", testAdminCredential))
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), response.Secret) || strings.Contains(w.Body.String(), "secret_digest") {
			t.Fatalf("list %s = %d: %s", resource, w.Code, w.Body.String())
		}
		w = serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs/acme/"+resource+"/"+id+"/revoke", "", testAdminCredential))
		if w.Code != http.StatusNoContent {
			t.Fatalf("revoke %s = %d: %s", resource, w.Code, w.Body.String())
		}
	}
}

func TestAdminReleasesInviteAndListsAndRevokesInstall(t *testing.T) {
	server, store := testAdminServer(t)
	w := serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs/acme/invites", `{"expires_at":"2030-01-01T00:00:00Z"}`, testAdminCredential))
	if w.Code != http.StatusCreated {
		t.Fatalf("create invite = %d: %s", w.Code, w.Body.String())
	}
	var secret adminSecretResponse
	if err := json.Unmarshal(w.Body.Bytes(), &secret); err != nil {
		t.Fatal(err)
	}
	_, id, err := organizationToken(secret.Secret)
	if err != nil {
		t.Fatal(err)
	}
	w = serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs/acme/invites/"+id+"/release", "", testAdminCredential))
	if w.Code != http.StatusNoContent {
		t.Fatalf("release invite = %d: %s", w.Code, w.Body.String())
	}

	installID := "11111111-1111-1111-1111-111111111111"
	now := server.manager.time()
	record := InstallRecord{Schema: schemaVersion, Organization: "acme", InstallID: installID, Status: InstallActive, CreatedAt: now, UpdatedAt: now}
	raw, _ := encodeRecord(record)
	if err := store.Create(context.Background(), installKey("acme", installID), raw); err != nil {
		t.Fatal(err)
	}
	w = serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/installs", "", testAdminCredential))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), installID) {
		t.Fatalf("list installs = %d: %s", w.Code, w.Body.String())
	}
	w = serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs/acme/installs/"+installID+"/revoke", "", testAdminCredential))
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke install = %d: %s", w.Code, w.Body.String())
	}
	stored, _, err := getRecord[InstallRecord](context.Background(), store, installKey("acme", installID))
	if err != nil || stored.Status != InstallRevoked {
		t.Fatalf("stored install = %#v, %v", stored, err)
	}
}

func TestAuthenticatedUnknownAdminRouteReturnsJSON(t *testing.T) {
	server, _ := testAdminServer(t)
	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/not-a-route", "", testAdminCredential))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unknown route = %d %q: %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	var response errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Error != "not found" {
		t.Fatalf("unknown route response = %#v, %v", response, err)
	}
}

func TestAdminETagRoundTripHandlesProviderVersions(t *testing.T) {
	for _, version := range []string{"17", `"012345abcdef"`, "generation/with punctuation"} {
		encoded := encodeETag(version)
		decoded, ok := decodeETag(encoded)
		if !ok || decoded != version {
			t.Errorf("round trip %q through %q = %q, %v", version, encoded, decoded, ok)
		}
	}
	if _, ok := decodeETag("*"); ok {
		t.Fatal("wildcard ETag accepted")
	}
}

func TestValidAdminCredentialShape(t *testing.T) {
	if !validAdminCredential(testAdminCredential) {
		t.Fatal("valid credential rejected")
	}
	encoded31 := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 31))
	for _, credential := range []string{"", "fma1.", "other." + strings.TrimPrefix(testAdminCredential, "fma1."), "fma1." + encoded31} {
		if validAdminCredential(credential) {
			t.Errorf("malformed credential accepted: %q", credential)
		}
	}
	padded := "fma1." + base64.URLEncoding.EncodeToString(make([]byte, 32))
	if !validAdminCredential(padded) {
		t.Fatal("Terraform-compatible padded base64url credential rejected")
	}
	store := newMemoryStore()
	storeAdminCredential(t, store, padded)
	manager, _ := NewManager(store)
	if verified, err := manager.VerifyAdminCredential(context.Background(), padded); err != nil || !verified {
		t.Fatalf("Terraform-compatible credential did not authenticate: verified=%v err=%v", verified, err)
	}
}

func TestMultiTenantOrganizationLifecycleAndIsolation(t *testing.T) {
	server, _ := testMultiTenantAdminServer(t)
	create := func(slug, name string) {
		raw, _ := json.Marshal(adminCreateOrganizationRequest{Slug: slug, DisplayName: name,
			AgeRecipients: twoRecipients(t), IncludeInstallRecipient: true, AuthoredYAML: "mode: once"})
		w := serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs", string(raw), testAdminCredential))
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s = %d: %s", slug, w.Code, w.Body.String())
		}
	}
	create("acme.prod", "Acme Production")
	create("other", "Other")

	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs", "", testAdminCredential))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Acme Production") || !strings.Contains(w.Body.String(), "other") {
		t.Fatalf("list organizations = %d: %s", w.Code, w.Body.String())
	}
	if w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/config", "", testAdminCredential)); w.Code != http.StatusNotFound {
		t.Fatalf("unscoped config = %d", w.Code)
	}

	expiry := `{"expires_at":"2030-01-01T00:00:00Z"}`
	w = serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs/acme.prod/grants", expiry, testAdminCredential))
	if w.Code != http.StatusCreated {
		t.Fatalf("create scoped grant = %d: %s", w.Code, w.Body.String())
	}
	var secret adminSecretResponse
	_ = json.Unmarshal(w.Body.Bytes(), &secret)
	org, id, err := organizationToken(secret.Secret)
	if err != nil || org != "acme.prod" {
		t.Fatalf("self-routing grant = %q: %q %q %v", secret.Secret, org, id, err)
	}
	w = serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/other/grants", "", testAdminCredential))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), id) {
		t.Fatalf("other grants = %d: %s", w.Code, w.Body.String())
	}

	w = serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme.prod/config", "", testAdminCredential))
	etag := w.Header().Get("ETag")
	if w.Code != http.StatusOK || etag == "" {
		t.Fatalf("get scoped config = %d: %s", w.Code, w.Body.String())
	}
	req := adminRequest("PUT", "/v1/admin/orgs/acme.prod/config", configJSON(t, twoRecipients(t)), testAdminCredential)
	req.Header.Set("If-Match", etag)
	if w = serveAdmin(t, server, req); w.Code != http.StatusNoContent {
		t.Fatalf("update scoped config = %d: %s", w.Code, w.Body.String())
	}
	req = adminRequest("PUT", "/v1/admin/orgs/acme.prod/config", configJSON(t, twoRecipients(t)), testAdminCredential)
	req.Header.Set("If-Match", etag)
	if w = serveAdmin(t, server, req); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale scoped config = %d: %s", w.Code, w.Body.String())
	}
	w = serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme.prod/config", "", testAdminCredential))
	name := "Acme Renamed"
	update := adminConfigRequest{DisplayName: &name, AgeRecipients: twoRecipients(t), IncludeInstallRecipient: true}
	raw, _ := json.Marshal(update)
	req = adminRequest("PUT", "/v1/admin/orgs/acme.prod/config", string(raw), testAdminCredential)
	req.Header.Set("If-Match", w.Header().Get("ETag"))
	if w = serveAdmin(t, server, req); w.Code != http.StatusNoContent {
		t.Fatalf("rename organization = %d: %s", w.Code, w.Body.String())
	}
	w = serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs", "", testAdminCredential))
	if !strings.Contains(w.Body.String(), name) {
		t.Fatalf("renamed organization absent: %s", w.Body.String())
	}
}

func TestScopedAdminRoutesRequireAnExistingOrganization(t *testing.T) {
	server, store := testMultiTenantAdminServer(t)
	expiry := `{"expires_at":"2030-01-01T00:00:00Z"}`
	w := serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs/missing/grants", expiry, testAdminCredential))
	if w.Code != http.StatusNotFound {
		t.Fatalf("create grant for missing organization = %d: %s", w.Code, w.Body.String())
	}
	if objects, err := store.List(context.Background(), controlPrefix("missing")); err != nil || len(objects) != 0 {
		t.Fatalf("missing organization objects = %v, %v", objects, err)
	}
}

func TestExistingConfigUsesSlugAsDisplayName(t *testing.T) {
	server, store := testMultiTenantAdminServer(t)
	cfg := FleetConfig{Schema: schemaVersion, Organization: "legacy", AgeRecipients: twoRecipients(t), IncludeInstallRecipient: true, UpdatedAt: server.manager.time()}
	if err := createRecord(context.Background(), store, configKey("legacy"), cfg); err != nil {
		t.Fatal(err)
	}
	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs", "", testAdminCredential))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"display_name":"legacy"`) {
		t.Fatalf("legacy display name = %d: %s", w.Code, w.Body.String())
	}
}

func TestOrganizationDisplayNameValidation(t *testing.T) {
	server, _ := testMultiTenantAdminServer(t)
	for _, name := range []string{"", "   ", "bad\nname", strings.Repeat("é", 101)} {
		raw, _ := json.Marshal(adminCreateOrganizationRequest{Slug: "acme", DisplayName: name,
			AgeRecipients: twoRecipients(t), IncludeInstallRecipient: true})
		w := serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs", string(raw), testAdminCredential))
		if w.Code != http.StatusBadRequest {
			t.Errorf("display name %q = %d: %s", name, w.Code, w.Body.String())
		}
	}
}
