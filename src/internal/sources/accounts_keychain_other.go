//go:build !darwin

package sources

import (
	"context"
	"fmt"
)

func readAccountKeychain(context.Context, string) ([]byte, error) {
	return nil, fmt.Errorf("Keychain unavailable")
}
