package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const accountInterval = 15 * time.Minute

type Accounts struct {
	client     *http.Client
	retryAfter map[string]time.Time
	keychain   func(context.Context, string) ([]byte, error)
}

func (*Accounts) Name() string { return "account" }

type accountObservation struct {
	Source     string          `json:"source"`
	ObservedAt time.Time       `json:"observed_at"`
	HTTPStatus int             `json:"http_status,omitempty"`
	Error      string          `json:"error,omitempty"`
	RetryAfter *time.Time      `json:"retry_after,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
}

type accountSnapshot struct {
	SchemaVersion int                  `json:"schema_version"`
	BucketStart   time.Time            `json:"bucket_start"`
	Observations  []accountObservation `json:"observations"`
}

func (p *Accounts) Discover(req Request) (Discovery, error) {
	d := Discovery{Health: RootPresentNoMatch, Sniff: SniffOK}
	if !req.Capture {
		return d, nil
	}
	if req.Now == nil || req.Context == nil || req.Env.Home == "" {
		return d, fmt.Errorf("account collection requires clock, context and home")
	}
	bucket := req.Now().UTC().Truncate(accountInterval)
	name := strings.TrimSuffix(req.Source.ID, "-account") + ".account." + bucket.Format("20060102T150405Z") + ".json"
	ctx, cancel := context.WithTimeout(req.Context, 30*time.Second)
	defer cancel()
	observations, present := p.collect(ctx, req, p.retryAfter[req.Source.ID])
	if !present {
		d.Health = AgentAbsent
		return d, nil
	}
	if err := req.Context.Err(); err != nil {
		return d, err
	}
	raw, err := json.Marshal(accountSnapshot{SchemaVersion: 1, BucketStart: bucket, Observations: observations})
	if err != nil {
		return d, err
	}
	d.Candidates = []Candidate{{Path: name, RelPath: name, Size: int64(len(raw)), MTime: bucket, Content: raw}}
	d.Health = Collected
	for _, obs := range observations {
		if obs.RetryAfter != nil {
			if p.retryAfter == nil {
				p.retryAfter = make(map[string]time.Time)
			}
			p.retryAfter[req.Source.ID] = *obs.RetryAfter
		}
		if obs.Error != "" {
			d.Unreadable++
			d.UnreadableReason = obs.Source + ": " + obs.Error
		}
	}
	if d.Unreadable > 0 {
		d.Reason = "partial account snapshot: " + d.UnreadableReason
	}
	return d, nil
}
