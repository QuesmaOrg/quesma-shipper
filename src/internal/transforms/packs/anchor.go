package packs

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// anchorScan is a rule's literal entry point: the corpus keywords, one of which every match must
// begin with, plus an anchored copy of the pattern, since Go's regexp only memchr-skips on a plain
// literal prefix. The head property is corpus DATA, not derived from the pattern: a rule whose
// keywords occur mid-match declares "sweep": true, and TestAnchorMatchesSweep turns a keyword edit
// that breaks it into a loud diff rather than a quiet missed redaction.
type anchorScan struct {
	// verify is \A(?:pattern) run against value[p:]: the non-capturing group keeps every
	// group number, and the leading \A makes the engine's anchored fast exit apply.
	verify *regexp.Regexp

	// lits are the entry literals: the rule's corpus keywords.
	lits []string

	// fold means the literals match under Go's (?i), Unicode simple folding, not an ASCII flip.
	fold bool

	// wordEdge means the pattern opened with \b, which verify cannot check: value[p:] starts
	// a text, so the boundary is decided here against the byte before p.
	wordEdge bool
}

const (
	// maxAnchorLits bounds the litCursor's fixed scratch; the corpora carry at most five.
	maxAnchorLits = 8
	// minAnchorLen: a single-byte literal is a memchr that lands on prose constantly, and
	// verifying the candidate then costs more than the NFA step it replaced.
	minAnchorLen = 2
)

// cursor sentinels. Positions are byte offsets, so both are out of band.
const (
	litUnscanned = -2
	litExhausted = -1
)

// newAnchorScan builds a rule's entry points from its corpus keywords, or (nil, nil) when the rule
// cannot be soundly or profitably anchored: refusing costs only speed. Only the checks decidable
// from the pattern's text; a rule that must not anchor for correctness or cost declares "sweep":
// true in the corpus (see TestPEMHeaderFloodStaysLinear).
func newAnchorScan(pattern string, keywords []string, sweep bool) (*anchorScan, error) {
	if sweep || len(keywords) == 0 || len(keywords) > maxAnchorLits {
		return nil, nil
	}
	rest, fold := strings.CutPrefix(pattern, "(?i)")
	if strings.Contains(rest, "(?i") {
		// A (?i) region anywhere but the head means the keywords' spelling is not the matches'.
		return nil, nil
	}
	wordEdge := strings.HasPrefix(rest, `\b`)
	for _, k := range keywords {
		if len(k) < minAnchorLen {
			return nil, nil
		}
		for i := 0; i < len(k); i++ {
			if k[i] >= utf8.RuneSelf {
				return nil, nil
			}
		}
		if wordEdge && !isWordByte(k[0]) {
			// With \b in front, a non-word entry byte needs the byte before it to be a
			// word byte, which the anchored verify cannot see. Not decidable here.
			return nil, nil
		}
		// The candidate search jumps between the two ASCII cases of the first byte, so a
		// first character that also folds onto a non-ASCII rune would be searched short.
		if fold && !foldOrbitASCII(rune(asciiLower(k[0]))) {
			return nil, nil
		}
	}
	verify, err := regexp.Compile(`\A(?:` + pattern + `)`)
	if err != nil {
		// Unreachable: Load compiles the same pattern first. Returned rather than swallowed,
		// since a silently dropped fast path would be invisible except as a slowdown.
		return nil, fmt.Errorf("anchor: anchored form of %q: %w", pattern, err)
	}
	return &anchorScan{verify: verify, lits: keywords, fold: fold, wordEdge: wordEdge}, nil
}

// foldOrbitASCII reports whether every rune Go's (?i) folds r with is ASCII.
func foldOrbitASCII(r rune) bool {
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// The \b test this file needs is isWordByte, in pii.go, with the argument for why it is exact.

// litCursor remembers, per entry literal, the next occurrence at or after the scan position,
// as per-call scratch: nothing about a scan is stored on the Rule, which is shared.
type litCursor struct {
	at [maxAnchorLits]int
}

func (c *litCursor) init() {
	for i := range c.at {
		c.at[i] = litUnscanned
	}
}

// next returns the lowest candidate position at or after pos, or -1 when none remains.
func (c *litCursor) next(s *anchorScan, value string, pos int) int {
	best := -1
	for i, lit := range s.lits {
		if c.at[i] == litExhausted {
			continue
		}
		if c.at[i] < pos {
			c.at[i] = s.index(value, pos, lit)
		}
		if c.at[i] >= 0 && (best < 0 || c.at[i] < best) {
			best = c.at[i]
		}
	}
	return best
}

// index finds the next occurrence of one entry literal at or after from, which the caller
// keeps within len(value).
func (s *anchorScan) index(value string, from int, lit string) int {
	var at int
	if s.fold {
		at = indexFold(value[from:], lit)
	} else {
		at = strings.Index(value[from:], lit)
	}
	if at < 0 {
		return litExhausted
	}
	return from + at
}

// indexFold is strings.Index under Go's (?i) folding. The skip loop runs on the two ASCII
// cases of the needle's first byte, which keeps it a pair of memchrs over ordinary text.
func indexFold(s, lit string) int {
	lo, hi := foldCases(lit[0])
	for i := 0; i < len(s); {
		j := indexEitherByte(s[i:], lo, hi)
		if j < 0 {
			return -1
		}
		at := i + j
		if hasFoldPrefix(s[at:], lit) {
			return at
		}
		i = at + 1
	}
	return -1
}

// foldCases returns the two ASCII spellings of a byte, equal when it is not a letter.
func foldCases(c byte) (byte, byte) {
	switch {
	case 'a' <= c && c <= 'z':
		return c, c - ('a' - 'A')
	case 'A' <= c && c <= 'Z':
		return c, c + ('a' - 'A')
	}
	return c, c
}

func indexEitherByte(s string, a, b byte) int {
	ia := strings.IndexByte(s, a)
	if a == b || ia == 0 {
		return ia
	}
	if ia < 0 {
		return strings.IndexByte(s, b)
	}
	// Bounded by the first hit: the answer is the earlier of the two.
	if ib := strings.IndexByte(s[:ia], b); ib >= 0 {
		return ib
	}
	return ia
}

// hasFoldPrefix reports whether s starts with lit under Go's (?i) folding. lit is ASCII,
// checked at build time; s is arbitrary, so a wide rune is compared through its fold orbit.
func hasFoldPrefix(s, lit string) bool {
	i := 0
	for k := 0; k < len(lit); k++ {
		if i >= len(s) {
			return false
		}
		want := lit[k]
		c := s[i]
		if c < utf8.RuneSelf {
			lo, hi := foldCases(want)
			if c != lo && c != hi {
				return false
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if !foldEq(r, rune(want)) {
			return false
		}
		i += size
	}
	return true
}

// foldEq reports whether two runes are the same under Go's (?i) simple folding.
func foldEq(a, b rune) bool {
	if a == b {
		return true
	}
	for f := unicode.SimpleFold(a); f != a; f = unicode.SimpleFold(f) {
		if f == b {
			return true
		}
	}
	return false
}
