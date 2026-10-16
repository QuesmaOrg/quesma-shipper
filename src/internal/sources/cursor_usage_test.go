package sources

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// cursorUsageFixture is a signed-in Cursor store and a fake API serving events newest first, two per page.
func cursorUsageFixture(t *testing.T, events *[]string, bodies *[]string, failAt int) (*CursorUsage, Request) {
	t.Helper()
	req := accountFixture(t)
	req.Source = Resolved{Source: Source{ID: "cursor-usage", Family: "cursor", Gather: "cursor_usage"}, Root: t.TempDir()}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(req.Source.Root, "state.vscdb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB); INSERT INTO ItemTable VALUES ('cursorAuth/accessToken', 'fixture-access')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	client := &http.Client{Transport: accountTransport(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(raw))
		if r.Header.Get("Authorization") != "Bearer fixture-access" || !strings.HasSuffix(r.URL.Path, "/GetFilteredUsageEvents") {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		status, reply := 200, `{}`
		if len(*bodies) == failAt {
			status = 503
		} else {
			var q struct{ Page int }
			json.Unmarshal(raw, &q)
			all := *events
			page := all[min(2*(q.Page-1), len(all)):min(2*q.Page, len(all))]
			reply = fmt.Sprintf(`{"totalUsageEventsCount":%d,"usageEventsDisplay":[%s]}`, len(all), strings.Join(page, ","))
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(reply)), Header: http.Header{}}, nil
	})}
	return &CursorUsage{accounts: Accounts{client: client}}, req
}

func TestCursorUsageDays(t *testing.T) {
	var events, bodies []string
	u, req := cursorUsageFixture(t, &events, &bodies, 0)
	d, err := u.Discover(req)
	if err != nil || len(d.Candidates) != 1+cursorUsageDays || len(bodies) != 0 {
		t.Fatalf("%v %d candidates, %d calls", err, len(d.Candidates), len(bodies))
	}
	// 2026-09-16T14:17:03Z: today and yesterday follow the bucket, the day before is settled.
	days, bucket := d.Candidates, time.Date(2026, 9, 16, 14, 15, 0, 0, time.UTC)
	if days[0].Path != "cursor.usage.20260916.jsonl" || !days[0].MTime.Equal(bucket) || !days[1].MTime.Equal(bucket) ||
		!days[2].MTime.Equal(time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)) || days[len(days)-1].Path != "cursor.usage.20260618.jsonl" {
		t.Fatalf("%+v %+v %+v", days[0], days[1], days[2])
	}
}

func TestCursorUsagePaging(t *testing.T) {
	events := []string{`{"timestamp":"3"}`, `{"timestamp":"2"}`, `{"timestamp":"1"}`}
	var bodies []string
	u, _ := cursorUsageFixture(t, &events, &bodies, 0)
	start := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	got, err := u.fetchEvents(t.Context(), "fixture-access", start, start.AddDate(0, 0, 1))
	if err != nil || len(got) != 3 || string(got[0]) != events[0] {
		t.Fatalf("%v %s", err, got)
	}
	// 2026-09-16 to 2026-09-17, no teamId, paged until the reported total.
	if len(bodies) != 2 || bodies[1] != `{"startDate":"1789516800000","endDate":"1789603200000","page":2,"pageSize":500}` {
		t.Fatalf("%q", bodies)
	}
	u, _ = cursorUsageFixture(t, &events, &bodies, 2)
	bodies = nil
	if _, err := u.fetchEvents(t.Context(), "fixture-access", start, start.AddDate(0, 0, 1)); err == nil {
		t.Fatal("a day with a failed page must not ship")
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
	var bodies []string
	u, req := cursorUsageFixture(t, &events, &bodies, 0)
	d, err := u.Discover(req)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := d.Candidates[0].Load(req.Context)
	if err != nil || string(payload.Bytes) != events[1]+"\n"+events[0]+"\n" || !payload.MTime.Equal(d.Candidates[0].MTime) {
		t.Fatalf("%v %s", err, payload.Bytes)
	}
}
