package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

// Quesma's own deployments: every organization that says nothing keeps what it had before the
// defaults moved out of the code.
var quesmaDefaults = OrganizationDefaults{AllowQuesmaETL: true, TelemetryCollectorURL: quesmaTelemetryCollectorURL}

func TestOrganizationDefaultsFromEnv(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	got, err := organizationDefaultsFromEnv(env(nil))
	if want := (OrganizationDefaults{AllowQuesmaETL: true}); err != nil || got != want || got != defaultOrganizationDefaults() {
		t.Fatalf("unset = %+v, %v; want %+v", got, err, want)
	}
	got, err = organizationDefaultsFromEnv(env(map[string]string{"FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL": "false"}))
	if err != nil || got != (OrganizationDefaults{}) {
		t.Fatalf("opted out = %+v, %v; want nothing on", got, err)
	}
	got, err = organizationDefaultsFromEnv(env(map[string]string{
		"FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL":        "true",
		"FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL": " telemetry.quesma.com ",
	}))
	if err != nil || got != quesmaDefaults {
		t.Fatalf("set = %+v, %v; want %+v", got, err, quesmaDefaults)
	}
	for name, value := range map[string]string{
		"FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL":        "yes please",
		"FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL": "http://collector.example",
	} {
		if _, err := organizationDefaultsFromEnv(env(map[string]string{name: value})); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: err = %v, want a refusal naming the variable", name, value, err)
		}
	}
}

// The defaults fill what an organization left unset, and nothing else: a stated false or "" is a
// choice, and the record keeps saying nothing, so changing the deployment's defaults later still
// reaches it.
func TestDeploymentDefaultsFillOnlyUnsetSettings(t *testing.T) {
	server, _ := testMultiTenantAdminServer(t)
	server.defaults = quesmaDefaults
	create := func(slug string, allow *bool, collector *string) {
		t.Helper()
		body, _ := json.Marshal(adminCreateOrganizationRequest{Slug: slug, DisplayName: slug, AgeRecipients: twoRecipients(t),
			AllowQuesmaETL: allow, TelemetryCollectorURL: collector})
		if w := serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs", string(body), testAdminCredential)); w.Code != http.StatusCreated {
			t.Fatalf("create %s = %d: %s", slug, w.Code, w.Body)
		}
	}
	create("silent", nil, nil)
	create("stated", ptr(false), ptr(""))

	read := func(slug string) (FleetConfig, string) {
		t.Helper()
		w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/"+slug+"/config", "", testAdminCredential))
		var cfg FleetConfig
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &cfg) != nil {
			t.Fatalf("read %s = %d: %s", slug, w.Code, w.Body)
		}
		return cfg, w.Header().Get("ETag")
	}
	for slug, want := range map[string]OrganizationDefaults{"silent": quesmaDefaults, "stated": {}} {
		cfg, _ := read(slug)
		if *cfg.AllowQuesmaETL != want.AllowQuesmaETL || *cfg.TelemetryCollectorURL != want.TelemetryCollectorURL {
			t.Errorf("%s reads allow_quesma_etl=%v telemetry_collector_url=%q, want %+v", slug, *cfg.AllowQuesmaETL, *cfg.TelemetryCollectorURL, want)
		}
	}

	stored := func() FleetConfig {
		t.Helper()
		manager, _ := server.manager.ForOrganization("silent")
		cfg, _, err := manager.LoadConfig(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	if cfg := stored(); cfg.AllowQuesmaETL != nil || cfg.TelemetryCollectorURL != nil {
		t.Fatalf("defaults were written into the record: %+v", cfg)
	}
	// A write that names neither setting leaves both unset. (A client that writes back what it read
	// states the effective values instead, which is what that client showed and chose to keep.)
	cfg, etag := read("silent")
	body, _ := json.Marshal(adminConfigRequest{AgeRecipients: cfg.AgeRecipients, AuthoredYAML: "mode: daemon"})
	req := adminRequest("PUT", "/v1/admin/orgs/silent/config", string(body), testAdminCredential)
	req.Header.Set("If-Match", etag)
	if w := serveAdmin(t, server, req); w.Code != http.StatusNoContent {
		t.Fatalf("update = %d: %s", w.Code, w.Body)
	}
	if cfg := stored(); cfg.AllowQuesmaETL != nil || cfg.TelemetryCollectorURL != nil {
		t.Fatalf("an update that named neither setting stated them: %+v", cfg)
	}
}

func TestAdminDefaultsEndpointNeedsTheCredential(t *testing.T) {
	server, _ := testMultiTenantAdminServer(t)
	server.defaults = quesmaDefaults
	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/defaults", "", testAdminCredential))
	var got OrganizationDefaults
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got != quesmaDefaults {
		t.Fatalf("defaults = %d %s, want %+v", w.Code, w.Body, quesmaDefaults)
	}
	if w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/defaults", "", "")); w.Code != http.StatusUnauthorized {
		t.Fatalf("without the credential = %d", w.Code)
	}
}

// What a shipper is served is where the defaults matter: the recipient it seals to and whether it
// may post telemetry. enrolledServer's organization states neither setting.
func TestServedConfigurationFollowsDeploymentDefaults(t *testing.T) {
	for _, tc := range []struct {
		name      string
		defaults  OrganizationDefaults
		recipient bool
		endpoint  string
	}{
		{"a deployment that states nothing", defaultOrganizationDefaults(), true, ""},
		{"a deployment that opted out", OrganizationDefaults{}, false, ""},
		{"Quesma's deployments", quesmaDefaults, true, telemetryPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shipper, _, key, install := enrolledServer(t)
			shipper.defaults = tc.defaults
			w := httptest.NewRecorder()
			shipper.Handler().ServeHTTP(w, signedRequest("POST", "/v1/config", []byte(`{"agent_version":"test","config_versions":[1]}`), install, key, ""))
			if w.Code != http.StatusOK {
				t.Fatal(w.Body.String())
			}
			var response configResponse
			_ = json.Unmarshal(w.Body.Bytes(), &response)
			var doc map[string]any
			if err := yaml.Unmarshal(response.Config, &doc); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(response.Config), quesmaETLAgeRecipient); got != tc.recipient {
				t.Errorf("Quesma recipient served = %v, want %v", got, tc.recipient)
			}
			if doc["telemetry_endpoint"] != tc.endpoint {
				t.Errorf("telemetry_endpoint = %v, want %q", doc["telemetry_endpoint"], tc.endpoint)
			}
		})
	}
}

// Forwarding reads the collector the same way: an organization that names none goes where the
// deployment says, and an organization that stated "" stays off whatever the deployment says.
func TestTelemetryForwardsToTheDeploymentDefaultCollector(t *testing.T) {
	s, manager, key, install := enrolledServer(t)
	var calls atomic.Int32
	collector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusAccepted) }))
	defer collector.Close()
	telemetryTestProxy(t, s, collector)
	body := telemetryBody(t, manager)
	if w := submitTelemetry(s, key, install, body); w.Code != http.StatusForbidden || calls.Load() != 0 {
		t.Fatalf("no default collector: %d %s, calls=%d", w.Code, w.Body, calls.Load())
	}
	s.defaults = OrganizationDefaults{TelemetryCollectorURL: collector.URL}
	if w := submitTelemetry(s, key, install, telemetryBody(t, manager)); w.Code != http.StatusNoContent || calls.Load() != 1 {
		t.Fatalf("default collector: %d %s, calls=%d", w.Code, w.Body, calls.Load())
	}
	setTelemetryCollectorURL(t, manager, "")
	if w := submitTelemetry(s, key, install, telemetryBody(t, manager)); w.Code != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("stated off: %d %s, calls=%d", w.Code, w.Body, calls.Load())
	}
}
