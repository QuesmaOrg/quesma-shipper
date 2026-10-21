package engine_test

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The cursor-usage source end to end: every day it generates must seal and upload.
func TestCursorUsageDaysSealAndShip(t *testing.T) {
	f := newFixture(t)
	root := filepath.Join(f.home, "cursor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, "state.vscdb"))
	if err != nil {
		t.Fatal(err)
	}
	// A fake token whose sub is auth0|user_a.
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB); INSERT INTO ItemTable VALUES ('cursorAuth/accessToken', 'e30.eyJzdWIiOiJhdXRoMHx1c2VyX2EifQ.sig')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	o := f.opts()
	now := o.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	// The source has no client to inject through the engine, so the default transport answers: today has two events.
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		var q struct{ StartDate string }
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &q)
		body := `{}`
		if q.StartDate == strconv.FormatInt(today.UnixMilli(), 10) {
			body = `{"totalUsageEventsCount":2,"usageEventsDisplay":[{"timestamp":"2","model":"m"},{"timestamp":"1","model":"m"}]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	catalog, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := catalog.Source("cursor-usage")
	o.Plan.Interval = 15 * time.Minute
	o.Plan.MaxFilesPerRun = 200
	o.Env = sources.Env{Home: f.home, Lookup: func(string) (string, bool) { return "", false }}
	o.Plan.Sources = []sources.Resolved{{Source: spec, Root: root, Enabled: true, SpecFingerprint: sources.SpecFingerprint(spec)}}
	o.Now = func() time.Time { return now }
	rep := runEnrich(t, f, o)
	if rep.Shipped != 91 || rep.Failed != 0 || rep.Parked != 0 {
		t.Fatalf("every day must seal and ship: %+v", rep)
	}
	todayName := "cursor.usage.c518a59a73e6." + today.Format("20060102") + ".jsonl"
	for _, key := range f.port.keys() {
		obj, _ := f.port.get(key)
		m, payload, err := transforms.Open(obj.Body, f.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if m.Gather != "cursor_usage" || m.SourceID != "cursor-usage" {
			t.Fatalf("%+v", m)
		}
		if strings.HasSuffix(m.NativePath, todayName) && string(payload) != "{\"timestamp\":\"1\",\"model\":\"m\"}\n{\"timestamp\":\"2\",\"model\":\"m\"}\n" {
			t.Fatalf("today: %q", payload)
		}
	}
}
