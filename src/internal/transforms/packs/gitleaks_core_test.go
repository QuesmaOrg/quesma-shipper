package packs

import (
	"strings"
	"testing"
)

// github-pat is written from GitHub's published token format: a documented prefix, then at least
// 36 letters and digits (30 random plus a 6-character checksum), with no fixed upper length because
// GitHub states token lengths change. Longer tokens must therefore match; shorter must not.
func TestGitHubPATFollowsDocumentedFormat(t *testing.T) {
	alnum := func(n int) string {
		return strings.Repeat("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", n/62+1)[:n]
	}
	type probe struct{ in, want string }
	tok := func(s string) probe { return probe{"out: " + s + " done", s} }
	not := func(s string) probe { return probe{in: "out: " + s + " done"} }
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
	rules, err := Load(GitleaksCore)
	if err != nil {
		t.Fatal(err)
	}
	var r *Rule
	for _, x := range rules {
		if x.id == "github-pat" {
			r = x
		}
	}
	if r == nil {
		t.Fatal("github-pat missing from the pack")
	}
	for _, p := range probes {
		spans := r.MatchScanned(p.in)
		if p.want == "" {
			if len(spans) != 0 {
				t.Errorf("near-miss %q matched: %v", p.in, spans)
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
			t.Errorf("%q: want exact span, got %v", p.in, spans)
		}
	}
}
