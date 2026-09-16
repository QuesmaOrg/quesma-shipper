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
)

type accountTransport func(*http.Request) (*http.Response, error)

func (f accountTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func accountFixture(t *testing.T) Request {
	t.Helper()
	home := t.TempDir()
	return Request{
		Source:   Resolved{Source: Source{ID: "codex-account", Family: "codex", Gather: "account"}, Root: filepath.Join(home, ".codex")},
		StateDir: filepath.Join(home, "shipper"), Env: Env{Home: home, Lookup: func(string) (string, bool) { return "", false }},
		Context: context.Background(), Capture: true,
		Now: func() time.Time { return time.Date(2026, 9, 16, 14, 17, 3, 0, time.UTC) },
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

func TestAccountSnapshotsPreserveProviderJSONInMemory(t *testing.T) {
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
	raw := c.Content
	for _, field := range []string{`"input_tokens":9007199254740993`, `"utilization":123.456`, `"optional":null`, `"windows":[]`, `"chatgpt_plan_type":"pro"`} {
		if !bytes.Contains(raw, []byte(field)) {
			t.Fatalf("lost %s: %s", field, raw)
		}
	}
	again, err := p.Discover(req)
	if err != nil || len(again.Candidates) != 1 || calls != 2 || again.Candidates[0].Path != c.Path {
		t.Fatalf("same bucket: %+v %v calls %d", again, err, calls)
	}
	req.Now = func() time.Time { return time.Date(2026, 9, 16, 14, 31, 0, 0, time.UTC) }
	next, err := p.Discover(req)
	if err != nil || len(next.Candidates) != 1 || calls != 3 || next.Candidates[0].Path == c.Path {
		t.Fatalf("new bucket: %+v %v calls %d", next, err, calls)
	}
	if _, err := os.Stat(req.StateDir); !os.IsNotExist(err) {
		t.Fatal("account collection wrote local state")
	}
}

func TestAccountDiscoveryDoesNotFetchOrWrite(t *testing.T) {
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Env.Home, ".codex", "auth.json"), `{"tokens":{"access_token":"fixture"}}`)
	p := Accounts{client: &http.Client{Transport: accountTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("discovery fetched account data")
		return nil, nil
	})}}
	req.Capture = false
	if d, err := p.Discover(req); err != nil || len(d.Candidates) != 0 {
		t.Fatalf("discovery: %+v %v", d, err)
	}
	if _, err := os.Stat(req.StateDir); !os.IsNotExist(err) {
		t.Fatal("discovery wrote state")
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
		{"rate limited", 429, `fixture-secret`, "http_error"},
		{"malformed", 200, `{"accessToken":"fixture-secret"`, "invalid_json"},
		{"oversized", 200, strings.Repeat("x", accountResponseLimit+1), "response_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Accounts{client: &http.Client{Transport: accountTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
			})}}
			obs := p.fetch(context.Background(), accountFixture(t), "test", "GET", "https://example.org", "fixture-secret", "")
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
	obs := p.fetch(context.Background(), accountFixture(t), "test", "GET", origin.URL, "fixture-secret", "")
	if redirected || obs.HTTPStatus != 302 {
		t.Fatalf("followed credential redirect: %+v", obs)
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
	raw := d.Candidates[0].Content
	var snap accountSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Observations) != 3 || bytes.Contains(raw, []byte("not collected")) || !bytes.Contains(raw, []byte(`"future":42`)) {
		t.Fatalf("%s", raw)
	}
}
