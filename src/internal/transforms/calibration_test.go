package transforms_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// The seeded corpus DefaultEntropyConfig promises. Three populations, three contracts: benign
// path-shaped content draws ZERO entropy hits, since a path the backstop eats is a repository
// name lost downstream; git SHAs get a small budget rather than zero, because uniform 40-hex
// averages ~3.73 bits/char against the 3.8 threshold and the tail crosses it; planted secrets
// are caught at 100% recall. A fixed seed makes a retune a deterministic diff, not a flake.
func TestSeededCorpusCalibration(t *testing.T) {
	s := newScrubber(t)
	rng := &xorshift{state: 0x9E3779B97F4A7C15}

	var corpus []string

	// --- population 1: benign, strict zero -----------------------------------
	benign := generateBenignLines(rng, 400)
	corpus = append(corpus, benign...)
	for i, line := range benign {
		res := scrubJSONL(t, s, "claude-code", line+"\n")
		if res.RuleHits["generic-entropy"] != 0 {
			t.Errorf("benign line %d drew an entropy hit:\n in %s\nout %s", i, line, res.Out)
		}
		if strings.Contains(string(res.Out), "__REDACTED:") {
			t.Errorf("benign line %d was redacted by %v:\n in %s\nout %s", i, res.RuleHits, line, res.Out)
		}
	}

	// --- population 2: git SHAs, budgeted ------------------------------------
	// With this seed exactly 7 of 50 fire (14%), a property of the hex threshold alone: a SHA has
	// no slash, so the alphabet cannot touch it. Recorded so a retune announces itself as a count
	// change rather than a surprise in fleet density.
	const shaLines = 50
	const shaFireWithThisSeed = 7
	fired := 0
	for i := 0; i < shaLines; i++ {
		sha := randomHex(rng, 40)
		line := `{"type":"user","uuid":"sha` + fmt.Sprint(i) + `","message":{"content":[{"type":"text","text":"commit ` + sha + ` touched the parser"}]}}`
		corpus = append(corpus, line)
		res := scrubJSONL(t, s, "claude-code", line+"\n")
		if res.RuleHits["generic-entropy"] != 0 {
			fired++
		}
	}
	if fired != shaFireWithThisSeed {
		t.Errorf("%d of %d random SHAs drew entropy hits, calibrated count is %d — the hex threshold moved; re-measure and update this note", fired, shaLines, shaFireWithThisSeed)
	}

	// --- population 3: planted secrets, 100%% recall --------------------------
	planted := []struct {
		name   string
		text   string
		secret string
		rule   string // empty when overlapping rules make attribution ambiguous
	}{
		{"aws access key id", "run with AKIAIOSFODNN7EXAMPLE as the principal", "AKIAIOSFODNN7EXAMPLE", "aws-access-key-id"},
		{"github pat", "push using ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "github-pat"},
		{"anthropic key", "export it: sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789", "anthropic-api-key"},
		// The slash-carrying base64 shape the backstop no longer covers bare: labeled, it must always
		// be caught. Attribution is not pinned, since key-name and aws-secret-key both claim it.
		{"labeled slash-bearing base64", "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", ""},
		{"shapeless value behind a telling name", "MY_SERVICE_TOKEN=plain-looking-value-1234", "plain-looking-value-1234", "key-name"},
		{"bare slash-free base64url blob", "stash " + randomHighEntropyToken(rng, 40) + " somewhere", "", "generic-entropy"},
	}
	for _, p := range planted {
		t.Run("planted/"+p.name, func(t *testing.T) {
			secret := p.secret
			if secret == "" {
				// The blob is embedded in the text between two words.
				secret = strings.Fields(p.text)[1]
			}
			line := `{"type":"user","uuid":"p1","toolUseResult":{"stdout":"` + p.text + `"}}`
			corpus = append(corpus, line)
			res := scrubJSONL(t, s, "claude-code", line+"\n")
			if strings.Contains(string(res.Out), secret) {
				t.Errorf("planted secret survived:\n in %s\nout %s", line, res.Out)
			}
			if p.rule != "" && res.RuleHits[p.rule] == 0 {
				t.Errorf("expected %q to claim the hit, ledger was %v", p.rule, res.RuleHits)
			}
		})
	}

	// --- the alarm ------------------------------------------------------------
	// Whole-corpus density is the fleet signal the manifests carry; the ceiling exists to catch a
	// rule that starts eating content.
	res, err := s.Scrub([]byte(strings.Join(corpus, "\n")+"\n"), transforms.Hint{Family: "claude-code", JSONL: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := res.Density(); d >= 0.05 {
		t.Errorf("whole-corpus redaction density %.4f crossed the 0.05 alarm — a rule is eating content", d)
	}
}

// generateBenignLines builds path-heavy records across the shapes that fired before '/' left the entropy alphabet.
// Carrier fields rotate between cwd (exempt), free text and tool output (both unexempt), so the
// alphabet is exercised and not just the exemptions.
func generateBenignLines(rng *xorshift, n int) []string {
	segs := []string{
		"Work2026", "SampleOrg", "blink-UI", "webFrontend", "GolandProjects",
		"ButtonGroup_v3", "CardPreview2", "vendorPortal", "telemetry-collectors",
		"polyglot-demo-apps", "Desktop", "Q3_2026", "node_modules", "react-dom",
		"internal", "scrub", "heuristic_v2", "EntropyMatcher9", "testdata",
		"apps", "src", "Components", "weather-widget", "loadbench-contractor",
	}
	exts := []string{"go", "ts", "tsx", "vue", "py", "jsonl", "md"}
	words := []string{"export", "const", "return", "func", "import", "class", "def"}

	path := func(depth int) string {
		parts := make([]string, depth)
		for i := range parts {
			parts[i] = segs[rng.intn(len(segs))]
		}
		return strings.Join(parts, "/")
	}
	slug := func(depth int) string {
		parts := make([]string, depth)
		for i := range parts {
			parts[i] = segs[rng.intn(len(segs))]
		}
		return "-Users-jane-" + strings.Join(parts, "-")
	}

	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var payload string
		switch i % 7 {
		case 0: // mixed-case absolute path
			payload = "/Users/jane/" + path(3+rng.intn(3))
		case 1: // dash-encoded project slug, the project_dir shape
			payload = "~/.claude/projects/" + slug(2+rng.intn(3)) + "/session.jsonl"
		case 2: // grep output: path:line:code
			payload = path(3) + "." + exts[rng.intn(len(exts))] + ":" +
				fmt.Sprint(1+rng.intn(999)) + ":" + words[rng.intn(len(words))] + " " + segs[rng.intn(len(segs))]
		case 3: // relative path from build tooling
			payload = "./" + path(4+rng.intn(2)) + "." + exts[rng.intn(len(exts))]
		case 4: // URL with a long path
			payload = "https://github.com/QuesmaOrg/" + segs[rng.intn(len(segs))] + "/blob/main/" + path(3)
		case 5: // HTTP header values
			payload = "Accept: application/vnd.github+json Cache-Control: max-age=31536000"
		case 6: // UUIDs: dashes drag them under threshold, and that must stay true
			payload = randomUUID(rng) + " spawned " + randomUUID(rng)
		}

		switch i % 3 {
		case 0:
			out = append(out, `{"type":"user","uuid":"b`+fmt.Sprint(i)+`","cwd":"`+payload+`"}`)
		case 1:
			out = append(out, `{"type":"user","uuid":"b`+fmt.Sprint(i)+`","message":{"content":[{"type":"text","text":"see `+payload+` for details"}]}}`)
		case 2:
			out = append(out, `{"type":"user","uuid":"b`+fmt.Sprint(i)+`","toolUseResult":{"stdout":"`+payload+`"}}`)
		}
	}
	return out
}

// xorshift keeps the corpus identical across runs: fixed seed, no time, no math/rand.
type xorshift struct{ state uint64 }

func (x *xorshift) next() uint64 {
	x.state ^= x.state << 13
	x.state ^= x.state >> 7
	x.state ^= x.state << 17
	return x.state
}

func (x *xorshift) intn(n int) int { return int(x.next() % uint64(n)) }

func randomHex(rng *xorshift, n int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, n)
	for i := range out {
		out[i] = digits[rng.intn(16)]
	}
	return string(out)
}

func randomUUID(rng *xorshift) string {
	h := randomHex(rng, 32)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// randomHighEntropyToken rejects drafts until a test-local Shannon measure clears the engine's
// threshold with margin, so the recall assertion is fair rather than seed-lucky.
func randomHighEntropyToken(rng *xorshift, n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for {
		out := make([]byte, n)
		for i := range out {
			out[i] = alphabet[rng.intn(len(alphabet))]
		}
		if localShannonBits(string(out)) >= 4.3 {
			return string(out)
		}
	}
}

func localShannonBits(s string) float64 {
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	total := float64(len(s))
	bits := 0.0
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / total
		bits -= p * math.Log2(p)
	}
	return bits
}
