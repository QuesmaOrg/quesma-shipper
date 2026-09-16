package config_test

import (
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
)

func TestLegacyAccountDisableMigratesAndStaysDisabled(t *testing.T) {
	home := fakeHome(t)
	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, `sources:
  - id: codex-rollouts
    enrichers:
      codex-account: false
`)},
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t, `sources:
  - id: codex-account
    enabled: true
`)},
	))
	if err != nil {
		t.Fatal(err)
	}
	if sourceByID(t, eff, "codex-account").Enabled {
		t.Fatal("remote reenabled a locally disabled legacy account probe")
	}
	if _, ok := sourceByID(t, eff, "codex-rollouts").Enrichers["codex-account"]; ok {
		t.Fatal("legacy enricher survived")
	}
}

func TestTranscriptDisableDoesNotDisableAccountSource(t *testing.T) {
	eff, err := config.Resolve(baseInput(t, fakeHome(t), config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, `sources:
  - id: codex-rollouts
    enabled: false
`)}))
	if err != nil {
		t.Fatal(err)
	}
	if !sourceByID(t, eff, "codex-account").Enabled {
		t.Fatal("account source coupled to transcripts")
	}
}
