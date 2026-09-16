package sources

import (
	"context"
	"strings"
	"testing"
)

func TestKeychainNativeMissingService(t *testing.T) {
	raw, err := readAccountKeychain(context.Background(), "quesma-shipper-test-missing-credential-914ebc04")
	if len(raw) != 0 || err == nil || !strings.HasPrefix(err.Error(), "Keychain unavailable (") {
		t.Fatalf("native Keychain query: bytes=%d err=%v", len(raw), err)
	}
}
