package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestForceUploadAuthorizationIssuesTicketsWithoutProbing(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	raw, err := readProtocol("fixtures", "v3", "uploads-authorize", "request.json")
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		uploadAuthorizeRequest
		ForceResend bool `json:"force_resend"`
	}
	if err := strictDecode(raw, &req); err != nil {
		t.Fatal(err)
	}
	req.IssuedAt = manager.time()
	req.Objects[0].Key = strings.ReplaceAll(req.Objects[0].Key, "3f2504e0-4f89-41d3-9a0c-0305e82c3301", installID)
	store := manager.store.(*memoryStore)
	store.setStoredHashes(req.Objects[0].Key, StoredHashes{Source: req.Objects[0].SourceHash, Shipped: req.Objects[0].Metadata["shipped-hash"]})
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	validateSchema(t, "v3/uploads-authorize-request.schema.json", req)
	for range 2 {
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, signedRequest(http.MethodPost, "/v3/uploads/authorize", body, installID, key, forceUploadAuthorizePreamble))
		if w.Code != http.StatusOK {
			t.Fatalf("force authorization: %d %s", w.Code, w.Body.String())
		}
		var response uploadAuthorizeResponse
		if err := strictDecode(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		validateSchema(t, "v3/uploads-authorize-response.schema.json", response)
		if len(response.Tickets) != 1 || response.Tickets[0].AlreadyPresent || response.Tickets[0].URL == "" {
			t.Fatal("force did not issue a PUT ticket")
		}
	}
	store.mu.Lock()
	reads := store.hashReads
	store.mu.Unlock()
	if reads != 0 {
		t.Fatalf("force request probed stored hashes %d times", reads)
	}

	body, _ = json.Marshal(req.uploadAuthorizeRequest)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, signedRequest(http.MethodPost, "/v2/uploads/authorize", body, installID, key, uploadAuthorizePreamble))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"already_present":true`) {
		t.Fatalf("v2 dedup changed: %d %s", w.Code, w.Body.String())
	}
}

func TestForceUploadAuthorizationPreservesValidationAndAuthentication(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	raw, err := readProtocol("fixtures", "v3", "uploads-authorize", "request.json")
	if err != nil {
		t.Fatal(err)
	}
	base := strings.ReplaceAll(string(raw), "3f2504e0-4f89-41d3-9a0c-0305e82c3301", installID)
	base = strings.ReplaceAll(base, "2026-08-19T12:00:00Z", manager.time().Format(time.RFC3339))
	var withoutForce map[string]any
	if err := json.Unmarshal([]byte(base), &withoutForce); err != nil {
		t.Fatal(err)
	}
	delete(withoutForce, "force_resend")
	missingForce, err := json.Marshal(withoutForce)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body, path, prefix string
		status                   int
	}{
		{"missing force", string(missingForce), "/v3/uploads/authorize", forceUploadAuthorizePreamble, http.StatusBadRequest},
		{"false force", strings.ReplaceAll(base, `"force_resend": true`, `"force_resend": false`), "/v3/uploads/authorize", forceUploadAuthorizePreamble, http.StatusBadRequest},
		{"wrong type", strings.ReplaceAll(base, `"force_resend": true`, `"force_resend": "true"`), "/v3/uploads/authorize", forceUploadAuthorizePreamble, http.StatusBadRequest},
		{"other install", strings.ReplaceAll(base, "/install="+installID, "/install=3f2504e0-4f89-41d3-9a0c-0305e82c3301"), "/v3/uploads/authorize", forceUploadAuthorizePreamble, http.StatusBadRequest},
		{"other organization", strings.ReplaceAll(base, "organization=acme", "organization=other"), "/v3/uploads/authorize", forceUploadAuthorizePreamble, http.StatusBadRequest},
		{"stale", strings.ReplaceAll(base, manager.time().Format(time.RFC3339), manager.time().Add(-time.Hour).Format(time.RFC3339)), "/v3/uploads/authorize", forceUploadAuthorizePreamble, http.StatusBadRequest},
		{"v2 signature on v3", base, "/v3/uploads/authorize", uploadAuthorizePreamble, http.StatusUnauthorized},
		{"v3 signature on v2", base, "/v2/uploads/authorize", forceUploadAuthorizePreamble, http.StatusUnauthorized},
		{"force field on v2", base, "/v2/uploads/authorize", uploadAuthorizePreamble, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, signedRequest(http.MethodPost, tc.path, []byte(tc.body), installID, key, tc.prefix))
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	if err := manager.RevokeInstall(context.Background(), installID); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, signedRequest(http.MethodPost, "/v3/uploads/authorize", []byte(base), installID, key, forceUploadAuthorizePreamble))
	if w.Code != http.StatusForbidden {
		t.Fatalf("revoked force authorization: %d", w.Code)
	}
}
