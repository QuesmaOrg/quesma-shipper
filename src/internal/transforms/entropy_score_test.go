package transforms

import (
	"math"
	"math/rand"
	"testing"
)

// The narrow histogram and the fused hex test are only allowed to be faster, never different.
// The wide-histogram original is copied here rather than referenced, to compare
// against the code as it was.

func refIsHexRun(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return len(s) > 0
}

func refShannonBits(s string) float64 {
	if s == "" {
		return 0
	}
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

func refClears(cfg EntropyConfig, candidate string) bool {
	threshold := cfg.MinBitsPerChar
	if refIsHexRun(candidate) {
		threshold = cfg.MinBitsPerCharHex
	}
	if threshold <= 0 {
		return false
	}
	return refShannonBits(candidate) >= threshold
}

func TestEntropyClassTableMatchesTheByteTests(t *testing.T) {
	rank := 0
	for c := 0; c < 256; c++ {
		b := byte(c)
		got := entropyClass[c]
		if !isCandidateByte(b) {
			if got != 0 {
				t.Fatalf("byte %d out of class but tabulated as %d", c, got)
			}
			continue
		}
		rank++
		if int(got&^entropyHexBit) != rank {
			t.Fatalf("byte %d: rank %d, want %d", c, got&^entropyHexBit, rank)
		}
		if (got&entropyHexBit != 0) != isHexByte(b) {
			t.Fatalf("byte %d: hex bit %v, want %v", c, got&entropyHexBit != 0, isHexByte(b))
		}
	}
	if rank != entropySymbols {
		t.Fatalf("alphabet has %d symbols, entropySymbols is %d", rank, entropySymbols)
	}
}

// The score must be bit-identical, not merely close: a last-ulp difference at the threshold is
// a redaction that appears or disappears.
func TestEntropyScoreIsBitIdenticalToTheWideHistogram(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	alphabet := make([]byte, 0, entropySymbols)
	for c := 0; c < 256; c++ {
		if isCandidateByte(byte(c)) {
			alphabet = append(alphabet, byte(c))
		}
	}
	m := newEntropyMatcher(DefaultEntropyConfig(), "")
	for i := 0; i < 200000; i++ {
		n := 1 + rng.Intn(200)
		width := 1 + rng.Intn(len(alphabet))
		buf := make([]byte, n)
		for j := range buf {
			buf[j] = alphabet[rng.Intn(width)]
		}
		s := string(buf)
		if got, want := m.clears(s), refClears(m.cfg, s); got != want {
			t.Fatalf("clears(%q) = %v, want %v (bits %v)", s, got, want, refShannonBits(s))
		}
	}
}

// Thresholds are configuration, so the distinct-symbol floor must hold at values sitting exactly
// on a log2(d) boundary, where the shortcut has least room.
func TestEntropyFloorHoldsAtBoundaryThresholds(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	alphabet := make([]byte, 0, entropySymbols)
	hex := []byte("0123456789abcdefABCDEF")
	for c := 0; c < 256; c++ {
		if isCandidateByte(byte(c)) {
			alphabet = append(alphabet, byte(c))
		}
	}
	var thresholds []float64
	for d := 1; d <= entropySymbols; d++ {
		v := math.Log2(float64(d))
		thresholds = append(thresholds, v, math.Nextafter(v, 0), math.Nextafter(v, 10))
	}
	thresholds = append(thresholds, 0, -1, 3.8, 4.2, 6)
	for _, th := range thresholds {
		cfg := EntropyConfig{MinLength: 1, MinBitsPerChar: th, MinBitsPerCharHex: th}
		m := newEntropyMatcher(cfg, "")
		for i := 0; i < 4000; i++ {
			pool := alphabet
			if i%2 == 0 {
				pool = hex
			}
			// Uniform draws over exactly d symbols: closest to the log2(d) ceiling the floor comes from.
			d := 1 + rng.Intn(len(pool))
			n := d * (1 + rng.Intn(4))
			buf := make([]byte, 0, n)
			for j := 0; j < n; j++ {
				buf = append(buf, pool[j%d])
			}
			s := string(buf)
			if got, want := m.clears(s), refClears(cfg, s); got != want {
				t.Fatalf("threshold %v: clears(%q) = %v, want %v", th, s, got, want)
			}
		}
	}
}

func TestContainsDelimitedMatchesTheNaiveScan(t *testing.T) {
	naive := func(s, sub string) bool {
		if sub == "" {
			return false
		}
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] != sub {
				continue
			}
			leftOK := i == 0 || !isAlnumByte(s[i-1])
			rightOK := i+len(sub) == len(s) || !isAlnumByte(s[i+len(sub)])
			if leftOK && rightOK {
				return true
			}
		}
		return false
	}
	rng := rand.New(rand.NewSource(13))
	subs := []string{"", "a", "aa", "__USER__", "__REDACTED", "quinn", "-", "aba"}
	bits := []string{"a", "b", "-", "_", "__USER__", "__REDACTED", "quinn", "aba", ":", "9"}
	for i := 0; i < 200000; i++ {
		n := rng.Intn(6)
		s := ""
		for j := 0; j < n; j++ {
			s += bits[rng.Intn(len(bits))]
		}
		sub := subs[rng.Intn(len(subs))]
		if got, want := containsDelimited(s, sub), naive(s, sub); got != want {
			t.Fatalf("containsDelimited(%q, %q) = %v, want %v", s, sub, got, want)
		}
	}
}
