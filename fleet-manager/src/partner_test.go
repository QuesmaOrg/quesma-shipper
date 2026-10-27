package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const partnerRefusal = "this credential may not call this route"

// partnerServer has organizations acme and globex, and a partner credential minted for acme.
func partnerServer(t *testing.T) (*Server, *memoryStore, string) {
	t.Helper()
	server, store := testAdminServer(t)
	globex, _ := server.manager.ForOrganization("globex")
	if err := globex.Init(context.Background(), FleetConfig{AgeRecipients: twoRecipients(t), IncludeInstallRecipient: true}); err != nil {
		t.Fatal(err)
	}
	return server, store, mintPartner(t, server, "acme")
}

func mintPartner(t *testing.T, server *Server, org string) string {
	t.Helper()
	w := serveAdmin(t, server, adminRequest(http.MethodPost, "/v1/admin/orgs/"+org+"/partner", "", testAdminCredential))
	var minted adminSecretResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &minted) != nil || !validPartnerCredential(minted.Secret) {
		t.Fatalf("mint = %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("mint Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
	return minted.Secret
}

func configVersion(t *testing.T, store *memoryStore, org string) string {
	t.Helper()
	_, version, err := store.Get(context.Background(), configKey(org))
	if err != nil {
		t.Fatal(err)
	}
	return version
}

// The reason the class exists: a partner manages machines and collection, and cannot change who
// can decrypt the organization's uploads.
func TestAPartnerCannotChangeCustody(t *testing.T) {
	server, store, partner := partnerServer(t)
	// Every custody field set away from its default, so a collection write that dropped one shows.
	acme := server.manager.mustOrg(t, "acme")
	cfg, version, err := acme.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	off, collector := false, "https://collector.example.com"
	cfg.AllowQuesmaETL, cfg.TelemetryCollectorURL, cfg.DisplayName = &off, &collector, "Acme"
	if err := acme.ApplyConfig(context.Background(), cfg, version); err != nil {
		t.Fatal(err)
	}
	w := serveAdmin(t, server, adminRequest(http.MethodGet, "/v1/admin/orgs/acme/config", "", partner))
	if w.Code != http.StatusOK || w.Header().Get("ETag") == "" {
		t.Fatalf("partner config GET = %d %s", w.Code, w.Body.String())
	}
	etag, before := w.Header().Get("ETag"), configVersion(t, store, "acme")

	put := adminRequest(http.MethodPut, "/v1/admin/orgs/acme/config", configJSON(t, twoRecipients(t)), partner)
	put.Header.Set("If-Match", etag)
	if w := serveAdmin(t, server, put); w.Code != http.StatusForbidden {
		t.Fatalf("partner config PUT = %d %s", w.Code, w.Body.String())
	}

	recipients, _ := json.Marshal(twoRecipients(t))
	for _, field := range []string{
		`"age_recipients":` + string(recipients),
		`"include_install_recipient":false`,
		`"allow_quesma_etl":false`,
		`"telemetry_collector_url":"https://collector.example.com"`,
		`"display_name":"Renamed"`,
	} {
		put := adminRequest(http.MethodPut, "/v1/admin/orgs/acme/collection", `{"authored_yaml":"mode: {schedule: 1m}",`+field+`}`, partner)
		put.Header.Set("If-Match", etag)
		if w := serveAdmin(t, server, put); w.Code != http.StatusBadRequest {
			t.Errorf("collection PUT with %s = %d %s", field, w.Code, w.Body.String())
		}
	}
	if configVersion(t, store, "acme") != before {
		t.Fatal("a refused write changed config.json")
	}

	stored, _, _ := acme.LoadConfig(context.Background())
	if stored.AllowQuesmaETL == nil || *stored.AllowQuesmaETL || stored.TelemetryCollectorURL == nil {
		t.Fatalf("fixture custody not set: %+v", stored)
	}
	put = adminRequest(http.MethodPut, "/v1/admin/orgs/acme/collection", `{"authored_yaml":"mode: {schedule: 5m}"}`, partner)
	put.Header.Set("If-Match", etag)
	if w := serveAdmin(t, server, put); w.Code != http.StatusNoContent {
		t.Fatalf("partner collection PUT = %d %s", w.Code, w.Body.String())
	}
	after, _, _ := acme.LoadConfig(context.Background())
	// DeepEqual follows the pointers, so an unset field does not equal a stored false.
	custody := func(c FleetConfig) []any {
		return []any{c.AgeRecipients, c.IncludeInstallRecipient, c.AllowQuesmaETL, c.TelemetryCollectorURL, c.DisplayName}
	}
	if !reflect.DeepEqual(custody(after), custody(stored)) {
		t.Fatalf("collection write changed custody: %+v -> %+v", stored, after)
	}
	if after.Collection == nil || after.Collection.Mode == nil || after.Collection.Mode.Schedule == nil || *after.Collection.Mode.Schedule != "5m" {
		t.Fatalf("collection was not written: %+v", after.Collection)
	}
}

func (m *Manager) mustOrg(t *testing.T, org string) *Manager {
	t.Helper()
	scoped, err := m.ForOrganization(org)
	if err != nil {
		t.Fatal(err)
	}
	return scoped
}

// A credential for acme is compared with globex's digest on globex's path, so it fails there whether
// or not globex exists. The deployment-level routes have no organization to compare with.
func TestAPartnerStaysInItsOrganization(t *testing.T) {
	server, _, partner := partnerServer(t)
	for _, org := range []string{"globex", "initech"} {
		for _, route := range organizationRoutes(t) {
			path := strings.ReplaceAll(route.path, "/acme/", "/"+org+"/")
			if w := serveAdmin(t, server, adminRequest(route.method, path, `{}`, partner)); w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s = %d, want 401", route.method, path, w.Code)
			}
		}
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v1/admin/orgs"}, {http.MethodPost, "/v1/admin/orgs"},
		{http.MethodGet, "/v1/admin/defaults"}, {http.MethodGet, "/v1/admin/not-a-route"},
	} {
		w := serveAdmin(t, server, adminRequest(route.method, route.path, `{}`, partner))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), partnerRefusal) {
			t.Errorf("%s %s = %d %s, want 403", route.method, route.path, w.Code, w.Body.String())
		}
	}
}

type recordedRoute struct{ method, path string }

type routeRecorder struct{ routes []recordedRoute }

func (r *routeRecorder) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	method, path, _ := strings.Cut(pattern, " ")
	path = strings.NewReplacer("{slug}", "acme", "{id}", "3f2504e0-4f89-41d3-9a0c-0305e82c3301").Replace(path)
	r.routes = append(r.routes, recordedRoute{method, path})
}

// organizationRoutes is every route registerOrganizationRoutes registers, as a request under acme.
func organizationRoutes(t *testing.T) []recordedRoute {
	t.Helper()
	server, _ := testAdminServer(t)
	recorder := &routeRecorder{}
	registerOrganizationRoutes(recorder, "/v1/admin/orgs/{slug}", server)
	return recorder.routes
}

// Default-deny, checked against the routes actually registered: a route added later refuses a
// partner until this list and partnerRoutes both name it.
func TestEveryUnlistedRouteRefusesAPartner(t *testing.T) {
	server, _, partner := partnerServer(t)
	const install = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	allowed := map[string]bool{
		"GET /v1/admin/orgs/acme/config": true, "PUT /v1/admin/orgs/acme/collection": true,
		"GET /v1/admin/orgs/acme/sources": true, "GET /v1/admin/orgs/acme/grants": true,
		"POST /v1/admin/orgs/acme/grants": true, "POST /v1/admin/orgs/acme/grants/" + install + "/revoke": true,
		"GET /v1/admin/orgs/acme/invites": true, "POST /v1/admin/orgs/acme/invites": true,
		"POST /v1/admin/orgs/acme/invites/" + install + "/revoke":  true,
		"POST /v1/admin/orgs/acme/invites/" + install + "/release": true,
		"GET /v1/admin/orgs/acme/installs":                         true, "GET /v1/admin/orgs/acme/installs/seen": true,
		"GET /v1/admin/orgs/acme/installs/health": true, "GET /v1/admin/orgs/acme/installs/tags": true,
		"POST /v1/admin/orgs/acme/installs/" + install + "/health":    true,
		"PUT /v1/admin/orgs/acme/installs/" + install + "/tags":       true,
		"PATCH /v1/admin/orgs/acme/installs/" + install + "/metadata": true,
		"POST /v1/admin/orgs/acme/installs/metadata/import":           true,
		"POST /v1/admin/orgs/acme/installs/" + install + "/revoke":    true,
	}
	routes := organizationRoutes(t)
	if len(routes) != len(allowed)+4 {
		t.Fatalf("%d organization routes; classify each new one here", len(routes))
	}
	// Each layer on its own: adminAuth's allowlist, then the route's wrapper with the allowlist bypassed.
	inner := http.NewServeMux()
	registerOrganizationRoutes(inner, "/v1/admin/orgs/{slug}", server)
	for _, route := range routes {
		want := allowed[route.method+" "+route.path]
		if partnerRoute(httptest.NewRequest(route.method, route.path, nil)) != want {
			t.Errorf("partnerRoute(%s %s) != %v", route.method, route.path, want)
		}
		for _, org := range []string{"acme", "globex"} {
			request := adminRequest(route.method, route.path, `{}`, "")
			request = request.WithContext(context.WithValue(request.Context(), callerKey{}, principal{who: callerPartner, org: org}))
			w := httptest.NewRecorder()
			inner.ServeHTTP(w, request)
			refused := w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), partnerRefusal)
			if refused == (want && org == "acme") {
				t.Errorf("route wrapper on %s %s for a %s partner = %d %s", route.method, route.path, org, w.Code, w.Body.String())
			}
		}
		w := serveAdmin(t, server, adminRequest(route.method, route.path, `{}`, partner))
		refused := w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), partnerRefusal)
		if refused == want || w.Code == http.StatusUnauthorized {
			t.Errorf("%s %s = %d %s, allowed %v", route.method, route.path, w.Code, w.Body.String(), want)
		}
	}
}

// adminAuth reads the organization before the mux does. Whatever reaches a handler must have been
// verified against the organization the handler receives.
func TestThePartnersOrganizationIsTheHandlers(t *testing.T) {
	server, _, partner := partnerServer(t)
	mux := http.NewServeMux()
	var reached []string
	mux.HandleFunc("GET /v1/admin/orgs/{slug}/installs", func(w http.ResponseWriter, r *http.Request) {
		if got := callerOf(r).org; got != r.PathValue("slug") {
			t.Errorf("%s: verified %q, handler got %q", r.URL.EscapedPath(), got, r.PathValue("slug"))
		}
		reached = append(reached, r.PathValue("slug"))
	})
	handler := server.adminAuth(mux)
	for _, c := range []struct {
		path string
		want int
	}{
		{"/v1/admin/orgs/acme/installs", http.StatusOK},
		{"/v1/admin/orgs/a%63me/installs", http.StatusOK},
		{"/v1/admin/orgs/acme%2Fx/installs", http.StatusUnauthorized},
		{"/v1/admin/orgs/acme%2F..%2Fglobex/installs", http.StatusUnauthorized},
		{"/v1/admin/orgs/%2E%2E/installs", http.StatusUnauthorized},
		{"/v1/admin/orgs/globex/installs", http.StatusUnauthorized},
		{"/v1/admin/orgs/acme/installs/%2E%2E/config", http.StatusForbidden},
		{"/v1/admin/orgs/acme/%2E%2E/globex/installs", http.StatusForbidden},
		{"/v1/admin/orgs/acme/installs/x/health/../../../config", http.StatusForbidden},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, adminRequest(http.MethodGet, c.path, "", partner))
		if w.Code != c.want {
			t.Errorf("GET %s = %d %s, want %d", c.path, w.Code, w.Body.String(), c.want)
		}
	}
	if !slices.Equal(reached, []string{"acme", "acme"}) {
		t.Fatalf("handlers reached for %q", reached)
	}
}

func TestPartnerRevocationAndRotation(t *testing.T) {
	server, _, first := partnerServer(t)
	installs := func(credential string) int {
		return serveAdmin(t, server, adminRequest(http.MethodGet, "/v1/admin/orgs/acme/installs", "", credential)).Code
	}
	if got := installs(first); got != http.StatusOK {
		t.Fatalf("minted credential = %d", got)
	}
	second := mintPartner(t, server, "acme")
	if installs(first) != http.StatusUnauthorized || installs(second) != http.StatusOK {
		t.Fatalf("after rotation: old = %d, new = %d", installs(first), installs(second))
	}
	if w := serveAdmin(t, server, adminRequest(http.MethodPost, "/v1/admin/orgs/acme/partner/revoke", "", testAdminCredential)); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", w.Code, w.Body.String())
	}
	if got := installs(second); got != http.StatusUnauthorized {
		t.Fatalf("revoked credential = %d", got)
	}
	w := serveAdmin(t, server, adminRequest(http.MethodGet, "/v1/admin/orgs/acme/partner", "", testAdminCredential))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"revoked_at"`) || strings.Contains(w.Body.String(), "digest") {
		t.Fatalf("partner status = %d %s", w.Code, w.Body.String())
	}
	third := mintPartner(t, server, "acme")
	if installs(third) != http.StatusOK || installs(second) != http.StatusUnauthorized {
		t.Fatal("a mint after revocation did not restore exactly the new credential")
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v1/admin/orgs/globex/partner"},
		{http.MethodPost, "/v1/admin/orgs/globex/partner/revoke"},
	} {
		if w := serveAdmin(t, server, adminRequest(route.method, route.path, "", testAdminCredential)); w.Code != http.StatusNotFound {
			t.Fatalf("%s %s with nothing minted = %d", route.method, route.path, w.Code)
		}
	}
}

// An invented credential is verified, not admitted by its prefix. Only on the deployment routes,
// where there is no organization to verify against, is it refused unread, as a reporter's is.
func TestAnInventedPartnerCredentialIsVerified(t *testing.T) {
	server, _, _ := partnerServer(t)
	const invented = "fmp1.CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	if w := serveAdmin(t, server, adminRequest(http.MethodGet, "/v1/admin/orgs/acme/installs", "", invented)); w.Code != http.StatusUnauthorized {
		t.Fatalf("organization route = %d", w.Code)
	}
	if w := serveAdmin(t, server, adminRequest(http.MethodGet, "/v1/admin/orgs", "", invented)); w.Code != http.StatusForbidden {
		t.Fatalf("deployment route = %d", w.Code)
	}
}

func TestOnlyAnAdministratorMintsOrRevokesAPartner(t *testing.T) {
	server, store, partner := partnerServer(t)
	storeReporterCredential(t, store)
	for _, credential := range []string{partner, testReporterCredential} {
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/v1/admin/orgs/acme/partner"},
			{http.MethodPost, "/v1/admin/orgs/acme/partner"},
			{http.MethodPost, "/v1/admin/orgs/acme/partner/revoke"},
		} {
			if w := serveAdmin(t, server, adminRequest(route.method, route.path, "", credential)); w.Code != http.StatusForbidden {
				t.Errorf("%s %s with %.5s = %d", route.method, route.path, credential, w.Code)
			}
		}
	}
}

// A partner reports health for its own organization, so the organization need not hand it the
// reporter credential, which covers the whole deployment.
func TestAPartnerReportsHealthForItsOwnOrganization(t *testing.T) {
	server, _, partner := partnerServer(t)
	installID := enrolledInstall(t, server.manager.mustOrg(t, "acme"))
	if got := report(t, server, partner, installID, `{"faults":[{"kind":"tick_failed"}]}`); got.Code != http.StatusNoContent {
		t.Fatalf("own organization = %d %s", got.Code, got.Body.String())
	}
	request := adminRequest(http.MethodPost, "/v1/admin/orgs/globex/installs/"+installID+"/health", `{"faults":[]}`, partner)
	if w := serveAdmin(t, server, request); w.Code != http.StatusUnauthorized {
		t.Fatalf("other organization = %d", w.Code)
	}
}
