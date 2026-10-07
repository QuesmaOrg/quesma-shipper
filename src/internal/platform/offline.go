package platform

import (
	"context"
	"errors"
	"net"
)

// Offline reports an error from before any connection existed: a name that would not resolve, or a
// dial that was refused, unreachable or timed out. Anything after a connection is the far end's
// answer, not the network: a TLS alert, a reset under a request, a response that never came. A
// dial cancelled by the caller's own context is not offline either.
func Offline(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
