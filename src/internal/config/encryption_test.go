package config_test

import (
	"slices"
	"testing"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
)

func testRecipient(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient().String()
}

func TestPolicyGradeLayersAddRecipientsAsAUnion(t *testing.T) {
	home := fakeHome(t)
	userRec, orgRec := testRecipient(t), testRecipient(t)

	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t,
			"encryption:\n  additional_recipients: ["+userRec+"]\n")},
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t,
			"encryption:\n  additional_recipients: ["+orgRec+", "+userRec+"]\n")},
	))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{userRec, orgRec}
	if !slices.Equal(eff.AdditionalRecipients, want) {
		t.Errorf("additional recipients:\n got %v\nwant %v (union, first-seen order, no duplicates)",
			eff.AdditionalRecipients, want)
	}
}

// Rejecting at resolve time is what makes the refresh path fall back to the last valid config.
func TestUnparseableRecipientIsRejectedAtResolveTime(t *testing.T) {
	home := fakeHome(t)
	body := "encryption:\n  additional_recipients: [not-an-age-key]\n"
	for _, layer := range []config.Layer{config.LayerUser, config.LayerRemote} {
		_, err := config.Resolve(baseInput(t, home,
			config.LayeredDocument{Layer: layer, Doc: doc(t, body)},
		))
		if err == nil {
			t.Fatalf("a recipient that does not parse must be a resolve-time rejection (%v layer)", layer)
		}
	}
}

// The served remote config is the org's recipient channel.
func TestServedRemoteConfigAddsRecipients(t *testing.T) {
	home := fakeHome(t)
	rec := testRecipient(t)

	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t,
			"org: acme\nencryption:\n  additional_recipients: ["+rec+"]\n")},
	))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(eff.AdditionalRecipients, []string{rec}) {
		t.Errorf("served recipients did not land: %v", eff.AdditionalRecipients)
	}
	if !eff.IncludeInstallRecipient {
		t.Error("a served config that does not mention include_install_recipient must not withhold it")
	}
	origin := eff.Provenance["encryption.additional_recipients"]
	if origin.Layer != config.LayerRemote {
		t.Errorf("provenance should be the remote layer, got %v", origin.Layer)
	}
}
