package sources

import (
	"context"
	"testing"
)

func TestKeychainMissingService(t *testing.T) {
	raw, err := readAccountKeychain(context.Background(), "quesma-shipper-test-missing-credential-914ebc04")
	if len(raw) != 0 || err == nil || err.Error() != "Keychain unavailable" {
		t.Fatalf("Keychain query: bytes=%d err=%v", len(raw), err)
	}
}
