package sources

import (
	"bytes"
	"context"
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

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

type accountTransport func(*http.Request) (*http.Response, error)

func (f accountTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func accountFixture(t *testing.T) Request {
	t.Helper()
	home := t.TempDir()
	return Request{
		Source:   Resolved{Source: Source{ID: "codex-account", Family: "codex", Gather: "account"}, Root: filepath.Join(home, ".codex")},
		StateDir: filepath.Join(home, "shipper"), Env: Env{Home: home, Lookup: func(string) (string, bool) { return "", false }},
		Context: context.Background(), Capture: true, Interval: 15 * time.Minute,
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
			request, err := http.NewRequest("GET", "https://example.org", nil)
			if err != nil {
				t.Fatal(err)
			}
			obs := p.fetch(accountObservation{Source: "test"}, request)
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
	request, err := http.NewRequest("GET", origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer fixture-secret")
	obs := p.fetch(accountObservation{Source: "test"}, request)
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
	payload, err := d.Candidates[0].Load(req.Context)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
	raw := payload.Bytes
	lines := bytes.Split(raw, []byte("\n"))
	if len(lines) != 4 || len(lines[3]) != 0 {
		t.Fatalf("expected three newline-terminated records: %s", raw)
	}
	for i, line := range lines[:3] {
		var record struct {
			BucketStart time.Time `json:"bucket_start"`
			accountObservation
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if !record.BucketStart.Equal(req.Now().Truncate(req.Interval)) || record.Source == "" || record.ObservedAt.IsZero() || len(record.Body) == 0 {
			t.Fatalf("incomplete record %d: %s", i, line)
		}
	}
	if bytes.Contains(raw, []byte(`"observations"`)) || bytes.Contains(raw, []byte("not collected")) || !bytes.Contains(raw, []byte(`"future":42`)) {
		t.Fatalf("%s", raw)
	}
}

func TestAccountBucketUsesCollectionInterval(t *testing.T) {
	for _, interval := range []time.Duration{5 * time.Minute, 30 * time.Minute, 0, -time.Minute} {
		t.Run(interval.String(), func(t *testing.T) {
			req := accountFixture(t)
			req.Interval = interval
			accountFile(t, filepath.Join(req.Source.Root, "auth.json"), `{}`)
			d, err := (&Accounts{}).Discover(req)
			if interval <= 0 {
				if err == nil {
					t.Fatal("accepted nonpositive interval")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			bucket := req.Now().UTC().Truncate(interval)
			c := d.Candidates[0]
			if c.RelPath != "codex.account."+bucket.Format("20060102T150405Z")+".jsonl" {
				t.Fatal(c.RelPath)
			}
			payload, err := c.Load(req.Context)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range bytes.Split(bytes.TrimSpace(payload.Bytes), []byte("\n")) {
				var record struct {
					BucketStart time.Time `json:"bucket_start"`
				}
				if err := json.Unmarshal(line, &record); err != nil || !record.BucketStart.Equal(bucket) {
					t.Fatalf("wrong bucket: %s (%v)", line, err)
				}
			}
		})
	}
}

func TestCandidateLoadLimitsAndCancellation(t *testing.T) {
	req := accountFixture(t)
	req.Source.MaxFileBytes = 1
	path := filepath.Join(req.Source.Root, "auth.json")
	accountFile(t, path, `{}`)
	d, err := (&Accounts{}).Discover(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, load := range []func(context.Context) (Payload, error){fileLoader(path, 1), d.Candidates[0].Load} {
		if _, err := load(context.Background()); !errors.Is(err, platform.ErrTooLarge) {
			t.Fatalf("expected size limit: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := load(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation: %v", err)
		}
	}
}
