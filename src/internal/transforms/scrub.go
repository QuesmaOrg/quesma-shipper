// Package scrub removes secrets and PII before anything is packaged or encrypted. It is
// the one pipeline step that fails CLOSED: a scrub-engine error means the file does not
// upload. Detectors are tuned for recall, a false negative being the expensive error. The
// contract is to match the DECODED value and replace exactly the matching source span.
package transforms

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/packs"
)

// Hint is advisory: a payload that does not parse falls back to raw-text scanning
// either way.
type Hint struct {
	// Family is the source family, used to select structural exemptions.
	Family string

	// JSONL means one JSON value per line; anything else is scanned as raw text.
	JSONL bool
}

// ScanMode records how a payload was actually handled, for the manifest.
const (
	ScanModeDecodedJSON = "decoded_json_values"
	ScanModeRawText     = "raw_text"
	ScanModeMixed       = "mixed"
)

// Result is the scrubbed payload plus the ledger.
type Result struct {
	Out           Scrubbed
	BytesRedacted int
	BytesTotal    int
	RuleHits      map[string]int
	ScanMode      string

	// A torn tail is expected (it ships byte-exact and the next flush supersedes it),
	// so these separate it from an error rather than reporting one.
	LinesParsed     int
	LinesRawScanned int
}

// Density is bytes redacted over bytes total. Recorded per object so downstream can
// drop shredded objects, and so a density jump names the rule that went haywire.
func (r Result) Density() float64 {
	if r.BytesTotal == 0 {
		return 0
	}
	return float64(r.BytesRedacted) / float64(r.BytesTotal)
}

func (r *Result) record(n int, hits map[string]int) {
	r.BytesRedacted += n
	for id, c := range hits {
		r.RuleHits[id] += c
	}
}

// Config builds a Scrubber.
type Config struct {
	RulePacks      []string
	Exemptions     map[string][]string
	Username       string
	SecretKeyNames []string
	Entropy        EntropyConfig
}

// DefaultConfig is the compiled baseline.
func DefaultConfig() Config {
	return Config{
		RulePacks:      []string{packs.GitleaksCore, packs.QuesmaExtra, packs.CloudKeys, packs.GenericEntropy, packs.PIICore},
		Exemptions:     CompiledExemptions(),
		Username:       "",
		SecretKeyNames: DefaultSecretKeyNames(),
		Entropy:        DefaultEntropyConfig(),
	}
}

// base64MinLength is the floor below which a speculative decode is not worth it. A
// constant, not a setting: a setting that reads as tunable invites tuning it below the floor.
const base64MinLength = 32

// Scrubber is a compiled, reusable redaction engine.
type Scrubber struct {
	patterns []gatedPattern

	// The one heuristic detector, nil when generic-entropy is not configured. It
	// mistakes structure for secrets, so the engine consults exemptions BEFORE it.
	entropy *entropyMatcher

	// The username the path-user rewriter replaces, empty when none is configured. Not a
	// detector, so it runs everywhere including on exempt fields.
	pathUser string

	keyNames *keyNameMatcher
	exempt   *ExemptionSet

	// Answers every pattern matcher's keyword question in one pass; read-only once
	// built, so a Scrubber stays safe to share.
	prefilter *packs.Prefilter

	// The keyValueRules configured, behind their own small prefilter.
	keyValue          []gatedPattern
	keyValuePrefilter *packs.Prefilter

	// The slowest single Scrub this Scrubber has served, and the payload that caused it. Atomic
	// because one Scrubber serves every worker. A pathological input can make a pattern backtrack
	// for seconds without erroring, which looks like a stalled install and nothing else reports it.
	slowestNanos atomic.Int64
	slowestBytes atomic.Int64
}

// Slowest reports the worst Scrub seen so far and the payload size behind it. Duration alone says
// little -- a large file is legitimately slow -- so the size travels with it.
func (s *Scrubber) Slowest() (time.Duration, int64) {
	return time.Duration(s.slowestNanos.Load()), s.slowestBytes.Load()
}

// noteCost keeps the maximum. A lost race costs one sample of a figure that is already only a
// worst-case hint, so the loop does not retry.
func (s *Scrubber) noteCost(d time.Duration, size int) {
	n := int64(d)
	for {
		prev := s.slowestNanos.Load()
		if n <= prev {
			return
		}
		if s.slowestNanos.CompareAndSwap(prev, n) {
			s.slowestBytes.Store(int64(size))
			return
		}
	}
}

// gatedMatcher is a high-confidence pattern detector whose keyword prefilter the engine
// hoists out: MatchScannedIn runs only when the shared automaton fired its gate. Its
// missing field argument is the mechanism, not an oversight: exemptions are
// detector-scoped, and with nowhere to pass a field path an exemption has nothing to hook
// onto, so pattern matchers provably scan exempt fields too.
type gatedMatcher interface {
	MatchScannedIn(value string, scan *packs.ValueScan) []Span
}

// gatedPattern pairs a matcher with its gate in the shared prefilter.
type gatedPattern struct {
	m    gatedMatcher
	gate packs.Gate
}

// keyValueRules match a key and its value read together as text, which a walk of an embedded
// document never shows them; capture groups only, so a hit keeps the document valid JSON.
var keyValueRules = []string{"aws-secret-key", "secret-access-key"}

// New compiles a Scrubber. A pack named in config but absent from the corpus is an
// error: running with fewer rules than configured must not be reachable by omission.
func New(cfg Config) (*Scrubber, error) {
	s := &Scrubber{exempt: NewExemptionSet(cfg.Exemptions)}
	// Rejected rather than clamped: the scanner would read a negative floor as "every
	// run", the opposite of what lowering a threshold means.
	if cfg.Entropy.MinLength < 0 {
		return nil, fmt.Errorf("scrub: entropy min_length %d is negative", cfg.Entropy.MinLength)
	}

	// One automaton over every rule's keywords; only here knows the whole ladder.
	prefilter := packs.NewPrefilterBuilder()
	keyValuePrefilter := packs.NewPrefilterBuilder()

	for _, name := range cfg.RulePacks {
		switch name {
		case packs.GenericEntropy:
			s.entropy = newEntropyMatcher(cfg.Entropy, cfg.Username)
		default:
			rules, err := packs.Load(name)
			if err != nil {
				return nil, err
			}
			for _, r := range rules {
				gate, err := prefilter.AddKeywords(r.Keywords())
				if err != nil {
					return nil, fmt.Errorf("scrub: pack %s rule %s: %w", name, r.RuleID(), err)
				}
				s.patterns = append(s.patterns, gatedPattern{m: r, gate: gate})
				if slices.Contains(keyValueRules, r.RuleID()) {
					kvGate, err := keyValuePrefilter.AddKeywords(r.Keywords())
					if err != nil {
						return nil, fmt.Errorf("scrub: pack %s rule %s: %w", name, r.RuleID(), err)
					}
					s.keyValue = append(s.keyValue, gatedPattern{m: r, gate: kvGate})
				}
			}
		}
	}

	s.keyNames = newKeyNameMatcher(cfg.SecretKeyNames)
	// nil stems yield AlwaysGate, so a non-ASCII configured name is still redacted, just
	// not prefiltered on bytes that cannot represent it.
	keyGate, err := prefilter.AddKeywords(s.keyNames.stems)
	if err != nil {
		return nil, fmt.Errorf("scrub: secret key names: %w", err)
	}
	s.patterns = append(s.patterns, gatedPattern{m: s.keyNames, gate: keyGate})
	s.prefilter = prefilter.Build()
	s.keyValuePrefilter = keyValuePrefilter.Build()

	s.pathUser = cfg.Username
	return s, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// Scrub redacts a payload. Errors returned here are engine errors and fail closed; a
// line that does not parse is NOT an error but raw-text scanned and counted in
// LinesRawScanned, which is what lets a torn tail still ship.
func (s *Scrubber) Scrub(payload []byte, hint Hint) (Result, error) {
	if kind := opaqueKind(payload); kind != "" {
		return Result{}, fmt.Errorf("scrub refused: %s payload", kind)
	}
	started := time.Now()
	defer func() { s.noteCost(time.Since(started), len(payload)) }()
	res := Result{
		RuleHits:   map[string]int{},
		BytesTotal: len(payload),
		ScanMode:   ScanModeDecodedJSON,
	}
	// One Scrubber serves many goroutines, so no per-value state may live on it.
	var scan packs.ValueScan
	if !hint.JSONL {
		res.Out = Scrubbed{b: []byte(s.scrubRawText(string(payload), &res, &scan)), via: viaScrubber}
		res.ScanMode = ScanModeRawText
		res.LinesRawScanned = 1
		return res, nil
	}

	var walker jsonWalker
	// Copy on first change: a file with no secret ships the input slice itself.
	var out []byte

	for rest := payload; len(rest) > 0; {
		lineStart := len(payload) - len(rest)
		ensureOutput := func() {
			if out == nil {
				out = make([]byte, 0, len(payload))
				out = append(out, payload[:lineStart]...)
			}
		}
		var body, ending []byte
		body, ending, rest = nextLine(rest)
		if len(bytes.TrimSpace(body)) == 0 {
			if out != nil {
				out = append(out, body...)
				out = append(out, ending...)
			}
			continue
		}

		walker.reset(s, hint.Family, &scan, body)
		walkErr := walker.walkLine()
		if errors.Is(walkErr, errEmbeddedEdit) {
			return Result{}, fmt.Errorf("scrub engine: %w", walkErr)
		}
		if walkErr == nil {
			res.LinesParsed++
			res.record(walker.redacted, walker.hits)
			if len(walker.edits) > 0 {
				ensureOutput()
				var err error
				out, err = walker.appendTo(out, body)
				if err != nil {
					return Result{}, fmt.Errorf("scrub engine: apply JSON redaction spans: %w", err)
				}
			} else if out != nil {
				out = append(out, body...)
			}
		} else {
			// Syntax errors, torn tails and over-deep records belong to the raw scanner.
			res.LinesRawScanned++
			text := string(body)
			scrubbed := s.scrubRawText(text, &res, &scan)
			if scrubbed != text {
				ensureOutput()
				out = append(out, scrubbed...)
			} else if out != nil {
				out = append(out, body...)
			}
		}
		if out != nil {
			out = append(out, ending...)
		}
	}

	switch {
	case res.LinesRawScanned > 0 && res.LinesParsed > 0:
		res.ScanMode = ScanModeMixed
	case res.LinesRawScanned > 0:
		res.ScanMode = ScanModeRawText
	}
	if out == nil {
		out = payload
	}
	res.Out = Scrubbed{b: out, via: viaScrubber}
	return res, nil
}

const viaScrubber = "scrubber"

// opaqueKind names a payload the scanners cannot see into by its magic header: a compressed or
// database file. Every text pattern passes such bytes untouched, so scrubbing them would report a
// clean ledger over content nothing read.
func opaqueKind(payload []byte) string {
	switch {
	case bytes.HasPrefix(payload, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return "zstd"
	case bytes.HasPrefix(payload, []byte{0x1f, 0x8b}):
		return "gzip"
	case bytes.HasPrefix(payload, []byte("PK\x03\x04")), bytes.HasPrefix(payload, []byte("PK\x05\x06")),
		bytes.HasPrefix(payload, []byte("PK\x07\x08")):
		return "zip"
	case isBzip2(payload):
		return "bzip2"
	case bytes.HasPrefix(payload, []byte("\xfd7zXZ\x00")):
		return "xz"
	case bytes.HasPrefix(payload, []byte("SQLite format 3\x00")):
		return "sqlite"
	}
	return ""
}

// isBzip2 wants the block or end-of-stream magic after "BZh<level>", so text that merely starts
// with "BZh" is not refused.
func isBzip2(p []byte) bool {
	if len(p) < 10 || !bytes.HasPrefix(p, []byte("BZh")) || p[3] < '1' || p[3] > '9' {
		return false
	}
	return bytes.Equal(p[4:10], []byte("\x31\x41\x59\x26\x53\x59")) ||
		bytes.Equal(p[4:10], []byte("\x17\x72\x45\x38\x50\x90"))
}

// scrubRawText scans a payload that is not JSON, or failed to parse, with the pattern
// packs and structural rewriters only. Heuristics are deliberately absent: with no field
// path there is no exemption to consult, and the entropy backstop would shred any hex
// digest or base64 blob in a terminal capture.
func (s *Scrubber) scrubRawText(text string, res *Result, scan *packs.ValueScan) string {
	plan := s.planValueWith(text, nil, false, "", scan)
	res.record(plan.redacted, plan.hits)
	return plan.apply(text)
}

type replacementSpan struct {
	Start, End  int
	Replacement string
}

type valuePlan struct {
	spans    []replacementSpan
	redacted int
	hits     map[string]int
}

func (p valuePlan) apply(value string) string {
	if len(p.spans) == 0 {
		return value
	}
	var b strings.Builder
	b.Grow(len(value) + 64)
	cursor := 0
	for _, span := range p.spans {
		b.WriteString(value[cursor:span.Start])
		b.WriteString(span.Replacement)
		cursor = span.End
	}
	b.WriteString(value[cursor:])
	return b.String()
}

// planValue applies the full ladder to one decoded JSON string without building the
// rewritten value; the walker maps the decoded spans back to the original token.
func (s *Scrubber) planValue(value string, secretKey bool, field FieldPath, family string, scan *packs.ValueScan) valuePlan {
	entropy := s.entropy
	if s.exempt.Exempt(family, field) {
		// Detector-scoped: the field stands down the heuristics and nothing else.
		entropy = nil
	}
	return s.planValueWith(value, entropy, secretKey, field, scan)
}

func (s *Scrubber) planValueWith(
	value string,
	entropy *entropyMatcher,
	secretKey bool,
	field FieldPath,
	scan *packs.ValueScan,
) valuePlan {
	// A key that names a secret takes the whole value, whatever its shape.
	if secretKey {
		return valuePlan{
			spans:    []replacementSpan{{Start: 0, End: len(value), Replacement: Sentinel("key-name")}},
			redacted: len(value),
			hits:     map[string]int{"key-name": 1},
		}
	}

	patternSpans := s.appendPatternSpans(nil, value, scan)
	var heuristicSpans []Span
	if entropy != nil {
		heuristicSpans = entropy.Match(value)
	}
	if strings.IndexByte(value, '\\') >= 0 {
		if shadow, escapes := escapeShadow(value); !escapes.empty() {
			patternSpans = s.unionEscapeShadow(value, string(shadow), escapes, patternSpans, scan)
			// A run can start at the `n` of `\n`; the lone `\` left behind would break encoded JSON.
			escapes.snapAll(heuristicSpans)
		}
	}

	// One level of base64, never recursion: work stays bounded per byte. The whole
	// encoded value goes rather than a patched re-encoding, which would rewrite bytes
	// the shipper is supposed to preserve.
	if len(patternSpans) == 0 && len(heuristicSpans) == 0 && len(value) >= base64MinLength {
		if id, hit := s.base64Hit(value, scan); hit {
			return valuePlan{
				spans:    []replacementSpan{{Start: 0, End: len(value), Replacement: Sentinel(id)}},
				redacted: len(value),
				hits:     map[string]int{id: 1},
			}
		}
	}

	resolved, redacted, hits := resolveSpans(value, patternSpans, heuristicSpans)
	plan := valuePlan{redacted: redacted, hits: hits}
	for _, span := range resolved {
		plan.spans = append(plan.spans, replacementSpan{
			Start: span.Start, End: span.End, Replacement: Sentinel(span.RuleID),
		})
	}

	// Runs last and unconditionally, including on values already redacted.
	if s.pathUser != "" {
		pathSpans, n := pathUserReplacementSpans(value, s.pathUser, plan.spans)
		if n > 0 {
			plan.spans = append(plan.spans, pathSpans...)
			slices.SortFunc(plan.spans, func(a, b replacementSpan) int {
				return cmp.Compare(a.Start, b.Start)
			})
			plan.redacted += n
			if plan.hits == nil {
				plan.hits = map[string]int{}
			}
			plan.hits["path-user"]++
		}
	}
	return plan
}

// appendPatternSpans appends what every pattern rule whose gate fires finds in value.
func (s *Scrubber) appendPatternSpans(dst []Span, value string, scan *packs.ValueScan) []Span {
	// A gate that did not fire means the matcher behind it cannot match.
	seen := s.prefilter.Scan(value)
	// The shared pass for rules with no keyword to gate on; lazy until one asks.
	scan.Reset(value)
	for _, p := range s.patterns {
		if seen.Has(p.gate) {
			dst = append(dst, p.m.MatchScannedIn(value, scan)...)
		}
	}
	return dst
}

// spansKeyAndValue reports whether a keyValueRules rule matches the text of a document.
func (s *Scrubber) spansKeyAndValue(doc string) bool {
	if len(s.keyValue) == 0 {
		return false
	}
	seen := s.keyValuePrefilter.Scan(doc)
	for _, p := range s.keyValue {
		if seen.Has(p.gate) && len(p.m.MatchScannedIn(doc, nil)) > 0 {
			return true
		}
	}
	return false
}

// keyNamesSecret skips a value already redacted, so the rule an earlier whole-value scan attributed stands.
func (s *Scrubber) keyNamesSecret(key, value string) bool {
	return key != "" && s.keyNames.MatchesKeyName(key) && value != "" && !isSentinel(value)
}

// unionEscapeShadow merges the pattern hits on the escape shadow into spans, all snapped to whole escapes.
func (s *Scrubber) unionEscapeShadow(value, shadow string, escapes escapeIndex, spans []Span, scan *packs.ValueScan) []Span {
	// A match of nothing but backslashes split an escape (key-name on `=\"value\"`); the shadow finds the value.
	spans = slices.DeleteFunc(spans, func(sp Span) bool {
		return strings.Trim(value[sp.Start:sp.End], `\`) == ""
	})
	escapes.snapAll(spans)
	found := s.appendPatternSpans(nil, shadow, scan)
	escapes.snapAll(found)
	for _, sp := range found {
		spans = unionSpan(spans, sp)
	}
	return spans
}

// unionSpan adds sp unless a span already covers it; spans it overlaps are folded into one
// span under the earliest rule, since resolveSpans would drop an overlapping tail.
func unionSpan(spans []Span, sp Span) []Span {
	if slices.ContainsFunc(spans, func(o Span) bool { return o.Start <= sp.Start && sp.End <= o.End }) {
		return spans
	}
	for {
		i := slices.IndexFunc(spans, func(o Span) bool { return o.Start < sp.End && sp.Start < o.End })
		if i < 0 {
			return append(spans, sp)
		}
		o := spans[i]
		if o.Start <= sp.Start {
			sp.RuleID = o.RuleID
		}
		sp.Start, sp.End = min(sp.Start, o.Start), max(sp.End, o.End)
		spans = slices.Delete(spans, i, i+1)
	}
}

// escapeIndex has a bit per byte boundary where a cut would split an escape or separate two touching ones.
type escapeIndex struct{ splits []uint64 }

func newBitset(n int) []uint64      { return make([]uint64, n/64+1) }
func setBit(b []uint64, k int)      { b[k/64] |= 1 << (k % 64) }
func hasBit(b []uint64, k int) bool { return k/64 < len(b) && b[k/64]&(1<<(k%64)) != 0 }

func (x escapeIndex) empty() bool { return x.splits == nil }

// snap widens [start, end) so neither edge splits an escape: a redaction that ate the backslash
// of `\"` would turn an escaped quote in raw JSON text into a closing one.
func (x escapeIndex) snap(start, end int) (int, int) {
	for start > 0 && hasBit(x.splits, start) {
		start--
	}
	for hasBit(x.splits, end) {
		end++
	}
	return start, end
}

func (x escapeIndex) snapAll(spans []Span) {
	for i := range spans {
		spans[i].Start, spans[i].End = x.snap(spans[i].Start, spans[i].End)
	}
}

// escapeShadow blanks each JSON escape of a twice-encoded string to its byte, so `\"TOKEN\"=` reads as
// a quoted name; `\\` keeps its backslash for the next pass, one encoding level per pass.
func escapeShadow(value string) ([]byte, escapeIndex) {
	first := strings.IndexByte(value, '\\')
	for first >= 0 {
		if n, _ := escapeAt(value, first); n > 0 {
			break
		}
		next := strings.IndexByte(value[first+1:], '\\')
		if next < 0 {
			return nil, escapeIndex{}
		}
		first += 1 + next
	}
	if first < 0 {
		return nil, escapeIndex{}
	}
	shadow := []byte(value)
	splits, starts, ends := newBitset(len(shadow)), newBitset(len(shadow)), newBitset(len(shadow))
	for again, from := true, first; again; from = 0 {
		again = false
		for i := from; i+1 < len(shadow); i++ {
			n, last := escapeAt(shadow, i)
			if n == 0 {
				continue
			}
			again = again || last == '\\'
			for j := i; j < i+n-1; j++ {
				shadow[j] = ' '
				setBit(splits, j+1)
			}
			shadow[i+n-1] = last
			setBit(starts, i)
			setBit(ends, i+n)
			i += n - 1
		}
	}
	// Touching escapes are one unit: splitting `\\` from the `\"` after it breaks the inner level.
	for w := range splits {
		splits[w] |= starts[w] & ends[w]
	}
	return shadow, escapeIndex{splits: splits}
}

// escapeAt is the length of the escape at s[i] and the byte its shadow keeps; 0 when there is none.
func escapeAt[T string | []byte](s T, i int) (int, byte) {
	if s[i] != '\\' || i+1 >= len(s) {
		return 0, 0
	}
	switch c := s[i+1]; c {
	case 'n':
		return 2, '\n'
	case 't':
		return 2, '\t'
	case 'r':
		return 2, '\r'
	case '"', '/', '\\':
		return 2, c
	case 'u':
		r, ok := hex4(s, i+2)
		if !ok {
			return 0, 0
		}
		// Never a backslash, which would let a chain of escaped U+005C cost a pass per escape.
		if r < utf8.RuneSelf && !isAlnumByte(byte(r)) && r != '_' && r != '\\' && r > ' ' {
			return 6, byte(r)
		}
		return 6, ' '
	}
	return 0, 0
}

func hex4[T string | []byte](s T, at int) (rune, bool) {
	var b [2]byte
	if at+4 > len(s) {
		return 0, false
	}
	if _, err := hex.Decode(b[:], []byte(s[at:at+4])); err != nil {
		return 0, false
	}
	return rune(b[0])<<8 | rune(b[1]), true
}

// pathUserReplacementSpans finds username occurrences in the detector-rewritten value
// without constructing it: a detector touching a neighbour contributes the sentinel's
// boundary byte, and an occurrence a detector swallowed no longer exists.
func pathUserReplacementSpans(value, username string, blocked []replacementSpan) ([]replacementSpan, int) {
	if len(username) < 2 || value == "" || !strings.Contains(value, username) {
		return nil, 0
	}
	var spans []replacementSpan
	redacted := 0
	blockedAt := 0
	for from := 0; from+len(username) <= len(value); {
		rel := strings.Index(value[from:], username)
		if rel < 0 {
			break
		}
		start := from + rel
		end := start + len(username)
		for blockedAt < len(blocked) && blocked[blockedAt].End <= start {
			blockedAt++
		}
		if blockedAt < len(blocked) && blocked[blockedAt].Start < end {
			from = end
			continue
		}

		leftOK := start == 0 || !isAlnumByte(value[start-1])
		if blockedAt > 0 && blocked[blockedAt-1].End == start {
			r := blocked[blockedAt-1].Replacement
			leftOK = !isAlnumByte(r[len(r)-1])
		}
		rightOK := end == len(value) || !isAlnumByte(value[end])
		if blockedAt < len(blocked) && blocked[blockedAt].Start == end {
			rightOK = !isAlnumByte(blocked[blockedAt].Replacement[0])
		}
		if leftOK && rightOK {
			spans = append(spans, replacementSpan{
				Start: start, End: end, Replacement: formats.UserPlaceholder,
			})
			redacted += len(username)
		}
		from = end
	}
	return spans, redacted
}

// base64Hit decodes one level and reports the first pattern rule that fires inside.
func (s *Scrubber) base64Hit(value string, scan *packs.ValueScan) (string, bool) {
	trimmed := strings.TrimSpace(value)
	// A byte outside every base64 alphabet means all four decoders would fail, so this
	// is the same answer without their buffers.
	if !base64Shaped(trimmed) {
		return "", false
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		decoded, err := enc.DecodeString(trimmed)
		if err != nil || len(decoded) == 0 {
			continue
		}
		text := string(decoded)
		seen := s.prefilter.Scan(text)
		// Safe to reuse the caller's scratch: this answer short-circuits the rest of
		// the ladder for the value.
		scan.Reset(text)
		for _, p := range s.patterns {
			if !seen.Has(p.gate) {
				continue
			}
			if found := p.m.MatchScannedIn(text, scan); len(found) > 0 {
				return found[0].RuleID, true
			}
		}
		return "", false
	}
	return "", false
}

// base64Shaped covers the standard and URL alphabets, padding, and the carriage return
// the decoders skip. Deliberately permissive: only the rejection has to be sound, since
// what passes still goes through the real decoder.
func base64Shaped(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isAlnumByte(c):
		case c == '+', c == '/', c == '=', c == '-', c == '_', c == '\r':
		default:
			return false
		}
	}
	return true
}
