package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const accountInterval = 15 * time.Minute

type Accounts struct {
	client   *http.Client
	keychain func(context.Context, string) ([]byte, error)
}

func (*Accounts) Name() string { return "account" }

type accountObservation struct {
	Source     string          `json:"source"`
	ObservedAt time.Time       `json:"observed_at"`
	HTTPStatus int             `json:"http_status,omitempty"`
	Error      string          `json:"error,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
}

func (p *Accounts) Discover(req Request) (Discovery, error) {
	d := Discovery{Health: RootPresentNoMatch, Sniff: SniffOK}
	if req.Source.Root == "" {
		d.Health = AgentAbsent
		return d, nil
	}
	if !req.Capture {
		d.Deferred = true
		d.Reason = "configured; checked during collection"
		return d, nil
	}
	if req.Now == nil || req.Context == nil || req.Env.Home == "" {
		return d, fmt.Errorf("account collection requires clock, context and home")
	}
	bucket := req.Now().UTC().Truncate(accountInterval)
	name := strings.TrimSuffix(req.Source.ID, "-account") + ".account." + bucket.Format("20060102T150405Z") + ".jsonl"
	ctx, cancel := context.WithTimeout(req.Context, 30*time.Second)
	defer cancel()
	observations, present := p.collect(ctx, req)
	if !present {
		d.Health = AgentAbsent
		return d, nil
	}
	if err := req.Context.Err(); err != nil {
		return d, err
	}
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	for _, obs := range observations {
		if err := encoder.Encode(struct {
			BucketStart time.Time `json:"bucket_start"`
			accountObservation
		}{bucket, obs}); err != nil {
			return d, err
		}
	}
	d.Candidates = []Candidate{{Path: name, RelPath: name, Size: int64(raw.Len()), MTime: bucket, Content: raw.Bytes()}}
	d.Health = Collected
	for _, obs := range observations {
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

func (p *Accounts) collect(ctx context.Context, req Request) ([]accountObservation, bool) {
	switch req.Source.ID {
	case "claude-account":
		return p.collectClaude(ctx, req)
	case "codex-account":
		return p.collectCodex(ctx, req)
	case "cursor-account":
		return p.collectCursor(ctx, req)
	default:
		return []accountObservation{localAccount(req, "account", nil, fmt.Errorf("unsupported account source"))}, true
	}
}
