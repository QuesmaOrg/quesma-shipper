package packaging

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

func notifyRevocation(ctx context.Context, message string) bool {
	info, err := os.Stat("/dev/console")
	if err != nil {
		return false
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Getuid() {
		return false
	}
	return exec.CommandContext(ctx, "/usr/bin/osascript", "-e", `on run argv
 display notification (item 1 of argv) with title "Quesma Shipper: install revoked"
end run`, message).Run() == nil
}
