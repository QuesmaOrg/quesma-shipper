package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A document that cannot be loaded, whatever the reason, is discarded rather than fatal: the run
// starts from an empty store, reports the discard, and the first flush replaces the file.
func TestAnUnloadableDocumentIsDiscardedAndReplaced(t *testing.T) {
	const install = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	valid := `{"state_schema": 1, "install_id": "` + install + `", "entries": []}`
	cases := []struct {
		name       string
		body       string
		maxBytes   int64
		unreadable bool
	}{
		{name: "invalid JSON", body: "{not json"},
		{name: "state_schema mismatch", body: `{"state_schema": 2, "entries": []}`},
		{name: "checksum mismatch", body: `{"state_schema": 1, "checksum": "deadbeef", "entries": []}`},
		{name: "another install", body: `{"state_schema": 1, "install_id": "9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d", "entries": []}`},
		{name: "negative attempts", body: `{"state_schema": 1, "entries": [{"source_id": "s", "native_path": "/x/a.jsonl", "attempts": -5}]}`},
		{name: "oversize", body: valid, maxBytes: 8},
		{name: "unreadable", body: valid, unreadable: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.unreadable && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("file modes do not deny the owner here")
			}
			dir := t.TempDir()
			path := filepath.Join(dir, FileName)
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if c.unreadable {
				if err := os.Chmod(path, 0o000); err != nil {
					t.Fatal(err)
				}
			}
			maxBytes := c.maxBytes
			if maxBytes == 0 {
				maxBytes = maxDocumentBytes
			}

			s, err := open(dir, install, maxBytes)
			if err != nil {
				t.Fatalf("open must succeed over a document it cannot load: %v", err)
			}
			if s.Len() != 0 || !s.Corrupt() {
				t.Fatalf("len=%d corrupt=%v, want an empty discarded store", s.Len(), s.Corrupt())
			}
			k := Key{SourceID: "s", ID: "/x/b.jsonl"}
			fp := Fingerprint{SourceSize: 1, SourceMTime: time.Unix(1, 0).UTC(), SourceHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
			if err := s.CommitAll(map[Key]Fingerprint{k: fp}); err != nil {
				t.Fatalf("the first flush must replace the file: %v", err)
			}
			s.Close()

			s2, err := Open(dir, install)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer s2.Close()
			if s2.Corrupt() || s2.Len() != 1 {
				t.Fatalf("after the replace: corrupt=%v len=%d, want a clean store of one", s2.Corrupt(), s2.Len())
			}
			if got, ok := s2.Get(k); !ok || got != fp {
				t.Errorf("the committed entry did not survive the replace: %+v ok=%v", got, ok)
			}
		})
	}
}

// Downgrade: an older binary decodes without the identity field, so its checksum still verifies a
// store of path-keyed entries and fails on one holding an identity entry. That store is discarded
// loudly (a re-hash and per-object probe, the unloadable path), never half-trusted.
func TestAnOlderBinaryVerifiesOnlyPathKeyedStores(t *testing.T) {
	olderVerifies := func(entries map[Key]Fingerprint) bool {
		t.Helper()
		body, err := encode("", time.Time{}, nil, entries)
		if err != nil {
			t.Fatal(err)
		}
		var doc wireDoc
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		for i := range doc.Entries {
			doc.Entries[i].Identity = ""
		}
		sum, err := checksumOf(doc)
		if err != nil {
			t.Fatal(err)
		}
		return sum == doc.Checksum
	}
	fp := Fingerprint{SourceHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
	if !olderVerifies(map[Key]Fingerprint{{SourceID: "s", ID: "/x/a.jsonl"}: fp}) {
		t.Error("an older binary cannot verify a store holding only path-keyed entries")
	}
	fp.NativePath = "/x/rollout-a.jsonl"
	if olderVerifies(map[Key]Fingerprint{{SourceID: "s", ID: "codex-session/a"}: fp}) {
		t.Error("an older binary would trust an identity entry under its path key")
	}
}
