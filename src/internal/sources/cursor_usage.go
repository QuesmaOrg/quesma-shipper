package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// CursorUsage collects the signed-in user's own Cursor requests: fetched without a teamId, which
// returns only that user's requests, even to a team owner. One object per UTC day, one event per
// line as Cursor sent it, oldest first, so a new request appends a line and ingest stores only it.
type CursorUsage struct{ accounts Accounts }

const cursorUsageDays = 90

func (u *CursorUsage) Discover(req Request) (Discovery, error) {
	d, bucket, ready, err := generatedDiscovery(req, "cursor usage")
	if !ready {
		return d, err
	}
	today := bucket.Truncate(24 * time.Hour)
	for i := range cursorUsageDays + 1 {
		start := today.AddDate(0, 0, -i)
		end := start.AddDate(0, 0, 1)
		// Live until a day after it ends, then fixed: one last load, then skipped. Not AlwaysLoad, which
		// would call Cursor on every pass, and a manual sync can run more often than the interval.
		mtime := end.Add(24 * time.Hour)
		if bucket.Before(mtime) {
			mtime = bucket
		}
		name := "cursor.usage." + start.Format("20060102") + ".jsonl"
		d.Candidates = append(d.Candidates, Candidate{Path: name, RelPath: name, Size: req.Source.MaxFileBytes, MTime: mtime,
			Load: func(ctx context.Context) (Payload, error) { return u.load(ctx, req, start, end, mtime) }})
	}
	d.Health = Collected
	return d, nil
}

func (u *CursorUsage) load(ctx context.Context, req Request, start, end, mtime time.Time) (Payload, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, token, present, err := cursorAccount(ctx, req)
	if !present {
		return Payload{}, os.ErrNotExist
	}
	if err != nil {
		return Payload{}, err
	}
	events, err := u.fetchEvents(ctx, token, start, end)
	if err != nil {
		return Payload{}, err
	}
	raw, err := cursorDayLines(events)
	if err != nil {
		return Payload{}, err
	}
	if req.Source.MaxFileBytes > 0 && int64(len(raw)) > req.Source.MaxFileBytes {
		return Payload{}, fmt.Errorf("%w: cursor usage %s exceeds file size limit", platform.ErrTooLarge, start.Format(time.DateOnly))
	}
	return Payload{Bytes: raw, MTime: mtime}, nil
}

// fetchEvents returns one day's events in Cursor's order, newest first. A failed page fails the
// day: a settled day loads once, so it must never ship incomplete.
func (u *CursorUsage) fetchEvents(ctx context.Context, token string, start, end time.Time) ([]json.RawMessage, error) {
	var events []json.RawMessage
	for page := 1; ; page++ {
		body := fmt.Sprintf(`{"startDate":"%d","endDate":"%d","page":%d,"pageSize":500}`, start.UnixMilli(), end.UnixMilli(), page)
		obs := u.accounts.cursorCall(ctx, token, "GetFilteredUsageEvents", body, accountObservation{})
		if obs.Error != "" {
			return nil, fmt.Errorf("cursor usage %s page %d: %s", start.Format(time.DateOnly), page, obs.Error)
		}
		var resp struct {
			Total  int               `json:"totalUsageEventsCount"`
			Events []json.RawMessage `json:"usageEventsDisplay"`
		}
		if err := json.Unmarshal(obs.Body, &resp); err != nil {
			return nil, err
		}
		events = append(events, resp.Events...)
		if len(resp.Events) == 0 || len(events) >= resp.Total {
			return events, nil
		}
	}
}

// cursorDayLines writes events oldest first, one per line; Compact only guarantees the one line.
func cursorDayLines(newestFirst []json.RawMessage) ([]byte, error) {
	var raw bytes.Buffer
	for i := len(newestFirst) - 1; i >= 0; i-- {
		if err := json.Compact(&raw, newestFirst[i]); err != nil {
			return nil, err
		}
		raw.WriteByte('\n')
	}
	return raw.Bytes(), nil
}
