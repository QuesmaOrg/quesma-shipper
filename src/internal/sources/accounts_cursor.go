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

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
)

// Usage events are fetched without a teamId: Cursor then returns only the signed-in user's
// requests, even to a team owner. Each UTC day is one object, one event per line as Cursor sent
// it, oldest first: a new request appends a line, which ingest stores without rewriting the day.

const cursorUsageDays = 90

func (p *Accounts) collectCursor(ctx context.Context, req Request) ([]accountObservation, bool) {
	var out []accountObservation
	values, token, present, err := cursorAccount(ctx, req)
	if !present {
		return nil, false
	}
	body, _ := json.Marshal(values)
	out = append(out, localAccount(req, "cursor.local.account", body, err))
	for _, endpoint := range []string{"GetPlanInfo", "GetCurrentPeriodUsage"} {
		obs := p.cursorCall(ctx, token, endpoint, "{}")
		obs.ObservedAt = req.Now().UTC()
		out = append(out, obs)
	}
	return out, true
}

func cursorAccount(ctx context.Context, req Request) (map[string]json.RawMessage, string, bool, error) {
	path := filepath.Join(req.Source.Root, "state.vscdb")
	if !accountPathExists(path) {
		return nil, "", false, nil
	}
	values, token, err := sqliteread.CursorAccount(ctx, path)
	return values, token, true, err
}

func (p *Accounts) cursorCall(ctx context.Context, token, endpoint, body string) accountObservation {
	obs := accountObservation{Source: "cursor.dashboard." + endpoint}
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

// A day's mtime follows the bucket until a day after it ends, then is fixed: one last load, then skipped.
func (p *Accounts) cursorDays(req Request, bucket time.Time) []Candidate {
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
	_, token, present, err := cursorAccount(ctx, req)
	if !present {
		return Payload{}, os.ErrNotExist
	}
	if err != nil {
		return Payload{}, err
	}
	var events []json.RawMessage
	for page := 1; ; page++ {
		body := fmt.Sprintf(`{"startDate":"%d","endDate":"%d","page":%d,"pageSize":500}`, start.UnixMilli(), end.UnixMilli(), page)
		obs := p.cursorCall(ctx, token, "GetFilteredUsageEvents", body)
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
		return Payload{}, fmt.Errorf("%w: cursor usage %s exceeds file size limit", platform.ErrTooLarge, start.Format(time.DateOnly))
	}
	return Payload{Bytes: raw.Bytes(), MTime: mtime}, nil
}
