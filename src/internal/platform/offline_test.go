package platform

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Real sockets, not hand-built errors: the dial timeout and the TLS alert both arrive as types a
// hand-built table gets wrong. Offline means no complete HTTP response arrived; any answer is not.
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

	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) }))
	defer stall.Close()
	_, stalled := (&http.Client{Timeout: 20 * time.Millisecond}).Get(stall.URL)
	_, headerTimeout := (&http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 20 * time.Millisecond}}).Get(stall.URL)

	// A 200 promising more body than it sends, then the connection closes.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "64")
		_, _ = io.WriteString(w, `{"tickets":`)
	}))
	defer short.Close()
	var truncated error
	if resp, err := http.Get(short.URL); err != nil {
		t.Fatal(err)
	} else {
		_, truncated = io.ReadAll(resp.Body)
		resp.Body.Close()
	}

	// The server closes the connection after reading the request, before any response byte.
	hangup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer hangup.Close()
	_, hungUp := http.Post(hangup.URL, "application/json", nil)

	// Through a configured proxy: one that refuses the connection, and one that answers CONNECT.
	closedProxy := &url.URL{Scheme: "http", Host: closedAddr}
	_, proxyRefused := (&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(closedProxy)}}).Get("https://example.invalid/")
	authProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer authProxy.Close()
	authURL, _ := url.Parse(authProxy.URL)
	_, proxyAnswered := (&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(authURL)}}).Get("https://example.invalid/")

	_, localRead := os.ReadFile(filepath.Join(t.TempDir(), "missing"))
	localDecode := json.Unmarshal([]byte(`{"tickets":`), &struct{}{})

	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"refused":           {refused, true},
		"dial timeout":      {timedOut, true},
		"unresolved":        {unresolved, true},
		"wrapped":           {fmt.Errorf("backend: /v2/uploads/authorize: %w", refused), true},
		"tls rejected":      {rejected, false},
		"stalled far end":   {stalled, true},
		"header timeout":    {headerTimeout, true},
		"truncated body":    {fmt.Errorf("backend: read response: %w", truncated), true},
		"hung up":           {hungUp, true},
		"reset after reply": {&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		"5xx":               {errors.New("backend: HTTP 503"), false},
		"local read":        {localRead, false},
		"local decode":      {localDecode, false},
		"proxy refused":     {proxyRefused, true},
		"proxy answered":    {proxyAnswered, false},
		"out of sockets":    {&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("socket", syscall.EMFILE)}, false},
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
