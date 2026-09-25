package sources

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

const (
	testUUID  = "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	otherUUID = "0199ffff-0000-7000-8000-000000000001"
)

func compress(t *testing.T, plain []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(plain, nil)
}

func writeAt(t *testing.T, path string, body []byte, mtime time.Time) {
	t.Helper()
	writeFile(t, path, string(body))
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func codexRollouts(t *testing.T, root string) Resolved {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s, ok := c.Source("codex-rollouts")
	if !ok {
		t.Fatal("codex-rollouts is not in the catalog")
	}
	return Resolved{Source: s, Root: root, Enabled: true}
}

func rollout(dir, uuid, ext string) string {
	return dir + "/rollout-2026-09-01T10-00-00-" + uuid + ext
}

const rolloutBody = `{"timestamp":"2026-09-01T10:00:00Z","type":"session_meta","payload":{"cli_version":"0.44.0","cwd":"/work/demo"}}` + "\n" +
	`{"timestamp":"2026-09-01T10:00:01Z","type":"response_item","payload":{"type":"message"}}` + "\n"

func TestZstdFileLoadsDecoded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl.zst")
	writeFile(t, path, string(compress(t, []byte(rolloutBody))))

	p, err := fileLoader(path, 1<<20)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(p.Bytes) != rolloutBody {
		t.Errorf("loader returned %q, want the decoded rollout", p.Bytes)
	}
}

// A few hundred compressed bytes can decode to gigabytes: the cap is on what the pipeline would hold.
func TestZstdDecodedSizeIsCappedByMaxFileBytes(t *testing.T) {
	const limit = 64 << 10
	dir := t.TempDir()
	bomb := filepath.Join(dir, "bomb.jsonl.zst")
	writeFile(t, bomb, string(compress(t, make([]byte, 16<<20))))
	if info, _ := os.Stat(bomb); info.Size() > limit {
		t.Fatalf("fixture is %d bytes compressed, it must pass the stat cap to test the decoded one", info.Size())
	}

	_, err := fileLoader(bomb, limit)(context.Background())
	if err == nil || !errors.Is(err, platform.ErrTooLarge) ||
		!strings.Contains(err.Error(), "decoded size exceeds max_file_bytes") ||
		!strings.Contains(err.Error(), bomb) {
		t.Fatalf("want a decoded-size error naming the file, got %v", err)
	}

	exact := filepath.Join(dir, "exact.jsonl.zst")
	writeFile(t, exact, string(compress(t, bytes.Repeat([]byte("a"), limit))))
	if _, err := fileLoader(exact, limit)(context.Background()); err != nil {
		t.Errorf("a file decoding to exactly the cap must load: %v", err)
	}
}

// The frame's declared size pre-sizes the buffer, as ReadWhole does with the stat size, so a large
// rollout is not held twice while a doubling buffer copies it. Capacity, not MemStats: -race inflates allocations.
func TestZstdLoadIsPreSizedToTheDecodedSize(t *testing.T) {
	plain := bytes.Repeat([]byte(rolloutBody), (32<<20)/len(rolloutBody))
	path := filepath.Join(t.TempDir(), "big.jsonl.zst")
	writeFile(t, path, string(compress(t, plain)))

	p, err := fileLoader(path, 1<<30)(context.Background())
	if err != nil || len(p.Bytes) != len(plain) {
		t.Fatalf("load: %d bytes, %v", len(p.Bytes), err)
	}
	if got, limit := cap(p.Bytes), len(plain)+len(plain)/8; got > limit {
		t.Errorf("loading %d decoded bytes left a %d-byte buffer, limit %d", len(plain), got, limit)
	}
}

func TestZstdWithoutMaxFileBytesIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl.zst")
	writeFile(t, path, string(compress(t, []byte(rolloutBody))))
	if _, err := fileLoader(path, 0)(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no max_file_bytes") {
		t.Fatalf("an unbounded decode must be refused, got %v", err)
	}
}

// The memory gate charges a .zst what it decodes to: the declared size when the frame has one, else the cap.
func TestZstdIsChargedItsDecodedSize(t *testing.T) {
	dir := t.TempDir()
	const limit = 1 << 20
	plain := make([]byte, 64<<10)
	// Zeros encode as RLE blocks, text as compressed ones, random bytes as raw ones: the frame walk reads all three.
	text := bytes.Repeat([]byte(rolloutBody), 300)
	random := make([]byte, 300<<10)
	rand.New(rand.NewSource(1)).Read(random)
	streamed := func(b []byte) []byte {
		var buf bytes.Buffer
		enc, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		// A flush before the end writes the frame header before the size is known.
		for _, half := range [][]byte{b[:len(b)/2], b[len(b)/2:]} {
			if _, err := enc.Write(half); err != nil {
				t.Fatal(err)
			}
			if err := enc.Flush(); err != nil {
				t.Fatal(err)
			}
		}
		if err := enc.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	for _, tc := range []struct {
		name string
		body []byte
		want int64
	}{
		{"declared.jsonl.zst", compress(t, plain), int64(len(plain))},
		{"declared-text.jsonl.zst", compress(t, text), int64(len(text))},
		{"declared-random.jsonl.zst", compress(t, random), int64(len(random))},
		{"undeclared.jsonl.zst", streamed(plain), limit},
		// Concatenated frames (pzstd, cat a.zst b.zst) all decode; the first declares only its own.
		{"two-frames.jsonl.zst", append(compress(t, plain[:4000]), compress(t, plain)...), limit},
		{"over-the-cap.jsonl.zst", compress(t, make([]byte, 2*limit)), limit},
		{"no-magic.jsonl.zst", []byte(rolloutBody), limit},
		{"plain.jsonl", []byte(rolloutBody), int64(len(rolloutBody))},
	} {
		path := filepath.Join(dir, tc.name)
		writeFile(t, path, string(tc.body))
		c := Candidate{Path: path, Size: int64(len(tc.body))}
		if got := LoadBytes(c, limit); got != tc.want {
			t.Errorf("%s (%d bytes on disk): charged %d, want %d", tc.name, len(tc.body), got, tc.want)
		}
	}
	if got := LoadBytes(Candidate{Path: filepath.Join(dir, "gone.jsonl.zst"), Size: 10}, limit); got != limit {
		t.Errorf("an unreadable .zst is charged %d, want the cap %d", got, limit)
	}
}

// A .zst name over plaintext or garbage is refused, never shipped as opaque bytes.
func TestZstdPathWithoutMagicIsRefused(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, rollout("sessions/2026/09/01", testUUID, ".jsonl.zst"))
	writeFile(t, path, rolloutBody)

	if _, err := fileLoader(path, 1<<20)(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "does not start with zstd magic") {
		t.Fatalf("want a magic error, got %v", err)
	}

	d, err := discoverByGlob(Request{Source: codexRollouts(t, root)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Sniff != SniffUnreadable || d.Health != MatchPresentUnreadable {
		t.Errorf("sniff %q health %q, want unreadable", d.Sniff, d.Health)
	}
}

func TestZstdHeadIsDecodedForSniffAndCWDProbe(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, rollout("sessions/2026/09/01", testUUID, ".jsonl.zst"))
	writeFile(t, path, string(compress(t, []byte(rolloutBody))))

	d, err := discoverByGlob(Request{Source: codexRollouts(t, root)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Health != Collected || d.Sniff != SniffOK || d.AgentVersion != "0.44.0" {
		t.Errorf("health %q sniff %q version %q: the decoded head should sniff as jsonl",
			d.Health, d.Sniff, d.AgentVersion)
	}
	if cwd, ok := probeCWD(path, testProbe()); !ok || cwd != "/work/demo" {
		t.Errorf("probeCWD = %q, %v; want the cwd from the decoded head", cwd, ok)
	}

	// A head cut at the budget is not drift, exactly as for plaintext.
	head, truncated, err := readHead(path, 10)
	if err != nil || len(head) != 10 || !truncated {
		t.Errorf("readHead(10) = %d bytes, truncated %v, err %v", len(head), truncated, err)
	}
}

func TestCodexIdentityIsTheSessionUUID(t *testing.T) {
	src := codexRollouts(t, "")
	id, err := src.Identity.compile()
	if err != nil || id == nil {
		t.Fatalf("codex-rollouts identity: %v", err)
	}
	want := "codex-session/" + testUUID
	for _, rel := range []string{
		rollout("sessions/2026/09/01", testUUID, ".jsonl"),
		rollout("sessions/2026/09/01", testUUID, ".jsonl.zst"),
		rollout("archived_sessions", testUUID, ".jsonl"),
		rollout("archived_sessions", testUUID, ".jsonl.zst"),
	} {
		if got := id.of(rel); got != want {
			t.Errorf("identity of %s = %q, want %q", rel, got, want)
		}
	}
	for _, rel := range []string{
		"session_index.jsonl",
		"sessions/2026/09/01/rollout-2026-09-01T10-00-00-not-a-uuid.jsonl",
		rollout("sessions/2026/09/01", testUUID, ".jsonl.bak"),
	} {
		if got := id.of(rel); got != "" {
			t.Errorf("identity of %s = %q, want path identity", rel, got)
		}
	}
}

// One session in several forms is one candidate, and which form wins never depends on walk order.
func TestCollapseKeepsOneFormPerIdentity(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	zst := compress(t, []byte(rolloutBody))

	// Plaintext beats .zst even when the .zst is newer; among plaintext the newest wins.
	liveOld := rollout("sessions/2026/09/01", testUUID, ".jsonl")
	archived := rollout("archived_sessions", testUUID, ".jsonl")
	cold := rollout("archived_sessions", testUUID, ".jsonl.zst")
	writeAt(t, filepath.Join(root, liveOld), []byte(rolloutBody), base)
	writeAt(t, filepath.Join(root, archived), []byte(rolloutBody), base.Add(time.Hour))
	writeAt(t, filepath.Join(root, cold), zst, base.Add(2*time.Hour))

	// Same kind and mtime: the lower RelPath wins.
	tieArchived := rollout("archived_sessions", otherUUID, ".jsonl")
	tieLive := rollout("sessions/2026/09/01", otherUUID, ".jsonl")
	writeAt(t, filepath.Join(root, tieArchived), []byte(rolloutBody), base)
	writeAt(t, filepath.Join(root, tieLive), []byte(rolloutBody), base)

	writeAt(t, filepath.Join(root, "session_index.jsonl"), []byte(`{"id":"x"}`+"\n"), base)

	d, err := discoverByGlob(Request{Source: codexRollouts(t, root)})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range d.Candidates {
		if _, dup := got[c.Identity]; dup {
			t.Errorf("identity %q appears twice", c.Identity)
		}
		got[c.Identity] = c.RelPath
	}
	want := map[string]string{
		"codex-session/" + testUUID:  archived,
		"codex-session/" + otherUUID: tieArchived,
		"":                           "session_index.jsonl",
	}
	if len(got) != len(want) {
		t.Fatalf("candidates %v, want %v", got, want)
	}
	for id, rel := range want {
		if got[id] != rel {
			t.Errorf("identity %q kept %q, want %q", id, got[id], rel)
		}
	}

	// Once the plaintext is gone the .zst is the session.
	for _, rel := range []string{liveOld, archived} {
		if err := os.Remove(filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
	}
	d, err = discoverByGlob(Request{Source: codexRollouts(t, root)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range d.Candidates {
		if c.Identity == "codex-session/"+testUUID && c.RelPath != cold {
			t.Errorf("kept %q, want the compressed form %q", c.RelPath, cold)
		}
	}
}

func TestSourceWithoutIdentityKeepsPathIdentity(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, rollout("sessions/a", testUUID, ".jsonl")), rolloutBody)
	writeFile(t, filepath.Join(root, rollout("sessions/b", testUUID, ".jsonl")), rolloutBody)

	src := codexRollouts(t, root)
	src.Identity = nil
	d, err := discoverByGlob(Request{Source: src})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Candidates) != 2 || d.Candidates[0].Identity != "" || d.Candidates[1].Identity != "" {
		t.Errorf("without identity every path is its own candidate: %+v", d.Candidates)
	}
}

func TestInvalidIdentityFailsDiscoveryLoudly(t *testing.T) {
	src := codexRollouts(t, t.TempDir())
	src.Identity = &Identity{Match: "(", Key: "$1"}
	if _, err := discoverByGlob(Request{Source: src}); err == nil ||
		!strings.Contains(err.Error(), "codex-rollouts") || !strings.Contains(err.Error(), "identity.match") {
		t.Fatalf("want an error naming the source and the field, got %v", err)
	}
}

// Identity decides object keys, so it is fingerprinted; adding the field must not move any other source's fingerprint.
func TestSpecFingerprintCoversIdentityOnlyWhenSet(t *testing.T) {
	s := Source{Gather: "file_glob", Roots: []string{"~/.codex"}, Include: []string{"*.jsonl"}}
	// Computed independently from the pre-identity encoding.
	bare := SpecFingerprint(s)
	if bare != "c9205c7c9a160d9019c5bc20315438d19aec0d2eac80ff16c5da61c6daa303a4" {
		t.Errorf("a source without identity changed fingerprint: %s", bare)
	}
	s.Identity = &Identity{Match: "(x)", Key: "a/$1"}
	withID := SpecFingerprint(s)
	s.Identity = &Identity{Match: "(x)", Key: "b/$1"}
	if withID == bare || SpecFingerprint(s) == withID {
		t.Error("identity match and key must both be part of the fingerprint")
	}
}
