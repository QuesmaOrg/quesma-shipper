package transforms

import (
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/scrub"
)

// The scrub module states two of the shipper's facts itself, since it imports nothing: they must stay the shipper's.
func TestScrubModuleAgreesWithTheShipper(t *testing.T) {
	if scrub.UserPlaceholder != formats.UserPlaceholder {
		t.Errorf("scrub.UserPlaceholder is %q, formats.UserPlaceholder %q", scrub.UserPlaceholder, formats.UserPlaceholder)
	}
	if want := int64(maxDecompressedBytes - maxManifestBytes - 4096); scrub.MaxPayloadBytes != want {
		t.Errorf("scrub.MaxPayloadBytes is %d, the container holds %d", scrub.MaxPayloadBytes, want)
	}
}
