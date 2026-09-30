package transforms_test

import (
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// These use the package's own newScrubber, which is what the engine builds.

const plantedAWSKey = "AKIAIOSFODNN7EXAMPLE"

// Compressed and database bytes pass every text pattern untouched, so a clean ledger over
// them would be a lie; Scrub refuses them whatever the hint says.
func TestOpaquePayloadsAreRefused(t *testing.T) {
	s := newScrubber(t)
	for _, tc := range []struct{ name, head, kind string }{
		{"zstd frame", "\x28\xb5\x2f\xfd", "zstd"},
		{"gzip member", "\x1f\x8b\x08\x00", "gzip"},
		{"zip local header", "PK\x03\x04", "zip"},
		{"empty zip", "PK\x05\x06", "zip"},
		{"spanned zip", "PK\x07\x08", "zip"},
		{"bzip2 block", "BZh9\x31\x41\x59\x26\x53\x59", "bzip2"},
		{"bzip2 empty stream", "BZh1\x17\x72\x45\x38\x50\x90", "bzip2"},
		{"xz stream", "\xfd7zXZ\x00", "xz"},
		{"sqlite database", "SQLite format 3\x00", "sqlite"},
	} {
		for _, jsonl := range []bool{true, false} {
			res, err := s.Scrub([]byte(tc.head+plantedAWSKey+"\n"), transforms.Hint{Family: "claude-code", JSONL: jsonl})
			if want := "scrub refused: " + tc.kind + " payload"; err == nil || err.Error() != want {
				t.Errorf("%s (jsonl=%v): err %v, want %q", tc.name, jsonl, err, want)
			}
			if len(res.Out.Bytes()) != 0 {
				t.Errorf("%s (jsonl=%v): a refused scrub returned a payload", tc.name, jsonl)
			}
		}
	}
}

// Text that only resembles a header is scanned.
func TestTextThatOnlyLooksOpaqueIsScrubbed(t *testing.T) {
	s := newScrubber(t)
	for _, text := range []string{
		"BZh9 is a bzip2 level, not a stream: " + plantedAWSKey + "\n",
		"PK is a primary key: " + plantedAWSKey + "\n",
	} {
		res, err := s.Scrub([]byte(text), transforms.Hint{Family: "claude-code"})
		if err != nil {
			t.Fatalf("%.40q: %v", text, err)
		}
		if strings.Contains(string(res.Out.Bytes()), plantedAWSKey) {
			t.Errorf("%.40q: the planted key shipped in the clear", text)
		}
	}
}
