package sources

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

func TestFileStreamSnapshotLimitsAndCancellation(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "zstd"}[compressed], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			raw := []byte(strings.Repeat("{}\r\n", 100))
			body := raw
			if compressed {
				path += ".zst"
				body = compress(t, raw)
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			stream, err := fileStream(path, int64(len(raw)))(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(stream)
			stream.Close()
			if err != nil || string(got) != string(raw) {
				t.Fatalf("round trip: %v", err)
			}
			stream, err = fileStream(path, int64(len(raw)-1))(context.Background())
			if err == nil {
				_, err = io.ReadAll(stream)
				stream.Close()
			}
			if !errors.Is(err, platform.ErrTooLarge) {
				t.Fatalf("cap not enforced: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			stream, err = fileStream(path, 0)(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			_, err = io.ReadAll(stream)
			stream.Close()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "append.jsonl")
	os.WriteFile(path, []byte("{}\n"), 0o600)
	stream, err := fileStream(path, 0)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("{}\n")
	f.Close()
	got, err := io.ReadAll(stream)
	if err != nil || string(got) != "{}\n" {
		t.Fatalf("append changed snapshot: %q, %v", got, err)
	}
}

func TestFileStreamRejectsTruncatedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncate.jsonl")
	if err := os.WriteFile(path, []byte("{}\n{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stream, err := fileStream(path, 0)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := os.Truncate(path, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(stream); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated snapshot accepted: %v", err)
	}
}
