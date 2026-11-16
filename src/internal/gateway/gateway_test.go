package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeShipper records what each ship saw in the spool; fail makes ships refuse.
type fakeShipper struct {
	mu    sync.Mutex
	fail  bool
	ships map[string][]string
}

func (f *fakeShipper) Ship(_ context.Context, _ Tenant, p Person) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("told to fail")
	}
	files, _ := filepath.Glob(filepath.Join(p.Spool(), "*", "*.jsonl"))
	for _, path := range files {
		raw, _ := os.ReadFile(path)
		f.ships[p.Label] = append(f.ships[p.Label], filepath.Base(filepath.Dir(path))+":"+string(raw))
	}
	return nil
}

type world struct {
	t       *testing.T
	gw      *Gateway
	shipper *fakeShipper
	token   string
	dataDir string
	cfg     string
}

func stage(t *testing.T, tenantExtra string) *world {
	t.Helper()
	w := &world{t: t, dataDir: t.TempDir(), shipper: &fakeShipper{ships: map[string][]string{}}}
	var sha string
	w.token, sha = NewToken("acme.eu", "s3cret")
	w.cfg = "data_dir: " + w.dataDir + "\nmax_body_bytes: 65536\ntenants:\n  - organization: acme.eu\n" +
		"    fleet_manager: https://fleet.example.com\n    grant: fmi2.acme.eu.id.secret\n    token_sha256: [" + sha + "]\n" + tenantExtra
	w.open()
	return w
}

// open builds a gateway over the data directory, as a restarted process would.
func (w *world) open() {
	w.t.Helper()
	cfg, err := ParseConfig([]byte(w.cfg))
	if err != nil {
		w.t.Fatal(err)
	}
	w.gw, err = New(cfg, w.shipper, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		w.t.Fatal(err)
	}
}

type kv map[string]string

func logsBody(service string, records ...kv) []byte {
	var recs []any
	for _, r := range records {
		var attrs []any
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			attrs = append(attrs, map[string]any{"key": k, "value": map[string]any{"stringValue": r[k]}})
		}
		recs = append(recs, map[string]any{"timeUnixNano": "1", "attributes": attrs})
	}
	raw, _ := json.Marshal(map[string]any{"resourceLogs": []any{map[string]any{
		"resource":  map[string]any{"attributes": []any{map[string]any{"key": "service.name", "value": map[string]any{"stringValue": service}}}},
		"scopeLogs": []any{map[string]any{"scope": map[string]any{"name": "s"}, "logRecords": recs}},
	}}})
	return raw
}

func (w *world) post(path string, body []byte, headers kv) int {
	w.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	w.gw.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func (w *world) flush(wantFailed int) {
	w.t.Helper()
	if failed := w.gw.Flush(context.Background()); failed != wantFailed {
		w.t.Fatalf("%d people failed, want %d", failed, wantFailed)
	}
}

func (w *world) shipped(label string) string {
	w.shipper.mu.Lock()
	defer w.shipper.mu.Unlock()
	return strings.Join(w.shipper.ships[label], "")
}

func TestOnlyTheOrganizationsTokenIsAccepted(t *testing.T) {
	w := stage(t, "")
	body := logsBody("claude-code", kv{"user.email": "ada@acme.eu", "session.id": "s"})
	other, _ := NewToken("globex", "s3cret")
	for name, auth := range map[string]string{"none": "", "wrong secret": "Bearer " + w.token + "x",
		"another organization": "Bearer " + other, "not bearer": w.token} {
		for _, path := range []string{"/v1/logs", "/v1/metrics", "/v1/traces"} {
			if status := w.post(path, body, kv{"Authorization": auth}); status != http.StatusUnauthorized {
				t.Errorf("%s on %s answered %d", name, path, status)
			}
		}
	}
	if status := w.post("/v1/logs", body, nil); status != http.StatusOK {
		t.Fatalf("the real token answered %d", status)
	}
	if status := w.post("/v1/traces", []byte("anything"), nil); status != http.StatusOK {
		t.Errorf("traces answered %d, want accepted and discarded", status)
	}
}

func TestBodiesAreBoundedAndGzipIsRead(t *testing.T) {
	w := stage(t, "")
	body := logsBody("claude-code", kv{"user.email": "ada@acme.eu", "session.id": "s", "prompt": "zipped"})
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write(body)
	_ = zw.Close()
	if status := w.post("/v1/logs", zipped.Bytes(), kv{"Content-Encoding": "gzip"}); status != http.StatusOK {
		t.Fatalf("a gzip body answered %d", status)
	}
	w.flush(0)
	if !strings.Contains(w.shipped("otel: ada@acme.eu"), "zipped") {
		t.Error("the gzip body's line was not spooled")
	}

	huge := logsBody("claude-code", kv{"user.email": "ada@acme.eu", "prompt": strings.Repeat("x", 70000)})
	if status := w.post("/v1/logs", huge, nil); status != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversize body answered %d", status)
	}
	// A small body that inflates past the limit is refused as well.
	zipped.Reset()
	zw = gzip.NewWriter(&zipped)
	_, _ = zw.Write(huge)
	_ = zw.Close()
	if status := w.post("/v1/logs", zipped.Bytes(), kv{"Content-Encoding": "gzip"}); status != http.StatusRequestEntityTooLarge {
		t.Errorf("an inflating body answered %d", status)
	}
	if status := w.post("/v1/logs", []byte("{"), nil); status != http.StatusBadRequest {
		t.Errorf("a malformed body answered %d", status)
	}
}

func TestEventsFindTheirPersonAcrossPartialClaims(t *testing.T) {
	w := stage(t, "")
	// Codex names the email on some events, only the account on others, and nobody on a few.
	w.post("/v1/logs", logsBody("codex_exec",
		kv{"user.email": "Ada@acme.eu", "user.account_id": "acct-1", "conversation.id": "c", "n": "one"},
		kv{"n": "inherits"}), nil)
	w.post("/v1/logs", logsBody("codex_exec", kv{"user.account_id": "acct-1", "n": "by-account"}), nil)
	w.post("/v1/logs", logsBody("claude-code", kv{"user.email": "ada@acme.eu", "session.id": "s", "n": "claude"}), nil)
	// Nobody named and nothing to inherit from: kept under the machine only if it is a session.
	w.post("/v1/logs", logsBody("codex_exec", kv{"n": "noise"}, kv{"conversation.id": "c2", "host.name": "ci-7", "n": "hosted"}), nil)
	w.flush(0)

	ada := w.shipped("otel: ada@acme.eu")
	for _, want := range []string{`"n":"one"`, `"n":"inherits"`, `"n":"by-account"`, `"n":"claude"`} {
		if !strings.Contains(ada, want) {
			t.Errorf("ada's install lacks %s", want)
		}
	}
	if !strings.Contains(w.shipped("otel: host.name:ci-7"), `"n":"hosted"`) {
		t.Errorf("the session without a person was not kept under its machine: %v", w.shipper.ships)
	}
	if len(w.shipper.ships) != 2 || strings.Contains(ada+w.shipped("otel: host.name:ci-7"), "noise") {
		t.Errorf("unexpected people or a kept noise event: %v", w.shipper.ships)
	}
}

func TestEmailDomainsAndThePeopleCapAreEnforced(t *testing.T) {
	w := stage(t, "    email_domains: [Acme.eu]\n    max_people: 1\n")
	if status := w.post("/v1/logs", logsBody("claude-code", kv{"user.email": "eve@evil.example", "session.id": "s", "n": "eve"}), nil); status != http.StatusOK {
		t.Fatalf("a refused domain answered %d, want accepted and dropped", status)
	}
	if status := w.post("/v1/logs", logsBody("claude-code", kv{"user.email": "ada@ACME.eu", "session.id": "s"}), nil); status != http.StatusOK {
		t.Fatalf("the organization's domain answered %d", status)
	}
	if status := w.post("/v1/logs", logsBody("claude-code", kv{"user.email": "bob@acme.eu", "session.id": "s"}), nil); status != http.StatusTooManyRequests {
		t.Errorf("a person past max_people answered %d", status)
	}
	w.flush(0)
	if len(w.shipper.ships) != 1 || w.shipped("otel: ada@acme.eu") == "" {
		t.Errorf("shipped for %v, want ada only", w.shipper.ships)
	}
}

func TestAFailedShipKeepsItsFilesAndARestartFindsThem(t *testing.T) {
	w := stage(t, "")
	w.shipper.fail = true
	w.post("/v1/logs", logsBody("claude-code", kv{"user.email": "ada@acme.eu", "user.id": "u-1", "session.id": "s", "n": "first"}), nil)
	w.flush(1)
	w.post("/v1/logs", logsBody("claude-code", kv{"user.id": "u-1", "session.id": "s", "n": "second"}), nil)

	// A new process over the same directory: the sealed file, the open one and the aliases are all still there.
	w.open()
	w.shipper.fail = false
	w.flush(0)
	got := w.shipped("otel: ada@acme.eu")
	if !strings.Contains(got, `"n":"first"`) || !strings.Contains(got, `"n":"second"`) || len(w.shipper.ships) != 1 {
		t.Fatalf("after the restart shipped %v", w.shipper.ships)
	}
	left, _ := filepath.Glob(filepath.Join(w.dataDir, "orgs", "*", "people", "*", "spool", "*", "*"))
	if len(left) != 0 {
		t.Errorf("shipped files were not reaped: %v", left)
	}
	identity, _ := filepath.Glob(filepath.Join(w.dataDir, "orgs", "*", "people", "*", "spool", "identity.jsonl"))
	if len(identity) != 1 {
		t.Fatalf("identity files: %v", identity)
	}
	raw, _ := os.ReadFile(identity[0])
	if strings.Count(string(raw), "\n") != 2 || !strings.Contains(string(raw), `"user.email":"ada@acme.eu"`) {
		t.Errorf("identity.jsonl should hold the two distinct claim sets once each: %s", raw)
	}
	w.flush(0)
}

func TestConfigRefusesWhatItCannotHonor(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	for name, cfg := range map[string]string{
		"no data_dir":      "tenants: []\n",
		"unknown field":    "data_dir: /d\nlistn: x\n",
		"bad organization": "data_dir: /d\ntenants:\n  - {organization: a/b, fleet_manager: u, grant: g, token_sha256: [" + sha + "]}\n",
		"no token":         "data_dir: /d\ntenants:\n  - {organization: a, fleet_manager: u, grant: g}\n",
		"not a hash":       "data_dir: /d\ntenants:\n  - {organization: a, fleet_manager: u, grant: g, token_sha256: [abc]}\n",
		"no grant":         "data_dir: /d\ntenants:\n  - {organization: a, fleet_manager: u, token_sha256: [" + sha + "]}\n",
		"repeated organization": "data_dir: /d\ntenants:\n  - {organization: a, fleet_manager: u, grant: g, token_sha256: [" + sha + "]}\n" +
			"  - {organization: a, fleet_manager: u, grant: g, token_sha256: [" + sha + "]}\n",
	} {
		if _, err := ParseConfig([]byte(cfg)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
