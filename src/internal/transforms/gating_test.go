package transforms_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// The prefilter is a speed change that must never be a redaction change: a configured name it
// cannot represent, and a config value that would make the candidate floor meaningless.

// A name carrying a multi-byte rune has no sound literal for the byte-folding automaton to gate
// on. The answer is to stop prefiltering the key-name regex, not to stop redacting.
func TestNonASCIIKeyNameStillRedacts(t *testing.T) {
	cfg := transforms.DefaultConfig()
	cfg.SecretKeyNames = append(transforms.DefaultSecretKeyNames(), "CLÉ_SECRÈTE")
	s, err := transforms.New(cfg)
	if err != nil {
		t.Fatalf("a non-ASCII key name must compile, got %v", err)
	}

	line := []byte(`{"type":"user","text":"CLÉ_SECRÈTE=hunter2"}` + "\n")
	res, err := s.Scrub(line, transforms.Hint{Family: "claude-code", JSONL: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Out), transforms.Sentinel("key-name")) {
		t.Errorf("the secret survived: %s", res.Out)
	}
	if strings.Contains(string(res.Out), "hunter2") {
		t.Errorf("the secret survived verbatim: %s", res.Out)
	}

	// Dropping the whole gate is the fallback; dropping a rule is not.
	res, err = s.Scrub([]byte(`{"type":"user","text":"GITHUB_TOKEN=hunter2"}`+"\n"),
		transforms.Hint{Family: "claude-code", JSONL: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Out), "hunter2") {
		t.Errorf("an ASCII name stopped firing next to a non-ASCII one: %s", res.Out)
	}
}

// A negative floor is refused: `{-3,}` compiles as literal text ("never fires") while the
// candidate scanner reads it as "every run fires", and neither is what someone typing it means.
func TestNegativeEntropyMinLengthFailsTheBuild(t *testing.T) {
	cfg := transforms.DefaultConfig()
	cfg.Entropy.MinLength = -3
	if _, err := transforms.New(cfg); err == nil {
		t.Fatal("a negative entropy min_length must not compile")
	} else if !strings.Contains(err.Error(), "-3") {
		t.Errorf("the error must name the offending value, got %v", err)
	}

	// Zero stays legal: it clamps to a one-byte floor, which no positive threshold can tell apart.
	cfg.Entropy.MinLength = 0
	if _, err := transforms.New(cfg); err != nil {
		t.Errorf("min_length 0 must still compile, got %v", err)
	}
}

// The token walk visits duplicate members independently instead of collapsing them into a map.
func TestWideObjectKeepsDuplicateKeySemantics(t *testing.T) {
	s, err := transforms.New(transforms.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	// Widths either side of the threshold where the parse switches to an index.
	for _, width := range []int{8, 63, 64, 65, 200} {
		for _, dup := range []bool{false, true} {
			var b strings.Builder
			b.WriteString(`{"type":"user"`)
			for i := 0; i < width; i++ {
				fmt.Fprintf(&b, `,"k%d":"value %d"`, i, i)
			}
			if dup {
				b.WriteString(`,"k0":"AKIAIOSFODNN7EXAMPLE"`)
			}
			b.WriteString("}\n")

			res, err := s.Scrub([]byte(b.String()), transforms.Hint{Family: "claude-code", JSONL: true})
			if err != nil {
				t.Fatalf("width %d dup %v: %v", width, dup, err)
			}
			name := fmt.Sprintf("width %d dup %v", width, dup)
			if dup {
				if res.LinesParsed != 1 || res.LinesRawScanned != 0 {
					t.Errorf("%s: expected the line to stay decoded, got parsed=%d raw=%d",
						name, res.LinesParsed, res.LinesRawScanned)
				}
				if strings.Contains(string(res.Out), "AKIAIOSFODNN7EXAMPLE") {
					t.Errorf("%s: the secret survived: %s", name, res.Out)
				}
				continue
			}
			if res.LinesParsed != 1 || res.LinesRawScanned != 0 {
				t.Errorf("%s: expected a parsed line, got parsed=%d raw=%d",
					name, res.LinesParsed, res.LinesRawScanned)
			}
			if string(res.Out) != b.String() {
				t.Errorf("%s: a record with nothing to redact must come out verbatim", name)
			}
		}
	}
}
