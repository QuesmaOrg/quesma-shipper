package platform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
)

type handshakeTimeout struct{}

func (handshakeTimeout) Error() string   { return "net/http: TLS handshake timeout" }
func (handshakeTimeout) Timeout() bool   { return true }
func (handshakeTimeout) Temporary() bool { return true }

func TestOfflineTellsTheNetworkFromTheFarEnd(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"dns":          {&net.DNSError{Err: "no such host", Name: "cp.example", IsNotFound: true}, true},
		"refused":      {&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true},
		"reset":        {&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		"handshake":    {handshakeTimeout{}, true},
		"wrapped":      {fmt.Errorf("backend: /v2/uploads/authorize: %w", &net.DNSError{Err: "no such host"}), true},
		"5xx":          {errors.New("backend: HTTP 503"), false},
		"own deadline": {&net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}, false},
		"cancelled":    {fmt.Errorf("upload: %w", context.Canceled), false},
		"nil":          {nil, false},
	} {
		if got := Offline(tc.err); got != tc.want {
			t.Errorf("%s: Offline(%v) = %v, want %v", name, tc.err, got, tc.want)
		}
	}
}
