package formats_test

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

// update regenerates the vector files (go test ./internal/formats -run Conformance -update).
// Never routine: a changed vector re-keys every previously shipped file and re-uploads it.
var update = flag.Bool("update", false, "regenerate the conformance vectors")

const vectorDir = "../../conformance/v1/naming"

type canonicalPathVectors struct {
	VectorSet     string `json:"vector_set"`
	VectorVersion int    `json:"vector_version"`
	Description   string `json:"description"`
	Vectors       []struct {
		Name          string `json:"name"`
		SourceRelPath string `json:"source_rel_path"`
		Username      string `json:"username"`
		CanonicalPath string `json:"canonical_path"`
	} `json:"vectors"`
}

type mirrorKeyVectors struct {
	VectorSet     string            `json:"vector_set"`
	VectorVersion int               `json:"vector_version"`
	Description   string            `json:"description"`
	Keys          map[string]string `json:"keys"`
	Vectors       []mirrorKeyVector `json:"vectors"`
}

type mirrorKeyVector struct {
	Name          string `json:"name"`
	Key           string `json:"key"`
	CanonicalPath string `json:"canonical_path"`
	MirrorName    string `json:"mirror_name"`
}

func readJSON(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(vectorDir, name))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("parse vectors %s: %v", name, err)
	}
}

// The canonical encoding is the HMAC input: one byte of disagreement re-keys every file.
func TestConformanceCanonicalPath(t *testing.T) {
	var v canonicalPathVectors
	readJSON(t, "canonical-path.json", &v)
	if len(v.Vectors) == 0 {
		t.Fatal("no canonical-path vectors")
	}
	for _, c := range v.Vectors {
		t.Run(c.Name, func(t *testing.T) {
			got := formats.CanonicalPath(c.SourceRelPath, c.Username)
			if got != c.CanonicalPath {
				t.Errorf("CanonicalPath(%q, %q)\n got %q\nwant %q",
					c.SourceRelPath, c.Username, got, c.CanonicalPath)
			}
		})
	}
}

func TestConformanceMirrorName(t *testing.T) {
	path := filepath.Join(vectorDir, "mirror-key.json")

	if *update {
		if err := os.WriteFile(path, generateMirrorKeyVectors(t), 0o644); err != nil {
			t.Fatalf("write vectors: %v", err)
		}
		t.Logf("regenerated %s", path)
	}

	var v mirrorKeyVectors
	readJSON(t, "mirror-key.json", &v)
	if len(v.Vectors) == 0 {
		t.Fatal("no mirror-key vectors")
	}
	for _, c := range v.Vectors {
		t.Run(c.Name, func(t *testing.T) {
			keyHex, ok := v.Keys[c.Key]
			if !ok {
				t.Fatalf("vector names key %q, which is not in keys", c.Key)
			}
			key, err := hex.DecodeString(keyHex)
			if err != nil {
				t.Fatalf("key %q is not hex: %v", c.Key, err)
			}
			got := formats.MirrorName(key, c.CanonicalPath)
			if got != c.MirrorName {
				t.Errorf("MirrorName(%s, %q)\n got %s\nwant %s", c.Key, c.CanonicalPath, got, c.MirrorName)
			}
		})
	}
}

func generateMirrorKeyVectors(t *testing.T) []byte {
	t.Helper()

	keys := map[string]string{
		"key_a": "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		"key_b": "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}
	cases := []struct{ name, key, path string }{
		{"empty canonical path still yields a full-length name", "key_a", ""},
		{"claude code transcript", "key_a", "projects/proj/a.jsonl"},
		{"claude code transcript under a pseudonymized slug", "key_a", "projects/-Users-__USER__-work-api/3f2504e0.jsonl"},
		{"claude code subagent transcript", "key_a", "projects/-Users-__USER__-work-api/3f2504e0/subagents/agent-9f2c.jsonl"},
		{"codex rollout", "key_a", "sessions/2026/07/30/rollout-2026-07-30T10-00-00-0199.jsonl"},
		{"codex cold rollout is a different file from its plaintext form", "key_a", "sessions/2026/07/30/rollout-2026-07-30T10-00-00-0199.jsonl.zst"},
		{"cursor transcript", "key_a", "c8cbeb0b/c8cbeb0b.jsonl"},
		{"enricher output is its own object", "key_a", "c8cbeb0b/c8cbeb0b.jsonl.enriched.jsonl"},
		{"percent-encoded segment", "key_a", "images/Screenshot%202026-07-27%20at%2014.32.36.png"},
		{"same path under a second name key gives an unrelated name", "key_b", "projects/proj/a.jsonl"},
	}

	out := mirrorKeyVectors{
		VectorSet:     "mirror-key",
		VectorVersion: 1,
		Description: "mirror_name = lowercase-hex HMAC-SHA256(name_key, \"mirror:\" + canonical_path). " +
			"Keyed rather than a bare hash, so a name is blinded against anyone holding bucket-list rights. " +
			"Derived from the path and never from content, so a file's whole history lands on one key as object " +
			"versions and a re-ship after state loss converges onto it. Fixed length, so a pathological path has " +
			"no key-length edge case. Note key_a and key_b over the same path: blinding is keyed.",
		Keys: keys,
	}
	for _, c := range cases {
		key, err := hex.DecodeString(keys[c.key])
		if err != nil {
			t.Fatal(err)
		}
		out.Vectors = append(out.Vectors, mirrorKeyVector{
			Name:          c.name,
			Key:           c.key,
			CanonicalPath: c.path,
			MirrorName:    formats.MirrorName(key, c.path),
		})
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}
