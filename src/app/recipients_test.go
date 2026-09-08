package app

import (
	"fmt"
	"testing"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
)

func testUnit(t *testing.T) *identity.Unit {
	t.Helper()
	unit, err := identity.Mint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return unit
}

func testRecipient(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient().String()
}

func recipientStrings(rs []age.Recipient) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.(fmt.Stringer).String())
	}
	return out
}

func TestRecipientsForComposesInstallThenAdditional(t *testing.T) {
	unit := testUnit(t)
	org := testRecipient(t)

	got, err := recipientsFor(&config.Effective{
		IncludeInstallRecipient: true,
		AdditionalRecipients:    []string{org},
	}, unit)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{unit.Recipient().String(), org}
	gotS := recipientStrings(got)
	if len(gotS) != 2 || gotS[0] != want[0] || gotS[1] != want[1] {
		t.Errorf("recipients:\n got %v\nwant %v", gotS, want)
	}
}

func TestRecipientsForHonorsAWithheldInstallRecipient(t *testing.T) {
	unit := testUnit(t)
	org := testRecipient(t)

	got, err := recipientsFor(&config.Effective{
		IncludeInstallRecipient: false,
		AdditionalRecipients:    []string{org},
	}, unit)
	if err != nil {
		t.Fatal(err)
	}
	if gotS := recipientStrings(got); len(gotS) != 1 || gotS[0] != org {
		t.Errorf("a withheld install recipient must not be encrypted to: %v", gotS)
	}
}

func TestRecipientsForRefusesAnEmptySet(t *testing.T) {
	if _, err := recipientsFor(&config.Effective{IncludeInstallRecipient: false}, testUnit(t)); err == nil {
		t.Error("an empty recipient set must be refused here, where the error names the cause, " +
			"rather than per-object at seal time")
	}
}

func TestRecipientsForRefusesAnUnparseableRecipient(t *testing.T) {
	eff := &config.Effective{
		IncludeInstallRecipient: true,
		AdditionalRecipients:    []string{"not-an-age-key"},
	}
	if _, err := recipientsFor(eff, testUnit(t)); err == nil {
		t.Error("an Effective built by hand with a bad recipient must be refused, " +
			"not sealed to fewer readers than configured")
	}
}
