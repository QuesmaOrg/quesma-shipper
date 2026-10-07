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
// requests, even to a team owner. Each snapshot carries the trailing week; closed months come
// once each from cursorHistory.

const cursorHistoryMonths = 12

func (p *Accounts) collectCursor(ctx context.Context, req Request) ([]accountObservation, bool) {
	var out []accountObservation
	path := filepath.Join(req.Source.Root, "state.vscdb")
	if !accountPathExists(path) {
		return nil, false
	}
	values, token, err := sqliteread.CursorAccount(ctx, path)
	body, _ := json.Marshal(values)
	out = append(out, localAccount(req, "cursor.local.account", body, err))
	now := req.Now().UTC()
	usage := fmt.Sprintf(`{"startDate":"%d","endDate":"%d","page":1,"pageSize":1000}`, now.Add(-7*24*time.Hour).UnixMilli(), now.UnixMilli())
	for _, call := range []struct{ endpoint, body string }{{"GetPlanInfo", "{}"}, {"GetCurrentPeriodUsage", "{}"}, {"GetFilteredUsageEvents", usage}} {
		out = append(out, p.cursorCall(ctx, token, call.endpoint, call.body, now))
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

// cursorHistory is one candidate per closed month. Its size and mtime never change, so the engine
// loads each month once and skips it after it ships; a failed load is retried with backoff.
func (p *Accounts) cursorHistory(req Request) []Candidate {
	now := req.Now().UTC()
	end := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if now.Sub(end) < 24*time.Hour {
		// The trailing week still covers the month that just ended.
		end = end.AddDate(0, -1, 0)
	}
	var out []Candidate
	for range cursorHistoryMonths {
		start, stop := end.AddDate(0, -1, 0), end
		name := "cursor.usage." + start.Format("200601") + ".jsonl"
		out = append(out, Candidate{Path: name, RelPath: name, Size: req.Source.MaxFileBytes, MTime: stop,
			Load: func(ctx context.Context) (Payload, error) { return p.loadCursorMonth(ctx, req, start, stop) }})
		end = start
	}
	return out
}

func (p *Accounts) loadCursorMonth(ctx context.Context, req Request, start, end time.Time) (Payload, error) {
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
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	for page, fetched := 1, 0; ; page++ {
		body := fmt.Sprintf(`{"startDate":"%d","endDate":"%d","page":%d,"pageSize":500}`, start.UnixMilli(), end.UnixMilli(), page)
		obs := p.cursorCall(ctx, token, "GetFilteredUsageEvents", body, req.Now().UTC())
		if obs.Error != "" {
			// Not shipped: a month ships once, so it must not ship incomplete.
			return Payload{}, fmt.Errorf("cursor usage %s page %d: %s", start.Format("2006-01"), page, obs.Error)
		}
		var resp struct {
			Total  int               `json:"totalUsageEventsCount"`
			Events []json.RawMessage `json:"usageEventsDisplay"`
		}
		if err := json.Unmarshal(obs.Body, &resp); err != nil {
			return Payload{}, err
		}
		if err := encoder.Encode(struct {
			BucketStart time.Time `json:"bucket_start"`
			accountObservation
		}{start, obs}); err != nil {
			return Payload{}, err
		}
		if req.Source.MaxFileBytes > 0 && int64(raw.Len()) > req.Source.MaxFileBytes {
			return Payload{}, fmt.Errorf("cursor usage %s: exceeds file size limit", start.Format("2006-01"))
		}
		if fetched += len(resp.Events); len(resp.Events) == 0 || fetched >= resp.Total {
			break
		}
	}
	return Payload{Bytes: raw.Bytes(), MTime: end}, nil
}
