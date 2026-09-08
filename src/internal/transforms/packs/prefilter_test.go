package packs

import (
	"math/rand"
	"strings"
	"testing"
)

// containsAny is the executable spec the automaton is held to: does any keyword occur in the
// value, folding ASCII letter bytes and nothing else.
func containsAny(value string, keywords []string) bool {
	folded := asciiLowered(value)
	for _, k := range keywords {
		if strings.Contains(folded, asciiLowered(k)) {
			return true
		}
	}
	return false
}

// asciiLowered folds ASCII upper-case letter bytes and leaves every other byte as it is.
func asciiLowered(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// The runes Go's case machinery relates to ASCII, all of which keyword matching ignores.
const (
	testDottedI  = "İ" // strings.ToLower gives "i"
	testKelvin   = "K" // strings.ToLower gives "k"
	testLongS    = "ſ" // Go's (?i) folds it with s and S
	testNonASCII = "é" // an ordinary accented letter, no ASCII relation at all
)

// The safety argument for the one-pass automaton: every gate must fire exactly when containsAny
// would. A gate firing too rarely is a silently weakened scrub floor, so predicates are compared.
func TestPrefilterAgreesWithContainsAny(t *testing.T) {
	var keywordSets [][]string
	for _, pack := range PatternPacks {
		rules, err := Load(pack)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rules {
			if len(r.Keywords()) > 0 {
				keywordSets = append(keywordSets, r.Keywords())
			}
		}
	}
	if len(keywordSets) < 10 {
		t.Fatalf("expected the corpora to carry keyword sets, got %d", len(keywordSets))
	}

	b := NewPrefilterBuilder()
	gates := make([]Gate, len(keywordSets))
	for i, kws := range keywordSets {
		g, err := b.AddKeywords(kws)
		if err != nil {
			t.Fatal(err)
		}
		gates[i] = g
	}
	p := b.Build()

	fragments := []string{
		"AKIA", "akia", "AkIa", "aws", "AWS", "sk-", "sk-ant-", "ghp_", "xoxb-",
		"authorization", "AUTHORIZATION", "://", "@", "SG.", "SK", "eyJ",
		"PRIVATE KEY", "private key", "OPENSSH PRIVATE KEY", "AccountKey=",
		"service_account", "hooks.slack.com", "token", "TOKEN", "npm_", "AIza",
		" ", "\n", "x", "0", testDottedI, testKelvin, testLongS, testNonASCII,
		"\xff\xfe", "日本語",
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		var sb strings.Builder
		for n := rng.Intn(12); n >= 0; n-- {
			sb.WriteString(fragments[rng.Intn(len(fragments))])
		}
		value := sb.String()
		seen := p.Scan(value)
		for j, kws := range keywordSets {
			if got, want := seen.Has(gates[j]), containsAny(value, kws); got != want {
				t.Fatalf("gate %d (%v) on %q: prefilter %v, containsAny %v", j, kws, value, got, want)
			}
		}
	}
}

// The contract in both directions: ASCII case flips fire the gate, and the runes Go relates
// to ASCII do not. containsAny must agree on every fixture.
func TestPrefilterFoldsASCIIOnly(t *testing.T) {
	keywords := []string{"apikey", "kubectl"}
	b := NewPrefilterBuilder()
	gate, err := b.AddKeywords(keywords)
	if err != nil {
		t.Fatal(err)
	}
	p := b.Build()

	for _, value := range []string{
		"apikey", "APIKEY", "ApiKey",
		"KUBECTL",
		"xxKubectl xx",
	} {
		if seen := p.Scan(value); !seen.Has(gate) {
			t.Errorf("gate did not fire on %q", value)
		}
		if !containsAny(value, keywords) {
			t.Errorf("containsAny disagrees on %q, the fixture is wrong", value)
		}
	}

	for _, value := range []string{
		"AP" + testDottedI + "KEY",
		testKelvin + "UBECTL",
		"ap" + testLongS + "key",
	} {
		if seen := p.Scan(value); seen.Has(gate) {
			t.Errorf("the fold is ASCII only: a keyword gate must not fire on %q", value)
		}
		if containsAny(value, keywords) {
			t.Errorf("containsAny disagrees on %q, the fixture is wrong", value)
		}
	}
}

func TestPrefilterRejectsNonASCIIKeywords(t *testing.T) {
	b := NewPrefilterBuilder()
	bad := "cl" + testNonASCII
	if _, err := b.AddKeywords([]string{"ok", bad}); err == nil {
		t.Fatal("expected a non-ASCII keyword to fail the build")
	} else if !strings.Contains(err.Error(), bad) {
		t.Errorf("the error must name the offending keyword, got %v", err)
	}
	if _, err := b.AddKeywords([]string{""}); err == nil {
		t.Fatal("expected an empty keyword to fail the build")
	}
}

// Same registrations, same tables: the build stays reproducible.
func TestPrefilterGatesAreDeterministic(t *testing.T) {
	build := func() *Prefilter {
		b := NewPrefilterBuilder()
		for _, pack := range PatternPacks {
			rules, err := Load(pack)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rules {
				if _, err := b.AddKeywords(r.Keywords()); err != nil {
					t.Fatal(err)
				}
			}
		}
		return b.Build()
	}
	a, c := build(), build()
	if a.width != c.width || len(a.next) != len(c.next) {
		t.Fatal("table shape differs between builds")
	}
	for i := range a.next {
		if a.next[i] != c.next[i] {
			t.Fatalf("transition %d differs between builds", i)
		}
	}
}
