//go:build !darwin && !windows

package packaging

import "context"

func notifyRevocation(context.Context, string) bool { return false }
