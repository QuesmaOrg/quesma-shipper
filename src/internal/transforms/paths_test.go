package transforms_test

import (
	"strings"
	"testing"
)

// --- the entropy backstop versus filesystem paths -----------------

// With "/" in the candidate alphabet a whole path prefix was one run, and the
// generic-entropy rule was 55% of all redaction hits on a real archive, shipping
// __REDACTED:generic-entropy__ as the repository name of 144 of 818 sessions. These
// fixtures are the shapes that fired: a lowercase path sits under threshold and proves
// nothing.
func TestOrdinaryPathsSurviveTheEntropyBackstop(t *testing.T) {
	s := newScrubber(t)

	cases := []struct {
		name string
		line string
		want string // a fragment that must survive, post path-user
	}{
		{
			// cwd is where the per-repository dimension downstream comes from. Exempt now, but the
			// next cases prove the regex fix protects it where no exemption applies.
			"mixed-case cwd",
			`{"type":"user","uuid":"u1","cwd":"/Users/jane/Work2026/SampleOrg/blink-UI/apps/webFrontend/src"}`,
			`/Users/__USER__/Work2026/SampleOrg/blink-UI/apps/webFrontend/src`,
		},
		{
			// The same path in free text, where no exemption reaches: it passes only because a
			// candidate can no longer span "/".
			"mixed-case path in prose",
			`{"type":"user","uuid":"u1","message":{"content":[{"type":"text","text":"the build lives in /Users/jane/Work2026/SampleOrg/blink-UI/apps/webFrontend/src now"}]}}`,
			`/Users/__USER__/Work2026/SampleOrg/blink-UI/apps/webFrontend/src`,
		},
		{
			// Grep output, the shape that shipped as __REDACTED:generic-entropy__.ts:185.
			"grep output",
			`{"type":"user","uuid":"u1","toolUseResult":{"stdout":"src/Components/CardPreview2/ButtonGroup_v3.tsx:42:export const ButtonGroup"}}`,
			`src/Components/CardPreview2/ButtonGroup_v3.tsx:42`,
		},
		{
			// A dash-encoded slug carrying the RAW username in free text: dashes stay in the candidate
			// class, so it is one long run at 4.35 bits/char, and heuristics see it BEFORE path-user
			// rewrites it. It survives only because the matcher skips candidates carrying the username.
			"raw-username slug in prose",
			`{"type":"user","uuid":"u1","message":{"content":[{"type":"text","text":"see ~/.claude/projects/-Users-jane-Work2026-SampleOrg-blink-UI-webFrontend/f00.jsonl"}]}}`,
			`-Users-__USER__-Work2026-SampleOrg-blink-UI-webFrontend`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := scrubJSONL(t, s, "claude-code", c.line+"\n")
			out := string(res.Out)

			if res.RuleHits["generic-entropy"] != 0 {
				t.Errorf("the entropy backstop fired on a path: %v\n%s", res.RuleHits, out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("path shape was not preserved:\n got %s\nwant a line containing %q", out, c.want)
			}
		})
	}
}

// __USER__ is built from in-class characters, so substituting it into a dash-encoded slug
// ADDS entropy: the rewritten path can cross a threshold its raw form sat under. This is
// the test that fails if the alphabet change lands without the placeholder skip.
func TestUserPlaceholderNeverTripsTheEntropyBackstop(t *testing.T) {
	s := newScrubber(t)

	// 4.22 bits/char as one run: over threshold, and in an unexempt field.
	payload := `{"type":"user","uuid":"u1","cwd":"/Users/__USER__/Work2026/SampleOrg/blink-UI",` +
		`"message":{"content":[{"type":"text","text":"logs under ~/.claude/projects/-Users-__USER__-Work2026-SampleOrg-blink-UI/f00.jsonl"}]}}` + "\n"

	res := scrubJSONL(t, s, "claude-code", payload)
	if string(res.Out) != payload {
		t.Errorf("already-placeholdered content changed:\n got %s\nwant %s", res.Out, payload)
	}
	if len(res.RuleHits) != 0 {
		t.Errorf("no rule should fire on already-scrubbed shapes: %v", res.RuleHits)
	}
}

// A sentinel begins with in-class characters and its ":" comes after them, so
// "activity-__REDACTED:card-pan__-KzYA" fuses into one candidate over the threshold and a
// second pass nests sentinels. The backstop must never eat its own ledger: candidates
// carrying the sentinel prefix are skipped.
func TestSentinelGlueNeverTripsTheEntropyBackstop(t *testing.T) {
	s := newScrubber(t)

	payload := `{"type":"user","uuid":"u1","message":{"content":[{"type":"text","text":"https://www.linkedin.com/posts/john-doe-1234567_acme-fastest-software-company-to-100m-in-activity-__REDACTED:card-pan__-KzYA"}]}}` + "\n"

	res := scrubJSONL(t, s, "claude-code", payload)
	if string(res.Out) != payload {
		t.Errorf("a second pass rewrote a sentinel-bearing value:\n got %s\nwant %s", res.Out, payload)
	}
	if res.RuleHits["generic-entropy"] != 0 {
		t.Errorf("the backstop fired on its own sentinel's surroundings: %v", res.RuleHits)
	}
}

// Dropping "/" from the entropy alphabet leans on the pattern packs for slash-carrying
// secrets: every labeled arrival of that shape must stay caught, and this test says so if
// a pack edit loosens one of the anchors.
func TestLabeledSecretsWithSlashesAreStillCaught(t *testing.T) {
	s := newScrubber(t)

	cases := []struct {
		name   string
		text   string
		secret string
	}{
		{
			"aws secret access key",
			"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		},
		{
			"azure storage account key",
			"DefaultEndpointsProtocol=https;AccountKey=abc123/def456+ghi789/jkl012+mno345/pqr678stu901vwx234yz567EXAMPLE==;",
			"abc123/def456+ghi789/jkl012+mno345/pqr678stu901vwx234yz567EXAMPLE==",
		},
		{
			"bearer token with slashes",
			"curl -H 'Authorization: Bearer x7Jq/2mVp+Rw9sTk/EXAMPLE='",
			"x7Jq/2mVp+Rw9sTk/EXAMPLE=",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := `{"type":"user","uuid":"u1","toolUseResult":{"stdout":"` + c.text + `"}}`
			res := scrubJSONL(t, s, "claude-code", line+"\n")
			if strings.Contains(string(res.Out), c.secret) {
				t.Errorf("a labeled slash-bearing secret survived:\n%s", res.Out)
			}
		})
	}
}

// The accepted residual of the path-entropy fix: a bare, UNLABELED std-base64 secret containing "/"
// splits at the slashes into segments under MinLength. Labeled arrivals of the same shape
// are still caught (TestLabeledSecretsWithSlashesAreStillCaught), as are slash-free bare
// secrets over MinLength.
func TestBareBase64WithSlashIsAKnownEscape(t *testing.T) {
	s := newScrubber(t)

	const bare = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	line := `{"type":"user","uuid":"u1","message":{"content":[{"type":"text","text":"deploy with ` + bare + ` for now"}]}}`

	res := scrubJSONL(t, s, "claude-code", line+"\n")
	if !strings.Contains(string(res.Out), bare) {
		t.Error("the backstop now catches bare slash-bearing base64 — the trade-off moved; update this note")
	}
}
