package engine

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

func TestAbandonedStreamClosesSpool(t *testing.T) {
	spool, err := platform.NewSpool()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = spool.WriteString("ciphertext"); err != nil {
		t.Fatal(err)
	}
	pass := sourcePass{fatal: true}
	result, done := pass.stageUpload(fileResult{pending: &pendingPut{spool: spool, size: 10}})
	if !done || !result.outcome.Fatal {
		t.Fatal("object not abandoned")
	}
	if _, err := spool.ReadAt(make([]byte, 1), 0); err == nil {
		t.Fatal("abandoned spool remains open")
	}
}

func TestStreamCiphertextLimitFailsBeforeOversizedWrite(t *testing.T) {
	writer := objectWriter{ctx: context.Background(), w: io.Discard, size: maxStreamObjectBytes - 2}
	if n, err := writer.Write([]byte("123")); n != 0 || err == nil || !strings.Contains(err.Error(), "upload limit") {
		t.Fatalf("oversized ciphertext accepted: %d, %v", n, err)
	}
	if writer.size != maxStreamObjectBytes-2 {
		t.Fatal("failed write changed declared size")
	}
}

func TestUnchangedLargeStreamHashesWithoutScrubbingOrSpooling(t *testing.T) {
	raw := strings.Repeat("{}\n", sources.StreamThreshold/3+1)
	now := time.Now()
	opened, closed := 0, 0
	cand := sources.Candidate{Path: "/session.jsonl", RelPath: "session.jsonl", Size: int64(len(raw)), MTime: now,
		Open: func(context.Context) (sources.Stream, error) {
			opened++
			return sources.Stream{ReadCloser: countedStream{Reader: strings.NewReader(raw), closed: &closed}}, nil
		}}
	fp := Fingerprint{SourceHash: transforms.Hash([]byte(raw)), SourceSize: cand.Size, SourceMTime: now.Add(-time.Hour), NativePath: cand.Path,
		Parked: true, Attempts: 2, LastError: "previous failure"}
	options := Options{Now: func() time.Time { return now }, scrubErr: errors.New("scrubber must not be invoked")}
	// A spool allocation here would fail, independently proving the hash-only path.
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("TMPDIR", missing)
	t.Setenv("TMP", missing)
	t.Setenv("TEMP", missing)
	result, pending := options.prepareFile(context.Background(), fileJob{cand: cand, fp: fp, seen: true}, sources.Resolved{Source: sources.Source{ID: "claude-code-transcripts"}}, sources.Discovery{}, false)
	if pending != nil || result.outcome.Decision != "unchanged" || result.intent.kind != intentRefresh {
		t.Fatalf("not hash-only: %+v", result)
	}
	if opened != 1 || closed != 1 || result.outcome.BytesIn != cand.Size {
		t.Fatalf("open/close=%d/%d bytes=%d", opened, closed, result.outcome.BytesIn)
	}
	refreshed := result.intent.fp
	if refreshed.Parked || refreshed.Attempts != 0 || refreshed.LastError != "" || !refreshed.SourceMTime.Equal(cand.MTime) {
		t.Fatalf("fingerprint not refreshed: %+v", refreshed)
	}
}

type countedStream struct {
	io.Reader
	closed *int
}

func (s countedStream) Close() error { *s.closed++; return nil }
