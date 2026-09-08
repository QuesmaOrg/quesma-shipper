package transforms_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// exemptions is the COMPILED baseline, not a test fixture: testing the same list the
// binary runs is the point.
func exemptions() map[string][]string {
	return transforms.CompiledExemptions()
}

func newScrubber(t *testing.T) *transforms.Scrubber {
	t.Helper()
	cfg := transforms.DefaultConfig()
	cfg.Exemptions = exemptions()
	cfg.Username = "jane"
	s, err := transforms.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func scrubJSONL(t *testing.T, s *transforms.Scrubber, family, payload string) transforms.Result {
	t.Helper()
	res, err := s.Scrub([]byte(payload), transforms.Hint{Family: family, JSONL: true})
	if err != nil {
		t.Fatalf("scrub returned an engine error: %v", err)
	}
	return res
}

// --- M3 gate: representative leaks -----------------------------------------

func TestRepresentativeLeaksAreRedacted(t *testing.T) {
	s := newScrubber(t)

	cases := []struct {
		name   string
		secret string
		ruleID string
	}{
		{"github pat", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "github-pat"},
		{"github fine-grained pat", "github_pat_11ABCDEFG0abcdefghijkl_mnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOP", "github-fine-grained-pat"},
		{"aws access key id", "AKIAIOSFODNN7EXAMPLE", "aws-access-key-id"},
		{"openai key", "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789", "openai-api-key"},
		{"anthropic key", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789", "anthropic-api-key"},
		{"slack bot token", "xoxb-123456789012-1234567890123-abcdefghijklmnopqrstuvwx", "slack-token"},
		{"stripe live key", "sk_live_abcdefghijklmnopqrstuvwx", "stripe-secret-key"},
		{"google api key", "AIzaSyAbCdEfGhIjKlMnOpQrStUvWxYz0123456", "google-api-key"},
		{"npm token", "npm_abcdefghijklmnopqrstuvwxyz0123456789", "npm-token"},
		{"gitlab pat", "glpat-abcdefghijklmnopqrst", "gitlab-pat"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk", "jwt"},
		{"digitalocean pat", "dop_v1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "digitalocean-token"},
		{"huggingface token", "hf_AbCdEfGhIjKlMnOpQrStUvWxYzAbCdEfGh", "huggingface-access-token"},
		{"vault service token", "hvs.CAESIJ0123456789abcdefghijklmnopqrstuvwxyz", "vault-token"},
		{"age secret key", "AGE-SECRET-KEY-1QPZRY9X8GF2TVDW0S3JN54KHCE6MUA7LQPZRY9X8GF2TVDW0S3JN54KHCE", "age-secret-key"},
		{"groq key", "gsk_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", "groq-api-key"},
		{"xai key", "xai-abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnopqr", "xai-api-key"},
		{"perplexity key", "pplx-abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUV", "perplexity-api-key"},
		{"dockerhub pat", "dckr_pat_abcdefghijklmnopqrstuvwxyzA", "dockerhub-token"},
		{"tailscale key", "tskey-auth-kFGiAS7CNTRL-abcdefghijklmnopqrstuv", "tailscale-key"},
		{"gitlab runner token", "glrt-abcdefghijklmnopqrst0", "gitlab-token-family"},
		{"stripe webhook secret", "whsec_abcdefghijklmnopqrstuvwxyzABCDEF", "stripe-webhook-secret"},
		{"atlassian api token", "ATATT3" + strings.Repeat("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", 3), "atlassian-api-token"},
		{"azure ad client secret", "abc8Q~dEfGhIjKlMnOpQrStUvWxYz0123456789~", "azure-ad-client-secret"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := `{"type":"user","message":{"content":[{"type":"text","text":"the key is ` + c.secret + ` ok"}]}}`
			res := scrubJSONL(t, s, "claude-code", line+"\n")

			if strings.Contains(string(res.Out), c.secret) {
				t.Errorf("secret survived:\n%s", res.Out)
			}
			if res.RuleHits[c.ruleID] == 0 {
				t.Errorf("expected rule %q to fire, hits were %v", c.ruleID, res.RuleHits)
			}
			if !strings.Contains(string(res.Out), "__REDACTED:") {
				t.Errorf("no sentinel in output:\n%s", res.Out)
			}
		})
	}
}

// The aws CLI labels the secret "SecretAccessKey", with no "aws" anywhere near it, and
// tool output reaches transcripts as JSON-in-string where key-name scrubbing cannot see
// the field. The whole value must go: a secret containing "/" must not survive as
// fragments around the entropy tokenizer's split points.
func TestAWSCliCredentialOutputIsFullyRedacted(t *testing.T) {
	s := newScrubber(t)
	const keyID = "AKIAIOSFODNN7EXAMPLE"
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"

	cases := []struct {
		name   string
		text   string
		ruleID string
	}{
		{"create-access-key tool output", `{\"AccessKey\": {\"UserName\": \"ingest\", \"AccessKeyId\": \"` + keyID + `\", \"SecretAccessKey\": \"` + secret + `\", \"Status\": \"Active\"}}`, "secret-access-key"},
		{"yaml-style label", "SecretAccessKey: " + secret, "secret-access-key"},
		{"env-style with aws prefix", "aws_secret_access_key = " + secret, "aws-secret-key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := `{"type":"user","message":{"content":[{"type":"tool_result","content":"` + c.text + `"}]}}`
			res := scrubJSONL(t, s, "claude-code", line+"\n")
			out := string(res.Out)
			if strings.Contains(out, secret) {
				t.Errorf("secret survived:\n%s", out)
			}
			if strings.Contains(out, "wJalrXUtnFEMI") {
				t.Errorf("secret fragment survived:\n%s", out)
			}
			if res.RuleHits[c.ruleID] == 0 {
				t.Errorf("%s did not fire: %v", c.ruleID, res.RuleHits)
			}
			if strings.Contains(out, keyID) {
				t.Errorf("access key id survived:\n%s", out)
			}
		})
	}
}

// A PEM block spans lines inside one JSON string value, the shape a byte-level regex
// over the raw file would mangle.
func TestPrivateKeyBlockIsRedacted(t *testing.T) {
	s := newScrubber(t)
	pem := "-----BEGIN RSA PRIVATE KEY-----\\nMIIEowIBAAKCAQEA3x2n\\n-----END RSA PRIVATE KEY-----"
	line := `{"type":"user","message":{"content":[{"type":"text","text":"` + pem + `"}]}}`

	res := scrubJSONL(t, s, "claude-code", line+"\n")
	if strings.Contains(string(res.Out), "MIIEowIBAAKCAQEA3x2n") {
		t.Errorf("key body survived:\n%s", res.Out)
	}
	if res.RuleHits["private-key-block"] == 0 {
		t.Errorf("private-key-block did not fire: %v", res.RuleHits)
	}
}

// printenv and kubectl output: the value has no recognisable shape but the key does,
// and only the value goes.
func TestKeyNameRulesCatchShapelessValues(t *testing.T) {
	s := newScrubber(t)

	cases := []string{
		"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY",
		"DATABASE_URL=postgres://app:hunter2@db.internal:5432/prod",
		"MY_SERVICE_TOKEN=plain-looking-value-1234",
		"ACME_PASSWORD: correct-horse-battery",
		"internal_secret = abcdefghijklmno",
	}
	for _, env := range cases {
		t.Run(env, func(t *testing.T) {
			line := `{"type":"user","toolUseResult":{"stdout":"` + env + `"}}`
			res := scrubJSONL(t, s, "claude-code", line+"\n")
			out := string(res.Out)

			name := env[:strings.IndexAny(env, "=:")]
			name = strings.TrimSpace(name)
			if !strings.Contains(out, name) {
				t.Errorf("the key name should survive — it is useful signal and not the secret:\n%s", out)
			}
			if !strings.Contains(out, "__REDACTED:") {
				t.Errorf("the value was not redacted:\n%s", out)
			}
		})
	}
}

// --- M3 gate: the exemption fixture ----------------------------------------

// A planted secret in EVERY structurally exempt field is still caught, which is only
// satisfiable because exemptions are detector-scoped: the pattern packs scan every
// field regardless.
func TestPlantedSecretInEveryExemptFieldIsStillCaught(t *testing.T) {
	s := newScrubber(t)
	const planted = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

	for family, paths := range exemptions() {
		for _, path := range paths {
			t.Run(family+"/"+path, func(t *testing.T) {
				line := buildRecordWithValueAt(t, path, planted)
				fam := family
				if fam == "*" {
					fam = "claude-code"
				}
				res := scrubJSONL(t, s, fam, line+"\n")

				if strings.Contains(string(res.Out), planted) {
					t.Errorf("a planted secret survived in exempt field %q:\n%s", path, res.Out)
				}
			})
		}
	}
}

// The other half of the same rule: the identifiers those exemptions protect must
// survive an ordinary scrub, or the causal graph dies.
func TestExemptIdentifiersSurvive(t *testing.T) {
	s := newScrubber(t)

	line := `{"type":"assistant","uuid":"9f2c4e10-3b7a-4c19-8f2e-1a2b3c4d5e6f",` +
		`"parentUuid":"1a2b3c4d-5e6f-4718-9a0b-1c2d3e4f5a6b",` +
		`"sessionId":"3f2504e0-4f89-41d3-9a0c-0305e82c3301",` +
		`"requestId":"req_011CQ8xKp3mNvRtY7wZa2bCd",` +
		`"timestamp":"2026-07-30T10:00:00.123Z","version":"2.1.220",` +
		`"message":{"id":"msg_01ABcdEfGhIjKlMnOpQrStUv","content":[{"type":"text","text":"hello"}]}}`

	res := scrubJSONL(t, s, "claude-code", line+"\n")
	out := string(res.Out)

	for _, id := range []string{
		"9f2c4e10-3b7a-4c19-8f2e-1a2b3c4d5e6f",
		"1a2b3c4d-5e6f-4718-9a0b-1c2d3e4f5a6b",
		"3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"req_011CQ8xKp3mNvRtY7wZa2bCd",
		"msg_01ABcdEfGhIjKlMnOpQrStUv",
		"2026-07-30T10:00:00.123Z",
		"2.1.220",
	} {
		if !strings.Contains(out, id) {
			t.Errorf("exempt identifier %q was redacted — the causal graph would not reassemble:\n%s", id, out)
		}
	}
}

// A redacted multi-record trajectory still reassembles: the join keys are what make a
// DAG out of a line-oriented file.
func TestGraphReassemblesAfterScrub(t *testing.T) {
	s := newScrubber(t)

	// The id must stay production-length: below the backstop's 24-char floor it passed
	// with an EMPTY exemption set while real archives shipped every tool_use id redacted.
	const toolUseID = "toolu_0183yENGzL6di289E8QTzyxi"

	payload := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"s1","message":{"content":[{"type":"text","text":"read the env"}]}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"s1","message":{"id":"m1","content":[{"type":"tool_use","id":"` + toolUseID + `","name":"Bash","input":{"command":"printenv"}}]}}`,
		`{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"s1","message":{"content":[{"type":"tool_result","tool_use_id":"` + toolUseID + `","content":"ok"}]},"toolUseResult":{"tool_use_id":"` + toolUseID + `","stdout":"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"}}`,
	}, "\n") + "\n"

	res := scrubJSONL(t, s, "claude-code", payload)
	if strings.Contains(string(res.Out), "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY") {
		t.Fatalf("secret survived:\n%s", res.Out)
	}

	// Walk the redacted output the way a downstream parser would.
	byUUID := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(res.Out)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("scrubbed output is not valid JSON: %v\n%s", err, line)
		}
		byUUID[rec["uuid"].(string)] = rec
	}
	if len(byUUID) != 3 {
		t.Fatalf("expected 3 records, got %d", len(byUUID))
	}
	if byUUID["a1"]["parentUuid"] != "u1" || byUUID["u2"]["parentUuid"] != "a1" {
		t.Error("parent chain broken")
	}
	content := byUUID["a1"]["message"].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["id"] != toolUseID {
		t.Error("tool_use block id broken — nothing can point at this call")
	}
	toolResult := byUUID["u2"]["toolUseResult"].(map[string]any)
	if toolResult["tool_use_id"] != toolUseID {
		t.Error("tool_use_id broken — the subagent join would fail")
	}
}

// The other end of the spawn-tree join: toolUseId is the only field in a subagent's
// meta.json that points anywhere, and it is exactly the shape the backstop eats.
func TestSubagentMetaJoinKeySurvives(t *testing.T) {
	s := newScrubber(t)

	payload := `{"agentType":"general-purpose","description":"Audit cache failures","toolUseId":"toolu_0183yENGzL6di289E8QTzyxi","spawnDepth":1}` + "\n"

	res := scrubJSONL(t, s, "claude-code", payload)
	var meta map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(res.Out))), &meta); err != nil {
		t.Fatalf("scrubbed meta.json is not valid JSON: %v", err)
	}
	if meta["toolUseId"] != "toolu_0183yENGzL6di289E8QTzyxi" {
		t.Errorf("toolUseId = %q — the subagent is an orphan", meta["toolUseId"])
	}
}

// --- opaque binary payloads -------------------------------------------------

// Cursor stores a hex-encoded image inside the assembled request: a declared opaque
// payload, exempt via the NESTED PATH content[].image.hex rather than a field name.
func TestDeclaredOpaqueBinaryPayloadSurvivesTheEntropyBackstop(t *testing.T) {
	s := newScrubber(t)

	// Generated rather than repeated: repeating hex has LOW Shannon entropy and would
	// never reach the backstop, so the test would pass for the wrong reason.
	imageHex := pseudoRandomHex(t, 6000)
	line := `{"composerId":"c8cbeb0b","content":[{"type":"image","image":{"hex":"` + imageHex + `"}}]}`

	res := scrubJSONL(t, s, "cursor", line+"\n")
	if !strings.Contains(string(res.Out), imageHex) {
		t.Error("the declared opaque payload was redacted; the entropy exemption did not apply")
	}

	// The same bytes in a field that is NOT declared opaque are fair game.
	other := `{"composerId":"c8cbeb0b","content":[{"type":"text","text":"` + imageHex + `"}]}`
	res2 := scrubJSONL(t, s, "cursor", other+"\n")
	if strings.Contains(string(res2.Out), imageHex) {
		t.Error("a high-entropy blob in an undeclared field should hit the backstop")
	}
}

// The backstop is blind to a long blob with repeating structure, however long it runs:
// it detects random-looking secrets, not binary, which is why the pattern packs stay broad.
func TestEntropyBackstopIsBlindToRepeatingStructure(t *testing.T) {
	s := newScrubber(t)
	repeating := strings.Repeat("89504e470d0a1a0a0000000d49484452", 200)

	res := scrubJSONL(t, s, "claude-code",
		`{"type":"user","message":{"content":[{"type":"text","text":"`+repeating+`"}]}}`+"\n")
	if !strings.Contains(string(res.Out), repeating) {
		t.Error("thresholds were retuned; update this note with the new behaviour")
	}
}

// pseudoRandomHex builds deterministic high-entropy hex from a fixed seed.
func pseudoRandomHex(t *testing.T, n int) string {
	t.Helper()
	const digits = "0123456789abcdef"
	out := make([]byte, n)
	state := uint64(0x2545F4914F6CDD1D)
	for i := range out {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		out[i] = digits[state%16]
	}
	return string(out)
}

// --- path_user --------------------------------------------------------------

// path_user is a rewriter, not a detector, so it runs on exempt fields too.
func TestPathUserRewritesEverywhereIncludingExemptFields(t *testing.T) {
	s := newScrubber(t)

	line := `{"type":"user","uuid":"u1","cwd":"/Users/jane/work/api",` +
		`"timestamp":"/Users/jane/x","message":{"content":[{"type":"text","text":"see /Users/jane/work/api/db.go"}]}}`

	res := scrubJSONL(t, s, "claude-code", line+"\n")
	out := string(res.Out)

	if strings.Contains(out, "/Users/jane") {
		t.Errorf("username survived in a path:\n%s", out)
	}
	if !strings.Contains(out, "/Users/__USER__/work/api") {
		t.Errorf("path shape was not preserved — paths must stay comparable across sessions:\n%s", out)
	}
	if res.RuleHits["path-user"] == 0 {
		t.Errorf("path-user did not register a hit: %v", res.RuleHits)
	}
}

// --- parse-miss versus engine error ---------------------------------

// A torn tail is raw-scanned and ships: a truncated last line is a parse-miss, never
// an engine error.
func TestTornTailIsRawScannedAndShips(t *testing.T) {
	s := newScrubber(t)

	payload := `{"type":"user","uuid":"u1","message":{"content":[{"type":"text","text":"fine"}]}}` + "\n" +
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"ghp_abcdefghijklmnopqrstuvwxyz0123456789 and tru`

	res, err := s.Scrub([]byte(payload), transforms.Hint{Family: "claude-code", JSONL: true})
	if err != nil {
		t.Fatalf("a torn tail must not be an engine error: %v", err)
	}
	out := string(res.Out)

	if strings.Contains(out, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Error("the pattern packs must still scan a torn line")
	}
	if res.LinesParsed != 1 || res.LinesRawScanned != 1 {
		t.Errorf("expected 1 parsed and 1 raw-scanned line, got %d and %d",
			res.LinesParsed, res.LinesRawScanned)
	}
	if res.ScanMode != transforms.ScanModeMixed {
		t.Errorf("scan_mode should be mixed, got %q", res.ScanMode)
	}
	if strings.HasSuffix(out, "\n") {
		t.Error("a missing final newline must not be added back: that would be fixing a tail")
	}
}

// A non-JSON payload is raw-scanned rather than skipped.
func TestNonJSONPayloadIsRawScanned(t *testing.T) {
	s := newScrubber(t)
	text := "$ printenv\nGITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789\nHOME=/Users/jane\n"

	res, err := s.Scrub([]byte(text), transforms.Hint{Family: "claude-code", JSONL: false})
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Out)
	if strings.Contains(out, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("secret survived a raw-text scan:\n%s", out)
	}
	if strings.Contains(out, "/Users/jane") {
		t.Errorf("path_user must apply to raw text too:\n%s", out)
	}
	if res.ScanMode != transforms.ScanModeRawText {
		t.Errorf("scan_mode: %q", res.ScanMode)
	}
}

// --- base64 -----------------------------------------------------------------

// A base64-encoded secret is invisible to every regex, so one level is decoded and
// scanned. One level only: recursion is unbounded work per byte.
func TestOneLevelOfBase64IsDecodedAndScanned(t *testing.T) {
	s := newScrubber(t)

	inner := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	once := base64.StdEncoding.EncodeToString([]byte(inner))
	twice := base64.StdEncoding.EncodeToString([]byte(once))

	res := scrubJSONL(t, s, "claude-code",
		`{"type":"user","message":{"content":[{"type":"text","text":"`+once+`"}]}}`+"\n")
	if strings.Contains(string(res.Out), once) {
		t.Errorf("a base64-wrapped secret must be caught:\n%s", res.Out)
	}

	// Double encoding is out of scope by design; pinning it documents the boundary.
	res2 := scrubJSONL(t, s, "claude-code",
		`{"type":"user","message":{"content":[{"type":"text","text":"`+twice+`"}]}}`+"\n")
	_ = res2 // no assertion on catching it; the entropy backstop may or may not fire
}

// --- fidelity ---------------------------------------------------------------

// A record with nothing to redact must come out byte-identical, so a re-shipped file
// differs downstream only where something was actually redacted.
func TestUntouchedRecordsAreByteIdentical(t *testing.T) {
	s := newScrubber(t)

	payload := `{"type":"user","uuid":"u1","timestamp":"2026-07-30T10:00:00Z","nested":{"b":2,"a":1},` +
		`"big":1234567890123456789,"float":1.5,"esc":"tab\there \"quoted\" <html>","nil":null,"arr":[3,2,1]}` + "\n"

	res := scrubJSONL(t, s, "claude-code", payload)
	if string(res.Out) != payload {
		t.Errorf("an untouched record was rewritten:\n got %s\nwant %s", res.Out, payload)
	}
	if res.BytesRedacted != 0 {
		t.Errorf("nothing should have been redacted, got %d bytes", res.BytesRedacted)
	}
}

// When a record IS modified, key order and number literals must still survive: 1.23e+18
// for a large integer is gratuitous damage to bytes the shipper preserves.
func TestModifiedRecordsPreserveKeyOrderAndNumbers(t *testing.T) {
	s := newScrubber(t)

	payload := `{"zeta":1,"alpha":"ghp_abcdefghijklmnopqrstuvwxyz0123456789","mid":1234567890123456789,"beta":true}` + "\n"
	res := scrubJSONL(t, s, "claude-code", payload)
	out := string(res.Out)

	if strings.Index(out, `"zeta"`) > strings.Index(out, `"alpha"`) {
		t.Errorf("key order was not preserved:\n%s", out)
	}
	if !strings.Contains(out, "1234567890123456789") {
		t.Errorf("a large integer lost precision:\n%s", out)
	}
	if !strings.Contains(out, "<html>") && strings.Contains(payload, "<html>") {
		t.Error("HTML escaping was applied where the input had none")
	}
}

// A recorded over-redaction, left alone deliberately. In a postgres URL the url-userinfo
// and email rules overlap, and the wider span wins: it takes the hostname with the
// password and attributes the hit to email. The safe direction on an overlap is the wider
// span, since preferring the narrower risks leaving a tail of a secret in the clear.
func TestKnownOverRedactionInConnectionStrings(t *testing.T) {
	s := newScrubber(t)
	line := `{"type":"user","toolUseResult":{"stdout":"psql postgres://app:hunter2@db.internal:5432/prod"}}`

	res := scrubJSONL(t, s, "claude-code", line+"\n")
	out := string(res.Out)

	if strings.Contains(out, "hunter2") {
		t.Error("the password must not survive")
	}
	if strings.Contains(out, "db.internal") {
		t.Error("the hostname now survives — the overlap rule changed; update this note")
	}
}

// --- the ledger -------------------------------------------------------------

func TestDensityAndRuleHitLedger(t *testing.T) {
	s := newScrubber(t)

	clean := `{"type":"user","message":{"content":[{"type":"text","text":"nothing to see"}]}}` + "\n"
	res := scrubJSONL(t, s, "claude-code", clean)
	if res.Density() != 0 {
		t.Errorf("a clean record should have zero density, got %v", res.Density())
	}

	dirty := `{"type":"user","message":{"content":[{"type":"text","text":"ghp_abcdefghijklmnopqrstuvwxyz0123456789"}]}}` + "\n"
	res = scrubJSONL(t, s, "claude-code", dirty)
	if res.Density() <= 0 || res.Density() > 1 {
		t.Errorf("density out of range: %v", res.Density())
	}
	if res.RuleHits["github-pat"] != 1 {
		t.Errorf("rule hits should count per rule: %v", res.RuleHits)
	}
	if res.BytesTotal != len(dirty) {
		t.Errorf("bytes_total %d, want %d", res.BytesTotal, len(dirty))
	}
}

// The sentinel's width depends only on the rule id, so it cannot leak the secret's
// length, and nothing in it derives from the value.
func TestSentinelLeaksNeitherLengthNorValue(t *testing.T) {
	s := newScrubber(t)

	short := "ghp_" + strings.Repeat("a", 36)
	long := "ghp_" + strings.Repeat("b", 200)

	outShort := scrubJSONL(t, s, "claude-code", `{"t":"`+short+`"}`+"\n")
	outLong := scrubJSONL(t, s, "claude-code", `{"t":"`+long+`"}`+"\n")

	if len(outShort.Out) != len(outLong.Out) {
		t.Errorf("sentinel width tracks the secret length: %d vs %d bytes",
			len(outShort.Out), len(outLong.Out))
	}
	if !strings.Contains(string(outShort.Out), transforms.Sentinel("github-pat")) {
		t.Errorf("sentinel should carry the rule id: %s", outShort.Out)
	}
	for _, frag := range []string{"aaaa", "bbbb"} {
		if strings.Contains(string(outShort.Out), frag) || strings.Contains(string(outLong.Out), frag) {
			t.Error("a fragment of the secret survived in the placeholder")
		}
	}
}

// Idempotence: a second pass must not redact the first pass's placeholders, which would
// destroy the ledger's meaning.
func TestScrubIsIdempotent(t *testing.T) {
	s := newScrubber(t)
	// Deliberately long and mixed-case: the first pass turns "jane" into "__USER__", which
	// ADDS entropy, and the second pass must not eat that.
	payload := `{"type":"user","cwd":"/Users/jane/Work2026/SampleOrg/blink-UI","message":{"content":[{"type":"text","text":"ghp_abcdefghijklmnopqrstuvwxyz0123456789 in ~/.claude/projects/-Users-jane-Work2026-SampleOrg-blink-UI/f00.jsonl"}]}}` + "\n"

	first := scrubJSONL(t, s, "claude-code", payload)
	second := scrubJSONL(t, s, "claude-code", string(first.Out))

	if string(second.Out) != string(first.Out) {
		t.Errorf("second pass changed the output:\n first %s\nsecond %s", first.Out, second.Out)
	}
}

// A pack named in config but absent from the corpus must fail loudly: running with fewer
// rules than configured weakens the floor with no signal.
func TestUnknownPackIsAnError(t *testing.T) {
	cfg := transforms.DefaultConfig()
	cfg.RulePacks = append(cfg.RulePacks, "acme-invented")
	if _, err := transforms.New(cfg); err == nil {
		t.Fatal("an unknown rule pack must be refused at compile time")
	}
}

// buildRecordWithValueAt plants a value at a dotted path ("[]" meaning an array element),
// so the fixture can hit exactly the path an exemption names.
func buildRecordWithValueAt(t *testing.T, path, value string) string {
	t.Helper()

	segments := strings.Split(path, ".")
	var build func(i int) any
	build = func(i int) any {
		if i == len(segments) {
			return value
		}
		seg := segments[i]
		if strings.HasSuffix(seg, "[]") {
			return map[string]any{
				strings.TrimSuffix(seg, "[]"): []any{build(i + 1)},
			}
		}
		return map[string]any{seg: build(i + 1)}
	}

	rec, ok := build(0).(map[string]any)
	if !ok {
		t.Fatalf("could not build a record for path %q", path)
	}
	rec["type"] = "user"
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The compiled default must protect the identifier spine on its own: exemptions once
// reached the scrubber through config resolution only, so DefaultConfig() redacted the ids
// joining a subagent transcript to its parent. Both ends redact to the same sentinel, which
// is what makes it silent.
func TestTheCompiledDefaultProtectsTheIdentifierSpine(t *testing.T) {
	s, err := transforms.New(transforms.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	// High-entropy on purpose: this is the shape the backstop fires on.
	const id = "toolu_01FcSqsnNxWeeDGKyfZKjJHbXk9QwErTyU"
	for _, field := range []string{"toolUseId", "sessionId", "uuid", "requestId"} {
		in := `{"` + field + `":"` + id + `"}` + "\n"
		res, err := s.Scrub([]byte(in), transforms.Hint{Family: "claude-code", JSONL: true})
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Out) != in {
			t.Errorf("%s was redacted by the compiled default:\n got %s\nwant %s",
				field, res.Out, in)
		}
	}

	// Exemption is detector-scoped: a credential planted in an exempt field is still caught.
	res, err := s.Scrub([]byte(`{"toolUseId":"AKIAIOSFODNN7EXAMPLE"}`+"\n"),
		transforms.Hint{Family: "claude-code", JSONL: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Out), "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("a credential in an exempt field survived: %s", res.Out)
	}

	// And a field nobody exempted is still scanned, or the test above proves nothing.
	res, err = s.Scrub([]byte(`{"someField":"`+id+`"}`+"\n"),
		transforms.Hint{Family: "claude-code", JSONL: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Out), "__REDACTED:") {
		t.Errorf("an unexempted high-entropy value was not redacted: %s", res.Out)
	}
}

// A pattern that backtracks pathologically stalls an install for seconds without erroring, so the
// worst case has to be kept. One Scrubber serves every worker, hence the atomic.
func TestTheScrubberKeepsItsWorstCase(t *testing.T) {
	s, err := transforms.New(transforms.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if d, n := s.Slowest(); d != 0 || n != 0 {
		t.Fatalf("a fresh Scrubber claims a slowest scrub: %v over %d bytes", d, n)
	}

	small := []byte(`{"a":"x"}`)
	big := make([]byte, 0, 64<<10)
	for len(big) < 64<<10 {
		big = append(big, `{"msg":"the quick brown fox jumps over the lazy dog"}`+"\n"...)
	}
	if _, err := s.Scrub(big, transforms.Hint{JSONL: true}); err != nil {
		t.Fatal(err)
	}
	afterBig, bytesBig := s.Slowest()
	if afterBig == 0 {
		t.Error("scrubbing 64 KB registered no cost at all")
	}
	if bytesBig != int64(len(big)) {
		t.Errorf("slowest scrub is attributed to %d bytes, want %d", bytesBig, len(big))
	}

	// The small one must not displace it: this is a maximum, not a last-value.
	if _, err := s.Scrub(small, transforms.Hint{JSONL: true}); err != nil {
		t.Fatal(err)
	}
	if d, n := s.Slowest(); d != afterBig || n != bytesBig {
		t.Errorf("a cheaper scrub overwrote the worst case: %v over %d bytes", d, n)
	}
}
