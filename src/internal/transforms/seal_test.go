package transforms_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

func identity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func manifest() transforms.Manifest {
	return transforms.Manifest{
		ManifestVersion: transforms.ManifestVersion,
		OrganizationID:  "default",
		InstallID:       "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		SourceID:        "claude-code-transcripts",
		NativePath:      "/Users/__USER__/.claude/projects/-Users-__USER__-work-api/s.jsonl",
		Gather:          "file_glob",
		ArtifactClass:   "trajectory",
		SourceHash:      strings.Repeat("a", 64),
		SealedAt:        "2026-07-30T10:00:00Z",
		ShapeSniff:      "ok",
		Client:          transforms.Client{Version: "0.1.0", OS: "darwin", Arch: "arm64"},
		RunID:           "0123456789abcdef",
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	id := identity(t)
	payload := []byte("{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n")

	obj, err := transforms.Seal(manifest(), payload, []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	gotM, gotPayload, err := transforms.Open(obj, id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Errorf("payload round trip:\n got %q\nwant %q", gotPayload, payload)
	}
	if gotM.NativePath != manifest().NativePath {
		t.Errorf("native_path: %q", gotM.NativePath)
	}
	if gotM.RunID != "0123456789abcdef" {
		t.Errorf("run_id: %q", gotM.RunID)
	}

	// Seal computes these from the bytes it actually wrote.
	sum := sha256.Sum256(payload)
	if gotM.ShippedHash != hex.EncodeToString(sum[:]) {
		t.Error("shipped_hash does not describe the payload")
	}
	if gotM.PayloadSize != int64(len(payload)) {
		t.Errorf("payload_size %d, want %d", gotM.PayloadSize, len(payload))
	}
}

// A payload of zero bytes is a real case and must round-trip, not be special-cased.
func TestSealEmptyPayload(t *testing.T) {
	id := identity(t)
	obj, err := transforms.Seal(manifest(), nil, []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	m, payload, err := transforms.Open(obj, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 0 || m.PayloadSize != 0 {
		t.Errorf("empty payload became %d bytes", len(payload))
	}
}

// Manifest-first is the container contract and what makes a ranged head-fetch possible, so
// assert the order at the tar layer directly.
func TestManifestIsTheFirstTarEntry(t *testing.T) {
	id := identity(t)
	obj, err := transforms.Seal(manifest(), bytes.Repeat([]byte("x"), 4096), []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}

	dec, err := age.Decrypt(bytes.NewReader(obj), id)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zstd.NewReader(dec)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	if len(names) != 2 || names[0] != transforms.ManifestEntry || names[1] != transforms.PayloadEntry {
		t.Errorf("entries %v, want [%s %s]", names, transforms.ManifestEntry, transforms.PayloadEntry)
	}
}

// A ranged GET of the head of a multi-MB object parses the full manifest. The prefix is a real
// byte slice, not an io.LimitReader over the whole object, which would hide the failure being
// tested: the lower layers error on a truncated final chunk and the manifest read must survive.
func TestReadManifestPrefixOnMultiMegabyteObject(t *testing.T) {
	id := identity(t)

	// Incompressible on purpose: a repeating payload would compress below the prefix size and
	// quietly turn this into a test of nothing.
	payload := make([]byte, 6<<20)
	rng := rand.New(rand.NewSource(1))
	if _, err := rng.Read(payload); err != nil {
		t.Fatal(err)
	}
	obj, err := transforms.Seal(manifest(), payload, []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	if len(obj) < 1<<20 {
		t.Fatalf("object compressed to %d bytes; the test needs one larger than the prefix", len(obj))
	}

	prefix := obj[:transforms.SuggestedPrefixBytes]
	m, err := transforms.ReadManifestPrefix(prefix, id)
	if err != nil {
		t.Fatalf("a %d-byte prefix of a %d-byte object must yield the manifest: %v",
			len(prefix), len(obj), err)
	}
	if m.NativePath != manifest().NativePath {
		t.Errorf("native_path from prefix: %q", m.NativePath)
	}
	if m.SourceHash != manifest().SourceHash {
		t.Errorf("source_hash from prefix: %q", m.SourceHash)
	}
	if m.PayloadSize != int64(len(payload)) {
		t.Errorf("payload_size from prefix: %d, want %d", m.PayloadSize, len(payload))
	}
}

// A prefix too short to hold the manifest must say so distinguishably, so the caller doubles
// its range instead of concluding the object is corrupt.
func TestReadManifestPrefixReportsTooShort(t *testing.T) {
	id := identity(t)
	big := make([]byte, 1<<20)
	rand.New(rand.NewSource(2)).Read(big)
	obj, err := transforms.Seal(manifest(), big, []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []int{1, 16, 128, 1024} {
		if n >= len(obj) {
			continue
		}
		_, err := transforms.ReadManifestPrefix(obj[:n], id)
		if !errors.Is(err, transforms.ErrPrefixTooShort) {
			t.Errorf("a %d-byte prefix should report ErrPrefixTooShort, got %v", n, err)
		}
	}
}

// The doubling loop a sink implements works from any starting point.
func TestPrefixDoublingConverges(t *testing.T) {
	id := identity(t)
	big := make([]byte, 2<<20)
	rand.New(rand.NewSource(3)).Read(big)
	obj, err := transforms.Seal(manifest(), big, []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}

	budget := 512
	attempts := 0
	for {
		attempts++
		if attempts > 20 {
			t.Fatal("doubling did not converge")
		}
		n := min(budget, len(obj))
		m, err := transforms.ReadManifestPrefix(obj[:n], id)
		if err == nil {
			if m.SourceID != "claude-code-transcripts" {
				t.Errorf("wrong manifest: %+v", m)
			}
			return
		}
		if !errors.Is(err, transforms.ErrPrefixTooShort) {
			t.Fatalf("unexpected error at %d bytes: %v", n, err)
		}
		budget *= 2
	}
}

// Uploaded bytes are ciphertext, unparseable without the matching identity.
func TestObjectIsOpaqueWithoutTheIdentity(t *testing.T) {
	id := identity(t)
	stranger := identity(t)
	secretPath := "/Users/jane/.claude/projects/-Users-jane-work-secret/s.jsonl"

	m := manifest()
	m.NativePath = secretPath
	payload := []byte(`{"text":"a distinctive sentence that must not appear in ciphertext"}`)

	obj, err := transforms.Seal(m, payload, []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}

	for _, needle := range []string{
		secretPath, "distinctive sentence", "claude-code-transcripts",
		"manifest.json", "jane", "payload",
	} {
		if bytes.Contains(obj, []byte(needle)) {
			t.Errorf("ciphertext leaks %q in plaintext", needle)
		}
	}
	if _, _, err := transforms.Open(obj, stranger); err == nil {
		t.Fatal("an object must not open with an unrelated identity")
	}
	if _, err := transforms.ReadManifestPrefix(obj, stranger); err == nil {
		t.Fatal("a manifest must not be readable with an unrelated identity")
	}
	if _, _, err := transforms.Open(obj); err == nil {
		t.Fatal("opening with no identity must fail")
	}
}

// Archival-only versus archival-plus-analysis recipients: who can read is decided at encryption
// time by which public recipients were included, and nothing later widens it.
func TestRecipientSetsDecideWhoCanRead(t *testing.T) {
	archival := identity(t)
	analysis := identity(t)
	payload := []byte(`{"a":1}`)

	archivalOnly, err := transforms.Seal(manifest(), payload, []age.Recipient{archival.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	both, err := transforms.Seal(manifest(), payload,
		[]age.Recipient{archival.Recipient(), analysis.Recipient()})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := transforms.Open(archivalOnly, archival); err != nil {
		t.Errorf("the archival identity must read an archival object: %v", err)
	}
	if _, _, err := transforms.Open(archivalOnly, analysis); err == nil {
		t.Error("the analysis identity must NOT read an archival-only object")
	}
	for _, id := range []age.Identity{archival, analysis} {
		if _, _, err := transforms.Open(both, id); err != nil {
			t.Errorf("both recipients must read a two-recipient object: %v", err)
		}
	}

	// Recipient key IDs are recorded, public only, so a rotation can find what to rewrap.
	m, _, err := transforms.Open(both, archival)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Encryption.RecipientKeyIDs) != 2 {
		t.Errorf("recipient_key_ids: %v", m.Encryption.RecipientKeyIDs)
	}
	for _, kid := range m.Encryption.RecipientKeyIDs {
		if strings.Contains(kid, "AGE-SECRET-KEY") {
			t.Fatal("a private key reached the manifest")
		}
	}
}

func TestSealRefusesWithNoRecipients(t *testing.T) {
	if _, err := transforms.Seal(manifest(), []byte("x"), nil); err == nil {
		t.Fatal("sealing with no recipients must fail: encryption is not optional")
	}
}

// A manifest that would fail downstream validation must not reach a bucket.
func TestSealValidatesTheManifestAgainstItsSchema(t *testing.T) {
	id := identity(t)

	bad := manifest()
	bad.ArtifactClass = "whatever"
	if _, err := transforms.Seal(bad, []byte("x"), []age.Recipient{id.Recipient()}); err == nil {
		t.Error("an artifact_class outside the enum must be refused")
	}

	bad = manifest()
	bad.ShapeSniff = "probably-fine"
	if _, err := transforms.Seal(bad, []byte("x"), []age.Recipient{id.Recipient()}); err == nil {
		t.Error("a shape_sniff outside the closed enum must be refused")
	}

	bad = manifest()
	bad.SourceHash = "deadbeef"
	if _, err := transforms.Seal(bad, []byte("x"), []age.Recipient{id.Recipient()}); err == nil {
		t.Error("a malformed source_hash must be refused")
	}

	bad = manifest()
	bad.Derived = true
	if _, err := transforms.Seal(bad, []byte("x"), []age.Recipient{id.Recipient()}); err == nil {
		t.Error("derived without enricher provenance must be refused")
	}
}

// An object whose payload was altered after sealing must not open: the manifest describes
// bytes, so it has to describe THESE bytes.
func TestOpenRejectsPayloadHashMismatch(t *testing.T) {
	id := identity(t)

	// Build a container by hand with a manifest that lies about its payload.
	m := manifest()
	m.ShippedHash = strings.Repeat("b", 64)
	m.PayloadSize = 1
	raw, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, e := range []struct {
		name string
		body []byte
	}{{transforms.ManifestEntry, raw}, {transforms.PayloadEntry, []byte("different")}} {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: e.name, Size: int64(len(e.body)),
			Mode: 0o600, ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var objBuf bytes.Buffer
	encW, err := age.Encrypt(&objBuf, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(encW, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(transforms.ZstdLevel)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	encW.Close()

	if _, _, err := transforms.Open(objBuf.Bytes(), id); err == nil {
		t.Fatal("a payload that does not match shipped_hash must be refused")
	}
}

// Plaintext object metadata carries the hashes and versions a listing-side consumer dedupes
// on, and never the path.
func TestObjectMetadataNeverCarriesThePath(t *testing.T) {
	m := manifest()
	m.NativePath = "/Users/jane/.claude/projects/-Users-jane-work-secret-project/s.jsonl"
	m.ShippedHash = strings.Repeat("c", 64)

	md := m.ObjectMetadata()
	for _, needle := range []string{"jane", "secret-project", "s.jsonl", "/Users", "projects"} {
		for k, v := range md {
			if strings.Contains(k, needle) || strings.Contains(v, needle) {
				t.Errorf("object metadata leaks %q: %v", needle, md)
			}
		}
	}
	for _, want := range []string{"source-hash", "shipped-hash", "artifact-class", "manifest-version"} {
		if _, ok := md[want]; !ok {
			t.Errorf("object metadata should carry %q for HEAD-side dedupe", want)
		}
	}
}

// The integrity hash must reach OBJECT METADATA, not only the sealed manifest: Seal takes the
// manifest by value, so a computed ShippedHash is invisible to the caller, and objects once
// shipped `shipped-hash: ""` in metadata while the manifest inside the ciphertext was correct.
func TestShippedHashReachesObjectMetadata(t *testing.T) {
	id := identity(t)
	payload := []byte(`{"line":"one"}` + "\n")

	m := manifest()
	m.ShippedHash = sha256Hex(payload)

	obj, err := transforms.Seal(m, payload, []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.ObjectMetadata()["shipped-hash"]; got != sha256Hex(payload) {
		t.Errorf("object metadata shipped-hash = %q, want the payload hash", got)
	}

	// And the sealed copy agrees, so decrypting and heading the object tell the same story.
	sealed, _, err := transforms.Open(obj, id)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.ShippedHash != sha256Hex(payload) {
		t.Errorf("sealed manifest shipped_hash = %q", sealed.ShippedHash)
	}
}

// A caller that hashed different bytes than it passed is a defect worth refusing.
func TestSealRefusesAShippedHashThatDoesNotMatchThePayload(t *testing.T) {
	id := identity(t)
	m := manifest()
	m.ShippedHash = sha256Hex([]byte("some other bytes"))

	_, err := transforms.Seal(m, []byte("the real payload"), []age.Recipient{id.Recipient()})
	if err == nil {
		t.Fatal("a manifest whose shipped_hash describes different bytes was accepted")
	}
	if !strings.Contains(err.Error(), "different bytes") {
		t.Errorf("the error does not name the mismatch: %v", err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// The build that sealed an object is recorded as FACTS, not a string a reader parses: commit,
// modified and go_version make "which objects came from that build" a comparison. go_version
// is there because a runtime-level defect is a property of the toolchain alone.
func TestTheManifestRecordsWhichBuildSealedTheObject(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	m := manifest()
	m.Client = transforms.Client{
		Version:   "0.0.0-d5f735643cbd+dirty",
		Commit:    "d5f735643cbd3c70f71d2ed52746be1cadfe3a15",
		Modified:  true,
		GoVersion: "go1.25.0",
		OS:        "linux",
		Arch:      "amd64",
	}

	sealed, err := transforms.Seal(m, []byte("{}\n"), []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := transforms.Open(sealed, id)
	if err != nil {
		t.Fatal(err)
	}

	if got.Client.Commit != m.Client.Commit {
		t.Errorf("commit did not survive: %q", got.Client.Commit)
	}
	if !got.Client.Modified {
		t.Error("a build from a modified tree is recorded as clean")
	}
	if got.Client.GoVersion != "go1.25.0" {
		t.Errorf("go_version did not survive: %q", got.Client.GoVersion)
	}
}

// A build with no VCS stamping must not invent one, and the schema refuses unknown fields, so
// an empty commit has to be OMITTED rather than sent as "".
func TestABuildWithNoStampSealsWithoutTheOptionalFields(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	m := manifest()
	m.Client = transforms.Client{Version: "unknown"}

	sealed, err := transforms.Seal(m, []byte("{}\n"), []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatalf("a manifest from an unstamped build did not seal: %v", err)
	}
	got, _, err := transforms.Open(sealed, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Client.Commit != "" || got.Client.Modified {
		t.Errorf("fields were invented: %+v", got.Client)
	}
}
