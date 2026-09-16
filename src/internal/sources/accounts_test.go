package sources

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

type accountTransport func(*http.Request) (*http.Response, error)

func (f accountTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func accountFixture(t *testing.T) Request {
	t.Helper()
	home := t.TempDir()
	scrub, err := transforms.New(transforms.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return Request{
		Source:   Resolved{Source: Source{ID: "codex-account", Family: "codex", Gather: "account"}},
		StateDir: filepath.Join(home, "shipper"), Env: Env{Home: home, Lookup: func(string) (string, bool) { return "", false }},
		Context: context.Background(), Capture: true,
		Now:   func() time.Time { return time.Date(2026, 9, 16, 14, 17, 3, 0, time.UTC) },
		Scrub: func(b []byte) ([]byte, error) { r, e := scrub.Scrub(b, transforms.Hint{JSONL: true}); return r.Out, e },
	}
}

func accountFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAccountSnapshotsPreserveProviderJSONAndRetryHistory(t *testing.T) {
	req := accountFixture(t)
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"dev@example.org","https://api.openai.com/auth":{"chatgpt_plan_type":"pro"}}`))
	accountFile(t, filepath.Join(req.Env.Home, ".codex", "auth.json"), `{"tokens":{"access_token":"fixture-access","refresh_token":"fixture-refresh","account_id":"workspace-1","id_token":"x.`+claims+`.x"}}`)
	calls := 0
	p := Accounts{client: &http.Client{Transport: accountTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fixture-access" || r.Header.Get("ChatGPT-Account-Id") != "workspace-1" {
			t.Error("wrong authentication")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{ "unknown":{"input_tokens":9007199254740993,"utilization":123.456,"optional":null,"accessToken":"fixture-secret"}, "windows":[] }`)), Header: http.Header{}}, nil
	})}}
	first, err := p.Discover(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Candidates) != 1 || calls != 1 {
		t.Fatalf("first: %+v calls %d", first, calls)
	}
	c := first.Candidates[0]
	if c.RelPath != "codex.account.20260916T141500Z.json" {
		t.Fatal(c.RelPath)
	}
	raw, err := os.ReadFile(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture-access", "fixture-refresh", "fixture-secret", "dev@example.org"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("leaked %s", secret)
		}
	}
	for _, field := range []string{`"input_tokens":9007199254740993`, `"utilization":123.456`, `"optional":null`, `"windows":[]`, `"chatgpt_plan_type":"pro"`} {
		if !bytes.Contains(raw, []byte(field)) {
			t.Fatalf("lost %s: %s", field, raw)
		}
	}
	again, err := p.Discover(req)
	if err != nil || len(again.Candidates) != 1 || calls != 1 {
		t.Fatalf("same bucket: %+v %v calls %d", again, err, calls)
	}
	same, _ := os.ReadFile(c.Path)
	if !bytes.Equal(raw, same) {
		t.Fatal("snapshot overwritten")
	}
	req.Now = func() time.Time { return time.Date(2026, 9, 16, 14, 31, 0, 0, time.UTC) }
	next, err := p.Discover(req)
	if err != nil || len(next.Candidates) != 2 {
		t.Fatalf("pending history: %+v %v", next, err)
	}
	req.Committed = func(c Candidate) bool { return c.RelPath == first.Candidates[0].RelPath }
	req.Now = func() time.Time { return time.Date(2026, 9, 16, 14, 46, 0, 0, time.UTC) }
	third, err := p.Discover(req)
	if err != nil || len(third.Candidates) != 2 {
		t.Fatalf("cleanup: %+v %v", third, err)
	}
	if _, err := os.Stat(c.Path); !os.IsNotExist(err) {
		t.Fatal("committed old snapshot retained")
	}
	if _, err := os.Stat(next.Candidates[1].Path); err != nil {
		t.Fatal("uncommitted snapshot removed", err)
	}
	req.Now = func() time.Time { return time.Date(2026, 9, 16, 14, 16, 0, 0, time.UTC) }
	before := calls
	if _, err := p.Discover(req); err != nil || calls != before {
		t.Fatal("clock rollback recaptured", err)
	}
}

func TestAccountDiscoveryAndScrubFailureDoNotWrite(t *testing.T) {
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Env.Home, ".codex", "auth.json"), `{"auth_mode":"apikey"}`)
	p := Accounts{}
	req.Capture = false
	if d, err := p.Discover(req); err != nil || len(d.Candidates) != 0 {
		t.Fatalf("discovery: %+v %v", d, err)
	}
	if _, err := os.Stat(req.StateDir); !os.IsNotExist(err) {
		t.Fatal("discovery wrote state")
	}
	req.Capture = true
	req.Scrub = func([]byte) ([]byte, error) { return nil, errors.New("fixture scrub failure") }
	if _, err := p.Discover(req); err == nil {
		t.Fatal("scrub failed open")
	}
	if _, err := os.Stat(req.StateDir); !os.IsNotExist(err) {
		t.Fatal("unscrubbed snapshot saved")
	}
}

func TestAccountHTTPFailuresAreBoundedAndDoNotLeak(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorized", 401, `fixture-secret`, "http_error"},
		{"malformed", 200, `{"accessToken":"fixture-secret"`, "invalid_json"},
		{"oversized", 200, strings.Repeat("x", accountResponseLimit+1), "response_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Accounts{client: &http.Client{Transport: accountTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
			})}}
			obs := p.fetch(context.Background(), accountObservation{ObservedAt: time.Now()}, "GET", "https://example.org", "fixture-secret", "")
			if obs.Error != tc.want || len(obs.Body) != 0 {
				t.Fatalf("%+v", obs)
			}
		})
	}
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer origin.Close()
	p := Accounts{}
	obs := p.fetch(context.Background(), accountObservation{}, "GET", origin.URL, "fixture-secret", "")
	if redirected || obs.HTTPStatus != 302 {
		t.Fatalf("followed credential redirect: %+v", obs)
	}
}

func TestAccountThrottleSurvivesRestart(t *testing.T) {
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Env.Home, ".codex", "auth.json"), `{"tokens":{"access_token":"fixture"}}`)
	calls := 0
	p := Accounts{client: &http.Client{Transport: accountTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader("secret error body")), Header: http.Header{"Retry-After": []string{"3600"}}}, nil
	})}}
	if _, err := p.Discover(req); err != nil {
		t.Fatal(err)
	}
	req.Now = func() time.Time { return time.Date(2026, 9, 16, 14, 31, 0, 0, time.UTC) }
	restarted := Accounts{client: p.client}
	d, err := restarted.Discover(req)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(d.Candidates) != 2 {
		t.Fatalf("lost cooldown: %+v calls %d", d, calls)
	}
	raw, _ := os.ReadFile(d.Candidates[1].Path)
	if !bytes.Contains(raw, []byte(`"error":"throttled"`)) {
		t.Fatalf("%s", raw)
	}
}

func TestClaudeUsesActiveCredentialsAndPreservesLocalAccount(t *testing.T) {
	req := accountFixture(t)
	req.Source.ID = "claude-account"
	req.Source.Family = "claude-code"
	home := filepath.Join(req.Env.Home, "other-claude")
	req.Source.Root = home
	req.Env.Lookup = func(k string) (string, bool) { return home, k == "CLAUDE_CONFIG_DIR" }
	accountFile(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"organizationType":"max","future":42},"unrelated":"not collected"}`)
	accountFile(t, filepath.Join(home, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"correct","scopes":["user:profile"]}}`)
	calls := 0
	p := Accounts{keychain: func(context.Context, string) ([]byte, error) { return nil, errors.New("locked") }, client: &http.Client{Transport: accountTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer correct" || r.Header.Get("anthropic-beta") == "" {
			t.Error("wrong Claude credentials")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"five_hour":null,"limits":[{"kind":"future","percent":111}]}`)), Header: http.Header{}}, nil
	})}}
	d, err := p.Discover(req)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
	raw, _ := os.ReadFile(d.Candidates[0].Path)
	var snap accountSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Observations) != 3 || bytes.Contains(raw, []byte("not collected")) || !bytes.Contains(raw, []byte(`"future":42`)) {
		t.Fatalf("%s", raw)
	}
}

func TestFullAccountBacklogRetainsPendingHistory(t *testing.T) {
	req := accountFixture(t)
	start := req.Now().Add(-time.Duration(maxAccountSnapshots+1) * accountInterval)
	dir := filepath.Join(req.StateDir, "snapshots", req.Source.ID)
	for i := 0; i < maxAccountSnapshots; i++ {
		name := "codex.account." + start.Add(time.Duration(i)*accountInterval).Truncate(accountInterval).Format("20060102T150405Z") + ".json"
		accountFile(t, filepath.Join(dir, name), `{"schema_version":1,"observations":[]}`)
	}
	p := Accounts{}
	d, err := p.Discover(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Candidates) != maxAccountSnapshots || d.Unreadable != 1 || !strings.Contains(d.Reason, "backlog full") {
		t.Fatalf("backlog: candidates=%d reason=%s err=%v", len(d.Candidates), d.Reason, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != maxAccountSnapshots {
		t.Fatal("pending history discarded", err)
	}
}
