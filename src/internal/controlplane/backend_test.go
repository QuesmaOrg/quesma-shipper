package controlplane_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// fakePlane is a control plane that behaves exactly as configured, including badly: the cases
// worth testing in a client's refusals are all servers that are wrong in a specific way.
type fakePlane struct {
	t *testing.T

	// config is the served document. Replaced between requests to simulate a config push.
	config string

	expiresAt time.Time

	// versionConflict answers 409, the "no config this client can execute" case.
	versionConflict bool

	// refuseCredentials answers 403, the revoked-install case.
	refuseCredentials bool

	enrollOrg string

	// counters, so a test can assert on traffic rather than on logs.
	configCalls int

	// lastReq is what the client posted, so the request body can be checked.
	lastReq controlplane.ConfigRequest

	// lastAuth is the Authorization header, so request signing can be checked.
	lastAuth string

	// headersByPath records each endpoint's request headers, so "every request carries the
	// client facts" is checkable per endpoint rather than on whichever came last.
	headersByPath map[string]http.Header
}

func newPlane(t *testing.T) *fakePlane {
	t.Helper()
	return &fakePlane{
		t:             t,
		headersByPath: map[string]http.Header{},
		config:        "org: acme\n",
		expiresAt:     time.Now().Add(24 * time.Hour),
		enrollOrg:     "acme",
	}
}

func (p *fakePlane) start() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/enroll", func(w http.ResponseWriter, r *http.Request) {
		p.headersByPath[r.URL.Path] = r.Header.Clone()
		var req controlplane.EnrollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.AgeRecipient == "" {
			http.Error(w, "no age recipient", http.StatusBadRequest)
			return
		}
		writeJSON(w, controlplane.EnrollResponse{Organization: p.enrollOrg})
	})

	mux.HandleFunc("/v1/config", func(w http.ResponseWriter, r *http.Request) {
		p.configCalls++
		p.lastAuth = r.Header.Get("Authorization")
		p.headersByPath[r.URL.Path] = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&p.lastReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if p.refuseCredentials {
			http.Error(w, "revoked", http.StatusForbidden)
			return
		}
		if p.versionConflict {
			http.Error(w, "this service serves config_version 1 only", http.StatusConflict)
			return
		}
		writeJSON(w, controlplane.ConfigResponse{
			Config:    []byte(p.config),
			ExpiresAt: p.expiresAt,
		})
	})

	srv := httptest.NewServer(mux)
	p.t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// enrolled returns a record pointing at the plane, as `shipper enroll` would have written it.
func (p *fakePlane) enrolled(t *testing.T, endpoint, installID string) *controlplane.Enrollment {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &controlplane.Enrollment{
		InstallID:    installID,
		Organization: p.enrollOrg,
		Endpoint:     endpoint,
		DeviceKey:    base64.StdEncoding.EncodeToString(priv),
		EnrolledAt:   controlplane.Now(),
	}
}

func (p *fakePlane) client(t *testing.T, endpoint, install string) *controlplane.Client {
	t.Helper()
	e := p.enrolled(t, endpoint, install)
	priv, err := e.PrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	c, err := controlplane.New(controlplane.Options{
		Endpoint:     endpoint,
		InstallID:    install,
		Organization: e.Organization,
		DeviceKey:    priv,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const installID = "0f5a6b3c-1d2e-4f60-8a9b-1c2d3e4f5061"

func TestEnrollStoresWhatTheServerAssigns(t *testing.T) {
	p := newPlane(t)
	srv := p.start()

	c, err := controlplane.New(controlplane.Options{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Enroll(context.Background(), controlplane.EnrollRequest{
		InstallID:       installID,
		DevicePublicKey: "device-pub",
		AgeRecipient:    "age1recipient",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Organization is server-assigned: an install must not place itself in another org's subtree.
	if resp.Organization != "acme" {
		t.Errorf("organization = %q, want acme", resp.Organization)
	}
}

// A document the client cannot parse is refused whole rather than partly applied.
func TestFetchConfigRefusesAnUnparseableDocument(t *testing.T) {
	p := newPlane(t)
	p.config = "org: [acme\n"
	srv := p.start()

	c := p.client(t, srv.URL, installID)
	if _, err := c.FetchConfig(context.Background(), controlplane.ConfigRequest{}); err == nil {
		t.Fatal("accepted a served config that does not parse")
	}
}

func TestFetchConfigSurfacesAVersionConflict(t *testing.T) {
	p := newPlane(t)
	p.versionConflict = true
	srv := p.start()

	c := p.client(t, srv.URL, installID)
	_, err := c.FetchConfig(context.Background(), controlplane.ConfigRequest{AgentVersion: "0.1.0"})
	// A 409 has to be distinguishable from a transport failure: the operator's next step is
	// to upgrade the client, not to check the network.
	if !errors.Is(err, controlplane.ErrUnsupportedVersion) {
		t.Fatalf("want ErrUnsupportedVersion, got %v", err)
	}
}

func TestConfigFetchIsSignedAndCarriesTheRequest(t *testing.T) {
	p := newPlane(t)
	srv := p.start()

	c := p.client(t, srv.URL, installID)
	req := controlplane.ConfigRequest{AgentVersion: "0.1.0", ConfigVersions: []int{1}}
	if _, err := c.FetchConfig(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.lastAuth, "Shipper-Device org=acme, install="+installID) {
		t.Errorf("request was not signed as this install: %q", p.lastAuth)
	}
	if p.lastReq.AgentVersion != "0.1.0" || len(p.lastReq.ConfigVersions) != 1 {
		t.Errorf("the config request did not reach the server: %+v", p.lastReq)
	}
}

func TestEveryRequestCarriesTheClientVersionHeader(t *testing.T) {
	p := newPlane(t)
	srv := p.start()

	// One call to each endpoint the client has. If an endpoint is ever added and misses the
	// header, it is post() that must have been bypassed, which is the actual bug.
	c, err := controlplane.New(controlplane.Options{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enroll(context.Background(), controlplane.EnrollRequest{
		InstallID:       installID,
		DevicePublicKey: "device-pub",
		AgeRecipient:    "age1recipient",
	}); err != nil {
		t.Fatal(err)
	}

	ec := p.client(t, srv.URL, installID)
	if _, err := ec.FetchConfig(context.Background(), controlplane.ConfigRequest{}); err != nil {
		t.Fatal(err)
	}

	// The exact string buildinfo reports, not merely something non-empty: a header that
	// disagreed with the version in the /config config request body could not be joined with it.
	// All facts share post(), so every endpoint must agree with the probes — including on
	// absence, where a probe reporting "" must leave the header unset.
	want := map[string]string{
		controlplane.VersionHeader: platform.Current().String(),
		controlplane.OSHeader:      platform.OSVersion(),
		controlplane.BootHeader:    platform.BootTime(),
	}
	for _, path := range []string{"/v1/enroll", "/v1/config"} {
		for header, w := range want {
			if got := p.headersByPath[path].Get(header); got != w {
				t.Errorf("%s: %s = %q, want %q", path, header, got, w)
			}
		}
	}
}

func TestEnrollmentRecordRoundTripsAndRefusesLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	dir := t.TempDir()
	p := newPlane(t)
	rec := p.enrolled(t, "https://plane.example", installID)
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}

	got, err := controlplane.LoadEnrollment(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Organization != rec.Organization || got.DeviceKey != rec.DeviceKey {
		t.Errorf("record did not round-trip: %+v", got)
	}
	if _, err := got.PrivateKey(); err != nil {
		t.Errorf("device key did not decode: %v", err)
	}

	// It holds a private signing key: a group-readable one is a finding, and the operator has to
	// know it was exposed rather than have it repaired silently.
	if err := os.Chmod(filepath.Join(dir, controlplane.EnrollmentFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := controlplane.LoadEnrollment(dir); err == nil {
		t.Fatal("loaded an enrollment record readable by everyone")
	}
}

func TestMissingEnrollmentIsNotAnError(t *testing.T) {
	// Standalone is a supported deployment, not a degraded one: LoadEnrollment must report
	// plain os.ErrNotExist so callers can tell "no backend" from "broken backend".
	_, err := controlplane.LoadEnrollment(t.TempDir())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", err)
	}
}

func TestEnrollmentSchemaMismatchIsRefused(t *testing.T) {
	dir := t.TempDir()
	body := fmt.Sprintf(`{"enrollment_schema":99,"install_id":%q,"organization":"acme",`+
		`"endpoint":"https://x","device_key":"","enrolled_at":"now"}`, installID)
	if err := os.WriteFile(filepath.Join(dir, controlplane.EnrollmentFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := controlplane.LoadEnrollment(dir); err == nil {
		t.Fatal("accepted a record written by a version this client does not speak")
	}
}

// Every historical schema is a frozen fixture in testdata/ that must load through the migration
// ladder: a fleet on auto-update meets old records routinely, never by hand-editing JSON.
func TestHistoricalEnrollmentSchemasMigrateOnLoad(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("testdata", "enrollment-schema-*.json"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no enrollment fixtures found: %v", err)
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			dir := t.TempDir()
			raw, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, controlplane.EnrollmentFile)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}

			e, err := controlplane.LoadEnrollment(dir)
			if err != nil {
				t.Fatalf("a known historical schema was refused: %v", err)
			}
			// The identity material must survive verbatim: a migration that loses the
			// device key silently re-keys the install.
			if e.InstallID != "0f5a6b3c-1d2e-4f60-8a9b-1c2d3e4f5061" ||
				e.Organization != "acme" || e.Endpoint != "https://cp.example.com" ||
				e.EnrolledAt == "" {
				t.Errorf("migrated record lost fields: %+v", e)
			}
			if _, err := e.PrivateKey(); err != nil {
				t.Errorf("device key did not survive migration: %v", err)
			}

			// The upgrade is persisted once, at the current schema, still private.
			persisted, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var onDisk map[string]any
			if err := json.Unmarshal(persisted, &onDisk); err != nil {
				t.Fatalf("persisted record is not JSON: %v", err)
			}
			if _, stale := onDisk["sink"]; stale {
				t.Error("persisted record still carries the schema-1 sink grant")
			}
			if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
				t.Errorf("persisted record is not private: %v %v", info.Mode(), err)
			}
			again, err := controlplane.LoadEnrollment(dir)
			if err != nil {
				t.Fatalf("the persisted migration does not load back: %v", err)
			}
			if *again != *e {
				t.Errorf("second load differs from first: %+v vs %+v", again, e)
			}
		})
	}
}

// A refusal must be identifiable as one, not merely described in prose: the engine stops a whole
// run on it, so errors.Is has to survive the credential cache and the AWS SDK's operation wrapper.
func TestARefusedInstallIsAMatchableError(t *testing.T) {
	p := newPlane(t)
	p.refuseCredentials = true
	srv := p.start()

	c := p.client(t, srv.URL, installID)
	_, err := c.FetchConfig(context.Background(), controlplane.ConfigRequest{AgentVersion: "0.1.0"})

	if !errors.Is(err, formats.ErrCredentialsRefused) {
		t.Fatalf("want formats.ErrCredentialsRefused, got %v", err)
	}
	// And it still says which endpoint and which status, because that is what an operator
	// needs after they know what kind of failure it is.
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("the error does not mention the status: %v", err)
	}
}
