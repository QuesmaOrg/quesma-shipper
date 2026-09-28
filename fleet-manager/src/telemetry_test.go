package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// quesmaTelemetryCollectorURL is the bare hostname Quesma's own deployments set by name. It lives
// here rather than in the service, which knows no collector until an organization names one.
const quesmaTelemetryCollectorURL = "telemetry.quesma.com"

func setTelemetryCollectorURL(t *testing.T, manager *Manager, endpoint string) {
	t.Helper()
	cfg, version, err := manager.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg.TelemetryCollectorURL = &endpoint
	if err := manager.ApplyConfig(context.Background(), cfg, version); err != nil {
		t.Fatal(err)
	}
}

// Every test server carries a proxy, as a served process does; production never runs without one.
func attachTelemetryProxy(t *testing.T, s *Server) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s.telemetry = newTelemetryProxy(telemetryIdentity{FleetManagerID: uuid.NewString()}, key, 32, 60, 10)
	t.Cleanup(func() { s.telemetry.client.CloseIdleConnections() })
}

func telemetryTestProxy(t *testing.T, s *Server, collector *httptest.Server) {
	t.Helper()
	attachTelemetryProxy(t, s)
	if collector != nil {
		s.telemetry.client.Transport = collector.Client().Transport
	}
	s.logger = log.New(io.Discard, "", 0)
}

func telemetryBody(t *testing.T, manager *Manager) []byte {
	t.Helper()
	body, err := json.Marshal(telemetryRequest{Schema: 1, BatchID: uuid.NewString(), IssuedAt: manager.time(), Payload: json.RawMessage(`{"nested": [1, true, {"unknown-future-field":"ok"}], "organization":"forged"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func submitTelemetry(s *Server, key ed25519.PrivateKey, install string, body []byte) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, signedRequest("POST", telemetryPath, body, install, key, telemetryShipperPreamble))
	return response
}

func TestTelemetrySignedForwardingPreservesPayloadAndProvenance(t *testing.T) {
	s, manager, deviceKey, install := enrolledServer(t)
	body := telemetryBody(t, manager)
	var received atomic.Int32
	var collector *httptest.Server
	collector = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var forwarded forwardedTelemetry
		if err := strictDecode(raw, &forwarded); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		auth := r.Header.Get("Authorization")
		prefix := "Fleet-Telemetry key=" + s.telemetry.identity.KeyID + ",sig="
		if !strings.HasPrefix(auth, prefix) {
			t.Errorf("unexpected authorization: %q", auth)
		}
		sig, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, prefix))
		if !ed25519.Verify(s.telemetry.key.Public().(ed25519.PublicKey), append([]byte(telemetryFleetPreamble(r.URL)), raw...), sig) {
			t.Error("fleet signature invalid")
		}
		if forwarded.Organization != "acme" || forwarded.InstallID != install || forwarded.FleetManagerID != s.telemetry.identity.FleetManagerID || forwarded.Audience != collector.URL+telemetryPath {
			t.Errorf("wrong provenance: %+v", forwarded)
		}
		if !bytes.Equal(forwarded.ShipperEnvelope, body) {
			t.Error("original envelope was rewritten")
		}
		if forwarded.ForwardedAt != manager.time() {
			t.Error("wrong forwarding clock")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer collector.Close()
	telemetryTestProxy(t, s, collector)
	setTelemetryCollectorURL(t, manager, collector.URL+telemetryPath)
	response := submitTelemetry(s, deviceKey, install, body)
	if response.Code != http.StatusNoContent || received.Load() != 1 {
		t.Fatalf("response=%d %s, calls=%d", response.Code, response.Body, received.Load())
	}
}

func TestTelemetryDisabled(t *testing.T) {
	s, manager, key, install := enrolledServer(t)
	body := telemetryBody(t, manager)
	setTelemetryCollectorURL(t, manager, "")
	if w := submitTelemetry(s, key, install, body); w.Code != 403 || !strings.Contains(w.Body.String(), "telemetry_disabled") {
		t.Fatalf("disabled: %d %s", w.Code, w.Body)
	}
	var calls atomic.Int32
	collector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer collector.Close()
	telemetryTestProxy(t, s, collector)
	if w := submitTelemetry(s, key, install, body); w.Code != 403 || calls.Load() != 0 {
		t.Fatal("disabled telemetry reached collector")
	}
	setTelemetryCollectorURL(t, manager, collector.URL)
	if w := submitTelemetry(s, key, install, body); w.Code != 204 {
		t.Fatalf("re-enabled: %d %s", w.Code, w.Body)
	}
	setTelemetryCollectorURL(t, manager, "")
	if w := submitTelemetry(s, key, install, body); w.Code != 403 || calls.Load() != 1 {
		t.Fatal("cached shipper request bypassed disable")
	}
}

func TestTelemetryRejectsInvalidAndUnauthorizedRequests(t *testing.T) {
	for _, tc := range []struct {
		name     string
		change   func(*telemetryRequest)
		preamble string
		status   int
	}{
		{"old", func(r *telemetryRequest) { r.IssuedAt = r.IssuedAt.Add(-5*time.Minute - time.Second) }, telemetryShipperPreamble, 400},
		{"future", func(r *telemetryRequest) { r.IssuedAt = r.IssuedAt.Add(time.Minute + time.Second) }, telemetryShipperPreamble, 400},
		{"bad batch", func(r *telemetryRequest) { r.BatchID = "bad" }, telemetryShipperPreamble, 400},
		{"null payload", func(r *telemetryRequest) { r.Payload = nil }, telemetryShipperPreamble, 204},
		{"bad schema", func(r *telemetryRequest) { r.Schema = 2 }, telemetryShipperPreamble, 400},
		{"config signature", func(*telemetryRequest) {}, "", 401},
		{"upload signature", func(*telemetryRequest) {}, uploadAuthorizePreamble, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, manager, key, install := enrolledServer(t)
			collector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
			defer collector.Close()
			telemetryTestProxy(t, s, collector)
			setTelemetryCollectorURL(t, manager, collector.URL)
			var request telemetryRequest
			_ = json.Unmarshal(telemetryBody(t, manager), &request)
			tc.change(&request)
			raw, _ := json.Marshal(request)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, signedRequest("POST", telemetryPath, raw, install, key, tc.preamble))
			if w.Code != tc.status {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
	s, manager, key, install := enrolledServer(t)
	telemetryTestProxy(t, s, nil)
	// Telemetry is off until an organization names a collector, and that refusal comes before the
	// body is read. These cases are about the body, so give the organization somewhere to forward
	// to -- nothing reaches it, because each request is rejected first.
	setTelemetryCollectorURL(t, manager, "https://collector.invalid/events")
	for _, body := range [][]byte{[]byte(`{"schema":1}`), []byte(`{"schema":1,"identity":"spoof"}`), []byte(`{} {}`), []byte(`garbage`)} {
		if w := submitTelemetry(s, key, install, body); w.Code != 400 {
			t.Fatalf("invalid request: %d %s", w.Code, w.Body)
		}
	}
	body := telemetryBody(t, manager)
	if w := submitTelemetry(s, key, uuid.NewString(), body); w.Code != 401 {
		t.Fatalf("unknown install: %d", w.Code)
	}
	tampered := signedRequest("POST", telemetryPath, body, install, key, telemetryShipperPreamble)
	tampered.Body = io.NopCloser(strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, tampered)
	if w.Code != 401 {
		t.Fatalf("tampered: %d", w.Code)
	}
	if err := manager.RevokeInstall(context.Background(), install); err != nil {
		t.Fatal(err)
	}
	if w := submitTelemetry(s, key, install, body); w.Code != 403 {
		t.Fatalf("revoked: %d", w.Code)
	}
}

func TestTelemetryBoundsAndUpstreamFailures(t *testing.T) {
	for _, status := range []int{204, 302, 400, 401, 403, 409, 413, 422, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s, manager, key, install := enrolledServer(t)
			var calls atomic.Int32
			collector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "secret upstream response")
			}))
			defer collector.Close()
			telemetryTestProxy(t, s, collector)
			setTelemetryCollectorURL(t, manager, collector.URL)
			w := submitTelemetry(s, key, install, telemetryBody(t, manager))
			want := 502
			if status == 204 || status == 400 || status == 409 || status == 413 || status == 422 {
				want = status
			}
			if w.Code != want || calls.Load() != 1 || strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("status=%d body=%s calls=%d", w.Code, w.Body, calls.Load())
			}
		})
	}
	s, manager, key, install := enrolledServer(t)
	telemetryTestProxy(t, s, nil)
	if w := submitTelemetry(s, key, install, bytes.Repeat([]byte("x"), requestLimit+1)); w.Code != 413 {
		t.Fatalf("oversize: %d", w.Code)
	}
	s.telemetry.slots = make(chan struct{}, 1)
	s.telemetry.slots <- struct{}{}
	if w := submitTelemetry(s, key, install, telemetryBody(t, manager)); w.Code != 429 {
		t.Fatalf("concurrency: %d", w.Code)
	}
	<-s.telemetry.slots
	collector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer collector.Close()
	s.telemetry.client.Transport = collector.Client().Transport
	s.telemetry.timeout = 20 * time.Millisecond
	setTelemetryCollectorURL(t, manager, collector.URL)
	if w := submitTelemetry(s, key, install, telemetryBody(t, manager)); w.Code != 504 {
		t.Fatalf("timeout: %d %s", w.Code, w.Body)
	}
}

func TestTelemetryRateLimitIsBoundedAndConcurrent(t *testing.T) {
	s, _, _, _ := enrolledServer(t)
	telemetryTestProxy(t, s, nil)
	p := s.telemetry
	now := time.Now()
	var allowed atomic.Int32
	var group sync.WaitGroup
	for range 50 {
		group.Add(1)
		go func() {
			defer group.Done()
			if p.allow("acme/install", now) {
				allowed.Add(1)
			}
		}()
	}
	group.Wait()
	if allowed.Load() != 10 {
		t.Fatalf("burst allowed %d", allowed.Load())
	}
	if !p.allow("acme/install", now.Add(time.Second)) || p.allow("acme/install", now.Add(time.Second)) {
		t.Fatal("refill wrong")
	}
	if !p.allow("other/install", now) {
		t.Fatal("organizations share quota")
	}
	for i := len(p.buckets); i < 10000; i++ {
		p.buckets[uuid.NewString()] = telemetryBucket{updated: now}
	}
	if p.allow("overflow", now) || len(p.buckets) != 10000 {
		t.Fatal("limiter memory is unbounded")
	}
	if !p.allow("after expiry", now.Add(time.Hour)) || len(p.buckets) > 2 {
		t.Fatal("idle limiter entries not reclaimed")
	}
}

func TestTelemetryCollectorURLValidation(t *testing.T) {
	for _, input := range []string{"telemetry.quesma.com", "https://collector.example/custom", "https://10.0.0.2:8443/events", ""} {
		if _, err := normalizeTelemetryCollectorURL(input); err != nil {
			t.Errorf("%q: %v", input, err)
		}
	}
	for _, input := range []string{"http://collector.example", "https://user:password@collector.example", "https://collector.example?secret=x", "https://collector.example?", "https://collector.example#", "https://collector.example#fragment", " https://collector.example", "collector.example/path", "https:///path"} {
		if _, err := normalizeTelemetryCollectorURL(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
	if got, err := normalizeTelemetryCollectorURL(quesmaTelemetryCollectorURL); err != nil || got != "https://telemetry.quesma.com/v1/telemetry" {
		t.Fatalf("default=%s %v", got, err)
	}
}

func TestTelemetryIdentityConfiguration(t *testing.T) {
	for _, name := range []string{"FLEET_MANAGER_TELEMETRY_CONCURRENCY", "FLEET_MANAGER_TELEMETRY_RATE", "FLEET_MANAGER_TELEMETRY_BURST"} {
		t.Setenv(name, "")
	}
	manager, _ := testManager(t)
	p, err := telemetryForManager(context.Background(), manager)
	if err != nil {
		t.Fatal(err)
	}
	again, err := telemetryForManager(context.Background(), manager)
	if err != nil || p.identity != again.identity {
		t.Fatalf("identity changed across startup: %v", err)
	}
	t.Setenv("FLEET_MANAGER_TELEMETRY_RATE", "0")
	if _, err := telemetryForManager(context.Background(), manager); err == nil {
		t.Fatal("zero rate accepted")
	}
}

func TestTelemetryOrganizationAPIAndServedConfiguration(t *testing.T) {
	s, _ := testAdminServer(t)
	path := "/v1/admin/orgs/acme/config"
	read := func() *httptest.ResponseRecorder {
		return serveAdmin(t, s, adminRequest("GET", path, "", testAdminCredential))
	}
	initial := read()
	// An organization that never named a collector forwards nowhere. The field is still reported,
	// so the administration UI shows the effective value rather than leaving it to be guessed.
	if !strings.Contains(initial.Body.String(), `"telemetry_collector_url":""`) {
		t.Fatalf("a fresh organization must report no collector: %s", initial.Body)
	}
	for _, endpoint := range []string{"", "https://private.example/events"} {
		get := read()
		var body map[string]any
		_ = json.Unmarshal([]byte(configJSON(t, twoRecipients(t))), &body)
		body["telemetry_collector_url"] = endpoint
		raw, _ := json.Marshal(body)
		req := adminRequest("PUT", path, string(raw), testAdminCredential)
		req.Header.Set("If-Match", get.Header().Get("ETag"))
		if w := serveAdmin(t, s, req); w.Code != 204 {
			t.Fatalf("update: %d %s", w.Code, w.Body)
		}
		cfg := read()
		var result FleetConfig
		_ = json.Unmarshal(cfg.Body.Bytes(), &result)
		if result.TelemetryCollectorURL == nil || *result.TelemetryCollectorURL != endpoint {
			t.Fatal("setting lost")
		}
		req = adminRequest("PUT", path, configJSON(t, twoRecipients(t)), testAdminCredential)
		req.Header.Set("If-Match", cfg.Header().Get("ETag"))
		if w := serveAdmin(t, s, req); w.Code != 204 {
			t.Fatalf("omitted update: %d %s", w.Code, w.Body)
		}
		_ = json.Unmarshal(read().Body.Bytes(), &result)
		if *result.TelemetryCollectorURL != endpoint {
			t.Fatal("UI save reset setting")
		}
		req = adminRequest("PUT", path, string(raw), testAdminCredential)
		req.Header.Set("If-Match", get.Header().Get("ETag"))
		if w := serveAdmin(t, s, req); w.Code != 412 {
			t.Fatalf("stale ETag: %d", w.Code)
		}
	}
	shipper, manager, key, install := enrolledServer(t)
	for _, endpoint := range []string{quesmaTelemetryCollectorURL, "", "https://collector.example/private"} {
		setTelemetryCollectorURL(t, manager, endpoint)
		w := httptest.NewRecorder()
		request := signedRequest("POST", "/v1/config", []byte(`{"agent_version":"test","config_versions":[1]}`), install, key, "")
		shipper.Handler().ServeHTTP(w, request)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var response configResponse
		_ = json.Unmarshal(w.Body.Bytes(), &response)
		var doc map[string]any
		if err := yaml.Unmarshal(response.Config, &doc); err != nil {
			t.Fatal(err)
		}
		want := "/v1/telemetry"
		if endpoint == "" {
			want = ""
		}
		if doc["telemetry_endpoint"] != want {
			t.Fatalf("served %v", doc["telemetry_endpoint"])
		}
	}
}

func TestTelemetryOrganizationIsolationAndDestinationRouting(t *testing.T) {
	s, manager, key, install := enrolledServer(t)
	received := make(chan string, 2)
	collector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope forwardedTelemetry
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Error(err)
		}
		received <- envelope.Organization + ":" + r.URL.Path
		w.WriteHeader(204)
	}))
	defer collector.Close()
	telemetryTestProxy(t, s, collector)
	setTelemetryCollectorURL(t, manager, collector.URL+"/acme")
	other, _ := manager.ForOrganization("other")
	otherURL := collector.URL + "/other"
	if err := other.Init(context.Background(), FleetConfig{AgeRecipients: twoRecipients(t), IncludeInstallRecipient: true, TelemetryCollectorURL: &otherURL}); err != nil {
		t.Fatal(err)
	}
	body := telemetryBody(t, manager)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, signedRequestForOrg("POST", telemetryPath, body, "other", install, key, telemetryShipperPreamble))
	if w.Code != 401 || len(received) != 0 {
		t.Fatal("shipper crossed organization boundary")
	}
	rec, err := manager.LoadActiveInstall(context.Background(), install)
	if err != nil {
		t.Fatal(err)
	}
	rec.Organization = "other"
	if err := createRecord(context.Background(), other.store, installKey("other", install), rec); err != nil {
		t.Fatal(err)
	}
	w = submitTelemetry(s, key, install, body)
	if w.Code != 204 {
		t.Fatalf("acme: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, signedRequestForOrg("POST", telemetryPath, body, "other", install, key, telemetryShipperPreamble))
	if w.Code != 204 {
		t.Fatalf("other: %d %s", w.Code, w.Body)
	}
	if <-received != "acme:/acme" || <-received != "other:/other" {
		t.Fatal("destination chosen outside authenticated organization")
	}
}

func TestTelemetryPublicKeyExportsOnlyRegistration(t *testing.T) {
	s, _, _, _ := enrolledServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/telemetry/public-key", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("public key: %d %s", w.Code, w.Body)
	}
	var record telemetryRegistration
	if err := strictDecode(w.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record != s.telemetry.identity {
		t.Fatal("public registration does not match active signer")
	}
	if bytes.Contains(w.Body.Bytes(), []byte(base64.StdEncoding.EncodeToString(s.telemetry.key.Seed()))) {
		t.Fatal("private seed leaked")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("public key must not be cached across rotation")
	}
}
