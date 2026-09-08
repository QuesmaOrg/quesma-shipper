package transforms

import (
	"math"
	"regexp"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/packs"
)

// EntropyConfig tunes the generic-entropy backstop, the rule most likely to start eating
// content rather than secrets. The thresholds are calibration inputs.
type EntropyConfig struct {
	MinLength int

	// Applies to base64 and base64url candidates, whose 64-symbol alphabet tops out
	// at 6 bits per character.
	MinBitsPerChar float64

	// Separate threshold for pure-hex candidates: 16 symbols cap per-character
	// entropy at 4.0 bits, so a shared 4.2 threshold never fires on hex at all.
	MinBitsPerCharHex float64
}

// DefaultEntropyConfig is the calibrated baseline; calibration_test.go's seeded corpus
// holds these numbers to account.
func DefaultEntropyConfig() EntropyConfig {
	return EntropyConfig{
		MinLength:         24,
		MinBitsPerChar:    4.2,
		MinBitsPerCharHex: 3.8,
	}
}

// entropyMatcher is the backstop for high-entropy strings no pattern rule claimed. The
// engine must check the exemption set before calling it: redacting a uuid or a tool_use_id
// kills the causal DAG and the subagent join.
type entropyMatcher struct {
	cfg EntropyConfig

	// Candidate length floor, at least 1: a zero-length candidate scores no entropy.
	minRun int

	// Distinct-symbol floors from entropyFloor: below them a candidate is rejected
	// without entering the logarithm loop.
	minDistinct, minDistinctHex int

	// Substrings whose delimited presence marks a candidate as the scrubber's own
	// output rather than a secret; see Match.
	skip []string
}

func newEntropyMatcher(cfg EntropyConfig, username string) *entropyMatcher {
	skip := []string{formats.UserPlaceholder, sentinelPrefix}
	if len(username) >= 2 {
		// Same floor as pathUserRewriter: a one-letter name would mark half the
		// alphabet path-shaped.
		skip = append(skip, username)
	}
	minRun := cfg.MinLength
	if minRun < 1 {
		minRun = 1
	}
	return &entropyMatcher{
		cfg:            cfg,
		minRun:         minRun,
		minDistinct:    entropyFloor(cfg.MinBitsPerChar),
		minDistinctHex: entropyFloor(cfg.MinBitsPerCharHex),
		// "/" is deliberately NOT in the candidate class: with it a candidate is a
		// whole path prefix, 55% of all redaction hits on a real archive. The accepted residual, a std-base64 secret whose slashes split it
		// under MinLength, is pinned in TestBareBase64WithSlashIsAKnownEscape.
		skip: skip,
	}
}

// isCandidateByte is the entropy candidate alphabet: base64url and hex, plus the "+"
// and "=" of standard base64. See newEntropyMatcher for why "/" is not in it.
func isCandidateByte(c byte) bool {
	return isAlnumByte(c) || c == '+' || c == '=' || c == '_' || c == '-'
}

// entropySymbols is the candidate alphabet size, and so the ceiling on distinct
// symbols.
const entropySymbols = 66

// entropyHexBit marks a class-table entry whose byte is also a hex digit; the low bits
// carry 1 + the byte's rank in the alphabet, 0 meaning out of class.
const entropyHexBit = 0x80

// entropyHistSlots is the histogram width: 128 rather than the 67 the ranks reach, so the
// compiler can prove a uint8 slot with the hex bit cleared is in range.
const entropyHistSlots = 128

// entropyClass tabulates isCandidateByte, which stays the definition. Ranks are handed
// out in ascending byte order so the histogram's partial sums are bit-for-bit stable;
// float addition does not associate, so that ordering is a correctness property.
var entropyClass = buildEntropyClass()

func buildEntropyClass() [256]uint8 {
	var t [256]uint8
	rank := uint8(0)
	for c := 0; c < 256; c++ {
		b := byte(c)
		if !isCandidateByte(b) {
			continue
		}
		rank++
		v := rank
		if isHexByte(b) {
			v |= entropyHexBit
		}
		t[c] = v
	}
	return t
}

// isHexByte marks the runs that earn MinBitsPerCharHex rather than MinBitsPerChar.
func isHexByte(c byte) bool {
	return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// entropyFloor is the smallest distinct-symbol count that could reach threshold: d
// symbols cannot score above log2(d). The 1e-9 slack sits three orders above the sum's
// rounding, so nothing the exact loop would accept is dropped.
func entropyFloor(threshold float64) int {
	for d := 1; d <= entropySymbols; d++ {
		if math.Log2(float64(d)) >= threshold-1e-9 {
			return d
		}
	}
	return entropySymbols + 1
}

func (m *entropyMatcher) RuleID() string { return "generic-entropy" }

func (m *entropyMatcher) Match(value string, _ FieldPath) []Span {
	if len(value) < m.cfg.MinLength {
		return nil
	}
	var out []Span
	// Probing every minRun'th byte, since a candidate run must cover a grid point. Must find
	// exactly the same runs, in the same left-to-right order, as a byte-at-a-time scan.
	scanned := 0
	for p := m.minRun - 1; p < len(value); p += m.minRun {
		if p < scanned || entropyClass[value[p]] == 0 {
			continue
		}
		start := p
		for start > scanned && entropyClass[value[start-1]] != 0 {
			start--
		}
		i := p + 1
		for i < len(value) && entropyClass[value[i]] != 0 {
			i++
		}
		scanned = i
		if i-start < m.minRun {
			continue
		}
		candidate := value[start:i]
		// Entropy first, skip list second: independent predicates, and one histogram
		// pass rejects nearly everything a substring search per skip string would.
		if !m.clears(candidate) {
			continue
		}
		// Required for idempotency: __USER__ and __REDACTED are built from in-class
		// characters and ADD entropy, so without this skip a second pass eats the first
		// pass's output. The username is that output one pass earlier.
		if m.skipsCandidate(candidate) {
			continue
		}
		out = append(out, Span{Start: start, End: i, RuleID: m.RuleID()})
	}
	return out
}

func (m *entropyMatcher) skipsCandidate(candidate string) bool {
	for _, s := range m.skip {
		if containsDelimited(candidate, s) {
			return true
		}
	}
	return false
}

// containsDelimited requires non-alphanumeric neighbours, the same boundary rule as
// formats.ApplyUserPlaceholder, so a token that merely embeds the username mid-run never
// earns the skip.
func containsDelimited(s, sub string) bool {
	if sub == "" {
		return false
	}
	for i := 0; i+len(sub) <= len(s); {
		at := strings.Index(s[i:], sub)
		if at < 0 {
			return false
		}
		i += at
		leftOK := i == 0 || !isAlnumByte(s[i-1])
		rightOK := i+len(sub) == len(s) || !isAlnumByte(s[i+len(sub)])
		if leftOK && rightOK {
			return true
		}
		// One byte on, so an occurrence starting inside this one is still seen.
		i++
	}
	return false
}

// clears scores a candidate against the threshold its alphabet earns, hex or base64.
// exactEntropyBits, which the thresholds were fitted against, decides inside the slack
// band. The histogram is a local, so a shared *Scrubber stays safe across goroutines.
func (m *entropyMatcher) clears(candidate string) bool {
	var counts [entropyHistSlots]int32
	distinct := 0
	// Accumulating class bytes makes the hex verdict one AND per byte instead of a
	// branch mixed-alphabet candidates mispredict.
	hexAll := uint8(0xff)
	for i := 0; i < len(candidate); i++ {
		class := entropyClass[candidate[i]]
		hexAll &= class
		slot := uint(class &^ entropyHexBit)
		if counts[slot] == 0 {
			distinct++
		}
		counts[slot]++
	}
	threshold, floor := m.cfg.MinBitsPerChar, m.minDistinct
	if hexAll&entropyHexBit != 0 {
		threshold, floor = m.cfg.MinBitsPerCharHex, m.minDistinctHex
	}
	if threshold <= 0 {
		return false
	}
	if distinct < floor {
		return false
	}
	total := float64(len(candidate))
	est := estimateEntropyBits(&counts, total)
	if est >= threshold+entropyEstSlack {
		return true
	}
	if est < threshold-entropyEstSlack {
		return false
	}
	// The estimate cannot name the side, so pay for the arithmetic that defines it.
	return exactEntropyBits(&counts, total) >= threshold
}

// estimateEntropyBits rearranges the sum to H = log2(T) - (1/T)*sum(c*log2(c)), whose
// only logarithms are of small integers a table can answer. Exact in the reals but not
// in float64, so its answer is trusted only away from the threshold (entropyEstSlack).
func estimateEntropyBits(counts *[entropyHistSlots]int32, total float64) float64 {
	weighted := 0.0
	for _, c := range counts[:entropySymbols+1] {
		n := uint(c)
		if n < log2SmallMax {
			weighted += nLog2Table[n]
			continue
		}
		weighted += float64(n) * math.Log2(float64(n))
	}
	return math.Log2(total) - weighted/total
}

// exactEntropyBits is the Shannon sum as the calibrated thresholds were fitted against:
// ascending symbol-rank order, accumulated left to right. Float addition does not
// associate, so that order is a correctness property.
func exactEntropyBits(counts *[entropyHistSlots]int32, total float64) float64 {
	h := 0.0
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / total
		h -= p * math.Log2(p)
	}
	return h
}

// entropyEstSlack is how far from the threshold the estimate may decide alone: the two
// computations differ by ~5e-13 at worst, and the band falls through to exactEntropyBits.
const entropyEstSlack = 1e-9

// log2SmallMax bounds the table; above it math.Log2 runs for that one slot.
const log2SmallMax = 1024

var nLog2Table = buildNLog2Table()

// nLog2Table holds n*log2(n), the rearranged sum's per-slot term, with slot 0 zero.
func buildNLog2Table() (nLog2 [log2SmallMax]float64) {
	for i := 1; i < log2SmallMax; i++ {
		nLog2[i] = float64(i) * math.Log2(float64(i))
	}
	return nLog2
}

// keyNameMatcher redacts a value because of the name attached to it, not its shape
// (`printenv` output has no recognisable value shape). It scans exempt fields too, since a
// secret named AWS_SECRET_ACCESS_KEY is one wherever it appears; the key name survives.
type keyNameMatcher struct {
	re *regexp.Regexp

	// names[i] uppercased once, so MatchesKeyName folds only the key it is given.
	upperNames []string

	// Marks names configured with a leading "*": suffix test rather than equality.
	suffix []bool

	// Lower-cased literal cores of the alternation ("_token" for *_TOKEN). Gating re behind the
	// shared automaton narrows the rule: re is (?i), which also folds non-ASCII runes (long s,
	// Kelvin sign) the byte automaton can never fire on. Accepted; nil stems mean no sound
	// literal, so the regex runs ungated.
	stems []string
}

// DefaultSecretKeyNames is the starting set; config additions only make scrubbing
// stricter.
func DefaultSecretKeyNames() []string {
	return []string{
		"AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_ACCESS_KEY_ID",
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY",
		"GITHUB_TOKEN", "GH_TOKEN", "GITLAB_TOKEN",
		"STRIPE_SECRET_KEY", "DATABASE_URL", "REDIS_URL",
		"*_TOKEN", "*_SECRET", "*_PASSWORD", "*_APIKEY", "*_API_KEY",
		"*_PRIVATE_KEY", "*_CREDENTIALS", "*_PASSWD",
	}
}

func newKeyNameMatcher(names []string) *keyNameMatcher {
	alts := make([]string, 0, len(names))
	stems := make([]string, 0, len(names))
	upper := make([]string, len(names))
	suffix := make([]bool, len(names))
	unfiltered := false
	for i, n := range names {
		literal := n
		if rest, ok := strings.CutPrefix(n, "*"); ok {
			literal = rest
			suffix[i] = true
			alts = append(alts, `[A-Za-z0-9_]*`+regexp.QuoteMeta(rest))
		} else {
			alts = append(alts, regexp.QuoteMeta(n))
		}
		upper[i] = strings.ToUpper(literal)
		if literal == "" || !isASCII(literal) {
			unfiltered = true
			continue
		}
		stems = append(stems, strings.ToLower(literal))
	}
	if unfiltered || len(stems) == 0 {
		stems = nil
	}
	// NAME=value, NAME: value, NAME = "value"; the value run stops at whitespace,
	// quotes and separators so one redaction cannot swallow a whole line.
	pattern := `(?i)\b(?:` + strings.Join(alts, "|") + `)\b\s*[:=]\s*"?([^\s"',;)]+)"?`
	return &keyNameMatcher{
		re:         regexp.MustCompile(pattern),
		upperNames: upper, suffix: suffix, stems: stems,
	}
}

func (m *keyNameMatcher) RuleID() string { return "key-name" }

// MatchScannedIn ignores the shared PII walk: this matcher has no candidate shape in it
// to collect.
func (m *keyNameMatcher) MatchScannedIn(value string, _ *packs.ValueScan) []Span {
	var out []Span
	for _, loc := range m.re.FindAllStringSubmatchIndex(value, -1) {
		if len(loc) < 4 || loc[2] < 0 {
			continue
		}
		out = append(out, Span{Start: loc[2], End: loc[3], RuleID: m.RuleID()})
	}
	return out
}

// MatchesKeyName reports whether a JSON key itself names a secret, in which case the
// whole value goes. Two vocabularies: configured names match as the environment spells
// them, author-spelled field names match as words (matchesSecretKeyName in keyname.go).
func (m *keyNameMatcher) MatchesKeyName(key string) bool {
	if key == "" {
		return false
	}
	if matchesSecretKeyName(key) {
		return true
	}
	// Configured environment names apply as written to field names too.
	upper := strings.ToUpper(key)
	for i, n := range m.upperNames {
		if m.suffix[i] {
			if strings.HasSuffix(upper, n) {
				return true
			}
			continue
		}
		if upper == n {
			return true
		}
	}
	return false
}

// pathUserRewriter replaces the OS user's home segment with a structural placeholder,
// keeping paths comparable across sessions. A rewriter, not a detector: it runs on every
// field including exempt ones, because a username in a cwd is PII either way.
type pathUserRewriter struct {
	username string
}

func (r *pathUserRewriter) RuleID() string { return "path-user" }

func isAlnumByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
