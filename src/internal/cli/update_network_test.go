package cli

// Pins what the boot-time self-update records when the network fails under it (field fault F3): no
// answer is the machine offline, said on stderr only. The real path runs from maybeSelfUpdate through
// go-tuf over real sockets; only http.DefaultTransport is swapped, to reach a local TLS "CDN".

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
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

const faultedVersion = "0.0.1-123.abcdef123456"

// The deepest request reachable without the release keys: right after 2.root.json answers 404.
const faultedPath = "/metadata/timestamp.json"

// Go words the same socket faults differently on Windows.
var (
	resetErrno  = error(syscall.ECONNRESET)
	resetText   = ": read: connection reset by peer"
	refusedText = ": connect: connection refused"
)

func init() {
	if runtime.GOOS == "windows" {
		resetErrno = syscall.Errno(10054) // WSAECONNRESET
		resetText = ": wsarecv: An existing connection was forcibly closed by the remote host."
		refusedText = ": connectex: No connection could be made because the target machine actively refused it."
	}
}

// faultHandler answers faultedPath; bodyRead fires once the client has consumed part of its body.
type faultHandler func(w http.ResponseWriter, r *http.Request, bodyRead <-chan struct{})

// updateCDN is a TLS server reached for every host the update client dials.
type updateCDN struct {
	srv      *httptest.Server
	bodyRead chan struct{}
}

func newUpdateCDN(t *testing.T, fault faultHandler) *updateCDN {
	t.Helper()
	cdn := &updateCDN{bodyRead: make(chan struct{}, 1)}
	cdn.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metadata/2.root.json":
			http.NotFound(w, r)
		case faultedPath:
			fault(w, r, cdn.bodyRead)
		default:
			t.Errorf("unexpected update request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cdn.srv.Close)
	return cdn
}

// transport trusts the server certificate and dials it whatever host the URL names.
func (c *updateCDN) transport() http.RoundTripper {
	tr := c.srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = "example.com" // a name the httptest certificate carries
	addr := c.srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return signalBodyRead{tr, c.bodyRead}
}

// Reports faultedPath's first body bytes, so a fault lands inside the body read, not racing headers.
type signalBodyRead struct {
	*http.Transport
	read chan<- struct{}
}

func (s signalBodyRead) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := s.Transport.RoundTrip(req)
	if err == nil && req.URL.Path == faultedPath {
		resp.Body = &signalBody{ReadCloser: resp.Body, read: s.read}
	}
	return resp, err
}

type signalBody struct {
	io.ReadCloser
	read chan<- struct{}
	sent bool
}

func (b *signalBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.sent {
		b.sent = true
		b.read <- struct{}{}
	}
	return n, err
}

func useTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() {
		http.DefaultTransport = previous
		rt.(interface{ CloseIdleConnections() }).CloseIdleConnections()
	})
}

// A 200 promising more body than it delivers, returned once the client has read what was sent.
func writePartial(t *testing.T, w http.ResponseWriter, bodyRead <-chan struct{}) (*tls.Conn, bool) {
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return nil, false
	}
	const body = `{"signed":{"_type":"timestamp",`
	fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n%s", body)
	if err := buf.Flush(); err != nil {
		t.Errorf("partial response: %v", err)
		conn.Close()
		return nil, false
	}
	select {
	case <-bodyRead:
	case <-time.After(10 * time.Second):
		t.Errorf("the client never read the partial body")
		conn.Close()
		return nil, false
	}
	return conn.(*tls.Conn), true
}

// resetMidBody aborts the TCP connection with an RST (SO_LINGER 0) under the body read.
func resetMidBody(t *testing.T) faultHandler {
	return func(w http.ResponseWriter, _ *http.Request, bodyRead <-chan struct{}) {
		conn, ok := writePartial(t, w, bodyRead)
		if !ok {
			return
		}
		tcp := conn.NetConn().(*net.TCPConn)
		if err := tcp.SetLinger(0); err != nil {
			t.Errorf("SO_LINGER: %v", err)
		}
		tcp.Close()
	}
}

// stallMidBody sends part of the body, reports it was read, then sends nothing until released.
func stallMidBody(t *testing.T, released <-chan struct{}, stalled chan<- struct{}) faultHandler {
	return func(w http.ResponseWriter, _ *http.Request, bodyRead <-chan struct{}) {
		conn, ok := writePartial(t, w, bodyRead)
		if !ok {
			return
		}
		defer conn.Close()
		if stalled != nil {
			close(stalled)
		}
		<-released
	}
}

// fakeDNS answers every query NXDOMAIN, so the pure-Go resolver returns a genuine *net.DNSError.
func fakeDNS(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			end := 12
			for end < n && q[end] != 0 {
				end += int(q[end]) + 1
			}
			end += 5 // root label, QTYPE, QCLASS
			if end > n {
				continue
			}
			resp := append([]byte{q[0], q[1], 0x81, 0x83, 0, 1, 0, 0, 0, 0, 0, 0}, q[12:end]...)
			pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func nxdomainTransport(t *testing.T) *http.Transport {
	server := fakeDNS(t)
	d := &net.Dialer{Resolver: &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", server)
	}}}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = d.DialContext
	return tr
}

// refusedTransport dials a loopback port that was just closed, so the kernel refuses it.
func refusedTransport(t *testing.T) *http.Transport {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return tr
}

// releaseDaemon isolates the state directory and opens the boot gate for a released build.
func releaseDaemon(t *testing.T) (app.Build, string) {
	t.Helper()
	if packaging.SystemManaged() {
		t.Skip("this machine has a system-managed install; maybeSelfUpdate would not reach the network")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv(app.NoSelfUpdateEnv, "")
	t.Setenv(app.ReexecGuardEnv, "")
	stateDir, err := app.StateDirWithoutConfig()
	if err != nil {
		t.Fatal(err)
	}
	return app.Build{Version: faultedVersion, Release: true}, stateDir
}

func runSelfUpdate(ctx context.Context, build app.Build) string {
	var out strings.Builder
	maybeSelfUpdate(ctx, build, true, &out)
	return out.String()
}

// assertMidBody pins that err came out of the body read: a round-trip failure is a *url.Error.
func assertMidBody(t *testing.T, err error) {
	t.Helper()
	var ue *url.Error
	if errors.As(err, &ue) {
		t.Fatalf("the fault surfaced before the response headers: %v", err)
	}
}

func readFailureRecordFile(t *testing.T, stateDir string) (formats.FailureRecord, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, "last-failure.json"))
	if errors.Is(err, os.ErrNotExist) {
		return formats.FailureRecord{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var rec formats.FailureRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("last-failure.json: %v\n%s", err, raw)
	}
	return rec, true
}

// assertRecorded pins one uncounted update_failed event whose message carries every substring.
func assertRecorded(t *testing.T, stateDir, stderr string, want ...string) {
	t.Helper()
	rec, ok := readFailureRecordFile(t, stateDir)
	if !ok || len(rec.Recent) != 1 {
		t.Fatalf("want one recorded failure, got %+v (file present: %v)\nstderr:\n%s", rec, ok, stderr)
	}
	ev := rec.Recent[0]
	if ev.Kind != formats.FailureUpdate {
		t.Errorf("kind = %q, want %q", ev.Kind, formats.FailureUpdate)
	}
	if rec.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0: a skipped update is not a failing collector", rec.ConsecutiveFailures)
	}
	for _, w := range append([]string{"self-update from " + faultedVersion + " did not happen: "}, want...) {
		if !strings.Contains(ev.Message, w) {
			t.Errorf("message %q does not contain %q\nstderr:\n%s", ev.Message, w, stderr)
		}
	}
}

// assertSuppressed pins that nothing was recorded; stderr is then the only trace of the fault.
func assertSuppressed(t *testing.T, stateDir, stderr string, want ...string) {
	t.Helper()
	if rec, ok := readFailureRecordFile(t, stateDir); ok {
		t.Fatalf("offline update was recorded: %+v\nstderr:\n%s", rec, stderr)
	}
	for _, w := range append([]string{"self-update: skipped: "}, want...) {
		if !strings.Contains(stderr, w) {
			t.Errorf("stderr does not contain %q:\n%s", w, stderr)
		}
	}
}

// F3: a reset under a TUF fetch body (metadata; the target download needs the release keys).
func TestSelfUpdateConnectionResetMidBodyIsSuppressedAsOffline(t *testing.T) {
	build, stateDir := releaseDaemon(t)
	cdn := newUpdateCDN(t, resetMidBody(t))
	useTransport(t, cdn.transport())

	_, err := packaging.Update(context.Background(), packaging.UpdateOptions{Current: faultedVersion})
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "read" || !errors.Is(err, resetErrno) {
		t.Fatalf("want a socket read reset, got %#v", err)
	}
	assertMidBody(t, err)

	stderr := runSelfUpdate(context.Background(), build)
	assertSuppressed(t, stateDir, stderr, "refreshing TUF metadata: read tcp ", resetText)
}

// A body that stops arriving holds the update until a deadline, which is no answer: suppressed.
func TestSelfUpdateStalledBodyIsSuppressedAtTheDeadline(t *testing.T) {
	build, stateDir := releaseDaemon(t)
	released := make(chan struct{})
	cdn := newUpdateCDN(t, stallMidBody(t, released, nil))
	t.Cleanup(func() { close(released) }) // before the server waits for its handlers
	useTransport(t, cdn.transport())

	// The 5 minute update deadline is a constant, so the caller's deadline stands in for it.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := packaging.Update(ctx, packaging.UpdateOptions{Current: faultedVersion})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline, got %v", err)
	}
	assertMidBody(t, err)
	// What the field sees when that internal deadline fires under a live daemon context.
	if _, record := updateFailure(context.Background(), faultedVersion, err); record {
		t.Fatalf("a stalled update under a live context was recorded: %v", err)
	}

	// The request's own deadline stands in for the internal one, so the daemon context stays live.
	useTransport(t, requestDeadline{cdn.transport(), 300 * time.Millisecond})
	stderr := runSelfUpdate(context.Background(), build)
	assertSuppressed(t, stateDir, stderr, "refreshing TUF metadata: context deadline exceeded")
}

// requestDeadline bounds each request, body read included, without touching the caller's context.
type requestDeadline struct {
	http.RoundTripper
	after time.Duration
}

func (d requestDeadline) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), d.after)
	resp, err := d.RoundTripper.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelOnClose{resp.Body, cancel}
	return resp, nil
}

func (d requestDeadline) CloseIdleConnections() {
	d.RoundTripper.(interface{ CloseIdleConnections() }).CloseIdleConnections()
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}

// A CDN answering 503 is a reachable far end: recorded, with go-tuf's status wording.
func TestSelfUpdateCDN503IsRecorded(t *testing.T) {
	build, stateDir := releaseDaemon(t)
	cdn := newUpdateCDN(t, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	})
	useTransport(t, cdn.transport())

	stderr := runSelfUpdate(context.Background(), build)
	assertRecorded(t, stateDir, stderr, "refreshing TUF metadata: "+
		"failed to download https://updates.quesma.dev/metadata/timestamp.json, http status code: 503")
}

// A name that does not resolve is the machine offline: said on stderr, not recorded.
func TestSelfUpdateDNSFailureIsSuppressedAsOffline(t *testing.T) {
	build, stateDir := releaseDaemon(t)
	useTransport(t, nxdomainTransport(t))

	stderr := runSelfUpdate(context.Background(), build)
	assertSuppressed(t, stateDir, stderr, `refreshing TUF metadata: `+
		`Get "https://updates.quesma.dev/metadata/2.root.json": dial tcp: lookup updates.quesma.dev`, ": no such host")
}

// A refused dial is the machine offline too: said on stderr, not recorded.
func TestSelfUpdateRefusedDialIsSuppressedAsOffline(t *testing.T) {
	build, stateDir := releaseDaemon(t)
	useTransport(t, refusedTransport(t))

	stderr := runSelfUpdate(context.Background(), build)
	assertSuppressed(t, stateDir, stderr, `refreshing TUF metadata: `+
		`Get "https://updates.quesma.dev/metadata/2.root.json": dial tcp `, refusedText)
}

// Shutdown cancelling the daemon context mid-download is recorded as a failed update today.
func TestSelfUpdateCancelledMidBodyIsRecorded(t *testing.T) {
	build, stateDir := releaseDaemon(t)
	released, stalled := make(chan struct{}), make(chan struct{})
	cdn := newUpdateCDN(t, stallMidBody(t, released, stalled))
	t.Cleanup(func() { close(released) }) // before the server waits for its handlers
	useTransport(t, cdn.transport())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stalled:
		case <-time.After(10 * time.Second):
		}
		cancel()
	}()
	stderr := runSelfUpdate(ctx, build)
	select {
	case <-stalled:
	default:
		t.Fatalf("the update never reached the stalled body\nstderr:\n%s", stderr)
	}
	assertRecorded(t, stateDir, stderr, "refreshing TUF metadata: context canceled")
}
