package engine_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

func TestOversizedRecordStreamsWithinConfiguredFileCap(t *testing.T) {
	prefix := "{\"text\":\"before alice@example.com\"}\r\n"
	large := `{"message":{"content":[{"type":"image","source":{"data":"` + strings.Repeat("A", transforms.StreamRecordThreshold+1) + `"}}]}}` + "\r\n"
	raw := prefix + large + `{"tail":"alice@example.com`
	s, err := transforms.New(transforms.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	want, err := s.Scrub([]byte(raw), transforms.Hint{Family: "claude-code", JSONL: true})
	if err != nil {
		t.Fatal(err)
	}
	sourceHash := transforms.Hash([]byte(raw))
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "zstd"}[compressed], func(t *testing.T) {
			f := newFixture(t)
			f.eff.Sources[0].MaxFileBytes = int64(len(raw))
			f.eff.Sources[0].Include = []string{"projects/**/*.jsonl*"}
			name, body := "p/large.jsonl", raw
			if compressed {
				name += ".zst"
				body = string(zstdOf(t, []byte(raw)))
			}
			f.writeTranscript(name, body)
			f.port.verdict = func(_, _ int, obj engine.PreparedObject) error {
				if obj.BodyReader == nil || obj.Body != nil {
					t.Error("oversized record fell back to whole-file buffering")
				}
				return nil
			}
			if compressed {
				f.eff.Sources[0].MaxFileBytes--
				rep := f.run()
				if rep.Parked != 1 || len(f.port.keys()) != 0 {
					t.Fatalf("decoded whole-file cap was not enforced: %+v", rep)
				}
				f.eff.Sources[0].MaxFileBytes++
			}
			rep := f.runWith(func(o *engine.Options) {
				o.Now = func() time.Time { return engineFixedTime().Add(2 * time.Minute) }
			})
			if rep.Shipped != 1 {
				t.Fatalf("record within the configured file cap did not ship: %+v", rep)
			}
			stored, _ := f.port.get(f.port.keys()[0])
			manifest, plain, err := transforms.Open(stored.Body, f.unit.Identity)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(plain, want.Out.Bytes()) || manifest.SourceHash != sourceHash || manifest.ShippedHash != transforms.Hash(plain) {
				t.Fatal("exceptional record changed payload or hashes")
			}
			if manifest.Redaction == nil || manifest.Redaction.Density != want.Density() || manifest.Redaction.ScanMode != want.ScanMode || !reflect.DeepEqual(manifest.Redaction.RuleHits, want.RuleHits) {
				t.Fatalf("exceptional record changed redaction accounting: %+v", manifest.Redaction)
			}
			if rep := f.run(); rep.Unchanged != 1 {
				t.Fatalf("successful upload was not committed: %+v", rep)
			}
		})
	}
}

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
				raw = strings.Repeat("a", transforms.StreamRecordThreshold+1)
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
			if kind == "record" && (rep.Parked != 1 || !strings.Contains(rep.Sources[0].Files[0].Reason, "positive max_file_bytes")) {
				t.Fatalf("record cap not explicit: %+v", rep)
			}
			if saved.BodyReader != nil {
				if _, err := saved.Reader().Read(make([]byte, 1)); err == nil {
					t.Fatal("failed upload leaked spool")
				}
			}
			for _, dir := range []string{tmp, filepath.Join(f.stateDir, "spool")} {
				leftovers, err := filepath.Glob(filepath.Join(dir, "quesma-spool-*"))
				if err != nil || len(leftovers) > 0 {
					t.Fatalf("spools remain in %s: %v, %v", dir, leftovers, err)
				}
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

// A .zst with no max_file_bytes is refused on both paths before anything decodes, as on main:
// nothing else bounds what a few KB of crafted frame decode to in the spool.
func TestUncappedZstdIsParkedWithoutSpool(t *testing.T) {
	f := newFixture(t)
	f.eff.Sources[0].Include = append(f.eff.Sources[0].Include, "projects/**/*.jsonl.zst")
	f.eff.Sources[0].MaxFileBytes = 0
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	small := enc.EncodeAll([]byte(strings.Repeat(`{"text":"hello"}`+"\n", 100)), nil)
	// Random text barely compresses, so this one is over the threshold on disk and takes the stream path.
	var large strings.Builder
	chunk := make([]byte, 3<<10)
	for large.Len() < sources.StreamThreshold*3/2 {
		rand.Read(chunk)
		large.WriteString(`{"r":"` + base64.StdEncoding.EncodeToString(chunk) + `"}` + "\n")
	}
	big := enc.EncodeAll([]byte(large.String()), nil)
	if len(big) <= sources.StreamThreshold {
		t.Fatalf("fixture compressed to %d bytes, not over the stream threshold", len(big))
	}
	f.writeTranscript("p/small.jsonl.zst", string(small))
	f.writeTranscript("p/large.jsonl.zst", string(big))

	rep := f.run()
	if rep.Parked != 2 || len(f.port.keys()) != 0 {
		t.Fatalf("uncapped zstd not refused: %+v", rep)
	}
	for _, file := range rep.Sources[0].Files {
		if !strings.Contains(file.Reason, "no max_file_bytes") {
			t.Errorf("%s: %q", file.RelPath, file.Reason)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(f.stateDir, "spool", "quesma-spool-*")); len(left) > 0 {
		t.Fatalf("spool created for a refused file: %v", left)
	}
}

// A crash leaves spool directories behind on Windows; the next run, holding the state lock, removes them.
func TestRunSweepsStaleSpools(t *testing.T) {
	f := newFixture(t)
	stale := filepath.Join(f.stateDir, "spool", "quesma-spool-crashed")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	f.run()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale spool survived a run: %v", err)
	}
}
