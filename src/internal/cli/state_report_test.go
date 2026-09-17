package cli

import (
	"bytes"
	"strings"
	"testing"
)

// An adopted document with nothing to forget used to print "nothing to do (0 entries)": the
// operator ran reset without --apply, was told there was nothing to apply, and stayed locked out of
// the one command that unblocks them.
func TestReportStateChangeNeverSaysNothingToDoAfterAnAdoption(t *testing.T) {
	for _, apply := range []bool{false, true} {
		var buf bytes.Buffer
		reportStateChange(&buf, 0, 0, apply, true, "forgotten")
		got := buf.String()
		if strings.Contains(got, "nothing to do") {
			t.Errorf("apply=%v: %q reads as a no-op after taking over a foreign document", apply, got)
		}
		if !strings.Contains(got, "another install's record") {
			t.Errorf("apply=%v: %q does not say what was replaced", apply, got)
		}
		if !apply && !strings.Contains(got, "--apply") {
			t.Errorf("a dry run over a foreign document must still name --apply: %q", got)
		}
	}
}

// Without an adoption the ordinary wording stands, so prune and a same-install reset are unchanged.
func TestReportStateChangeKeepsTheOrdinaryWording(t *testing.T) {
	var empty bytes.Buffer
	reportStateChange(&empty, 0, 7, true, false, "pruned")
	if !strings.Contains(empty.String(), "nothing to do") {
		t.Errorf("an ordinary no-op changed wording: %q", empty.String())
	}

	var did bytes.Buffer
	reportStateChange(&did, 2, 5, true, false, "pruned")
	if !strings.Contains(did.String(), "2 entries pruned, 5 remain") {
		t.Errorf("an ordinary apply changed wording: %q", did.String())
	}
}
