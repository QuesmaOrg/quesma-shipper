package packs

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// Two claims the per-rule tests do not make: a rule collecting its slot out of a shared scan
// gets what it would have got alone, and a scan carries nothing from the value before.

// fusedRules returns the loaded rules that have a slot in the shared walk.
func fusedRules(t *testing.T) []*Rule {
	t.Helper()
	rules, err := Load(PIICore)
	if err != nil {
		t.Fatal(err)
	}
	var out []*Rule
	for _, r := range rules {
		if r.fused != fusedNone {
			out = append(out, r)
		}
	}
	if len(out) != 3 {
		t.Fatalf("expected three fused pii-core rules, got %d", len(out))
	}
	return out
}

// Replays one ValueScan, reused the way the engine reuses it, against the per-rule scanners.
func TestFusedScanAgreesWithStandaloneScanners(t *testing.T) {
	rules := fusedRules(t)

	values := []string{
		"", " ", "-", "_", "0", "abc", "\xff", "é", "\x80\x80\x80",
		"12345678901", "x12345678901", "12345678901_", "123456789012",
		"44051401359", "4405140135944051401359", "44051401359 44051401359",
		"4111111111111111", "4111 1111 1111 1111", "4111-1111-1111-1111",
		"4111 1111 1111 1111 44051401359 4111111111111111",
		"1 2 3 4 5 6 7 8 9 0 1 2 3", "411111111111111 1",
		"DE89370400440532013000", "de89370400440532013000", "AB12CDEFGHIJKLM",
		"AB12CDEFGHIJKLMN", "AB12CDEFGHIJKLMNo", "AB12CDEFGHIJKLMN_",
		"44051401359-4111 1111 1111 1111-DE89370400440532013000",
		"DE8937040044053201300044051401359",
		strings.Repeat("4111", 20), strings.Repeat("1 ", 200), strings.Repeat("9", 40),
		strings.Repeat("1", 19) + "é" + strings.Repeat("1", 19),
	}

	rng := rand.New(rand.NewSource(20260817))
	for i := 0; i < 20000; i++ {
		values = append(values, randPIIValue(rng))
	}

	// One scan for the whole run, reset per value: the engine's own lifetime.
	var scan ValueScan
	for _, v := range values {
		scan.Reset(v)
		for _, r := range rules {
			want := r.MatchScanned(v)
			got := r.MatchScannedIn(v, &scan)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("%s on %q: standalone %v, fused %v", r.id, v, want, got)
			}
		}
	}
}

// A rule reading its slot must not depend on which rule ran the walk, or on how many did.
func TestFusedScanIsIndependentOfRuleOrder(t *testing.T) {
	rules := fusedRules(t)
	rng := rand.New(rand.NewSource(20260818))

	for i := 0; i < 20000; i++ {
		v := randPIIValue(rng)
		for skip := range rules {
			// One rule triggers the walk, so every other slot was filled by a walk it
			// did not ask for.
			var scan ValueScan
			scan.Reset(v)
			solo := rules[skip].MatchScannedIn(v, &scan)
			if want := rules[skip].MatchScanned(v); !reflect.DeepEqual(want, solo) {
				t.Fatalf("%s alone on %q: standalone %v, fused %v", rules[skip].id, v, want, solo)
			}
			for j, r := range rules {
				if j == skip {
					continue
				}
				if want := r.MatchScanned(v); !reflect.DeepEqual(want, r.MatchScannedIn(v, &scan)) {
					t.Fatalf("%s after %s on %q: %v", r.id, rules[skip].id, v, want)
				}
			}
		}
	}
}

// A rule with no slot must answer the same whatever the scan holds.
func TestNonFusedRuleIgnoresTheScan(t *testing.T) {
	rules, err := Load(PIICore)
	if err != nil {
		t.Fatal(err)
	}
	var scan ValueScan
	scan.Reset("4111111111111111 44051401359 DE89370400440532013000")
	scan.candidates(fusedPESEL) // force the walk, so the slots are non-empty

	for _, r := range rules {
		if r.fused != fusedNone {
			continue
		}
		v := "mail jane@example.com and 4111111111111111"
		if want, got := r.MatchScanned(v), r.MatchScannedIn(v, &scan); !reflect.DeepEqual(want, got) {
			t.Fatalf("%s: standalone %v, with a stale scan %v", r.id, want, got)
		}
	}
}
