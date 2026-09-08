package packs

import "strings"

// Hand scanner for the email rule, whose keyword "@" fires on nearly every value. It
// reproduces the pattern's leftmost-first semantics and handscan_test.go replays both, so a
// corpus edit cannot leave the scanner behind.

// emailRegex is the pii-core email pattern, verbatim, for the equivalence test.
const emailRegex = `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`

// Character classes of emailRegex, plus \w for the two \b assertions, packed one byte per input
// byte. ASCII-only: every word rune is one ASCII byte, so the byte test answers \b's question.
const (
	clsLocal  = 1 << 0 // [A-Za-z0-9._%+-], the local part
	clsDomain = 1 << 1 // [A-Za-z0-9.-], the domain
	clsWord   = 1 << 2 // [0-9A-Za-z_], for \b
	clsAlpha  = 1 << 3 // [A-Za-z], the TLD
)

var emailClass = func() (t [256]uint8) {
	for c := 0; c < 256; c++ {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
			t[c] = clsLocal | clsDomain | clsWord | clsAlpha
		case c >= '0' && c <= '9':
			t[c] = clsLocal | clsDomain | clsWord
		case c == '.', c == '-':
			t[c] = clsLocal | clsDomain
		case c == '_':
			t[c] = clsLocal | clsWord
		case c == '%', c == '+':
			t[c] = clsLocal
		}
	}
	return t
}()

// scanEmail finds what emailRegex would find, in one pass anchored on the '@' signs. The local
// run ends exactly at an '@', so leftmost is the first position in it where \b holds; the domain
// split is the last '.' that works, and the TLD can only be the whole letter run. Ids come from
// Rule.checked, so spans leave here without one.
func scanEmail(value string) []Span {
	var out []Span
	// from is where the next match may start, as FindAll resumes at the previous match's
	// end; at is the '@' cursor, which also advances past a failed candidate.
	from, at := 0, 0
	for {
		rel := strings.IndexByte(value[at:], '@')
		if rel < 0 {
			return out
		}
		sign := at + rel
		at = sign + 1

		// Back to the start of the local-part run, never earlier than from.
		p := sign
		for p > from && emailClass[value[p-1]]&clsLocal != 0 {
			p--
		}
		// Leftmost start in [p, sign) at which \b holds. The byte before p is read even
		// when from clipped the walk: Go's \b looks at the real text before it.
		start := -1
		for i := p; i < sign; i++ {
			prevWord := i > 0 && emailClass[value[i-1]]&clsWord != 0
			if prevWord != (emailClass[value[i]]&clsWord != 0) {
				start = i
				break
			}
		}
		if start < 0 {
			continue
		}

		// The domain run, then its last usable '.' walking left: the dot needs a domain
		// byte before it, a two-letter TLD after it, and a non-word byte to close \b.
		lo := sign + 1
		hi := lo
		for hi < len(value) && emailClass[value[hi]]&clsDomain != 0 {
			hi++
		}
		end := -1
		for dot := hi - 1; dot > lo; dot-- {
			if value[dot] != '.' {
				continue
			}
			tld := dot + 1
			for tld < hi && emailClass[value[tld]]&clsAlpha != 0 {
				tld++
			}
			if tld-dot-1 < 2 {
				continue
			}
			if tld < len(value) && emailClass[value[tld]]&clsWord != 0 {
				continue
			}
			end = tld
			break
		}
		if end < 0 {
			continue
		}

		out = append(out, Span{Start: start, End: end})
		from, at = end, end
	}
}
