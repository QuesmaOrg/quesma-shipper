package packs

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The entry literals of every vendored rule: a keyword edit shows up as a diff in this table,
// and a rule that quietly stopped being anchored shows up as "sweep".
func TestAnchorLiterals(t *testing.T) {
	want := map[string]string{
		// gitleaks-core. slack-webhook and gcp-service-account-key declare "sweep":
		// their keywords occur mid-match, so anchoring would miss every redaction.
		"aws-access-key-id":       `wb ["AKIA" "ASIA" "ABIA" "ACCA"]`,
		"aws-secret-key":          `fold ["aws"]`,
		"github-pat":              `wb ["ghp_" "gho_" "ghu_" "ghs_" "ghr_"]`,
		"github-fine-grained-pat": `wb ["github_pat_"]`,
		"gitlab-pat":              `wb ["glpat-"]`,
		"slack-token":             `wb ["xoxb-" "xoxa-" "xoxp-" "xoxr-" "xoxs-"]`,
		"slack-webhook":           "sweep",
		"stripe-secret-key":       `wb ["sk_live_" "rk_live_" "sk_test_" "rk_test_"]`,
		"openai-api-key":          `wb ["sk-"]`,
		"anthropic-api-key":       `wb ["sk-ant-"]`,
		"google-api-key":          `wb ["AIza"]`,
		"npm-token":               `wb ["npm_"]`,
		"pypi-token":              `wb ["pypi-AgEI"]`,
		"sendgrid-key":            `wb ["SG."]`,
		"twilio-key":              `wb ["SK"]`,
		"azure-storage-key":       `["AccountKey="]`,
		"gcp-service-account-key": "sweep",
		// quesma-extra. secret-access-key is deliberately case-sensitive: a folded rule
		// cannot anchor on a keyword starting with "s" (the long-s fold orbit is
		// non-ASCII), so the wild spellings are enumerated as their own anchors.
		// hashicorp-tf-api-token's keyword occurs mid-match and azure-ad-client-secret's
		// two bytes in, so both declare "sweep".
		"secret-access-key":               `["SecretAccessKey" "secretAccessKey" "secret_access_key" "SECRET_ACCESS_KEY"]`,
		"digitalocean-token":              `wb ["dop_v1_" "doo_v1_" "dor_v1_"]`,
		"databricks-api-token":            `wb ["dapi"]`,
		"huggingface-access-token":        `wb ["hf_"]`,
		"shopify-token":                   `wb ["shpat_" "shpca_" "shppa_" "shpss_"]`,
		"linear-api-key":                  `wb ["lin_api_"]`,
		"notion-token":                    `wb ["ntn_"]`,
		"postman-api-token":               `wb ["PMAK-"]`,
		"grafana-token":                   `wb ["glc_" "glsa_" "eyJrIjoi"]`,
		"doppler-api-token":               `wb ["dp.pt."]`,
		"flyio-access-token":              `wb ["fo1_" "fm1" "fm2_"]`,
		"planetscale-token":               `wb ["pscale_tkn_" "pscale_oauth_" "pscale_pw_"]`,
		"pulumi-api-token":                `wb ["pul-"]`,
		"rubygems-api-token":              `wb ["rubygems_"]`,
		"hashicorp-tf-api-token":          "sweep",
		"vault-token":                     `wb ["hvs." "hvb."]`,
		"azure-ad-client-secret":          "sweep",
		"age-secret-key":                  `wb ["AGE-SECRET-KEY-1"]`,
		"sentry-token":                    `wb ["sntrys_" "sntryu_"]`,
		"1password-service-account-token": `wb ["ops_eyJ"]`,
		"perplexity-api-key":              `wb ["pplx-"]`,
		"heroku-api-key":                  `wb ["HRKU-AA"]`,
		"gitlab-token-family":             `wb ["gldt-" "glimt-" "glagent-" "gloas-" "glptt-" "glrt-" "glsoat-"]`,
		"stripe-webhook-secret":           `wb ["whsec_"]`,
		"groq-api-key":                    `wb ["gsk_"]`,
		"xai-api-key":                     `wb ["xai-"]`,
		"replicate-api-token":             `wb ["r8_"]`,
		"dockerhub-token":                 `wb ["dckr_pat_" "dckr_oat_"]`,
		"tailscale-key":                   `wb ["tskey-"]`,
		"supabase-token":                  `wb ["sbp_"]`,
		"netlify-pat":                     `wb ["nfp_"]`,
		"atlassian-api-token":             `wb ["ATATT3"]`,
		"artifactory-token":               `wb ["AKCp" "cmVmd"]`,
		"langsmith-api-key":               `wb ["lsv2_pt_" "lsv2_sk_"]`,
		"pinecone-api-key":                `wb ["pcsk_"]`,
		"azure-sas-token":                 `wb ["sig="]`,
		// cloud-keys. private-key-block sweeps for cost, a failed candidate running to the
		// value's end (see TestPEMHeaderFloodStaysLinear); url-userinfo-credentials for head-ness.
		"private-key-block":        "sweep",
		"jwt":                      `wb ["eyJ"]`,
		"authorization-bearer":     `fold ["authorization"]`,
		"basic-auth-header":        `fold ["authorization"]`,
		"url-userinfo-credentials": "sweep",
		"kubeconfig-token":         `fold wb ["token"]`,
		"ssh-private-key-openssh":  `wb ["OPENSSH PRIVATE KEY"]`,
		// pii-core. The scanner rules never consult an anchor: the regex is not run.
		"email":    "sweep",
		"iban":     "sweep",
		"card-pan": "sweep",
		"pesel":    "sweep",
	}

	seen := map[string]bool{}
	for _, pack := range []string{GitleaksCore, QuesmaExtra, CloudKeys, PIICore} {
		rules, err := Load(pack)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rules {
			got := describeAnchor(r)
			if want[r.id] != got {
				t.Errorf("%s: anchor is %s, table says %s", r.id, got, want[r.id])
			}
			seen[r.id] = true
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("%s: in the table, not in any pack", id)
		}
	}
}

func describeAnchor(r *Rule) string {
	if r.anchor == nil {
		return "sweep"
	}
	var b strings.Builder
	if r.anchor.fold {
		b.WriteString("fold ")
	}
	if r.anchor.wordEdge {
		b.WriteString("wb ")
	}
	fmt.Fprintf(&b, "%q", r.anchor.lits)
	return b.String()
}

// The shapes the build must refuse: a refusal costs throughput, a wrong acceptance costs a
// redaction.
func TestAnchorRefusals(t *testing.T) {
	cases := []struct {
		pattern  string
		keywords []string
		sweep    bool
		want     string
	}{
		{`\b(eyJ[A-Za-z0-9_-]{8,})\b`, []string{"eyJ"}, false, `wb ["eyJ"]`},
		{`(?:AKIA|ASIA)[0-9]{4}`, []string{"AKIA", "ASIA"}, false, `["AKIA" "ASIA"]`},
		{`(?i)token\s*:\s*(\w+)`, []string{"token"}, false, `fold ["token"]`},
		// The corpus sweep flag wins over an otherwise anchorable shape.
		{`\b(eyJ[A-Za-z0-9_-]{8,})\b`, []string{"eyJ"}, true, "sweep"},
		// No keywords, nothing to anchor on.
		{`(?i)[a-z]{4}\d`, nil, false, "sweep"},
		// A one-byte literal is not selective enough to pay for a candidate check.
		{`A[0-9]{16}`, []string{"A"}, false, "sweep"},
		// A non-ASCII keyword cannot be searched for as folded bytes.
		{`x[0-9]{16}`, []string{"xé"}, false, "sweep"},
		// \b in front of a non-word entry byte: the verify sees a text start there.
		{`\b-----BEGIN`, []string{"-----BEGIN"}, false, "sweep"},
		// (?i) folds s and k onto runes the two-case byte skip would not find.
		{`(?i)secret\s*=\s*(\w+)`, []string{"secret"}, false, "sweep"},
		{`(?i)key\s*=\s*(\w+)`, []string{"key"}, false, "sweep"},
		{`(?i)aws[_-]?key`, []string{"aws"}, false, `fold ["aws"]`},
		// A (?i) region anywhere but the head means the keywords' spelling is not the matches'.
		{`AKIA(?i)key`, []string{"AKIA"}, false, "sweep"},
		{`(?is)token.`, []string{"token"}, false, "sweep"},
		// More keywords than the cursor's fixed scratch holds.
		{`t[0-9]{4}`, []string{"t0", "t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8"}, false, "sweep"},
	}
	for _, tc := range cases {
		scan, err := newAnchorScan(tc.pattern, tc.keywords, tc.sweep)
		if err != nil {
			t.Fatalf("%s: %v", tc.pattern, err)
		}
		got := describeAnchor(&Rule{anchor: scan})
		if got != tc.want {
			t.Errorf("%s: anchor is %s, want %s", tc.pattern, got, tc.want)
		}
	}
}

// The differential fuzz: for every anchored rule, the literal scan and the plain sweep must return
// the same spans. The per-rule alphabet is adversarial: the paths differ only on near-matches.
func TestAnchorMatchesSweep(t *testing.T) {
	rng := rand.New(rand.NewSource(20260817))
	rounds := 20000
	if testing.Short() {
		rounds = 2000
	}
	total := 0
	for _, pack := range []string{GitleaksCore, QuesmaExtra, CloudKeys, PIICore} {
		rules, err := Load(pack)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rules {
			if r.anchor == nil {
				continue
			}
			alphabet := fuzzAlphabet(r)
			for i := 0; i < rounds; i++ {
				value := buildFuzzValue(rng, alphabet)
				got := r.matchAnchored(value)
				want := r.matchSweep(value)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: %q\n anchored %v\n sweep    %v", r.id, value, got, want)
				}
				total++
			}
		}
	}
	t.Logf("%d cases", total)
}

// fuzzAlphabet is the token pool one rule's fuzz draws from.
func fuzzAlphabet(r *Rule) []string {
	pool := []string{
		"", " ", "\t", "\n", "x", "X", "_", "-", ".", ":", "/", "@", "=", "\"", "'",
		"0", "9", "aA", "zZ", "://", "//", "\\", "+", ",", ";", "%",
		"İ", "K", "ſ", "é", "\xff", "\xc3",
		strings.Repeat("A", 16), strings.Repeat("0", 16), strings.Repeat("a", 40),
		strings.Repeat("Z", 35), strings.Repeat("f", 32), strings.Repeat("q", 22),
		"eyJhbGciOi", ".", "abcdefghijklmnopqrstuvwxyz0123456789",
	}
	for _, l := range r.anchor.lits {
		pool = append(pool, l, strings.ToLower(l), strings.ToUpper(l), swapCase(l))
		if len(l) > 1 {
			pool = append(pool, l[:len(l)-1], l+l)
		}
	}
	return pool
}

func swapCase(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case 'a' <= c && c <= 'z':
			b[i] = c - ('a' - 'A')
		case 'A' <= c && c <= 'Z':
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func buildFuzzValue(rng *rand.Rand, pool []string) string {
	var b strings.Builder
	n := 1 + rng.Intn(12)
	for i := 0; i < n; i++ {
		b.WriteString(pool[rng.Intn(len(pool))])
	}
	return b.String()
}

// The hand-written half of the fuzz: a candidate the pattern rejects sitting in front of, or
// inside, a real match. Getting the resume position wrong loses exactly these.
func TestAnchorOverlapTraps(t *testing.T) {
	rules := map[string]*Rule{}
	for _, pack := range []string{GitleaksCore, QuesmaExtra, CloudKeys, PIICore} {
		loaded, err := Load(pack)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range loaded {
			rules[r.id] = r
		}
	}

	values := []string{
		// A rejected candidate in front of a live one, sharing a prefix.
		"xAKIA0123456789ABCDEF AKIA0123456789ABCDEF",
		"AKIAAKIA0123456789ABCDEF",
		"SKSK" + strings.Repeat("a", 32),
		"SK" + strings.Repeat("a", 32) + "SK" + strings.Repeat("f", 32),
		"eyJ.eyJ" + strings.Repeat("a", 12) + ".eyJ" + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12),
		// Boundary traps around the \b rules.
		"_AIza" + strings.Repeat("b", 35),
		"AIza" + strings.Repeat("b", 36),
		" AIza" + strings.Repeat("b", 35) + " AIza" + strings.Repeat("c", 35),
		// Case-folded heads, including the runes (?i) reaches outside ASCII.
		"ToKeN: " + strings.Repeat("a", 25),
		"toKen: " + strings.Repeat("a", 25),
		"MY_ſECRET token : " + strings.Repeat("a", 25),
		"AWS_SECRET_ACCESS_KEY=" + strings.Repeat("a", 40),
		"aws access key id = " + strings.Repeat("A", 40) + " aws_secret_access_key=" + strings.Repeat("b", 40),
		// URL userinfo: schemes that fail, schemes that overlap, no scheme at all.
		"://user:pass@host",
		"x://user:pass@host",
		"1http://user:pass@host",
		"http://user:pass@host postgres://admin:hunter2@db:5432/app",
		"see http://a://user:pass@host",
		"ftp://u:p@h ftp://u:p@h",
		"http://user@host",
		"http://user:pw@ http://user:password@host",
		strings.Repeat("a", 100) + "://u:ppp@h",
		"a" + strings.Repeat(".", 50) + "://u:ppp@h://u:qqq@h",
	}

	for _, v := range values {
		for id, r := range rules {
			if r.anchor == nil {
				continue
			}
			got, want := r.matchAnchored(v), r.matchSweep(v)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s on %q:\n anchored %v\n sweep    %v", id, v, got, want)
			}
		}
	}
}

// The regression for the shape that made anchoring quadratic: every line a PEM header, none a
// footer, so every candidate ran the body to the value's end. Both assertions are needed, the rule
// unanchored and doubled input not blowing the time up. The bound is loose on purpose: a shape
// test, not a throughput budget.
func TestPEMHeaderFloodStaysLinear(t *testing.T) {
	rules, err := Load(CloudKeys)
	if err != nil {
		t.Fatal(err)
	}
	var pem *Rule
	for _, r := range rules {
		if r.id == "private-key-block" {
			pem = r
		}
	}
	if pem == nil {
		t.Fatal("cloud-keys no longer carries private-key-block")
	}
	if pem.anchor != nil {
		t.Fatal("private-key-block is anchored: its unbounded body makes a failed " +
			"candidate cost the whole value, which is why the corpus declares \"sweep\"")
	}

	flood := func(headers int) string {
		var b strings.Builder
		for i := 0; i < headers; i++ {
			fmt.Fprintf(&b, "src/testdata/key%d.pem:1:-----BEGIN PRIVATE KEY-----\n", i)
		}
		return b.String()
	}
	timeFlood := func(headers int) time.Duration {
		v := flood(headers)
		start := time.Now()
		if spans := pem.MatchScanned(v); len(spans) != 0 {
			t.Fatalf("%d headers: %d spans, want none (no footer in the value)", headers, len(spans))
		}
		return time.Since(start)
	}

	// Warm the code paths first, so the small case does not pay the large one's page faults.
	timeFlood(200)
	small := timeFlood(1600)
	large := timeFlood(3200)
	if large > 8*small+2*time.Millisecond {
		t.Errorf("doubling the headers took %v against %v: that is the quadratic shape back",
			large, small)
	}
}
