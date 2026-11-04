package packaging

import (
	"context"
	"testing"
)

func TestSSHDoesNotConsumeDesktopNotification(t *testing.T) {
	for _, variable := range []string{"SSH_CONNECTION", "SSH_TTY"} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv(variable, "synthetic-ssh-session")
			if NotifyRevocation(context.Background(), "synthetic revocation") {
				t.Fatal("SSH consumed desktop notification")
			}
		})
	}
}
