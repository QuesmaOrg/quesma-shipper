package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

type Accounts struct {
	client   *http.Client
	keychain func(context.Context, string) ([]byte, error)
}

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
	if req.Interval <= 0 {
		return d, fmt.Errorf("account collection requires a positive interval")
	}
	bucket := req.Now().UTC().Truncate(req.Interval)
	name := strings.TrimSuffix(req.Source.ID, "-account") + ".account." + bucket.Format("20060102T150405Z") + ".jsonl"
	// The size bound stays stable within a bucket and reserves memory before loading.
	d.Candidates = []Candidate{{Path: name, RelPath: name, Size: req.Source.MaxFileBytes, MTime: bucket,
		Load: func(ctx context.Context) (Payload, error) { return p.load(ctx, req, bucket) },
	}}
	d.Health = Collected
	return d, nil
}

func (p *Accounts) load(ctx context.Context, req Request, bucket time.Time) (Payload, error) {
	if err := ctx.Err(); err != nil {
		return Payload{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	observations, present := p.collect(ctx, req)
	if !present {
		return Payload{}, os.ErrNotExist
	}
	if err := ctx.Err(); err != nil {
		return Payload{}, err
	}
	payload := Payload{MTime: bucket}
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	for _, obs := range observations {
		if err := encoder.Encode(struct {
			BucketStart time.Time `json:"bucket_start"`
			accountObservation
		}{bucket, obs}); err != nil {
			return Payload{}, err
		}
		if obs.Error != "" {
			payload.Warning = "partial account snapshot: " + obs.Source + ": " + obs.Error
		}
	}
	if req.Source.MaxFileBytes > 0 && int64(raw.Len()) > req.Source.MaxFileBytes {
		return Payload{}, fmt.Errorf("%w: generated content exceeds file size limit", platform.ErrTooLarge)
	}
	payload.Bytes = raw.Bytes()
	return payload, nil
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
