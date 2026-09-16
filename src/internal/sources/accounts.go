package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

const accountInterval = 15 * time.Minute
const maxAccountSnapshots = 672
const maxAccountSnapshotBytes = 8 << 20

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
	if req.StateDir == "" {
		return d, nil
	}
	dir := filepath.Join(req.StateDir, "snapshots", req.Source.ID)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return d, err
	}
	agent := strings.TrimSuffix(req.Source.ID, "-account")
	prefix := agent + ".account."
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		if _, err := time.Parse("20060102T150405Z", strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")); err != nil {
			continue
		}
		f, info, err := platform.Open(filepath.Join(dir, name))
		if err != nil {
			return d, err
		}
		f.Close()
		d.Candidates = append(d.Candidates, Candidate{Path: filepath.Join(dir, name), RelPath: name, Size: info.Size(), MTime: info.ModTime()})
	}
	if len(d.Candidates) > 0 {
		d.Health = Collected
	}
	if !req.Capture {
		return d, nil
	}
	if req.Now == nil || req.Scrub == nil || req.Context == nil || req.Env.Home == "" {
		return d, fmt.Errorf("account collection requires clock, scrubber, context and home")
	}
	now := req.Now().UTC()
	bucket := now.Truncate(accountInterval)
	name := prefix + bucket.Format("20060102T150405Z") + ".json"
	var retryAfter time.Time
	if len(d.Candidates) > 0 {
		latest := d.Candidates[len(d.Candidates)-1]
		// Keep the newest file as the durable sampling marker, including across clock rollback.
		if latest.RelPath >= name {
			return d, nil
		}
		raw, _, err := platform.ReadWhole(latest.Path, maxAccountSnapshotBytes)
		if err != nil {
			return d, err
		}
		var previous accountSnapshot
		if err := json.Unmarshal(raw, &previous); err != nil {
			return d, fmt.Errorf("invalid saved account snapshot")
		}
		for _, obs := range previous.Observations {
			if obs.RetryAfter != nil && obs.RetryAfter.After(retryAfter) {
				retryAfter = *obs.RetryAfter
			}
		}
		kept := d.Candidates[:0]
		for _, c := range d.Candidates {
			if c.Path != latest.Path && req.Committed != nil && req.Committed(c) {
				if err := platform.RemoveFile(c.Path); err != nil {
					return d, err
				}
			} else {
				kept = append(kept, c)
			}
		}
		d.Candidates = kept
	}
	var pendingBytes int64
	for _, c := range d.Candidates {
		pendingBytes += c.Size
	}
	if len(d.Candidates) >= maxAccountSnapshots || pendingBytes > (64<<20)-maxAccountSnapshotBytes {
		d.Unreadable = 1
		d.UnreadableReason = "account snapshot backlog full; pending history retained"
		d.Reason = d.UnreadableReason
		return d, nil
	}
	ctx, cancel := context.WithTimeout(req.Context, 30*time.Second)
	defer cancel()
	observations, present := p.collect(ctx, req, retryAfter)
	if !present {
		if len(d.Candidates) == 0 {
			d.Health = AgentAbsent
		}
		return d, nil
	}
	if err := req.Context.Err(); err != nil {
		return d, err
	}
	raw, err := json.Marshal(accountSnapshot{SchemaVersion: 1, BucketStart: bucket, Observations: observations})
	if err != nil {
		return d, err
	}
	raw, err = req.Scrub(raw)
	if err != nil {
		return d, fmt.Errorf("account snapshot scrub failed: %w", err)
	}
	if !json.Valid(raw) || len(raw) > maxAccountSnapshotBytes {
		return d, fmt.Errorf("invalid or oversized account snapshot")
	}
	if err := platform.EnsureDir(dir, 0o700); err != nil {
		return d, err
	}
	path := filepath.Join(dir, name)
	if err := platform.WriteAtomic(path, raw, 0o600); err != nil {
		return d, err
	}
	f, info, err := platform.Open(path)
	if err != nil {
		return d, err
	}
	f.Close()
	d.Candidates = append(d.Candidates, Candidate{Path: path, RelPath: name, Size: info.Size(), MTime: info.ModTime()})
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
