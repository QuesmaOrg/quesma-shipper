package sources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// CursorUsage collects the signed-in user's own Cursor requests: fetched without a teamId, which
// returns only that user's requests, even to a team owner. One object per account and UTC day, one
// event per line as Cursor sent it, oldest first, so a new request appends a line and ingest stores only it.
type CursorUsage struct{ accounts Accounts }

const (
	cursorUsageDays     = 90
	cursorUsageAttempts = 3
)

func (u *CursorUsage) Discover(req Request) (Discovery, error) {
	d, bucket, ready, err := generatedDiscovery(req, "cursor usage")
	if !ready {
		return d, err
	}
	// Named after the account, so after a switch the days are new objects, not the old account's
	// settled days, which the engine would skip by stat.
	account, _, err := cursorUsageAccount(req.Context, req)
	if err != nil || account == "" {
		d.Reason = "no signed-in Cursor account"
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
		name := "cursor.usage." + account + "." + start.Format("20060102") + ".jsonl"
		d.Candidates = append(d.Candidates, Candidate{Path: name, RelPath: name, Size: req.Source.MaxFileBytes, MTime: mtime,
			Load: func(ctx context.Context) (Payload, error) { return u.load(ctx, req, account, start, end, mtime) }})
	}
	d.Health = Collected
	return d, nil
}

// cursorUsageAccount is a short hash of the access token's sub claim, so no email or user id lands
// in an object name; empty when nobody is signed in.
func cursorUsageAccount(ctx context.Context, req Request) (account, token string, err error) {
	_, token, present, err := cursorAccount(ctx, req)
	if !present || err != nil || token == "" {
		return "", "", err
	}
	parts := strings.Split(token, ".")
	var claims struct {
		Sub string `json:"sub"`
	}
	if len(parts) != 3 {
		return "", "", fmt.Errorf("cursor access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil || claims.Sub == "" {
		return "", "", fmt.Errorf("cursor access token carries no account id")
	}
	sum := sha256.Sum256([]byte(claims.Sub))
	return hex.EncodeToString(sum[:6]), token, nil
}

func (u *CursorUsage) load(ctx context.Context, req Request, account string, start, end, mtime time.Time) (Payload, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	current, token, err := cursorUsageAccount(ctx, req)
	if err != nil {
		return Payload{}, err
	}
	if current != account {
		return Payload{}, fmt.Errorf("cursor account changed since discovery")
	}
	events, err := u.fetchEvents(ctx, token, start, end, req.Source.MaxFileBytes)
	if err != nil {
		return Payload{}, err
	}
	raw, err := cursorDayLines(events)
	if err != nil {
		return Payload{}, err
	}
	return Payload{Bytes: raw, MTime: mtime}, nil
}

// fetchEvents returns one day's events in Cursor's order, newest first. Any doubt fails the day
// rather than shipping it short, because a settled day loads once.
func (u *CursorUsage) fetchEvents(ctx context.Context, token string, start, end time.Time, limit int64) ([]json.RawMessage, error) {
	for range cursorUsageAttempts {
		events, consistent, err := u.readPages(ctx, token, start, end, limit)
		if err != nil || consistent {
			return events, err
		}
	}
	return nil, fmt.Errorf("cursor usage %s changed while it was read, %d times", start.Format(time.DateOnly), cursorUsageAttempts)
}

// readPages reads every page once. Pages are offsets into a live, newest-first list, so a request
// arriving mid-read shifts them; the read is consistent only if every page reported the same total.
func (u *CursorUsage) readPages(ctx context.Context, token string, start, end time.Time, limit int64) ([]json.RawMessage, bool, error) {
	var events []json.RawMessage
	var size int64
	total := -1
	for page := 1; ; page++ {
		body := fmt.Sprintf(`{"startDate":"%d","endDate":"%d","page":%d,"pageSize":500}`, start.UnixMilli(), end.UnixMilli(), page)
		obs := u.accounts.cursorCall(ctx, token, "GetFilteredUsageEvents", body, accountObservation{})
		if obs.Error != "" {
			return nil, false, fmt.Errorf("cursor usage %s page %d: %s", start.Format(time.DateOnly), page, obs.Error)
		}
		var resp struct {
			Total  int               `json:"totalUsageEventsCount"`
			Events []json.RawMessage `json:"usageEventsDisplay"`
		}
		if err := json.Unmarshal(obs.Body, &resp); err != nil {
			return nil, false, err
		}
		if total == -1 {
			total = resp.Total
		} else if resp.Total != total {
			return nil, false, nil
		}
		for _, e := range resp.Events {
			size += int64(len(e)) + 1
		}
		if limit > 0 && size > limit {
			return nil, false, fmt.Errorf("%w: cursor usage %s exceeds file size limit", platform.ErrTooLarge, start.Format(time.DateOnly))
		}
		events = append(events, resp.Events...)
		if len(events) >= total {
			return events, len(events) == total, nil
		}
		if len(resp.Events) == 0 {
			return nil, false, fmt.Errorf("cursor usage %s ran out at %d of %d events", start.Format(time.DateOnly), len(events), total)
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
