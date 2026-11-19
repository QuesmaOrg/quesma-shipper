package transforms_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
	"github.com/QuesmaOrg/quesma-shipper/scrub"
)

func TestStreamingSealPreservesTarAndManifest(t *testing.T) {
	s, _ := scrub.New(scrub.DefaultConfig())
	raw := []byte("{\"text\":\"alice@example.com\"}\r\n{\"tail\":")
	want, _ := s.Scrub(raw, scrub.Hint{JSONL: true})
	spool, _ := os.CreateTemp(t.TempDir(), "scrub")
	defer spool.Close()
	got, _, err := s.ScrubTo(context.Background(), spool, bytes.NewReader(raw), scrub.Hint{JSONL: true}, int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := age.GenerateX25519Identity()
	m := transforms.Manifest{ManifestVersion: transforms.ManifestVersion, OrganizationID: "default", InstallID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301", SourceID: "claude-code-transcripts", NativePath: "/test/session.jsonl", Gather: "file_glob", ArtifactClass: "trajectory", SourceHash: transforms.Hash(raw), Client: transforms.Client{Version: "test"}, SealedAt: time.Now().UTC().Format(time.RFC3339)}
	var streamed bytes.Buffer
	sm, err := transforms.SealTo(&streamed, m, got.Out, []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	normal, nm, err := transforms.Seal(m, want.Out, []age.Recipient{id.Recipient()})
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
