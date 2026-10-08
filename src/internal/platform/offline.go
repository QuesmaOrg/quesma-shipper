package platform

import (
	"context"
	"errors"
	"net"
)

// Offline reports an error from before any connection existed: a name that would not resolve, or a
// dial that was refused, unreachable or timed out. Anything after a connection is the far end's
// answer, not the network: a TLS alert, a reset under a request, a response that never came.
//
// A dial that ran out of the caller's own deadline reads exactly like one that ran out of the
// dialer's, so the error alone cannot tell them apart: the caller checks its context first, and
// asks only while that context is live.
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
