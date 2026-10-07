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
	d.Candidates = []Candidate{{Path: name, RelPath: name, Series: req.Source.ID, Size: req.Source.MaxFileBytes, MTime: bucket,
		Load: func(ctx context.Context) (Payload, error) { return p.load(ctx, req, bucket) },
	}}
	if req.Source.ID == "cursor-account" {
		d.Candidates = append(d.Candidates, p.cursorHistory(req)...)
	}
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
	var raw, compare bytes.Buffer
	encoder, compareEncoder := json.NewEncoder(&raw), json.NewEncoder(&compare)
	// The UTC day joins the comparison, so an unchanged account still ships once a day.
	compare.WriteString(bucket.Format(time.DateOnly) + "\n")
	for _, obs := range observations {
		if err := encoder.Encode(struct {
			BucketStart time.Time `json:"bucket_start"`
			accountObservation
		}{bucket, obs}); err != nil {
			return Payload{}, err
		}
		if err := compareEncoder.Encode(forComparison(obs)); err != nil {
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
	payload.Compare = compare.Bytes()
	return payload, nil
}

// forComparison drops what changes on every read of an idle account, as seen on live providers.
func forComparison(obs accountObservation) accountObservation {
	obs.ObservedAt = time.Time{}
	decoder := json.NewDecoder(bytes.NewReader(obs.Body))
	decoder.UseNumber()
	var body any
	if len(obs.Body) == 0 || decoder.Decode(&body) != nil {
		return obs
	}
	if normalized, err := json.Marshal(withoutReadNoise(body)); err == nil {
		obs.Body = normalized
	}
	return obs
}

func withoutReadNoise(v any) any {
	switch v := v.(type) {
	case map[string]any:
		delete(v, "as_of")
		delete(v, "reset_after_seconds")
		for k, child := range v {
			v[k] = withoutReadNoise(child)
		}
	case []any:
		for i, child := range v {
			v[i] = withoutReadNoise(child)
		}
	case string:
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t.Round(time.Minute).Format(time.RFC3339)
		}
	}
	return v
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
