package transforms

import (
	"math"
	"math/rand"
	"strings"
	"testing"
)

// The grid scan and the tabulated score are only allowed to be faster, never different; each is
// held to the shape it replaced.

// refMatch is the byte-at-a-time run walk the grid scan replaced: every byte inspected, every
// maximal in-class run of at least minRun emitted.
func refMatch(m *entropyMatcher, value string) []Span {
	if len(value) < m.cfg.MinLength {
		return nil
	}
	if m.cfg.MinBitsPerChar <= 0 && m.cfg.MinBitsPerCharHex <= 0 {
		return nil
	}
	var out []Span
	for i := 0; i < len(value); {
		if entropyClass[value[i]] == 0 {
			i++
			continue
		}
		start := i
		for i < len(value) && entropyClass[value[i]] != 0 {
			i++
		}
		if i-start < m.minRun {
			continue
		}
		candidate := value[start:i]
		if m.skipsCandidate(candidate) {
			continue
		}
		if !m.clears(candidate) {
			continue
		}
		out = append(out, Span{Start: start, End: i, RuleID: m.RuleID()})
	}
	return out
}

func TestEntropyGridScanFindsTheSameRunsAsTheByteWalk(t *testing.T) {
	rng := rand.New(rand.NewSource(31))
	// Fragments chosen so runs land on and across the grid: separators of every length, runs just
	// under and just over the floor, the scrubber's own output.
	frag := []string{
		"a", "-", "/", ".", " ", ":", "_", "==",
		"0123456789abcdef", "AKIA1234567890ABCDEF", "__USER__", "__REDACTED:card-pan__",
		"devuser", "/Users/devuser/git/proj", "sk-ant-api03-QmFzZTY0U2VjcmV0",
		"7f3c9a1b2e5d8046", "the quick brown fox", "aGVsbG8gd29ybGQgdGhpcyBpcyBiYXNlNjQ=",
		strings.Repeat("A", 40), strings.Repeat("ab", 20),
	}
	cfgs := []EntropyConfig{
		DefaultEntropyConfig(),
		{MinLength: 1, MinBitsPerChar: 4.2, MinBitsPerCharHex: 3.8},
		{MinLength: 2, MinBitsPerChar: 3.0, MinBitsPerCharHex: 3.0},
		{MinLength: 7, MinBitsPerChar: 4.2, MinBitsPerCharHex: 3.8},
		{MinLength: 64, MinBitsPerChar: 4.2, MinBitsPerCharHex: 3.8},
		{MinLength: 0, MinBitsPerChar: 4.2, MinBitsPerCharHex: 3.8},
	}
	for _, cfg := range cfgs {
		m := newEntropyMatcher(cfg, "devuser")
		for i := 0; i < 40000; i++ {
			var b strings.Builder
			for j := rng.Intn(8); j >= 0; j-- {
				b.WriteString(frag[rng.Intn(len(frag))])
			}
			v := b.String()
			got, want := m.Match(v, FieldPath("")), refMatch(m, v)
			if len(got) != len(want) {
				t.Fatalf("MinLength %d, %q: got %v spans, want %v", cfg.MinLength, v, got, want)
			}
			for k := range got {
				if got[k] != want[k] {
					t.Fatalf("MinLength %d, %q: span %d = %v, want %v", cfg.MinLength, v, k, got[k], want[k])
				}
			}
		}
	}
}

// entropyEstSlack is only sound if the rearranged score tracks the exact one far more closely
// than the slack, so measure the gap rather than assume it. It also proves the band is not
// swallowing the corpus, since a candidate inside it is paid for twice.
func TestEntropyEstimateTracksTheExactScore(t *testing.T) {
	rng := rand.New(rand.NewSource(37))
	alphabet := make([]byte, 0, entropySymbols)
	for c := 0; c < 256; c++ {
		if isCandidateByte(byte(c)) {
			alphabet = append(alphabet, byte(c))
		}
	}
	worst := 0.0
	for i := 0; i < 200000; i++ {
		n := 1 + rng.Intn(400)
		width := 1 + rng.Intn(len(alphabet))
		buf := make([]byte, n)
		for j := range buf {
			buf[j] = alphabet[rng.Intn(width)]
		}
		s := string(buf)

		var counts [entropyHistSlots]int32
		for j := 0; j < len(s); j++ {
			counts[uint(entropyClass[s[j]]&^entropyHexBit)]++
		}
		total := float64(len(s))
		gap := math.Abs(estimateEntropyBits(&counts, total) - exactEntropyBits(&counts, total))
		if gap > worst {
			worst = gap
		}
	}
	// Three orders below the slack, on the widest candidates the matcher scores.
	if worst >= entropyEstSlack/1000 {
		t.Fatalf("estimate drifts from the exact score by %g, slack is %g", worst, entropyEstSlack)
	}
}
