package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
)

// Usage events are fetched without a teamId: Cursor then returns only the signed-in user's
// requests, even to a team owner. Each UTC day is one object, one event per line as Cursor sent
// it, oldest first: a new request appends a line, which ingest stores without rewriting the day.

const cursorUsageDays = 90

func (p *Accounts) collectCursor(ctx context.Context, req Request) ([]accountObservation, bool) {
	var out []accountObservation
	path := filepath.Join(req.Source.Root, "state.vscdb")
	if !accountPathExists(path) {
		return nil, false
	}
	values, token, err := sqliteread.CursorAccount(ctx, path)
	body, _ := json.Marshal(values)
	out = append(out, localAccount(req, "cursor.local.account", body, err))
	for _, endpoint := range []string{"GetPlanInfo", "GetCurrentPeriodUsage"} {
		out = append(out, p.cursorCall(ctx, token, endpoint, "{}", req.Now().UTC()))
	}
	return out, true
}

func (p *Accounts) cursorCall(ctx context.Context, token, endpoint, body string, now time.Time) accountObservation {
	obs := accountObservation{Source: "cursor.dashboard." + endpoint, ObservedAt: now}
	if token == "" {
		obs.Error = "credentials_unavailable"
		return obs
	}
	request, err := http.NewRequestWithContext(ctx, "POST", "https://api2.cursor.sh/aiserver.v1.DashboardService/"+endpoint, strings.NewReader(body))
	if err != nil {
		obs.Error = "request_failed"
		return obs
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "quesma-shipper")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connect-Protocol-Version", "1")
	return p.fetch(obs, request)
}

// cursorDays is one candidate per UTC day. Until a day has been over for a day its mtime is the
// collection bucket, so it is reloaded and overwritten when its events change; after that its
// mtime is fixed, so the engine loads it one last time and then skips it.
func (p *Accounts) cursorDays(req Request) []Candidate {
	bucket := req.Now().UTC().Truncate(req.Interval)
	today := bucket.Truncate(24 * time.Hour)
	var out []Candidate
	for i := range cursorUsageDays + 1 {
		start := today.AddDate(0, 0, -i)
		end := start.AddDate(0, 0, 1)
		mtime := end.Add(24 * time.Hour)
		if bucket.Before(mtime) {
			mtime = bucket
		}
		name := "cursor.usage." + start.Format("20060102") + ".jsonl"
		out = append(out, Candidate{Path: name, RelPath: name, Size: req.Source.MaxFileBytes, MTime: mtime,
			Load: func(ctx context.Context) (Payload, error) { return p.loadCursorDay(ctx, req, start, end, mtime) }})
	}
	return out
}

func (p *Accounts) loadCursorDay(ctx context.Context, req Request, start, end, mtime time.Time) (Payload, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	path := filepath.Join(req.Source.Root, "state.vscdb")
	if !accountPathExists(path) {
		return Payload{}, os.ErrNotExist
	}
	_, token, err := sqliteread.CursorAccount(ctx, path)
	if err != nil {
		return Payload{}, err
	}
	var events []json.RawMessage
	for page := 1; ; page++ {
		body := fmt.Sprintf(`{"startDate":"%d","endDate":"%d","page":%d,"pageSize":500}`, start.UnixMilli(), end.UnixMilli(), page)
		obs := p.cursorCall(ctx, token, "GetFilteredUsageEvents", body, req.Now().UTC())
		if obs.Error != "" {
			// Not shipped: a settled day loads once, so it must never ship incomplete.
			return Payload{}, fmt.Errorf("cursor usage %s page %d: %s", start.Format(time.DateOnly), page, obs.Error)
		}
		var resp struct {
			Total  int               `json:"totalUsageEventsCount"`
			Events []json.RawMessage `json:"usageEventsDisplay"`
		}
		if err := json.Unmarshal(obs.Body, &resp); err != nil {
			return Payload{}, err
		}
		events = append(events, resp.Events...)
		if len(resp.Events) == 0 || len(events) >= resp.Total {
			break
		}
	}
	var raw bytes.Buffer
	// Cursor pages newest first.
	for i := len(events) - 1; i >= 0; i-- {
		// Compact only guarantees one line; Cursor already sends compact JSON.
		if err := json.Compact(&raw, events[i]); err != nil {
			return Payload{}, err
		}
		raw.WriteByte('\n')
	}
	if req.Source.MaxFileBytes > 0 && int64(raw.Len()) > req.Source.MaxFileBytes {
		return Payload{}, fmt.Errorf("cursor usage %s: exceeds file size limit", start.Format(time.DateOnly))
	}
	return Payload{Bytes: raw.Bytes(), MTime: mtime}, nil
}
