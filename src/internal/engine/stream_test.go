package engine_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

func TestLargeTranscriptUsesReplayableSpoolAndCommitsAfterUpload(t *testing.T) {
	f := newFixture(t)
	line := `{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}` + "\r\n"
	raw := strings.Repeat(line, sources.StreamThreshold/len(line)+1) + `{"tail":"alice@example.com`
	path := f.writeTranscript("p/large.jsonl", raw)
	var saved engine.PreparedObject
	var first []byte
	f.port.verdict = func(call, _ int, obj engine.PreparedObject) error {
		if obj.BodyReader == nil || obj.Body != nil {
			t.Error("large object was buffered")
		}
		data, err := io.ReadAll(obj.Reader())
		if err != nil {
			t.Error(err)
			return err
		}
		if int64(len(data)) != obj.Size() {
			t.Error("incorrect signed length")
		}
		if call == 0 {
			first = data
			saved = obj
			return engine.ErrTicketExpired
		}
		if !bytes.Equal(data, first) {
			t.Error("reauthorization changed ciphertext")
		}
		return nil
	}
	rep := f.run()
	if rep.Shipped != 1 || f.port.calls != 2 {
		t.Fatalf("report %+v, calls %d", rep, f.port.calls)
	}
	if _, err := saved.Reader().Read(make([]byte, 1)); err == nil {
		t.Fatal("spool still open after upload")
	}
	stored, _ := f.port.get(f.port.keys()[0])
	_, plain, err := transforms.Open(stored.Body, f.unit.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plain, []byte(`{"tail":`)) || bytes.Contains(plain, []byte("alice@example.com")) {
		t.Fatal("tail missing or not scrubbed")
	}
	f.port.verdict = nil
	if rep := f.run(); rep.Unchanged != 1 {
		t.Fatalf("successful stream not committed: %+v", rep)
	}
	if err := os.WriteFile(path, append([]byte(raw), []byte("\n{}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if rep := f.run(); rep.Shipped != 1 {
		t.Fatalf("append not shipped: %+v", rep)
	}
}

func TestLargeTranscriptFailuresLeaveNoCommittedUploadOrSpool(t *testing.T) {
	for _, kind := range []string{"upload", "record", "dry run", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			t.Setenv("TMP", tmp)
			t.Setenv("TEMP", tmp)
			f := newFixture(t)
			line := `{"text":"hello"}` + "\n"
			raw := strings.Repeat(line, sources.StreamThreshold/len(line)+1)
			if kind == "record" {
				raw = strings.Repeat("a", transforms.MaxStreamRecordBytes+1)
			}
			f.writeTranscript("p/large.jsonl", raw)
			var saved engine.PreparedObject
			if kind == "upload" {
				f.port.verdict = func(_, _ int, obj engine.PreparedObject) error { saved = obj; return errors.New("PUT failed") }
			}
			o := f.opts()
			o.DryRun = kind == "dry run"
			ctx := context.Background()
			if kind == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			rep, err := engine.Run(ctx, f.store, o)
			if kind != "cancel" && err != nil {
				t.Fatal(err)
			}
			if len(f.port.keys()) != 0 {
				t.Fatal("failure or preview uploaded an object")
			}
			if kind == "record" && (rep.Parked != 1 || !strings.Contains(rep.Sources[0].Files[0].Reason, "record exceeds")) {
				t.Fatalf("record cap not explicit: %+v", rep)
			}
			if saved.BodyReader != nil {
				if _, err := saved.Reader().Read(make([]byte, 1)); err == nil {
					t.Fatal("failed upload leaked spool")
				}
			}
			leftovers, err := filepath.Glob(filepath.Join(tmp, "quesma-spool-*"))
			if err != nil || len(leftovers) > 0 {
				t.Fatalf("spools remain: %v, %v", leftovers, err)
			}
			if kind == "upload" {
				f.port.verdict = nil
				if rep := f.run(); rep.Shipped != 1 {
					t.Fatal("failed upload was committed")
				}
			}
		})
	}
}

// An opt-in subprocess probe lets /usr/bin/time measure the full pipeline without retaining uploaded ciphertext.
func TestLargeTranscriptMemoryProbe(t *testing.T) {
	path := os.Getenv("SHIPPER_STREAM_PROBE_FILE")
	if path == "" {
		t.Skip("set SHIPPER_STREAM_PROBE_FILE to a private JSONL fixture")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.eff.Sources[0].Root = filepath.Dir(path)
	f.eff.Sources[0].Include = []string{filepath.Base(path)}
	f.eff.Sources[0].MaxFileBytes = info.Size() + 1
	rep := f.runWith(func(o *engine.Options) { o.Upload = discardStreamPort{t: t}; o.Workers = 1; o.UploadWorkers = 1 })
	if rep.Shipped != 1 {
		t.Fatalf("expected complete upload, got %+v", rep)
	}
	t.Logf("input=%d uploaded=%d", rep.Sources[0].Files[0].BytesIn, rep.Sources[0].Files[0].BytesOut)
}

type discardStreamPort struct{ t *testing.T }

func (p discardStreamPort) AuthorizeAndUpload(ctx context.Context, objects []engine.PreparedObject) []error {
	out := make([]error, len(objects))
	for i, obj := range objects {
		if obj.BodyReader == nil {
			p.t.Error("memory probe did not use streaming")
		}
		n, err := io.Copy(io.Discard, obj.Reader())
		out[i] = err
		if n != obj.Size() {
			p.t.Errorf("body length %d != %d", n, obj.Size())
		}
	}
	return out
}

func TestLargeTranscriptTouchAndSameSizeMutation(t *testing.T) {
	f := newFixture(t)
	line := `{"text":"hello"}` + "\n"
	raw := strings.Repeat(line, sources.StreamThreshold/len(line)+1)
	path := f.writeTranscript("p/large.jsonl", raw)
	if rep := f.run(); rep.Shipped != 1 {
		t.Fatalf("initial upload: %+v", rep)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	touched := info.ModTime().Add(time.Second)
	if err := os.Chtimes(path, touched, touched); err != nil {
		t.Fatal(err)
	}
	rep := f.run()
	if rep.Unchanged != 1 || rep.Sources[0].Files[0].BytesIn != int64(len(raw)) {
		t.Fatalf("touch did not hash without uploading: %+v", rep)
	}
	if rep := f.run(); rep.Unchanged != 1 || rep.Sources[0].Files[0].BytesIn != 0 {
		t.Fatalf("touch fingerprint not refreshed: %+v", rep)
	}
	mutated := strings.Replace(raw, "hello", "world", 1)
	if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := touched.Add(time.Second)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	if rep := f.run(); rep.Shipped != 1 {
		t.Fatalf("same-size mutation did not ship: %+v", rep)
	}
	stored, _ := f.port.get(f.port.keys()[0])
	_, body, err := transforms.Open(stored.Body, f.unit.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Versions != 2 || !bytes.HasPrefix(body, []byte(`{"text":"world"}`)) {
		t.Fatal("same-size mutation uploaded stale data")
	}
}
