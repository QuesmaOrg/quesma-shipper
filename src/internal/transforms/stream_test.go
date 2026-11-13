package transforms

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

func TestStreamingScrubMatchesWholeFile(t *testing.T) {
	s, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		"", " \r\n\n",
		strings.Repeat(`{"type":"user","message":{"content":[{"type":"text","text":"hello world"}]}}`+"\r\n", 15000) + "  \r\n" + `{"api_key":"sensitive-value"}` + "\n" + `{"torn":"alice@example.com`,
		strings.Repeat("\n", streamBatchBytes) + "PK\x03\x04" + "\n" + `{"valid":true}`,
		`{"message":{"content":[{"type":"image","source":{"data":"` + strings.Repeat("A", 3<<20) + `"}}]}}` + "\n",
	}
	for i, raw := range cases {
		want, err := s.Scrub([]byte(raw), Hint{Family: "claude-code", JSONL: true})
		if err != nil {
			t.Fatal(err)
		}
		spool, err := os.CreateTemp(t.TempDir(), "scrub")
		if err != nil {
			t.Fatal(err)
		}
		defer spool.Close()
		got, hash, err := s.ScrubTo(context.Background(), spool, strings.NewReader(raw), Hint{Family: "claude-code", JSONL: true}, int64(len(raw)))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		body, err := io.ReadAll(got.Out.reader)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, want.Out.Bytes()) {
			t.Fatalf("case %d: payload differs", i)
		}
		if hash != Hash([]byte(raw)) || got.Out.hash != Hash(body) || got.Out.size != int64(len(body)) {
			t.Fatalf("case %d: hashes or length differ", i)
		}
		got.Out, want.Out = Scrubbed{}, Scrubbed{}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d: ledger differs: got %+v, want %+v", i, got, want)
		}
	}
}

func TestStreamingScrubFailsClosed(t *testing.T) {
	s, _ := New(DefaultConfig())
	cases := []struct {
		name   string
		reader io.Reader
		ctx    context.Context
		want   string
	}{
		{"opaque", strings.NewReader("PK\x03\x04hello"), context.Background(), "zip"},
		{"record limit", io.LimitReader(repeatByte('a'), StreamRecordThreshold+1), context.Background(), "positive max_file_bytes"},
		{"read failure", io.MultiReader(strings.NewReader("{}\n"), brokenReader{}), context.Background(), "read broke"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cases = append(cases, struct {
		name   string
		reader io.Reader
		ctx    context.Context
		want   string
	}{"cancel", strings.NewReader("{}\n"), ctx, "context canceled"})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spool, _ := os.CreateTemp(t.TempDir(), "scrub")
			defer spool.Close()
			got, _, err := s.ScrubTo(tc.ctx, spool, tc.reader, Hint{JSONL: true}, 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) || got.Out.via != "" {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

type repeatByte byte

func (b repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("read broke") }

func TestStreamingSealPreservesTarAndManifest(t *testing.T) {
	s, _ := New(DefaultConfig())
	raw := []byte("{\"text\":\"alice@example.com\"}\r\n{\"tail\":")
	want, _ := s.Scrub(raw, Hint{JSONL: true})
	spool, _ := os.CreateTemp(t.TempDir(), "scrub")
	defer spool.Close()
	got, _, err := s.ScrubTo(context.Background(), spool, bytes.NewReader(raw), Hint{JSONL: true}, int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := age.GenerateX25519Identity()
	m := Manifest{ManifestVersion: ManifestVersion, OrganizationID: "default", InstallID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301", SourceID: "claude-code-transcripts", NativePath: "/test/session.jsonl", Gather: "file_glob", ArtifactClass: "trajectory", SourceHash: Hash(raw), Client: Client{Version: "test"}, SealedAt: time.Now().UTC().Format(time.RFC3339)}
	var streamed bytes.Buffer
	sm, err := SealTo(&streamed, m, got.Out, []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	normal, nm, err := Seal(m, want.Out, []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sm, nm) {
		t.Fatal("manifest changed")
	}
	decode := func(raw []byte) []byte {
		r, err := age.Decrypt(bytes.NewReader(raw), id)
		if err != nil {
			t.Fatal(err)
		}
		// The tar bytes, rather than randomized age ciphertext, are the format contract.
		zr, err := zstd.NewReader(r)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		b, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !bytes.Equal(decode(normal), decode(streamed.Bytes())) {
		t.Fatal("tar bytes changed")
	}
}

func TestStreamingPrivateTranscriptParity(t *testing.T) {
	path := os.Getenv("SHIPPER_STREAM_PARITY_FILE")
	if path == "" {
		t.Skip("set SHIPPER_STREAM_PARITY_FILE to a private JSONL transcript")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	scrubber, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	want, err := scrubber.Scrub(raw, Hint{Family: "claude-code", JSONL: true})
	if err != nil {
		t.Fatal(err)
	}
	spool, err := os.CreateTemp(t.TempDir(), "parity")
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	got, hash, err := scrubber.ScrubTo(context.Background(), spool, bytes.NewReader(raw), Hint{Family: "claude-code", JSONL: true}, int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(got.Out.reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want.Out.Bytes()) || hash != Hash(raw) || got.Out.hash != Hash(body) || got.Out.size != int64(len(body)) {
		t.Fatal("private transcript bytes, hash or size differ")
	}
	got.Out, want.Out = Scrubbed{}, Scrubbed{}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("private transcript ledger differs")
	}
	t.Logf("byte-exact parity: input=%d output=%d parsed=%d raw=%d", len(raw), len(body), got.LinesParsed, got.LinesRawScanned)
}

type failedSpool struct{ *os.File }

func (failedSpool) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func TestStreamingSpoolWriteFailure(t *testing.T) {
	s, _ := New(DefaultConfig())
	f, err := os.CreateTemp(t.TempDir(), "failed")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	result, _, err := s.ScrubTo(context.Background(), failedSpool{f}, strings.NewReader("{}\n"), Hint{JSONL: true}, 3)
	if err == nil || !strings.Contains(err.Error(), "disk full") || result.Out.via != "" {
		t.Fatalf("scrub did not fail closed: %v", err)
	}
}
