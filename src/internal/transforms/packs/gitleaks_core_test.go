package packs

import (
	"strings"
	"testing"
)

type probe struct {
	in   string // the scanned value
	want string // exact span the rule must produce; "" means it must find nothing
}

func tok(s string) probe { return probe{in: "out: " + s + " done", want: s} }
func not(s string) probe { return probe{in: "out: " + s + " done"} }

func checkProbes(t *testing.T, id string, r *Rule, probes []probe) {
	t.Helper()
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

// github-pat is written from GitHub's published token format: a documented prefix, then at least
// 36 letters and digits (30 random plus a 6-character checksum), with no fixed upper length because
// GitHub states token lengths change. Longer tokens must therefore match; shorter must not.
func TestGitHubPATFollowsDocumentedFormat(t *testing.T) {
	alnum := func(n int) string {
		return strings.Repeat("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", n/62+1)[:n]
	}
	probes := []probe{
		tok("ghp_" + alnum(36)), tok("gho_" + alnum(36)), tok("ghu_" + alnum(36)),
		tok("ghs_" + alnum(36)), tok("ghr_" + alnum(36)),
		tok("ghp_" + alnum(300)), // no documented upper bound
		not("ghp_" + alnum(35)),
		not("ghx_" + alnum(36)),                  // undocumented prefix
		not("ghp_" + alnum(30) + "-" + alnum(5)), // wrong alphabet
		not("xghp_" + alnum(36)),                 // glued to a word
		not("github_pat_" + alnum(82)),           // the fine-grained rule owns this prefix
	}
	r := rulesByID(t, GitleaksCore)["github-pat"]
	if r == nil {
		t.Fatal("github-pat missing from the pack")
	}
	checkProbes(t, "github-pat", r, probes)
}
