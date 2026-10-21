package sources

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// Fake tokens for auth0|user_a and auth0|user_b, whose account names are the first 12 hex digits of sha256(sub).
const (
	cursorTokenA, cursorAccountA = "e30.eyJzdWIiOiJhdXRoMHx1c2VyX2EifQ.sig", "c518a59a73e6"
	cursorTokenB, cursorAccountB = "e30.eyJzdWIiOiJhdXRoMHx1c2VyX2IifQ.sig", "494bbc669888"
)

type cursorAnswer func(call, page int) (status int, body string)

// cursorUsageFixture is a Cursor store signed in with token ("" for signed out) and a fake API.
func cursorUsageFixture(t *testing.T, token string, answer cursorAnswer) (*CursorUsage, Request, *[]string) {
	t.Helper()
	req := accountFixture(t)
	req.Source = Resolved{Source: Source{ID: "cursor-usage", Family: "cursor", Gather: "cursor_usage"}, Root: t.TempDir()}
	cursorSignIn(t, req.Source.Root, token)
	var bodies []string
	client := &http.Client{Transport: accountTransport(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if r.Header.Get("Authorization") != "Bearer "+token || !strings.HasSuffix(r.URL.Path, "/GetFilteredUsageEvents") {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		var q struct{ Page int }
		json.Unmarshal(raw, &q)
		status, body := answer(len(bodies), q.Page)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	return &CursorUsage{accounts: Accounts{client: client}}, req, &bodies
}

func cursorSignIn(t *testing.T, root, token string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, "state.vscdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS ItemTable (key TEXT PRIMARY KEY, value BLOB); DELETE FROM ItemTable`); err != nil {
		t.Fatal(err)
	}
	if token != "" {
		if _, err := db.Exec(`INSERT INTO ItemTable VALUES ('cursorAuth/accessToken', ?)`, token); err != nil {
			t.Fatal(err)
		}
	}
}

// cursorPage serves events (newest first) two per page, reporting their count as the total.
func cursorPage(events []string, page int) (int, string) {
	part := events[min(2*(page-1), len(events)):min(2*page, len(events))]
	return 200, fmt.Sprintf(`{"totalUsageEventsCount":%d,"usageEventsDisplay":[%s]}`, len(events), strings.Join(part, ","))
}

var cursorDay = time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)

func TestCursorUsageDaysAreNamedAfterTheAccount(t *testing.T) {
	u, req, bodies := cursorUsageFixture(t, cursorTokenA, nil)
	d, err := u.Discover(req)
	if err != nil || len(d.Candidates) != 1+cursorUsageDays || len(*bodies) != 0 {
		t.Fatalf("%v %d candidates, %d calls", err, len(d.Candidates), len(*bodies))
	}
	// 2026-09-16T14:17:03Z: today and yesterday follow the bucket, the day before is settled.
	days, bucket := d.Candidates, time.Date(2026, 9, 16, 14, 15, 0, 0, time.UTC)
	if days[0].Path != "cursor.usage."+cursorAccountA+".20260916.jsonl" || !days[0].MTime.Equal(bucket) || !days[1].MTime.Equal(bucket) ||
		!days[2].MTime.Equal(cursorDay) || days[len(days)-1].Path != "cursor.usage."+cursorAccountA+".20260618.jsonl" {
		t.Fatalf("%+v %+v %+v", days[0], days[1], days[2])
	}
}

func TestCursorUsageWithoutAnAccountHasNoDays(t *testing.T) {
	u, req, _ := cursorUsageFixture(t, "", nil)
	if d, err := u.Discover(req); err != nil || len(d.Candidates) != 0 {
		t.Fatalf("signed out: %v %d candidates", err, len(d.Candidates))
	}
	req.Source.Root = t.TempDir()
	if d, err := u.Discover(req); err != nil || len(d.Candidates) != 0 {
		t.Fatalf("no store: %v %d candidates", err, len(d.Candidates))
	}
}

func TestCursorUsageAccountSwitchIsNewObjects(t *testing.T) {
	u, req, _ := cursorUsageFixture(t, cursorTokenA, func(int, int) (int, string) { return cursorPage(nil, 1) })
	d, err := u.Discover(req)
	if err != nil {
		t.Fatal(err)
	}
	cursorSignIn(t, req.Source.Root, cursorTokenB)
	if _, err := d.Candidates[0].Load(req.Context); err == nil {
		t.Fatal("account A's day loaded account B's events")
	}
	if d, _ := u.Discover(req); d.Candidates[0].Path != "cursor.usage."+cursorAccountB+".20260916.jsonl" {
		t.Fatal(d.Candidates[0].Path)
	}
}

func TestCursorUsagePaging(t *testing.T) {
	events := []string{`{"timestamp":"3"}`, `{"timestamp":"2"}`, `{"timestamp":"1"}`}
	u, _, bodies := cursorUsageFixture(t, cursorTokenA, func(_, page int) (int, string) { return cursorPage(events, page) })
	got, err := u.fetchEvents(t.Context(), cursorTokenA, cursorDay, cursorDay.AddDate(0, 0, 1), 0)
	if err != nil || len(got) != 3 || string(got[0]) != events[0] {
		t.Fatalf("%v %s", err, got)
	}
	// 2026-09-16 to 2026-09-17, no teamId, paged until the reported total.
	if len(*bodies) != 2 || (*bodies)[1] != `{"startDate":"1789516800000","endDate":"1789603200000","page":2,"pageSize":500}` {
		t.Fatalf("%q", *bodies)
	}
}

func TestCursorUsageRereadsWhenARequestArrivesMidRead(t *testing.T) {
	before := []string{`{"timestamp":"3"}`, `{"timestamp":"2"}`, `{"timestamp":"1"}`}
	after := append([]string{`{"timestamp":"4"}`}, before...)
	u, _, bodies := cursorUsageFixture(t, cursorTokenA, func(call, page int) (int, string) {
		if call == 1 {
			return cursorPage(before, page)
		}
		return cursorPage(after, page)
	})
	got, err := u.fetchEvents(t.Context(), cursorTokenA, cursorDay, cursorDay.AddDate(0, 0, 1), 0)
	lines, _ := cursorDayLines(got)
	if err != nil || len(*bodies) != 4 || string(lines) != "{\"timestamp\":\"1\"}\n{\"timestamp\":\"2\"}\n{\"timestamp\":\"3\"}\n{\"timestamp\":\"4\"}\n" {
		t.Fatalf("%v %d calls %s", err, len(*bodies), lines)
	}
	u, _, _ = cursorUsageFixture(t, cursorTokenA, func(call, _ int) (int, string) {
		return 200, fmt.Sprintf(`{"totalUsageEventsCount":%d,"usageEventsDisplay":[{"timestamp":"%d"},{"timestamp":"0"}]}`, 10+call, call)
	})
	if _, err := u.fetchEvents(t.Context(), cursorTokenA, cursorDay, cursorDay.AddDate(0, 0, 1), 0); err == nil {
		t.Fatal("a day that never holds still must not ship")
	}
}

func TestCursorUsageIncompleteDayFails(t *testing.T) {
	events := []string{`{"timestamp":"3"}`, `{"timestamp":"2"}`}
	ranOut, _, _ := cursorUsageFixture(t, cursorTokenA, func(_, page int) (int, string) {
		_, body := cursorPage(events, page)
		return 200, strings.Replace(body, `"totalUsageEventsCount":2`, `"totalUsageEventsCount":3`, 1)
	})
	failed, _, _ := cursorUsageFixture(t, cursorTokenA, func(call, page int) (int, string) {
		if call == 2 {
			return 503, `{}`
		}
		return cursorPage(append(events, `{"timestamp":"1"}`), page)
	})
	for name, u := range map[string]*CursorUsage{"ran out before the total": ranOut, "failed page": failed} {
		if _, err := u.fetchEvents(t.Context(), cursorTokenA, cursorDay, cursorDay.AddDate(0, 0, 1), 0); err == nil {
			t.Errorf("%s: shipped short", name)
		}
	}
}

func TestCursorUsageStopsAtTheSizeLimit(t *testing.T) {
	events := []string{`{"timestamp":"4"}`, `{"timestamp":"3"}`, `{"timestamp":"2"}`, `{"timestamp":"1"}`}
	u, _, bodies := cursorUsageFixture(t, cursorTokenA, func(_, page int) (int, string) { return cursorPage(events, page) })
	_, err := u.fetchEvents(t.Context(), cursorTokenA, cursorDay, cursorDay.AddDate(0, 0, 1), 30)
	if !errors.Is(err, platform.ErrTooLarge) || len(*bodies) != 1 {
		t.Fatalf("%v after %d calls", err, len(*bodies))
	}
}

func TestCursorDayLinesOnlyAppend(t *testing.T) {
	lines := func(newestFirst ...string) string {
		var raw []json.RawMessage
		for _, e := range newestFirst {
			raw = append(raw, json.RawMessage(e))
		}
		b, err := cursorDayLines(raw)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	earlier, later := lines(`{"timestamp":"2"}`, "{\n  \"timestamp\": \"1\"\n}"), lines(`{"timestamp":"3"}`, `{"timestamp":"2"}`, `{"timestamp":"1"}`)
	if later != "{\"timestamp\":\"1\"}\n{\"timestamp\":\"2\"}\n{\"timestamp\":\"3\"}\n" || !strings.HasPrefix(later, earlier) {
		t.Fatalf("%q then %q", earlier, later)
	}
}

func TestCursorUsageLoadShipsTheDay(t *testing.T) {
	events := []string{`{"timestamp":"2","tokenUsage":{"totalCents":72.0478}}`, `{"timestamp":"1"}`}
	u, req, _ := cursorUsageFixture(t, cursorTokenA, func(_, page int) (int, string) { return cursorPage(events, page) })
	d, err := u.Discover(req)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := d.Candidates[0].Load(req.Context)
	if err != nil || string(payload.Bytes) != events[1]+"\n"+events[0]+"\n" || !payload.MTime.Equal(d.Candidates[0].MTime) {
		t.Fatalf("%v %s", err, payload.Bytes)
	}
}
