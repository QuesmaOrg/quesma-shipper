package packs

import (
	"strings"
	"testing"
)

// Adversarial coverage for every quesma-extra rule: true tokens the rule must redact as
// one exact span, and near-misses (length off by one, wrong charset or case, glued
// prefix, missing delimiter) it must leave alone, so a widened or narrowed regex fails loud.
func TestQuesmaExtraAdversarial(t *testing.T) {
	rep := func(alphabet string, n int) string {
		return strings.Repeat(alphabet, n/len(alphabet)+1)[:n]
	}
	hexs := func(n int) string { return rep("0123456789abcdef", n) }
	alnum := func(n int) string {
		return rep("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", n)
	}
	letters := func(n int) string { return rep("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", n) }
	bech := func(n int) string { return rep("QPZRY9X8GF2TVDW0S3JN54KHCE6MUA7L", n) }

	type probe struct {
		in   string // the scanned value
		want string // exact span the rule must produce; "" means it must find nothing
	}
	tok := func(s string) probe { return probe{in: "out: " + s + " done", want: s} }
	not := func(s string) probe { return probe{in: "out: " + s + " done"} }

	const s40 = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	azTail := alnum(31) + "~_."

	cases := map[string][]probe{
		"secret-access-key": {
			{in: "SecretAccessKey: " + s40, want: s40},
			{in: `"secretAccessKey": "` + s40 + `"`, want: s40},
			{in: "SECRET_ACCESS_KEY=" + s40, want: s40},
			{in: "secretaccesskey: " + s40},                                  // unlisted spelling stays unmatched
			{in: "SecretAccessKey: " + s40[:39], want: s40[:39]},             // non-AWS providers vary the length
			{in: "SecretAccessKey: " + s40 + s40[:24], want: s40 + s40[:24]}, // 64-char R2/MinIO-style
			{in: "SecretAccessKey: " + s40[:11]},
			{in: "SecretAccessKeyId " + s40}, // no separator after the longer label
		},
		"digitalocean-token": {
			tok("dop_v1_" + hexs(64)),
			tok("dor_v1_" + hexs(64)),
			not("dop_v1_" + hexs(63)),
			not("Xdop_v1_" + hexs(64)), // glued to a word: \b must refuse
		},
		"databricks-api-token": {
			tok("dapi" + hexs(32)),
			not("dapi" + hexs(31)),
			not("updapi" + hexs(32)),
		},
		"huggingface-access-token": {
			tok("hf_" + letters(34)),
			tok("hf_" + alnum(34)),
			not("hf_" + letters(33)),
			not("hf_" + letters(35)),
		},
		"shopify-token": {
			tok("shpat_" + hexs(32)),
			tok("shpss_" + hexs(32)),
			not("shpat_" + hexs(31)),
			not("shpzz_" + hexs(32)),
		},
		"linear-api-key": {
			tok("lin_api_" + alnum(40)),
			not("lin_api_" + alnum(39)),
		},
		"notion-token": {
			tok("ntn_12345678901" + alnum(35)),
			not("ntn_1234567890" + letters(36)), // ten digits, not eleven
		},
		"postman-api-token": {
			tok("PMAK-" + hexs(24) + "-" + hexs(34)),
			not("pmak-" + hexs(24) + "-" + hexs(34)),
		},
		"grafana-token": {
			tok("glc_" + alnum(64) + "="),
			tok("glsa_" + alnum(32) + "_" + hexs(8)),
			tok("eyJrIjoi" + alnum(80)),
			not("glsa_" + alnum(32) + "_" + hexs(7)),
		},
		"doppler-api-token": {
			tok("dp.pt." + alnum(43)),
			not("dpxptx" + alnum(43)), // the dots are literal
			not("dp.pt." + alnum(39)),
		},
		"flyio-access-token": {
			tok("fo1_" + alnum(43)),
			tok("fm2_" + alnum(100) + "=="),
			not("fo1_" + alnum(42)),
			not("fm3_" + alnum(100)),
		},
		"planetscale-token": {
			tok("pscale_tkn_" + alnum(43)),
			tok("pscale_pw_" + alnum(43)),
			not("pscale_tkn_" + alnum(31)),
		},
		"pulumi-api-token": {
			tok("pul-" + hexs(40)),
			not("pul-" + strings.ToUpper(hexs(40))),
		},
		"rubygems-api-token": {
			tok("rubygems_" + hexs(48)),
			not("rubygems_" + hexs(47)),
		},
		"hashicorp-tf-api-token": {
			tok(alnum(14) + ".atlasv1." + alnum(64)),
			not(alnum(13) + ".atlasv1." + alnum(64)),
			not(alnum(14) + ".atlasv2." + alnum(64)),
		},
		"vault-token": {
			tok("hvs." + alnum(95)),
			tok("hvb." + alnum(150)),
			not("hvs." + alnum(23)),
			not("hvx." + alnum(95)),
		},
		"azure-ad-client-secret": {
			{in: "password: abc8Q~" + azTail + " end", want: "abc8Q~" + azTail},
			{in: "PWD=abc8Q~" + azTail + ";Encrypt=yes", want: "abc8Q~" + azTail},
			{in: "client_secret=abc8Q~" + azTail + "&grant_type=cc", want: "abc8Q~" + azTail},
			{in: "map[secret:-bc8Q~" + azTail + "]", want: "-bc8Q~" + azTail},
			{in: "password: abcdQ~" + azTail + " end"}, // no digit before Q~
			{in: "password: abc8Q~" + alnum(30) + " end"},
		},
		"age-secret-key": {
			tok("AGE-SECRET-KEY-1" + bech(58)),
			not("AGE-SECRET-KEY-1" + bech(57)),
			not("age-secret-key-1" + strings.ToLower(bech(58))),
		},
		"sentry-token": {
			tok("sntryu_" + hexs(64)),
			tok("sntrys_" + alnum(120)),
			not("sntryz_" + hexs(64)),
		},
		"1password-service-account-token": {
			tok("ops_eyJ" + alnum(220) + "="),
			not("ops_eyJ" + alnum(150)),
		},
		"langsmith-api-key": {
			tok("lsv2_pt_" + hexs(32) + "_" + hexs(10)),
			tok("lsv2_sk_" + hexs(32) + "_" + hexs(10)),
			not("lsv2_pt_" + hexs(32) + "_" + hexs(9)),
			not("lsv2_xx_" + hexs(32) + "_" + hexs(10)),
			not("lsv2_pt_" + strings.ToUpper(hexs(32)) + "_" + hexs(10)),
		},
		"pinecone-api-key": {
			tok("pcsk_" + alnum(5) + "_" + alnum(63)),
			tok("pcsk_" + alnum(6) + "_" + alnum(63)),
			not("pcsk_" + alnum(5) + "_" + alnum(62)),
			not("pcsk_" + alnum(8) + "_" + alnum(63)), // label too long for the 5-6 window
		},
		"azure-sas-token": {
			{in: "https://a.blob.core.windows.net/c?sp=r&sig=" + alnum(43) + "=&se=2026", want: alnum(43) + "="},
			{in: "out: sig=" + alnum(20) + "%2F" + alnum(20) + "%3D done", want: alnum(20) + "%2F" + alnum(20) + "%3D"},
			not("sig=" + alnum(39)),
			not("Xsig=" + alnum(43) + "="), // glued to a word: \b must refuse
			not("SIG=" + alnum(43) + "="),  // the query param is lowercase
		},
		"perplexity-api-key": {
			tok("pplx-" + alnum(48)),
			not("pplx-" + alnum(47)),
		},
		"heroku-api-key": {
			tok("HRKU-AA" + alnum(58)),
			tok("HRKU-AA" + alnum(57) + "-"),
			not("HRKU-AA" + alnum(57)),
			not("hrku-aa" + alnum(58)),
		},
		"gitlab-token-family": {
			tok("glrt-" + alnum(20)),
			tok("glagent-" + alnum(50)),
			tok("glrt-" + alnum(19) + "-"),
			not("glrt-" + alnum(19)),
			not("glzz-" + alnum(20)),
		},
		"stripe-webhook-secret": {
			tok("whsec_" + alnum(32)),
			tok("whsec_" + alnum(20) + "/" + alnum(20)),
			not("whsec_" + alnum(31)),
		},
		"groq-api-key": {
			tok("gsk_" + alnum(52)),
			not("gsk_" + alnum(51)),
		},
		"xai-api-key": {
			tok("xai-" + alnum(80)),
			not("xai-" + alnum(79)),
		},
		"replicate-api-token": {
			tok("r8_" + alnum(37)),
			tok("r8_" + alnum(36) + "-"),
			not("r8_" + alnum(36)),
		},
		"dockerhub-token": {
			tok("dckr_pat_" + alnum(27)),
			tok("dckr_oat_" + alnum(32)),
			not("dckr_pat_" + alnum(26)),
		},
		"tailscale-key": {
			tok("tskey-auth-kFGiAS7CNTRL-" + alnum(22)),
			not("tskey-auth-kFGiAS7CNTRL"),
			not("tskey-AUTH-kFGiAS7CNTRL-" + alnum(22)), // type segment is lowercase
		},
		"supabase-token": {
			tok("sbp_" + hexs(40)),
			not("sbp_" + hexs(39)),
			not("sbp_" + strings.ToUpper(hexs(40))),
		},
		"netlify-pat": {
			tok("nfp_" + alnum(36)),
			not("nfp_" + alnum(35)),
		},
		"atlassian-api-token": {
			tok("ATATT3" + alnum(186)),
			tok("ATATT3" + alnum(102)), // the official 108-char short-exception token
			not("ATATT3" + alnum(99)),
		},
		"artifactory-token": {
			tok("AKCp" + alnum(69)),
			tok("cmVmd" + alnum(59)),
			not("AKCp" + alnum(68)),
		},
	}

	rules, err := Load(QuesmaExtra)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*Rule{}
	for _, r := range rules {
		byID[r.id] = r
	}
	for id, probes := range cases {
		r := byID[id]
		if r == nil {
			t.Errorf("%s: in the table, not in the pack", id)
			continue
		}
		for _, p := range probes {
			spans := r.MatchScanned(p.in)
			if p.want == "" {
				if len(spans) != 0 {
					t.Errorf("%s: near-miss %q matched: %v", id, p.in, spans)
				}
				continue
			}
			found := false
			for _, s := range spans {
				if p.in[s.Start:s.End] == p.want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: %q: want exact span %q, got %v", id, p.in, p.want, spans)
			}
		}
	}
	for _, r := range rules {
		if _, ok := cases[r.id]; !ok {
			t.Errorf("%s: no adversarial probes", r.id)
		}
	}
}
