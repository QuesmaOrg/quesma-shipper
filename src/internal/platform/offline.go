package platform

import (
	"context"
	"errors"
	"net"
)

// Offline reports an error that never reached the far end: a name that would not resolve, a dial
// that was refused or timed out, a handshake that stalled, a connection reset underneath a request.
// The far end's own answers, a 5xx or a refusal, are not offline, and neither is a deadline this
// process set itself.
func Offline(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var dns *net.DNSError
	var op *net.OpError
	if errors.As(err, &dns) || errors.As(err, &op) {
		return true
	}
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}
