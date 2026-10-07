package sources

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

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
	if len(first.Candidates) != 1 || calls != 0 {
		t.Fatalf("first: %+v calls %d", first, calls)
	}
	c := first.Candidates[0]
	if c.RelPath != "codex.account.20260916T141500Z.jsonl" {
		t.Fatal(c.RelPath)
	}
	payload, err := c.Load(req.Context)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("load calls %d", calls)
	}
	raw := payload.Bytes
	for _, field := range []string{`"input_tokens":9007199254740993`, `"utilization":123.456`, `"optional":null`, `"windows":[]`, `"chatgpt_plan_type":"pro"`} {
		if !bytes.Contains(raw, []byte(field)) {
			t.Fatalf("lost %s: %s", field, raw)
		}
	}
	again, err := p.Discover(req)
	if err != nil || len(again.Candidates) != 1 || calls != 1 || again.Candidates[0].Path != c.Path {
		t.Fatalf("same bucket: %+v %v calls %d", again, err, calls)
	}
	req.Now = func() time.Time { return time.Date(2026, 9, 16, 14, 31, 0, 0, time.UTC) }
	next, err := p.Discover(req)
	if err != nil || len(next.Candidates) != 1 || calls != 1 || next.Candidates[0].Path == c.Path {
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
	for _, capture := range []bool{false, true} {
		req.Capture = capture
		d, err := p.Discover(req)
		if err != nil || (len(d.Candidates) == 1) != capture {
			t.Fatalf("capture=%v discovery: %+v %v", capture, d, err)
		}
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

func TestAccountCompareIgnoresReadTimeAndCountdowns(t *testing.T) {
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Env.Home, ".codex", "auth.json"), `{"tokens":{"access_token":"fixture-access"}}`)
	var body string
	p := Accounts{client: &http.Client{Transport: accountTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}}
	load := func(at time.Time, usage string) Payload {
		t.Helper()
		req.Now = func() time.Time { return at }
		body = usage
		d, err := p.Discover(req)
		if err != nil || len(d.Candidates) != 1 || d.Candidates[0].Series != "codex-account" {
			t.Fatalf("discover: %+v %v", d, err)
		}
		payload, err := d.Candidates[0].Load(req.Context)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	start := time.Date(2026, 9, 16, 14, 17, 3, 0, time.UTC)
	// Shapes seen live: a Codex countdown, and Claude's read time and jittered window timestamps.
	window := func(used, reset, resetsAt, asOf string) string {
		return `{"total":9007199254740993,"rate_limit":{"primary_window":{"used_percent":` + used + `,"reset_after_seconds":` + reset + `}},` +
			`"five_hour":{"resets_at":"` + resetsAt + `"},"seven_day_breakdown":{"as_of":"` + asOf + `"}}`
	}
	first := load(start, window("23", "506259", "2026-10-05T16:20:00.821987+00:00", "2026-10-05T12:34:59.844282+00:00"))
	later := load(start.Add(time.Hour), window("23", "502659", "2026-10-05T16:20:01.396746+00:00", "2026-10-05T13:35:20.417662+00:00"))
	if bytes.Equal(first.Bytes, later.Bytes) || !bytes.Equal(first.Compare, later.Compare) {
		t.Fatalf("an idle account must ship different bytes but compare equal:\n%s\n%s", first.Compare, later.Compare)
	}
	if !bytes.Contains(later.Bytes, []byte(`"reset_after_seconds":502659`)) || !bytes.Contains(later.Compare, []byte(`9007199254740993`)) {
		t.Fatalf("countdown dropped from the object or a large integer rounded:\n%s\n%s", later.Bytes, later.Compare)
	}
	if used := load(start.Add(2*time.Hour), window("24", "499059", "2026-10-05T16:20:00.5+00:00", "2026-10-05T14:00:00Z")); bytes.Equal(first.Compare, used.Compare) {
		t.Fatal("a usage change compared equal")
	}
	if nextDay := load(start.Add(24*time.Hour), window("23", "419859", "2026-10-05T16:20:00.5+00:00", "2026-10-06T12:00:00Z")); bytes.Equal(first.Compare, nextDay.Compare) {
		t.Fatal("an unchanged account on the next day compared equal")
	}
	if rolled := load(start.Add(3*time.Hour), window("23", "495459", "2026-10-05T21:20:00.5+00:00", "2026-10-05T15:00:00Z")); bytes.Equal(first.Compare, rolled.Compare) {
		t.Fatal("a new window compared equal")
	}
}

func TestCursorUsageDays(t *testing.T) {
	req := accountFixture(t)
	req.Source = Resolved{Source: Source{ID: "cursor-account", Family: "cursor", Gather: "account"}, Root: t.TempDir()}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(req.Source.Root, "state.vscdb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB); INSERT INTO ItemTable VALUES ('cursorAuth/accessToken', 'fixture-access')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// Cursor's order: newest first, two per page.
	events := []string{`{"timestamp":"3","tokenUsage":{"totalCents":72.0478}}`, `{"timestamp":"2"}`, `{"timestamp":"1"}`}
	var bodies []string
	failPage := 0
	p := Accounts{client: &http.Client{Transport: accountTransport(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		reply := `{}`
		if strings.HasSuffix(r.URL.Path, "/GetFilteredUsageEvents") {
			bodies = append(bodies, string(raw))
			var q struct{ Page int }
			json.Unmarshal(raw, &q)
			page := events[min(2*(q.Page-1), len(events)):min(2*q.Page, len(events))]
			reply = fmt.Sprintf(`{"totalUsageEventsCount":%d,"usageEventsDisplay":[%s]}`, len(events), strings.Join(page, ","))
			if len(bodies) == failPage {
				return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply)), Header: http.Header{}}, nil
	})}}
	d, err := p.Discover(req)
	if err != nil || len(d.Candidates) != 2+cursorUsageDays {
		t.Fatalf("%+v %v", d, err)
	}
	snapshot, err := d.Candidates[0].Load(req.Context)
	if err != nil || len(bodies) != 0 {
		t.Fatalf("usage events belong in the day objects, not the account snapshot: %v %s", err, snapshot.Bytes)
	}
	// 2026-09-16T14:17:03Z: today and yesterday follow the bucket, the day before is settled.
	days := d.Candidates[1:]
	bucket := time.Date(2026, 9, 16, 14, 15, 0, 0, time.UTC)
	if days[0].Path != "cursor.usage.20260916.jsonl" || !days[0].MTime.Equal(bucket) || !days[1].MTime.Equal(bucket) ||
		!days[2].MTime.Equal(time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)) || days[len(days)-1].Path != "cursor.usage.20260618.jsonl" {
		t.Fatalf("%+v %+v %+v", days[0], days[1], days[2])
	}
	all := events
	events = all[1:]
	earlier, err := days[0].Load(req.Context)
	if err != nil {
		t.Fatal(err)
	}
	events = all
	bodies = nil
	today, err := days[0].Load(req.Context)
	if err != nil {
		t.Fatal(err)
	}
	// 2026-09-16T00:00:00Z to 2026-09-17T00:00:00Z, without a teamId, paged until the reported total.
	if len(bodies) != 2 || bodies[1] != `{"startDate":"1789516800000","endDate":"1789603200000","page":2,"pageSize":500}` {
		t.Fatalf("%q", bodies)
	}
	// Each event as Cursor sent it, oldest first, so a new request only appends.
	if string(today.Bytes) != events[2]+"\n"+events[1]+"\n"+events[0]+"\n" || !bytes.HasPrefix(today.Bytes, earlier.Bytes) {
		t.Fatalf("%s then %s", earlier.Bytes, today.Bytes)
	}
	bodies, failPage = nil, 2
	if _, err := days[0].Load(req.Context); err == nil {
		t.Fatal("a day with a failed page must not ship")
	}
}
