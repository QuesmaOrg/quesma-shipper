package packs

import "fmt"

// One classification walk for the three keywordless PII rules (card-pan, iban, pesel). Each byte
// walk returns exactly the spans its regex returns, in the same order; the regex stays compiled as
// the reference and pii_test.go replays it. Go's \b is ASCII-only, so a byte test decides it.

// The byte classes the three shapes are stated in, one bit each so a run's classes
// accumulate with an AND: a bit survives exactly when every byte in the run carried it.
const (
	piiWord  = 1 << iota // [0-9A-Za-z_], Go's \w and so the whole of what \b looks at
	piiDigit             // [0-9]
	piiUpper             // [0-9A-Z], the IBAN body alphabet
	piiAlpha             // [A-Z], the IBAN country prefix
)

// piiClass packs those classes one byte per input byte, so a walk does one table load per
// position. A byte >= 0x80 gets class 0: non-word however it decodes, and unconsumable.
var piiClass = func() (t [256]uint8) {
	for c := 0; c < 256; c++ {
		switch {
		case c >= '0' && c <= '9':
			t[c] = piiWord | piiDigit | piiUpper
		case c >= 'A' && c <= 'Z':
			t[c] = piiWord | piiUpper | piiAlpha
		case c >= 'a' && c <= 'z', c == '_':
			t[c] = piiWord
		}
	}
	return t
}()

// isWordByte reports Go's ASCII \w, which is the whole of what \b looks at.
func isWordByte(c byte) bool { return piiClass[c]&piiWord != 0 }

// panMaxDigits is one more than the pattern's 18 repetitions: a span is at most 19 digits.
const panMaxDigits = 19

// panMinReps is the pattern's lower repetition bound; a span holds at least 13 digits.
const panMinReps = 12

// fusedKind names the rule a fused candidate list belongs to; fusedNone matches on its own.
type fusedKind uint8

const (
	fusedNone fusedKind = iota
	fusedPESEL
	fusedIBAN
	fusedCardPAN
)

// ValueScan is the per-value scratch the fused walk fills. Scratch, not state: the caller
// owns one per Scrub call and never shares it, which keeps a compiled Scrubber concurrent.
type ValueScan struct {
	value string
	done  bool

	pesel []Span
	iban  []Span
	pan   []Span
}

// Reset points the scan at a new value; the walk is deferred until a rule asks.
func (c *ValueScan) Reset(value string) {
	c.value = value
	c.done = false
}

// candidates returns one rule's candidate spans, walking the value on the first ask. The
// slice aliases the scan's storage, which the engine must copy out before the next Reset.
func (c *ValueScan) candidates(kind fusedKind) []Span {
	if !c.done {
		c.walk()
		c.done = true
	}
	// An unnamed kind panics rather than falling through to a neighbour's list, whose spans
	// would be stamped with the asking rule's id: wrong spans, wrong attribution, no signal.
	switch kind {
	case fusedPESEL:
		return c.pesel
	case fusedIBAN:
		return c.iban
	case fusedCardPAN:
		return c.pan
	default:
		panic(fmt.Sprintf("packs: fused kind %d has no candidate list", kind))
	}
}

// walk is the fused pass: one traversal producing all three candidate lists, each in its own
// scanner's order. The unit is a maximal \w run because each rule is stated in runs: PESEL an
// all-digit run of eleven bytes, IBAN a whole upper-alnum run of 15-34 shaped AANN, card-pan a run
// starting on a digit but spanning separators (see scanCardPANFrom).
func (c *ValueScan) walk() {
	c.pesel = c.pesel[:0]
	c.iban = c.iban[:0]
	c.pan = c.pan[:0]

	value := c.value
	// panCursor reproduces FindAll's non-overlap for card-pan: a match consumes its bytes.
	panCursor := 0

	for i := 0; i < len(value); {
		first := piiClass[value[i]]
		if first&piiWord == 0 {
			i++
			continue
		}
		start := i
		acc := first
		for i++; i < len(value); i++ {
			cl := piiClass[value[i]]
			if cl&piiWord == 0 {
				break
			}
			acc &= cl
		}
		n := i - start

		if acc&piiDigit != 0 && n == 11 {
			c.pesel = append(c.pesel, Span{Start: start, End: i})
		}
		if acc&piiUpper != 0 && n >= 15 && n <= 34 &&
			first&piiAlpha != 0 &&
			piiClass[value[start+1]]&piiAlpha != 0 &&
			piiClass[value[start+2]]&piiDigit != 0 &&
			piiClass[value[start+3]]&piiDigit != 0 {
			c.iban = append(c.iban, Span{Start: start, End: i})
		}
		if first&piiDigit != 0 && start >= panCursor {
			if end, ok := scanCardPANFrom(value, start); ok {
				c.pan = append(c.pan, Span{Start: start, End: end})
				panCursor = end
			}
		}
	}
}

// scanCardPANFrom returns the end of the match `\b(?:[0-9][ -]?){12,18}[0-9]\b` makes starting at
// a digit with \b in front, and whether there is one. The digit chain is determined; the only
// freedom is the repetition count, greedy from 18 down to 12, and the first count whose trailing
// digit sits on a \b wins, since a separator satisfies \b and a chain can match its own prefix.
func scanCardPANFrom(value string, start int) (int, bool) {
	var chain [panMaxDigits]int
	n := 0
	for p := start; ; {
		chain[n] = p
		n++
		if n == panMaxDigits {
			break
		}
		if p+1 < len(value) && piiClass[value[p+1]]&piiDigit != 0 {
			p++
			continue
		}
		if p+2 < len(value) && (value[p+1] == ' ' || value[p+1] == '-') &&
			piiClass[value[p+2]]&piiDigit != 0 {
			p += 2
			continue
		}
		break
	}
	for reps := n - 1; reps >= panMinReps; reps-- {
		end := chain[reps] + 1
		if end < len(value) && piiClass[value[end]]&piiWord != 0 {
			continue
		}
		return end, true
	}
	return 0, false
}

// The standalone scanners are the fused walk with two of its three answers thrown away: one
// implementation, so the regex equivalence the tests prove is the code the engine runs.

func scanPESEL(value string) []Span { return fusedCandidates(fusedPESEL, value) }

func scanIBAN(value string) []Span { return fusedCandidates(fusedIBAN, value) }

func scanCardPAN(value string) []Span { return fusedCandidates(fusedCardPAN, value) }

func fusedCandidates(kind fusedKind, value string) []Span {
	var c ValueScan
	c.Reset(value)
	return c.candidates(kind)
}
