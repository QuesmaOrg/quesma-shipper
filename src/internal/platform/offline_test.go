package platform

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

// Real sockets, not hand-built errors: the dial timeout and the TLS alert both arrive as types a
// hand-built table gets wrong.
func TestOfflineTellsTheNetworkFromTheFarEnd(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	closed.Close()

	_, refused := net.Dial("tcp", closedAddr)
	_, timedOut := (&net.Dialer{Timeout: time.Nanosecond}).DialContext(context.Background(), "tcp", "127.0.0.1:1")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, byCaller := (&net.Dialer{}).DialContext(cancelled, "tcp", closedAddr)
	_, unresolved := net.DefaultResolver.LookupHost(context.Background(), "nonexistent.invalid")

	// A server that wants a client certificate answers the handshake with an alert.
	tlsSrv := httptest.NewUnstartedServer(http.NotFoundHandler())
	tlsSrv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
	tlsSrv.StartTLS()
	defer tlsSrv.Close()
	_, rejected := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}).Get(tlsSrv.URL)

	// A server that took the request and never answers is a stalled far end, not a missing network.
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) }))
	defer stall.Close()
	_, stalled := (&http.Client{Timeout: 20 * time.Millisecond}).Get(stall.URL)

	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"refused":           {refused, true},
		"dial timeout":      {timedOut, true},
		"unresolved":        {unresolved, true},
		"wrapped":           {fmt.Errorf("backend: /v2/uploads/authorize: %w", refused), true},
		"tls rejected":      {rejected, false},
		"stalled far end":   {stalled, false},
		"reset after reply": {&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, false},
		"5xx":               {errors.New("backend: HTTP 503"), false},
		"caller cancelled":  {byCaller, false},
		"nil":               {nil, false},
	} {
		if tc.err == nil && name != "nil" {
			t.Fatalf("%s: the probe produced no error", name)
		}
		if got := Offline(tc.err); got != tc.want {
			t.Errorf("%s: Offline(%v) = %v, want %v", name, tc.err, got, tc.want)
		}
	}
}
