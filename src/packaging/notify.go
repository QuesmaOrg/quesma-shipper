package packaging

import (
	"context"
	"os"
)

// A remote diagnostic must not consume the local user's notification.
func NotifyRevocation(ctx context.Context, message string) bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	return notifyRevocation(ctx, message)
}
