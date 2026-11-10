package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSealPolicyChecksumAndDowngrade(t *testing.T) {
	for _, policy := range []string{"", strings.Repeat("a", 64)} {
		raw, err := encode("", time.Time{}, nil, map[Key]Fingerprint{
			{SourceID: "test", ID: "/session.jsonl"}: {SourceHash: strings.Repeat("b", 64), SealPolicy: policy},
		})
		if err != nil {
			t.Fatal(err)
		}
		var doc wireDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Entries[0].SealPolicy != policy {
			t.Fatal("seal policy lost in state encoding")
		}
		checksum, err := checksumOf(doc)
		if err != nil || checksum != doc.Checksum {
			t.Fatalf("current reader rejects state: %v", err)
		}
		// Older readers omit the new field when verifying their typed document's checksum.
		doc.Entries[0].SealPolicy = ""
		checksum, err = checksumOf(doc)
		if err != nil || (checksum == doc.Checksum) != (policy == "") {
			t.Fatalf("unexpected downgrade behavior for policy %q: %v", policy, err)
		}
		if policy == "" && strings.Contains(string(raw), "seal_policy") {
			t.Fatal("ordinary state acquired a new field")
		}
	}
}
