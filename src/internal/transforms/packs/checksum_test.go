package packs_test

import (
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/packs"
)

// Checksum-verified rules are the only precise rules: their shapes are otherwise so common
// that an unverified rule would fire on every number in a transcript.
func TestChecksumRulesRejectShapeWithoutChecksum(t *testing.T) {
	rules, err := packs.Load(packs.PIICore)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*packs.Rule{}
	for _, r := range rules {
		byID[r.RuleID()] = r
	}

	cases := []struct {
		rule  string
		valid string
		junk  string
	}{
		// Test PAN from the card networks' published test set.
		{"card-pan", "4111111111111111", "4111111111111112"},
		{"pesel", "44051401359", "44051401358"},
		{"iban", "GB82WEST12345698765432", "GB82WEST12345698765433"},
	}

	for _, c := range cases {
		t.Run(c.rule, func(t *testing.T) {
			r, ok := byID[c.rule]
			if !ok {
				t.Fatalf("rule %q missing from the pack", c.rule)
			}
			if len(r.MatchScanned(c.valid)) == 0 {
				t.Errorf("a checksum-valid value must match: %s", c.valid)
			}
			if len(r.MatchScanned(c.junk)) != 0 {
				t.Errorf("a checksum-INVALID value of the same shape must not match: %s", c.junk)
			}
		})
	}
}

// A PESEL-shaped number whose embedded month is impossible is an id, not an identity number.
func TestPESELRejectsImpossibleDates(t *testing.T) {
	rules, err := packs.Load(packs.PIICore)
	if err != nil {
		t.Fatal(err)
	}
	var pesel *packs.Rule
	for _, r := range rules {
		if r.RuleID() == "pesel" {
			pesel = r
		}
	}
	if pesel == nil {
		t.Fatal("pesel rule missing")
	}
	// Month 99 cannot occur in any PESEL century encoding.
	if len(pesel.MatchScanned("44991401351")) != 0 {
		t.Error("a PESEL-shaped number with an impossible month must not match")
	}
}

// A rule with no id would produce a sentinel of "__REDACTED:__", which tells a consumer nothing.
func TestAllPacksCompileWithNamedRules(t *testing.T) {
	for _, pack := range packs.PatternPacks {
		rules, err := packs.Load(pack)
		if err != nil {
			t.Fatalf("%s: %v", pack, err)
		}
		if len(rules) == 0 {
			t.Errorf("%s: no rules", pack)
		}
		for _, r := range rules {
			if strings.TrimSpace(r.RuleID()) == "" {
				t.Errorf("%s: a rule has no id", pack)
			}
		}
	}
}

// An audit of a real archive found zero genuine cards among tens of thousands of Luhn-only hits.
// Each row below is a shape that rule redacted; real PANs, in every notation, must still match.
func TestPanRejectsTheAuditedFalsePositiveClasses(t *testing.T) {
	rules, err := packs.Load(packs.PIICore)
	if err != nil {
		t.Fatal(err)
	}
	var pan *packs.Rule
	for _, r := range rules {
		if r.RuleID() == "card-pan" {
			pan = r
		}
	}
	if pan == nil {
		t.Fatal("card-pan missing from the pack")
	}

	stillCards := []struct{ name, text string }{
		{"visa 16 contiguous", "pay with 4111111111111111 now"},
		{"visa 16 spaced", "card: 4111 1111 1111 1111"},
		{"visa 16 dashed", "card: 4111-1111-1111-1111"},
		{"visa 13", "old visa 4222222222222"},
		{"amex 15 contiguous", "amex 378282246310005"},
		{"amex 15 grouped 4-6-5", "amex 3782 822463 10005"},
		{"mastercard 16", "mc 5555555555554444"},
		{"mastercard 2-series", "mc 2223003122003222"},
		{"discover 16", "discover 6011111111111117"},
		{"diners 14", "diners 30569309025904"},
		{"unionpay 16", "up 6200000000000005"},
		{"sentence-final card", "the card is 4111111111111111."},
		{"whole value is the card", "4111111111111111"},
	}
	for _, c := range stillCards {
		t.Run("card/"+c.name, func(t *testing.T) {
			if len(pan.MatchScanned(c.text)) == 0 {
				t.Errorf("a real card notation stopped matching: %s", c.text)
			}
		})
	}

	// Every row is Luhn-valid and was matched by the old rule; the comment names the gate
	// that now rejects it.
	notCards := []struct{ name, text string }{
		// decimal-fraction: fractional digits of a float, both spellings. Guard, not checksum.
		{"float fraction", "rmse: 0.4712949612297219 after 40 epochs"},
		{"bare fraction", "coefficient .4712949612297219 stored"},
		{"float integer part", "value 4111111111111111.25 overflows"},
		// job-or-run-datestamp-id: 8-6 fails grouping, contiguous fails the issuer window.
		{"datestamp id dashed", "run 20260619-100853 failed"},
		{"datestamp id contiguous", "tag taiga-20260619100853 pushed"},
		// epoch-timestamp (5%): 13 digits starting 17/18 fail the issuer gate.
		{"epoch ms in filename", "tool-results/webfetch-1783361082810-3jalbx.pdf"},
		{"epoch ms bare", "startedAt 1783361082810 elapsed 4162"},
		// linkedin-activity-id: 19 digits and Luhn-valid; the issuer gate rejects the 7.
		{"linkedin activity id", "linkedin.com/posts/x_activity-7382019465738291045-AbCd"},
		// uuid-fragment (5%): digit chunks of UUIDs; 4-4-4-4 shape but issuer 1.
		{"uuid digit fragment", "sessionId 1111-1111-1111-1111 resumed"},
		// timetable-or-spaced-digit-groups: uniform separator, wrong grouping.
		{"grouped but not card-shaped", "seats 411 1111 1111 11111 booked"},
		// mixed separators: nobody writes a card that way.
		{"mixed separators", "ref 4111 1111-1111 1111 logged"},
		// company-registry-id: 14-digit SIRET written 3-3-3-5.
		{"siret grouped", "SIRET 552 100 554 00031 registered"},
		// plain luhn failure of card shape, kept from the old test.
		{"luhn-invalid", "4111111111111112"},
	}
	for _, c := range notCards {
		t.Run("notcard/"+c.name, func(t *testing.T) {
			if got := pan.MatchScanned(c.text); len(got) != 0 {
				t.Errorf("an audited false-positive class matches again: %s (spans %v)", c.text, got)
			}
		})
	}
}
