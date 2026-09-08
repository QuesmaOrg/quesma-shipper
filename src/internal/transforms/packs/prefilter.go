package packs

import (
	"fmt"
	"unicode/utf8"
)

// Prefilter matches every rule's keywords over a value in one ASCII-case-insensitive Aho-Corasick
// pass: a gate fires exactly when a keyword occurs under an ASCII letter-byte fold, nothing else.
type Prefilter struct {
	// classes maps a byte to its alphabet column, folded to lower case, column 0 standing
	// for every byte no keyword contains. The reduced alphabet keeps the table in cache.
	classes [256]uint8

	// next is the flattened transition table, state*width+class; folding hasOutput into the
	// transition saves a second load per input byte.
	next  []uint32
	width int

	// outputs is indexed by state, failure links unioned in; read only when hasOutput fired.
	outputs []Seen
}

// hasOutput marks a transition whose target completes a keyword. State ids stay below 2^31:
// the corpora are vendored data of a few hundred keyword bytes.
const hasOutput = uint32(1) << 31

// Gate identifies one registered keyword set, asked of Seen before its matcher runs.
type Gate int32

// AlwaysGate belongs to a matcher with nothing to prefilter on: it runs on every value.
const AlwaysGate Gate = -1

const (
	// maxGates bounds Seen to a fixed-size value type, which is what makes a scan
	// allocation-free. Exceeding it is a loud build error, never a wider bitset.
	maxGates  = 256
	seenWords = maxGates / 64
)

// Seen is one scan's answer: the gates whose keywords occur in the scanned value.
type Seen struct {
	bits [seenWords]uint64
}

// Has reports whether the gate's keyword set was present.
func (s *Seen) Has(g Gate) bool {
	if g < 0 {
		return true
	}
	return s.bits[g>>6]&(uint64(1)<<uint(g&63)) != 0
}

func (s *Seen) or(o *Seen) {
	for i := range s.bits {
		s.bits[i] |= o.bits[i]
	}
}

func (s *Seen) set(g Gate) {
	s.bits[g>>6] |= uint64(1) << uint(g&63)
}

// PrefilterBuilder collects keyword sets and compiles them into one Prefilter. Registration
// order fixes gate ids and insertion order fixes the trie, so the automaton is deterministic.
type PrefilterBuilder struct {
	keywords []gatedKeyword
	gates    int
}

type gatedKeyword struct {
	folded string
	gate   Gate
}

// NewPrefilterBuilder starts an empty build.
func NewPrefilterBuilder() *PrefilterBuilder { return &PrefilterBuilder{} }

// AddKeywords registers a rule's keyword set and returns its gate; an empty set means the
// rule declares no keywords and always runs.
func (b *PrefilterBuilder) AddKeywords(keywords []string) (Gate, error) {
	if len(keywords) == 0 {
		return AlwaysGate, nil
	}
	if b.gates >= maxGates {
		return 0, fmt.Errorf("packs: prefilter holds %d keyword gates, the fixed-size limit; widen Seen", maxGates)
	}
	g := Gate(b.gates)
	for _, k := range keywords {
		if k == "" {
			return 0, fmt.Errorf("packs: prefilter: empty keyword: a marker present in every value is not a prefilter")
		}
		folded, err := foldKeyword(k)
		if err != nil {
			return 0, err
		}
		b.keywords = append(b.keywords, gatedKeyword{folded: folded, gate: g})
	}
	b.gates++
	return g, nil
}

// foldKeyword lower-cases an ASCII keyword and rejects anything else loudly: the automaton
// scans bytes, so a multi-byte rune would leave its rule quietly unprefiltered.
func foldKeyword(k string) (string, error) {
	out := make([]byte, len(k))
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c >= utf8.RuneSelf {
			return "", fmt.Errorf("packs: prefilter: keyword %q is not ASCII: keyword matching folds bytes, not runes", k)
		}
		out[i] = asciiLower(c)
	}
	return string(out), nil
}

func asciiLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// Build compiles the registered keywords into the automaton.
func (b *PrefilterBuilder) Build() *Prefilter {
	p := &Prefilter{}

	// The alphabet is the distinct folded keyword bytes; column 0 returns to the root.
	var used [utf8.RuneSelf]bool
	for _, k := range b.keywords {
		for i := 0; i < len(k.folded); i++ {
			used[k.folded[i]] = true
		}
	}
	p.width = 1
	for c := 0; c < utf8.RuneSelf; c++ {
		if used[c] {
			p.classes[c] = uint8(p.width)
			p.width++
		}
	}
	// The case fold lives in the table, so the scan itself never lower-cases anything.
	for c := byte('a'); c <= 'z'; c++ {
		p.classes[c-('a'-'A')] = p.classes[c]
	}

	type node struct {
		next []int32
		out  Seen
		fail int32
	}
	newNode := func() *node {
		n := &node{next: make([]int32, p.width)}
		for i := range n.next {
			n.next[i] = -1
		}
		return n
	}
	nodes := []*node{newNode()}

	for _, k := range b.keywords {
		cur := int32(0)
		for i := 0; i < len(k.folded); i++ {
			c := p.classes[k.folded[i]]
			if nodes[cur].next[c] < 0 {
				nodes = append(nodes, newNode())
				nodes[cur].next[c] = int32(len(nodes) - 1)
			}
			cur = nodes[cur].next[c]
		}
		nodes[cur].out.set(k.gate)
	}

	// Failure links in breadth-first order, resolving absent transitions into the failure
	// target's: the table ends up a plain DFA, so a scan never chases a failure chain.
	queue := make([]int32, 0, len(nodes))
	for c := 0; c < p.width; c++ {
		child := nodes[0].next[c]
		if child < 0 {
			nodes[0].next[c] = 0
			continue
		}
		nodes[child].fail = 0
		queue = append(queue, child)
	}
	for i := 0; i < len(queue); i++ {
		u := queue[i]
		nodes[u].out.or(&nodes[nodes[u].fail].out)
		for c := 0; c < p.width; c++ {
			child := nodes[u].next[c]
			if child < 0 {
				nodes[u].next[c] = nodes[nodes[u].fail].next[c]
				continue
			}
			nodes[child].fail = nodes[nodes[u].fail].next[c]
			queue = append(queue, child)
		}
	}

	p.next = make([]uint32, len(nodes)*p.width)
	p.outputs = make([]Seen, len(nodes))
	var empty Seen
	for i, n := range nodes {
		p.outputs[i] = n.out
		for c := 0; c < p.width; c++ {
			target := n.next[c]
			entry := uint32(target)
			if nodes[target].out != empty {
				entry |= hasOutput
			}
			p.next[i*p.width+c] = entry
		}
	}
	return p
}

// Scan walks the value once and reports which gates fired. It allocates nothing and the
// tables are read-only, so a Prefilter is safe to share across goroutines.
func (p *Prefilter) Scan(value string) Seen {
	var seen Seen
	state := uint32(0)
	for i := 0; i < len(value); i++ {
		// A non-ASCII byte takes column 0 and leads back to the root: a keyword spelled
		// with a Unicode look-alike deliberately does not fire the gate.
		entry := p.next[int(state)*p.width+int(p.classes[value[i]])]
		state = entry &^ hasOutput
		if entry&hasOutput != 0 {
			seen.or(&p.outputs[state])
		}
	}
	return seen
}
