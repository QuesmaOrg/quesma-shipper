// Package packs compiles the vendored rule corpora into matchers. Gitleaks is one of the four
// packs rather than the engine: trajectories carry tool output at higher secret density than source
// code, with no git history to bound the search and secrets JSON-escaped inside JSON strings. The
// corpora are vendored as DATA, never imported, so an auditable collector inherits no dependencies.
package packs

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

//go:embed data/*.json
var corpora embed.FS

// Names of the compiled packs.
const (
	GitleaksCore   = "gitleaks-core"
	QuesmaExtra    = "quesma-extra"
	CloudKeys      = "cloud-keys"
	PIICore        = "pii-core"
	GenericEntropy = "generic-entropy"
)

// PatternPacks are the high-confidence packs. They scan every field, exempt or not.
var PatternPacks = []string{GitleaksCore, QuesmaExtra, CloudKeys, PIICore}

// HeuristicPacks are the low-confidence packs; the engine checks exemptions first.
var HeuristicPacks = []string{GenericEntropy}

// Span mirrors scrub.Span, duplicated rather than imported to keep this package a leaf.
type Span struct {
	Start  int
	End    int
	RuleID string
}

type ruleSpec struct {
	ID       string   `json:"id"`
	Regex    string   `json:"regex"`
	Keywords []string `json:"keywords"`
	Capture  int      `json:"capture"`
	Checksum string   `json:"checksum"`
	Guard    string   `json:"guard"`
	Scanner  string   `json:"scanner"`

	// Sweep pins the rule to the plain FindAll path: for keywords that occur mid-match
	// rather than at every match's start, or where anchoring is a proven cost cliff.
	Sweep bool `json:"sweep"`
}

// corpus omits the data files' description and todo keys, which the compiler never reads.
type corpus struct {
	Pack  string     `json:"pack"`
	Rules []ruleSpec `json:"rules"`
}

// Rule is one compiled pattern rule.
type Rule struct {
	id       string
	re       *regexp.Regexp
	keywords []string
	capture  int
	checksum func(string) bool
	guard    func(value string, start, end int) bool

	// hand is a byte walk that replaces the regex outright, declared by the corpus entry.
	hand candidateScanner

	// fused names this rule's slot in the shared PII walk: the keywordless shapes read
	// candidates out of a ValueScan the caller filled once for the whole ladder.
	fused fusedKind

	// anchor is the literal entry point built from the corpus keywords, nil when the rule
	// cannot anchor. It decides where the regex is asked, never what the answer is.
	anchor *anchorScan
}

// candidateScanner finds by byte walk the spans the rule's regex would have found, before
// the guard and checksum. See pii.go and handscan.go for each equivalence argument.
type candidateScanner func(value string) []Span

// The one registry of hand scanners, one entry per name, so a scanner cannot be wired into
// the standalone path and silently dropped from the fused one. fusedNone runs on its own.
var scanners = map[string]struct {
	fn    candidateScanner
	fused fusedKind
}{
	"card-pan": {scanCardPAN, fusedCardPAN},
	"iban":     {scanIBAN, fusedIBAN},
	"pesel":    {scanPESEL, fusedPESEL},
	"email":    {scanEmail, fusedNone},
}

// RuleID names the rule, for engine wiring and error messages.
func (r *Rule) RuleID() string { return r.id }

// Keywords are the rule's literal markers, for registering with a shared Prefilter.
func (r *Rule) Keywords() []string { return r.keywords }

// MatchScanned finds every occurrence in a value the caller has already scanned for keywords.
// Hand scanner, anchor or sweep: all three answer identically, which the tests beside each pin.
func (r *Rule) MatchScanned(value string) []Span {
	if r.hand != nil {
		return r.checked(r.hand(value), value)
	}
	if r.anchor != nil {
		return r.matchAnchored(value)
	}
	return r.matchSweep(value)
}

// MatchScannedIn is MatchScanned for an engine holding a ValueScan Reset to this exact
// value; every other rule takes MatchScanned's paths unaffected.
func (r *Rule) MatchScannedIn(value string, scan *ValueScan) []Span {
	if r.fused != fusedNone {
		// Guard and checksum must read the scan's own bytes, not the caller's argument: a
		// scan left pointing at an earlier value would checksum the wrong string silently.
		return r.checked(scan.candidates(r.fused), scan.value)
	}
	return r.MatchScanned(value)
}

// checked drops the candidates their guard or checksum rejects, in place, and stamps the
// survivors. A rejected candidate still consumed its bytes, as FindAll commits them.
func (r *Rule) checked(found []Span, value string) []Span {
	out := found[:0]
	for _, s := range found {
		if !r.accepts(value, s.Start, s.End) {
			continue
		}
		s.RuleID = r.id
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// The one statement of the guard-then-checksum policy: both filtered paths decide through
// it, so a change to either predicate cannot reach one path and miss the other.
func (r *Rule) accepts(value string, start, end int) bool {
	if r.guard != nil && !r.guard(value, start, end) {
		return false
	}
	if r.checksum != nil && !r.checksum(value[start:end]) {
		return false
	}
	return true
}

// matchSweep hands the whole value to the regex engine, and is the reference the two fast
// paths are held to: the tests replay each against it over adversarial input.
func (r *Rule) matchSweep(value string) []Span {
	var out []Span
	for _, loc := range r.re.FindAllStringSubmatchIndex(value, -1) {
		if s, ok := r.span(value, loc); ok {
			out = append(out, s)
		}
	}
	return out
}

// matchAnchored is matchSweep's answer reached by literal search: it reproduces FindAll's loop,
// where the leftmost candidate that verifies IS the leftmost match, since a position with no entry
// literal cannot start one. A rejected candidate moves one byte, so a nested match stays findable.
func (r *Rule) matchAnchored(value string) []Span {
	s := r.anchor
	var cursor litCursor
	cursor.init()

	var out []Span
	for pos := 0; pos <= len(value); {
		at := cursor.next(s, value, pos)
		if at < 0 {
			break
		}
		if s.wordEdge && at > 0 && isWordByte(value[at-1]) {
			// \b failed, which the anchored pattern cannot see: value[at:] begins a text.
			// Every entry literal starts on a word byte, so the byte before is the test.
			pos = at + 1
			continue
		}
		loc := s.verify.FindStringSubmatchIndex(value[at:])
		if loc == nil {
			pos = at + 1
			continue
		}
		for i := range loc {
			if loc[i] >= 0 {
				loc[i] += at
			}
		}
		if sp, ok := r.span(value, loc); ok {
			out = append(out, sp)
		}
		// A match carries an entry literal, so it is never empty.
		pos = loc[1]
		if pos <= at {
			pos = at + 1
		}
	}
	return out
}

// span turns one match's index list into the span to redact, guard and checksum applied.
func (r *Rule) span(value string, loc []int) (Span, bool) {
	start, end := loc[0], loc[1]
	// A capture group redacts the value and leaves the context: for assignments and
	// headers the key name is useful signal, and the name is not the secret.
	if r.capture > 0 && len(loc) > 2*r.capture+1 && loc[2*r.capture] >= 0 {
		start, end = loc[2*r.capture], loc[2*r.capture+1]
	}
	if !r.accepts(value, start, end) {
		return Span{}, false
	}
	return Span{Start: start, End: end, RuleID: r.id}, true
}

// What the corpora name; an unknown name is a loud compile error.
var checksums = map[string]func(string) bool{
	"iban":  ibanValid,
	"pesel": peselValid,
	"pan":   panValid,
}

// Context checks a rule can declare: a guard sees the characters around the match, which
// is how "not part of a decimal number" is expressed where \b cannot say it.
var guards = map[string]func(value string, start, end int) bool{
	// Rejects a match that is a piece of a larger decimal number: preceded by a dot, or
	// followed by a dot that continues into digits. Any other dot is prose punctuation.
	"no-decimal-neighbor": func(value string, start, end int) bool {
		if start > 0 && value[start-1] == '.' {
			return false
		}
		if end < len(value) && value[end] == '.' &&
			end+1 < len(value) && value[end+1] >= '0' && value[end+1] <= '9' {
			return false
		}
		return true
	},
}

// Load compiles one pack's corpus.
func Load(pack string) ([]*Rule, error) {
	raw, err := corpora.ReadFile("data/" + pack + ".json")
	if err != nil {
		return nil, fmt.Errorf("packs: pack %q has no corpus: %w", pack, err)
	}
	var c corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("packs: %s: %w", pack, err)
	}
	if c.Pack != pack {
		return nil, fmt.Errorf("packs: %s.json declares pack %q", pack, c.Pack)
	}

	out := make([]*Rule, 0, len(c.Rules))
	for _, spec := range c.Rules {
		r, err := compileSpec(pack, spec)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Split out of Load so the tests can compile a spec the corpora deliberately do not have.
func compileSpec(pack string, spec ruleSpec) (*Rule, error) {
	re, err := regexp.Compile(spec.Regex)
	if err != nil {
		return nil, fmt.Errorf("packs: %s rule %s: %w", pack, spec.ID, err)
	}
	r := &Rule{id: spec.ID, re: re, keywords: spec.Keywords, capture: spec.Capture}
	if spec.Checksum != "" {
		fn, ok := checksums[spec.Checksum]
		if !ok {
			return nil, fmt.Errorf("packs: %s rule %s: unknown checksum %q",
				pack, spec.ID, spec.Checksum)
		}
		r.checksum = fn
	}
	if spec.Guard != "" {
		fn, ok := guards[spec.Guard]
		if !ok {
			return nil, fmt.Errorf("packs: %s rule %s: unknown guard %q",
				pack, spec.ID, spec.Guard)
		}
		r.guard = fn
	}
	if spec.Scanner != "" {
		sc, ok := scanners[spec.Scanner]
		if !ok {
			return nil, fmt.Errorf("packs: %s rule %s: unknown scanner %q",
				pack, spec.ID, spec.Scanner)
		}
		// A scanner returns whole spans, so pairing one with a capture group would redact
		// more than the regex it reproduces: the combination does not compile.
		if spec.Capture > 0 {
			return nil, fmt.Errorf("packs: %s rule %s: scanner %q with capture group %d",
				pack, spec.ID, spec.Scanner, spec.Capture)
		}
		r.hand = sc.fn
		r.fused = sc.fused
		// No anchor for a rule that never runs its regex: it would be dead state.
		return r, nil
	}
	anchor, err := newAnchorScan(spec.Regex, spec.Keywords, spec.Sweep)
	if err != nil {
		return nil, fmt.Errorf("packs: %s rule %s: %w", pack, spec.ID, err)
	}
	r.anchor = anchor
	return r, nil
}

// Available lists the packs with an embedded corpus, plus the code-only heuristic packs.
func Available() []string {
	entries, err := corpora.ReadDir("data")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	out = append(out, HeuristicPacks...)
	slices.Sort(out)
	return out
}
