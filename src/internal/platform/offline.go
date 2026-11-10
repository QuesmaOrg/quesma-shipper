package platform

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
)

// Offline reports a request that got no complete HTTP response: a name that would not resolve, a
// dial, handshake, write or read that failed or timed out, or a body cut short. Any answer is the
// far end speaking, not the network: a status, a TLS alert, a certificate refused, a body that does
// not decode. A local error, file descriptors run out included, is never offline.
//
// A timeout that ran out of the caller's own deadline reads exactly like one that ran out of the
// client's, so the error alone cannot tell them apart: the caller checks its context first, and
// asks only while that context is live.
func Offline(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) {
		if op.Op == "proxyconnect" {
			// net/http wraps the hop to a configured proxy; what failed under it decides.
			return Offline(op.Err)
		}
		return op.Op == "dial" || op.Op == "read" || op.Op == "write"
	}
	return timedOut(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// timedOut walks the whole chain: *url.Error answers Timeout for its direct cause alone.
func timedOut(err error) bool {
	if t, ok := err.(interface{ Timeout() bool }); ok && t.Timeout() {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return u.Unwrap() != nil && timedOut(u.Unwrap())
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if timedOut(e) {
				return true
			}
		}
	}
	return false
}
