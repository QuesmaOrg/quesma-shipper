package transforms_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

var update = flag.Bool("update", false, "regenerate the conformance vectors (scrub and container)")

const scrubVectorPath = "../../conformance/v1/scrub/redaction.json"

// scrubVectors are before/after pairs as data, so a second-language port is checked against the
// same corpus. Every "after" must contain only sentinels and structural placeholders, never a
// secret, which is what makes this file safe to commit; TestConformanceVectorsCarryNoSecrets
// asserts it rather than assuming it.
type scrubVectors struct {
	VectorSet     string              `json:"vector_set"`
	VectorVersion int                 `json:"vector_version"`
	Description   string              `json:"description"`
	Sentinel      string              `json:"sentinel_form"`
	Username      string              `json:"username"`
	Exemptions    map[string][]string `json:"structural_exempt"`
	Vectors       []scrubVector       `json:"vectors"`
}

type scrubVector struct {
	Name     string         `json:"name"`
	Family   string         `json:"family"`
	JSONL    bool           `json:"jsonl"`
	Before   string         `json:"before"`
	After    string         `json:"after"`
	RuleHits map[string]int `json:"rule_hits"`
	ScanMode string         `json:"scan_mode"`
	Note     string         `json:"note,omitempty"`
}

func TestConformanceRedaction(t *testing.T) {
	if *update {
		if err := os.MkdirAll(filepath.Dir(scrubVectorPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(scrubVectorPath, generateScrubVectors(t), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("regenerated %s", scrubVectorPath)
	}

	raw, err := os.ReadFile(scrubVectorPath)
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v scrubVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.Sentinel != transforms.Sentinel("{rule_id}") {
		t.Errorf("sentinel form drifted: vector %q, code %q", v.Sentinel, transforms.Sentinel("{rule_id}"))
	}

	cfg := transforms.DefaultConfig()
	cfg.Exemptions = v.Exemptions
	cfg.Username = v.Username
	s, err := transforms.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range v.Vectors {
		t.Run(c.Name, func(t *testing.T) {
			res, err := s.Scrub([]byte(c.Before), transforms.Hint{Family: c.Family, JSONL: c.JSONL})
			if err != nil {
				t.Fatalf("engine error: %v", err)
			}
			if string(res.Out) != c.After {
				t.Errorf("output drifted:\n got %q\nwant %q", res.Out, c.After)
			}
			if c.ScanMode != "" && res.ScanMode != c.ScanMode {
				t.Errorf("scan_mode: got %q want %q", res.ScanMode, c.ScanMode)
			}
			for rule, want := range c.RuleHits {
				if res.RuleHits[rule] != want {
					t.Errorf("rule %q: %d hits, want %d (all: %v)", rule, res.RuleHits[rule], want, res.RuleHits)
				}
			}
		})
	}
}

// The vector file is committed, so it must not become a place secrets live.
func TestConformanceVectorsCarryNoSecrets(t *testing.T) {
	raw, err := os.ReadFile(scrubVectorPath)
	if err != nil {
		t.Fatal(err)
	}
	var v scrubVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Vectors {
		for _, planted := range plantedValues() {
			if contains(c.After, planted) {
				t.Errorf("vector %q records a secret in its AFTER value: %q", c.Name, planted)
			}
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

func plantedValues() []string {
	return []string{
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"AKIAIOSFODNN7EXAMPLE",
		"wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789",
		"tOk3nZq8vLmXw2Rf5uJhPd0aYc6eBn1KsG9iD4",
		"4111 1111 1111 1111",
		"hunter2",
		"jane",
	}
}

func generateScrubVectors(t *testing.T) []byte {
	t.Helper()

	cfg := transforms.DefaultConfig()
	cfg.Exemptions = exemptions()
	cfg.Username = "jane"
	s, err := transforms.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		family string
		jsonl  bool
		before string
		note   string
	}{
		{
			"provider key in an assistant text block", "claude-code", true,
			`{"type":"assistant","uuid":"a1","message":{"id":"m1","content":[{"type":"text","text":"use ghp_abcdefghijklmnopqrstuvwxyz0123456789"}]}}` + "\n",
			"",
		},
		{
			"printenv output keeps the key name and loses the value", "claude-code", true,
			`{"type":"user","uuid":"u1","toolUseResult":{"stdout":"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"}}` + "\n",
			"",
		},
		{
			"identifiers and timestamps survive untouched", "claude-code", true,
			`{"type":"assistant","uuid":"9f2c4e10-3b7a-4c19-8f2e-1a2b3c4d5e6f","parentUuid":"1a2b3c4d-5e6f-4718-9a0b-1c2d3e4f5a6b","sessionId":"s1","timestamp":"2026-07-30T10:00:00Z","version":"2.1.220"}` + "\n",
			"",
		},
		{
			"username in a cwd becomes a structural placeholder", "claude-code", true,
			`{"type":"user","uuid":"u1","cwd":"/Users/jane/work/api"}` + "\n",
			"",
		},
		{
			// Recorded as-is including its collateral damage: see
			// TestKnownOverRedactionInConnectionStrings.
			"connection string in a tool result (email rule over-reaches)", "claude-code", true,
			`{"type":"user","uuid":"u1","toolUseResult":{"stdout":"psql postgres://app:hunter2@db.internal:5432/prod"}}` + "\n",
			"",
		},
		{
			"torn final line is raw-scanned and keeps its missing newline", "claude-code", true,
			`{"type":"user","uuid":"u1","message":{"content":[{"type":"text","text":"ok"}]}}` + "\n" +
				`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"AKIAIOSFODNN7EXAMPLE and tru`,
			"",
		},
		{
			"non-JSON tool-result spill file", "claude-code", false,
			"$ printenv\nANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789\nHOME=/Users/jane\n",
			"",
		},
		{
			"a secret in a field named for what it is", "claude-code", true,
			`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"config"}],"api_key":"zx81-plain-value-no-shape"}}` + "\n",
			"The key-name rule is the only backstop for a credential with no recognisable shape. It is " +
				"matched on the field name as WORDS, so api_key, apiKey, x-api-key and Authorization all " +
				"count; matching them as suffixes of environment-variable names caught none of them.",
		},
		{
			"token counts are numbers about credentials, not credentials", "claude-code", true,
			`{"type":"assistant","uuid":"a1","message":{"usage":{"input_tokens":120,"output_tokens":340,"cache_read_input_tokens":9000},"max_tokens":"4096"}}` + "\n",
			"Every token figure this project reports comes from keys shaped like these. A key-name rule " +
				"that fired on anything containing 'token' would redact the product.",
		},
		{
			"a bare string is the whole record", "claude-code", true,
			`"AKIAIOSFODNN7EXAMPLE"` + "\n",
			"Legal JSONL and rare in a transcript. The walker handled objects and arrays only, so this " +
				"fell through it while the caller still reported the line as parsed — neither path scanned it.",
		},
		{
			"a repeated key is decoded, and keeps both values", "claude-code", true,
			`{"a":"AKIAIOSFODNN7EXAMPLE","a":"kept"}` + "\n",
			"The token walk visits every occurrence and patches only dirty string tokens, so both " +
				"members survive.",
		},
		{
			"a secret in key position", "claude-code", true,
			`{"AKIAIOSFODNN7EXAMPLE":"value"}` + "\n",
			"printenv and `kubectl get secret -o yaml` both put the name in key position. Only a pattern " +
				"hit renames a key — renaming is a bigger change than rewriting a value.",
		},
		{
			"a long mixed-case cwd survives the entropy backstop", "claude-code", true,
			`{"type":"user","uuid":"u1","cwd":"/Users/jane/Work2026/SampleOrg/blink-UI/apps/webFrontend/src"}` + "\n",
			"With '/' in the entropy candidate alphabet this cwd was one 4.6-bit run and " +
				"shipped as __REDACTED:generic-entropy__ — the repository dimension downstream is the " +
				"basename of cwd, so 144 of 818 real sessions had a sentinel for a repo name. Only the " +
				"username changes now.",
		},
		{
			"a path carrying the user placeholder survives re-scrubbing", "claude-code", true,
			`{"type":"user","uuid":"u1","cwd":"/Users/__USER__/Work2026/SampleOrg/blink-UI","message":{"content":[{"type":"text","text":"logs under ~/.claude/projects/-Users-__USER__-Work2026-SampleOrg-blink-UI/f00.jsonl"}]}}` + "\n",
			"The placeholder is built from in-class characters and ADDS entropy when substituted, so the " +
				"username rule's own output used to push dash-encoded slugs over the threshold: a second " +
				"scrub ate what the first pass had preserved, and project-map records arrive with __USER__ " +
				"already baked in. Byte-identical now, and it must stay that way.",
		},
		{
			"grep output with long descriptive paths stays legible", "claude-code", true,
			`{"type":"user","uuid":"u1","toolUseResult":{"stdout":"src/Components/CardPreview2/ButtonGroup_v3.tsx:42:export const ButtonGroup"}}` + "\n",
			"The path:line:code shape real archives shipped as __REDACTED:generic-entropy__.tsx:185. A " +
				"candidate can no longer span '/', so each segment is scored alone and none is long enough " +
				"to reach the backstop.",
		},
		{
			"a labeled aws secret key containing a slash is still redacted", "claude-code", true,
			`{"type":"user","uuid":"u1","toolUseResult":{"stdout":"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}}` + "\n",
			"Dropping '/' from the entropy alphabet leans on the pattern packs for slash-bearing secrets. " +
				"Every labeled arrival of the shape is context-anchored and unaffected; the bare unlabeled " +
				"variant is the accepted residual, recorded in TestBareBase64WithSlashIsAKnownEscape.",
		},
		{
			"a bare slash-free base64 blob still hits the entropy backstop", "claude-code", true,
			`{"type":"user","uuid":"u1","message":{"content":[{"type":"text","text":"stash tOk3nZq8vLmXw2Rf5uJhPd0aYc6eBn1KsG9iD4 somewhere"}]}}` + "\n",
			"The backstop's job after the alphabet change, stated positively: an unlabeled slash-free " +
				"token at 5.2 bits/char is exactly what it exists to catch, and the path fix must not have " +
				"cost this.",
		},
		{
			"a float's fractional digits are not a payment card", "claude-code", true,
			`{"type":"user","uuid":"u1","toolUseResult":{"stdout":"rmse: 0.4712949612297219 after 40 epochs"}}` + "\n",
			"A 54,592-hit audit found ZERO genuine cards; the largest class (35%) was " +
				"exactly this — a long float's fraction is Luhn-valid one time in ten, and the sentinel " +
				"landed mid-number. The card-pan rule now refuses matches that continue a decimal, and " +
				"demands real issuer prefixes, PAN lengths and card-shaped grouping.",
		},
		{
			"a payment card in human notation is still a card", "claude-code", true,
			`{"type":"user","uuid":"u1","toolUseResult":{"stdout":"charged to 4111 1111 1111 1111 as expected"}}` + "\n",
			"The other side of the card-pan tightening: the networks' published test number, written the way " +
				"people write cards, must keep matching through every added gate.",
		},
	}

	out := scrubVectors{
		VectorSet:     "redaction",
		VectorVersion: 1,
		Description: "Before/after redaction pairs. The sentinel is fixed per rule — its width depends " +
			"only on the rule id, never on the secret, so it cannot leak a length — and carries the rule " +
			"id so 'this session touched AWS credentials' stays queryable without recovering a value. " +
			"Note what survives: uuid, parentUuid, sessionId, timestamp and version are structurally " +
			"exempt because redacting them would destroy the causal graph, and an env var's NAME survives " +
			"while its value does not. Every AFTER value here contains only sentinels and placeholders; a " +
			"test asserts no planted secret is recorded in this file.",
		Sentinel:   transforms.Sentinel("{rule_id}"),
		Username:   "jane",
		Exemptions: exemptions(),
	}

	for _, c := range cases {
		res, err := s.Scrub([]byte(c.before), transforms.Hint{Family: c.family, JSONL: c.jsonl})
		if err != nil {
			t.Fatalf("%s: engine error: %v", c.name, err)
		}
		out.Vectors = append(out.Vectors, scrubVector{
			Name:     c.name,
			Family:   c.family,
			JSONL:    c.jsonl,
			Before:   c.before,
			After:    string(res.Out),
			RuleHits: res.RuleHits,
			ScanMode: res.ScanMode,
			Note:     c.note,
		})
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}
