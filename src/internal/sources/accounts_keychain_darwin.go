package sources

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"os/user"
	"time"
)

// Use the system credential reader, which has its own Keychain access permissions.
func readAccountKeychain(ctx context.Context, service string) ([]byte, error) {
	account, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("Keychain account unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-a", account.Username, "-s", service, "-w").Output()
	if err != nil {
		return nil, fmt.Errorf("Keychain unavailable")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > accountResponseLimit {
		return nil, fmt.Errorf("invalid Keychain data size")
	}
	return raw, nil
}
