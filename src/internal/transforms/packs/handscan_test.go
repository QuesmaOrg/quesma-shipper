package packs

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// The wiring: the email rule has a scanner, and no rule with a capture group does, since a
// scanner returns whole spans and would redact a different span than the pattern.
func TestHandScannersEngage(t *testing.T) {
	found := false
	for _, pack := range []string{GitleaksCore, QuesmaExtra, CloudKeys, PIICore} {
		rules, err := Load(pack)
		if err != nil {
			t.Fatalf("Load(%s): %v", pack, err)
		}
		for _, r := range rules {
			if r.hand == nil {
				continue
			}
			if r.capture != 0 {
				t.Errorf("rule %s has a hand scanner and a capture group", r.id)
			}
			// A rule the regex never runs for has no business carrying an anchor.
			if r.anchor != nil {
				t.Errorf("rule %s has both a hand scanner and an anchor", r.id)
			}
			if r.id == "email" {
				found = true
			}
		}
	}
	if !found {
		t.Error(`the email rule has no hand scanner: check "scanner": "email" in pii-core.json`)
	}
}

// The equivalence proof scanEmail rests on: scanner and regex agree on every input.
func TestScanEmailMatchesRegex(t *testing.T) {
	re := regexp.MustCompile(emailRegex)
	// The scanner yields candidates without a rule id; Rule.checked stamps those.
	want := func(v string) []Span {
		var out []Span
		for _, loc := range re.FindAllStringIndex(v, -1) {
			out = append(out, Span{Start: loc[0], End: loc[1]})
		}
		return out
	}
	checked := 0
	check := func(t *testing.T, v string) {
		t.Helper()
		checked++
		got := scanEmail(v)
		w := want(v)
		if len(got) != len(w) {
			t.Fatalf("scanEmail(%q) = %v, regex = %v", v, got, w)
		}
		for i := range got {
			if got[i] != w[i] {
				t.Fatalf("scanEmail(%q) = %v, regex = %v", v, got, w)
			}
		}
	}

	// Edges: value boundaries, clustered '@', TLD length bounds, non-ASCII either side.
	for _, v := range []string{
		"", "@", "a@b.com", "@a.com", "a@", "..@a.com", "a@@b.com", "a@b@c.com",
		"a@b.com@c.de", "a@b.com1", "a@b.com_", "a@b.co.1", "a@b.c.com", "a@b..com",
		".a@b.com", "a.@b.com", "-a@b.com", "_@a.com", "1@a.com", "%@a.com",
		"a@-.com", "a@.com", "a@b.-com", "x@y.c", "x@y.cc", "x@y.c1", "@@@@",
		"a@a@a@a.com", "a@b.comX@d.ee", "a@b.co@d.ee", "\xffa@b.com", "a@b.com\xff",
		"użytkownik@przykład.com", "用户@example.com",
		"mail to foo.bar%baz+q@sub.example.co.uk!", "a@b.com.", "a@b.com-",
		"aaaa@bbbb.cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	} {
		check(t, v)
	}

	// Exhaustive over every string up to five bytes of the pattern's own structural bytes.
	alphabet := []string{"@", ".", "-", "_", "%", "a", "c", "o", "m", "0", "!", "Z"}
	var walk func(prefix string, depth int)
	walk = func(prefix string, depth int) {
		check(t, prefix)
		if depth == 0 {
			return
		}
		for _, s := range alphabet {
			walk(prefix+s, depth-1)
		}
	}
	walk("", 5)

	// Random '@'-dense values assembled from tokens, so real shapes appear at every offset.
	tokens := []string{
		"@", "a", "Z", "0", "9", ".", "-", "_", "%", "+", "!", " ", "=", "/", "\"",
		"com", "co", "c", "example", "é", "\xff", "\n", "\t", "..", "@@",
	}
	rng := rand.New(rand.NewSource(20260816))
	for i := 0; i < 200000; i++ {
		var b strings.Builder
		for n := rng.Intn(24); n > 0; n-- {
			b.WriteString(tokens[rng.Intn(len(tokens))])
		}
		check(t, b.String())
	}
	t.Logf("compared %d values against the regex", checked)
}
