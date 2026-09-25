package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testReporterCredential = "fmr1.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"

func storeReporterCredential(t *testing.T, store *memoryStore) {
	t.Helper()
	digest := sha256.Sum256([]byte(testReporterCredential))
	rec := AdminCredentialRecord{Schema: schemaVersion, SecretDigest: hex.EncodeToString(digest[:])}
	raw, err := encodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), deploymentReporterCredentialKey(), raw); err != nil {
		t.Fatal(err)
	}
}

// initialisedManager is an organization ready to enrol into: testManager alone has no config, and
// enrollment needs the recipients.
func initialisedManager(t *testing.T) *Manager {
	t.Helper()
	manager, _ := testManager(t)
	if err := manager.Init(context.Background(), FleetConfig{
		AgeRecipients: testAgeRecipients(t, 2), IncludeInstallRecipient: true,
	}); err != nil {
		t.Fatal(err)
	}
	return manager
}

// enrolledInstall puts one active install in the organization, which is what a report needs to be
// about. Enrolment is the only way an install record comes to exist.
func enrolledInstall(t *testing.T, manager *Manager) string {
	t.Helper()
	installID, body := enrollBody(t, manager)
	server, err := NewServer(manager, fakeSigner{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("enroll: %d %s", recorder.Code, recorder.Body.String())
	}
	return installID
}

func TestHealthRoundTrips(t *testing.T) {
	manager := initialisedManager(t)
	installID := enrolledInstall(t, manager)

	err := manager.ReportHealth(context.Background(), installID, HealthReport{
		Consecutive: 3,
		Faults:      []Fault{{Kind: "tick_failed", Message: "upload: PUT object: refused", At: "2026-09-07T09:00:00Z", RunID: "r1"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	records, err := manager.ListHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("want one record, got %d", len(records))
	}
	rec := records[0]
	if rec.InstallID != installID || rec.Consecutive != 3 || len(rec.Faults) != 1 {
		t.Fatalf("record did not survive: %+v", rec)
	}
	if rec.ReportedAt.IsZero() {
		t.Fatal("no report time was stamped")
	}
}

// The health prefix must not become storage for anything the organization does not have.
func TestHealthForAnUnknownInstallIsRefused(t *testing.T) {
	manager := initialisedManager(t)
	err := manager.ReportHealth(context.Background(), "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		HealthReport{Faults: []Fault{{Kind: "tick_failed"}}})
	if err == nil {
		t.Fatal("a report for an unknown install was stored")
	}
}

// A revoked install's row is about to disappear; health on it would outlive what it describes.
func TestRevokedInstallsAreNotReportable(t *testing.T) {
	manager := initialisedManager(t)
	installID := enrolledInstall(t, manager)
	if err := manager.RevokeInstall(context.Background(), installID); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReportHealth(context.Background(), installID,
		HealthReport{Faults: []Fault{{Kind: "tick_failed"}}}); err == nil {
		t.Fatal("a revoked install accepted a report")
	}
}

// Twenty faults ride a heartbeat, but every write leaves a permanent noncurrent version.
func TestOnlyTheNewestFaultsAreKept(t *testing.T) {
	manager := initialisedManager(t)
	installID := enrolledInstall(t, manager)

	var faults []Fault
	for i := 0; i < 20; i++ {
		faults = append(faults, Fault{Kind: "tick_failed", Message: fmt.Sprintf("failure %d", i)})
	}
	if err := manager.ReportHealth(context.Background(), installID, HealthReport{Faults: faults}); err != nil {
		t.Fatal(err)
	}

	records, _ := manager.ListHealth(context.Background())
	kept := records[0].Faults
	if len(kept) != maxFaults {
		t.Fatalf("kept %d faults, want the %d cap", len(kept), maxFaults)
	}
	// The newest, not the oldest: an operator wants what just happened.
	if !strings.Contains(kept[len(kept)-1].Message, "failure 19") {
		t.Fatalf("the newest fault was dropped: %+v", kept)
	}
}

// This text was chosen by a machine nobody here administers, and it reaches a browser.
func TestFaultTextIsBounded(t *testing.T) {
	manager := initialisedManager(t)
	installID := enrolledInstall(t, manager)

	if err := manager.ReportHealth(context.Background(), installID, HealthReport{Faults: []Fault{
		{Kind: "tick_failed\x00\x1b[31m", Message: strings.Repeat("x", 5000) + "\n\rmore"},
	}}); err != nil {
		t.Fatal(err)
	}

	records, _ := manager.ListHealth(context.Background())
	got := records[0].Faults[0]
	if strings.ContainsAny(got.Kind, "\x00\x1b") {
		t.Fatalf("control characters survived in the kind: %q", got.Kind)
	}
	// Exactly the message cap, not the shorter bound the other client facts use: running the
	// message through clientFact first would make this constant dead and cut every cause at 200.
	if n := len([]rune(got.Message)); n != maxFaultMessage {
		t.Fatalf("message is %d runes, want the %d cap", n, maxFaultMessage)
	}
}

// reported_at is this service's clock and nobody else's: the UI decides "stale" from it, so a
// reporter with a skewed clock must not be able to keep obsolete health looking current.
func TestReportedAtIsTheServersClock(t *testing.T) {
	manager := initialisedManager(t)
	installID := enrolledInstall(t, manager)
	received := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return received }

	if err := manager.ReportHealth(context.Background(), installID,
		HealthReport{Faults: []Fault{{Kind: "tick_failed", At: "2099-01-01T00:00:00Z"}}}); err != nil {
		t.Fatal(err)
	}

	records, _ := manager.ListHealth(context.Background())
	if len(records) != 1 || !records[0].ReportedAt.Equal(received) {
		t.Fatalf("reported_at = %v, want the server's %v", records[0].ReportedAt, received)
	}
}

func TestAFaultWithoutAKindIsRejected(t *testing.T) {
	if err := ValidateHealthReport(HealthReport{Faults: []Fault{{Message: "something"}}}); err == nil {
		t.Fatal("a fault with no kind was accepted")
	}
	if err := ValidateHealthReport(HealthReport{Faults: []Fault{{Kind: "tick_failed"}}}); err != nil {
		t.Fatalf("a usable fault was rejected: %v", err)
	}
}

// --- over HTTP -------------------------------------------------------------------------------

func healthPath(installID string) string {
	return "/v1/admin/orgs/acme/installs/" + installID + "/health"
}

func report(t *testing.T, server *Server, credential, installID, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, healthPath(installID), strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+credential)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func TestHealthOverTheAdminAPI(t *testing.T) {
	server, store := testAdminServer(t)
	storeReporterCredential(t, store)
	root, _ := NewManager(store)
	acme, _ := root.ForOrganization("acme")
	installID := enrolledInstall(t, acme)

	body := `{"consecutive_failures":2,"faults":[{"kind":"crashed","message":"never exited","run_id":"r9"}]}`
	if got := report(t, server, testReporterCredential, installID, body); got.Code != http.StatusNoContent {
		t.Fatalf("report: %d %s", got.Code, got.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/admin/orgs/acme/installs/health", nil)
	request.Header.Set("Authorization", "Bearer "+testAdminCredential)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list: %d %s", recorder.Code, recorder.Body.String())
	}
	var records []HealthRecord
	if err := json.Unmarshal(recorder.Body.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Consecutive != 2 || records[0].Faults[0].Kind != "crashed" {
		t.Fatalf("what came back is not what went in: %+v", records)
	}
}

func TestReportingHealthForAnUnknownInstallIs404(t *testing.T) {
	server, store := testAdminServer(t)
	storeReporterCredential(t, store)

	got := report(t, server, testReporterCredential,
		"3f2504e0-4f89-41d3-9a0c-0305e82c3301", `{"faults":[{"kind":"tick_failed"}]}`)
	if got.Code != http.StatusNotFound {
		t.Fatalf("status = %d %s", got.Code, got.Body.String())
	}
}

func TestAKindlessFaultIsRejectedOverHTTP(t *testing.T) {
	server, store := testAdminServer(t)
	storeReporterCredential(t, store)
	root, _ := NewManager(store)
	acme, _ := root.ForOrganization("acme")
	installID := enrolledInstall(t, acme)

	got := report(t, server, testReporterCredential, installID, `{"faults":[{"message":"no kind"}]}`)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("status = %d %s", got.Code, got.Body.String())
	}
}

// The stored record's own fields are not the caller's to send. The decoder is strict, so a body
// carrying reported_at -- or the install id, or the schema -- is refused outright rather than
// accepted and overwritten, which would leave a client believing its timestamp counted.
func TestServerOwnedFieldsAreRefusedInAReport(t *testing.T) {
	server, store := testAdminServer(t)
	storeReporterCredential(t, store)
	root, _ := NewManager(store)
	acme, _ := root.ForOrganization("acme")
	installID := enrolledInstall(t, acme)

	for _, body := range []string{
		`{"reported_at":"2099-01-01T00:00:00Z","faults":[{"kind":"tick_failed"}]}`,
		`{"install_id":"someone-else","faults":[{"kind":"tick_failed"}]}`,
		`{"schema":1,"faults":[{"kind":"tick_failed"}]}`,
	} {
		got := report(t, server, testReporterCredential, installID, body)
		if got.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d %s, want 400", body, got.Code, got.Body.String())
		}
	}
	if records, _ := acme.ListHealth(context.Background()); len(records) != 0 {
		t.Fatalf("a refused report was stored: %+v", records)
	}
}

// --- the credential boundary -----------------------------------------------------------------

// The whole point of a second credential: it reports, and it can do nothing else. Every other
// administrative route must refuse it, including the ones that revoke.
func TestAReporterCredentialIsRefusedEverywhereElse(t *testing.T) {
	server, store := testAdminServer(t)
	storeReporterCredential(t, store)

	for _, route := range []struct{ method, path string }{
		// The organization and configuration routes are listed FIRST because they were the ones
		// missing: they carried no administrator check, so a reporter could read the config and its
		// ETag and then rewrite age_recipients -- which decides who can decrypt what is uploaded.
		{http.MethodGet, "/v1/admin/orgs"},
		{http.MethodPost, "/v1/admin/orgs"},
		{http.MethodGet, "/v1/admin/orgs/acme/config"},
		{http.MethodPut, "/v1/admin/orgs/acme/config"},
		{http.MethodGet, "/v1/admin/orgs/acme/installs"},
		{http.MethodGet, "/v1/admin/orgs/acme/installs/seen"},
		{http.MethodGet, "/v1/admin/orgs/acme/installs/health"},
		{http.MethodGet, "/v1/admin/orgs/acme/grants"},
		{http.MethodPost, "/v1/admin/orgs/acme/grants"},
		{http.MethodPost, "/v1/admin/orgs/acme/invites"},
		{http.MethodPost, "/v1/admin/orgs/acme/installs/3f2504e0-4f89-41d3-9a0c-0305e82c3301/revoke"},
	} {
		request := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer "+testReporterCredential)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", route.method, route.path, recorder.Code)
		}
	}
}

// The route a reporter may reach is decided before the mux sees the request, so it is decided on a
// path the mux has not normalised yet.
func TestOnlyTheHealthRouteAdmitsAReporter(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/v1/admin/orgs/acme/installs/i1/health", true},
		{http.MethodPost, "/v1/admin/orgs/acme/installs/i1/health/", true},
		{http.MethodGet, "/v1/admin/orgs/acme/installs/i1/health", false},
		{http.MethodPost, "/v1/admin/orgs/acme/config", false},
		{http.MethodPost, "/v1/admin/orgs", false},
		{http.MethodPost, "/v1/admin/orgs/acme/installs/i1/health/../../../config", false},
		{http.MethodPost, "/v1/admin/orgs/acme/installs/i1/revoke", false},
	} {
		request := httptest.NewRequest(c.method, c.path, nil)
		if got := reporterRoute(request); got != c.want {
			t.Errorf("reporterRoute(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// An administrator may report too: one credential fewer to hold when there is no separate reporter.
func TestTheAdminCredentialMayAlsoReport(t *testing.T) {
	server, store := testAdminServer(t)
	root, _ := NewManager(store)
	acme, _ := root.ForOrganization("acme")
	installID := enrolledInstall(t, acme)

	got := report(t, server, testAdminCredential, installID, `{"faults":[{"kind":"tick_failed"}]}`)
	if got.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s", got.Code, got.Body.String())
	}
}

// A deployment that provisioned no reporter credential authenticates none: the capability appears
// when an operator creates it, not before.
func TestNoReporterCredentialMeansNoReporter(t *testing.T) {
	server, _ := testAdminServer(t)

	got := report(t, server, testReporterCredential,
		"3f2504e0-4f89-41d3-9a0c-0305e82c3301", `{"faults":[{"kind":"tick_failed"}]}`)
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d %s, want 401", got.Code, got.Body.String())
	}
}
