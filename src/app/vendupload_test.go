package app

// The classifications the vend port owes the engine: each decides whether a run stops for good,
// stops until the next tick, or asks for one fresh ticket, so each is asserted on its own.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/upload"
)

func TestAlreadyPresentCommitsWithoutSpendingACapability(t *testing.T) {
	p := &vendPort{}
	err := p.send(context.Background(), engine.PreparedObject{ObjectID: "trajectory-1"},
		controlplane.Ticket{TicketID: "b1bd1a73-f16d-4a51-aac6-29f1f48b0658", ObjectID: "trajectory-1", AlreadyPresent: true})
	if err != nil {
		t.Fatalf("already-present answer tried to validate or upload a capability: %v", err)
	}
}

// A refusal kills the install, an outage stops only this run: conflating them turns a rate limit
// into a permanently dead install, so the sentinels must not reach each other.
func TestAuthorizeFailuresKeepRefusalAndUnavailabilityApart(t *testing.T) {
	refused := fmt.Errorf("backend: /v2/uploads/authorize refused this install (HTTP 403): %w",
		formats.ErrCredentialsRefused)
	if got := classifyAuthorize(context.Background(), refused); !errors.Is(got, formats.ErrCredentialsRefused) {
		t.Errorf("a refusal was reclassified as %v", got)
	}
	if got := classifyAuthorize(context.Background(), refused); errors.Is(got, engine.ErrUploadUnavailable) {
		t.Error("a refusal also reads as an unavailable control plane")
	}

	unavailable := fmt.Errorf("%w (HTTP 503)", controlplane.ErrAuthorizeUnavailable)
	got := classifyAuthorize(context.Background(), unavailable)
	if !errors.Is(got, engine.ErrUploadUnavailable) {
		t.Errorf("%v was not classified as unavailable: %v", unavailable, got)
	}
	if errors.Is(got, formats.ErrCredentialsRefused) {
		t.Errorf("%v reads as a credentials refusal, which would kill the install", unavailable)
	}

	// Everything else is one batch's failure: no sentinel, so the next run retries it.
	for _, other := range []error{
		errors.New("backend: decode response"),
		errors.New("backend: /v2/uploads/authorize returned HTTP 409"),
	} {
		if got := classifyAuthorize(context.Background(), other); got != other {
			t.Errorf("an unclassified failure was rewritten to %v", got)
		}
	}
}

// Exactly one PUT verdict earns a second authorization inside a run: a store refusal on a ticket
// whose expiry has passed. A refusal on a live ticket must not, or a wrong key would cost two.
func TestOnlyAnExpiredTicketAsksForAnotherAuthorization(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	p := &vendPort{now: func() time.Time { return now }}

	expired := upload.Ticket{ExpiresAt: now.Add(-time.Second)}
	live := upload.Ticket{ExpiresAt: now.Add(time.Minute)}
	refusal := &upload.StatusError{Status: 403, Reason: "Request has expired"}

	if got := p.classifyPut(context.Background(), refusal, expired); !errors.Is(got, engine.ErrTicketExpired) {
		t.Errorf("a refusal on an expired ticket was classified as %v", got)
	}
	if got := p.classifyPut(context.Background(), refusal, live); errors.Is(got, engine.ErrTicketExpired) {
		t.Error("a refusal on a live ticket asked for a second authorization")
	}
	notFound := &upload.StatusError{Status: 404}
	if got := p.classifyPut(context.Background(), notFound, expired); errors.Is(got, engine.ErrTicketExpired) {
		t.Error("a 404 on an expired ticket was read as an expiry")
	}
	transport := errors.New("connection reset")
	if got := p.classifyPut(context.Background(), transport, expired); got != transport {
		t.Errorf("a transport failure was rewritten to %v", got)
	}
}

func preparedObject(id string) engine.PreparedObject {
	return engine.PreparedObject{
		ObjectID: id, Key: "k" + id, Body: []byte("sealed " + id),
		SourceHash: strings.Repeat("a", 64),
		Metadata:   map[string]string{"source-id": "claude-code-transcripts"},
	}
}

// portAgainst is one vend port whose control plane and store are both fn, so the test sees every
// request the port makes.
func portAgainst(t *testing.T, fn http.HandlerFunc) *vendPort {
	t.Helper()
	srv := httptest.NewServer(fn)
	t.Cleanup(srv.Close)
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := controlplane.New(controlplane.Options{
		Endpoint:  srv.URL,
		InstallID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301", Organization: "acme",
		DeviceKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := upload.NewUploadTarget(upload.TargetSpec{
		Origin: srv.URL, Addressing: upload.PathStyle, PathPrefix: "/b", AllowLoopbackHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &vendPort{client: client, uploader: upload.New(), targets: upload.UploadTargetList{target},
		writerID: "writer", now: time.Now}
}

// An object the archive already holds is finished at the answer: no ticket validation and no
// PUT, while the absent one in the same batch is still sent.
func TestAnAlreadyPresentAnswerSkipsThePutAndTheRestStillShips(t *testing.T) {
	var puts []string
	p := portAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts = append(puts, r.URL.Path)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tickets":[`+
			`{"ticket_id":"b1bd1a73-f16d-4a51-aac6-29f1f48b0658","object_id":"0","already_present":true},`+
			`{"ticket_id":"b1bd1a73-f16d-4a51-aac6-29f1f48b0659","object_id":"1","method":"PUT",`+
			`"url":"http://%s/b/k1","expires_at":"2099-01-01T00:00:00Z",`+
			`"required_headers":{"x-amz-meta-source-hash":"%s","x-amz-meta-ticket-id":"b1bd1a73-f16d-4a51-aac6-29f1f48b0659",`+
			`"x-amz-meta-source-id":"claude-code-transcripts"},`+
			`"content_length":8,"content_length_signed":true}]}`, r.Host, strings.Repeat("a", 64))
	})

	out := p.AuthorizeAndUpload(context.Background(), []engine.PreparedObject{preparedObject("0"), preparedObject("1")})
	if len(out) != 2 || !errors.Is(out[0], engine.ErrAlreadyPresent) || out[1] != nil {
		t.Fatalf("the batch did not succeed whole: %v", out)
	}
	if len(puts) != 1 || puts[0] != "/b/k1" {
		t.Errorf("the port PUT %v, want the absent object alone", puts)
	}
}

// The request metadata set is closed: a name outside it fails the whole batch, because dropping
// it would ship an object whose plaintext metadata disagrees with the manifest sealed inside.
func TestUploadMetadataRefusesAnythingOutsideTheClosedSet(t *testing.T) {
	md, _, err := uploadMetadata(map[string]string{
		"manifest-version": "1",
		"source-id":        "claude-code-transcripts",
		"shipped-hash":     "abc",
		"artifact-class":   "trajectory",
		"derived":          "true",
	})
	if err != nil {
		t.Fatalf("the manifest's own metadata was refused: %v", err)
	}
	if md.ManifestVersion != "1" || md.SourceID != "claude-code-transcripts" || md.Derived != "true" {
		t.Errorf("metadata did not map across: %+v", md)
	}

	// Both are named specifically: "unknown name" would send an operator hunting a typo that is
	// not there.
	for _, name := range []string{"source-hash", "ticket-id"} {
		if _, _, err := uploadMetadata(map[string]string{name: "x"}); err == nil {
			t.Errorf("%s was accepted as client-declarable metadata", name)
		}
	}
	if _, _, err := uploadMetadata(map[string]string{"native-path": "/home/dev/x.jsonl"}); err == nil {
		t.Error("a metadata name outside the closed set was accepted")
	}
}

// agent-version is read out of a transcript, so out of the server's grammar it would fail the
// whole batch for as long as that file exists: one poisoned file killing a source. Dropped loudly.
func TestAnOutOfGrammarAgentVersionIsDroppedRatherThanShipped(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"a control character", "1.0\n0"},
		{"non-ASCII", "1.0.0é"},
		{"over 128 bytes", strings.Repeat("9", 129)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md, dropped, err := uploadMetadata(map[string]string{"agent-version": tc.value})
			if err != nil {
				t.Fatalf("one bad agent-version failed the whole batch: %v", err)
			}
			if md.AgentVersion != "" {
				t.Errorf("an out-of-grammar agent-version was sent: %q", md.AgentVersion)
			}
			if len(dropped) != 1 || !strings.Contains(dropped[0], "agent-version") {
				t.Errorf("the drop was not reported: %v", dropped)
			}
		})
	}

	// The values a real agent writes still travel: this drops the ungrammatical, not the unfamiliar.
	for _, ok := range []string{"1.0.0", "0.2.145-beta+build.7", strings.Repeat("9", 128)} {
		md, dropped, err := uploadMetadata(map[string]string{"agent-version": ok})
		if err != nil || md.AgentVersion != ok || len(dropped) != 0 {
			t.Errorf("agent-version %q was dropped: %+v %v %v", ok, md, dropped, err)
		}
	}
}

// An authorization that never reached the control plane is the one verdict the judge files as the
// machine offline; it must never read as a refusal, which kills the install.
func TestAnUnreachableControlPlaneIsClassifiedOffline(t *testing.T) {
	unreachable := fmt.Errorf("backend: /v2/uploads/authorize: %w",
		&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED})
	got := classifyAuthorize(context.Background(), unreachable)
	if !errors.Is(got, engine.ErrOffline) || !errors.Is(got, syscall.ECONNREFUSED) {
		t.Fatalf("%v was not classified offline with its cause kept: %v", unreachable, got)
	}
	if errors.Is(got, formats.ErrCredentialsRefused) || errors.Is(got, engine.ErrUploadUnavailable) {
		t.Fatal("offline also reads as a refusal or an unavailable control plane")
	}
}

// A dial that ran out of the caller's deadline looks exactly like one that ran out of the
// dialer's; only the live context tells them apart, so an expired caller is never offline.
func TestADialCutShortByTheCallerIsNotOffline(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := (&net.Dialer{}).DialContext(expired, "tcp", "127.0.0.1:1")
	if err == nil {
		t.Fatal("the probe produced no error")
	}
	if got := classifyAuthorize(expired, err); errors.Is(got, engine.ErrOffline) {
		t.Fatalf("a caller-owned deadline was classified offline: %v", got)
	}
	if got := classifyAuthorize(context.Background(), err); !errors.Is(got, engine.ErrOffline) {
		t.Fatalf("the same dial error with a live caller is not offline: %v", got)
	}
}

// ticketsFor answers an authorization with one live PUT ticket per object id, against host.
func ticketsFor(w http.ResponseWriter, host string, ids ...string) {
	w.Header().Set("Content-Type", "application/json")
	var tickets []string
	for i, id := range ids {
		ticketID := fmt.Sprintf("b1bd1a73-f16d-4a51-aac6-29f1f48b%04d", i)
		tickets = append(tickets, fmt.Sprintf(`{"ticket_id":"%s","object_id":"%s","method":"PUT",`+
			`"url":"http://%s/b/k%s","expires_at":"2099-01-01T00:00:00Z",`+
			`"required_headers":{"x-amz-meta-source-hash":"%s","x-amz-meta-ticket-id":"%s",`+
			`"x-amz-meta-source-id":"claude-code-transcripts"},`+
			`"content_length":%d,"content_length_signed":true}`,
			ticketID, id, host, id, strings.Repeat("a", 64), ticketID, len(preparedObject(id).Body)))
	}
	fmt.Fprintf(w, `{"tickets":[%s]}`, strings.Join(tickets, ","))
}

// hangUp reads the whole request and closes the connection without answering.
func hangUp(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		conn.Close()
	}
}

// retryingPort counts what reaches the far end; authorize answers the nth request (from 1), a PUT
// the nth attempt at its own object, so concurrent PUTs cannot trade places.
type retryingPort struct {
	mu                sync.Mutex
	authorizes, puts  int
	attempts          map[string]int
	authorize, putter func(n int, w http.ResponseWriter, r *http.Request)
}

func (rp *retryingPort) port(t *testing.T) *vendPort {
	return portAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		rp.mu.Lock()
		var n int
		handle := rp.authorize
		if r.Method == http.MethodPut {
			rp.puts++
			if rp.attempts == nil {
				rp.attempts = map[string]int{}
			}
			rp.attempts[r.URL.Path]++
			n, handle = rp.attempts[r.URL.Path], rp.putter
		} else {
			rp.authorizes++
			n = rp.authorizes
		}
		rp.mu.Unlock()
		handle(n, w, r)
	})
}

func (rp *retryingPort) counts() (int, int) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return rp.authorizes, rp.puts
}

func healthyAuthorize(_ int, w http.ResponseWriter, r *http.Request) { ticketsFor(w, r.Host, "0", "1") }

func healthyPut(_ int, w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusOK)
}

var twoObjects = []engine.PreparedObject{preparedObject("0"), preparedObject("1")}

// An authorization that got no answer is sent once more; a second silence stops the run offline.
func TestAnUnansweredAuthorizationIsRetriedOnceThenOffline(t *testing.T) {
	rp := &retryingPort{authorize: func(_ int, w http.ResponseWriter, r *http.Request) { hangUp(w, r) }, putter: healthyPut}
	out := rp.port(t).AuthorizeAndUpload(context.Background(), twoObjects)
	for i, err := range out {
		if !errors.Is(err, engine.ErrOffline) || !errors.Is(err, io.EOF) {
			t.Errorf("object %d: want offline with its cause kept, got %v", i, err)
		}
	}
	if a, p := rp.counts(); a != 2 || p != 0 {
		t.Errorf("sent %d authorizations and %d PUTs, want 2 and 0", a, p)
	}
}

func TestAnAuthorizationAnsweredOnTheRetryShips(t *testing.T) {
	var issued []time.Time
	rp := &retryingPort{putter: healthyPut, authorize: func(n int, w http.ResponseWriter, r *http.Request) {
		var req struct {
			IssuedAt time.Time `json:"issued_at"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		issued = append(issued, req.IssuedAt)
		if n == 1 {
			hangUp(w, r)
			return
		}
		healthyAuthorize(n, w, r)
	}}
	p := rp.port(t)
	clock := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	p.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	out := p.AuthorizeAndUpload(context.Background(), twoObjects)
	if out[0] != nil || out[1] != nil {
		t.Fatalf("the retry did not ship the batch: %v", out)
	}
	if a, p := rp.counts(); a != 2 || p != 2 {
		t.Errorf("sent %d authorizations and %d PUTs, want 2 and 2", a, p)
	}
	if len(issued) != 2 || !issued[1].After(issued[0]) {
		t.Errorf("the retried authorization was not restamped: issued_at %v", issued)
	}
}

// The production pause: about one second, once.
func TestTheRetryPauseIsOneSecond(t *testing.T) {
	p, err := newUploadPort(nil, &config.Effective{})
	if err != nil {
		t.Fatal(err)
	}
	if p.retryPause != time.Second {
		t.Errorf("retryPause = %s, want 1s", p.retryPause)
	}
}

// cutAnswer sends a status line and part of the body it promises, then closes the connection.
func cutAnswer(status int) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			fmt.Fprintf(conn, "HTTP/1.1 %d X\r\nContent-Length: 100\r\n\r\nupstream", status)
			conn.Close()
		}
	}
}

// A status line that arrived is the answer even when its body is cut short: classified by the
// status, never retried, never offline.
func TestAnAnsweredAuthorizationWithACutBodyIsClassifiedByItsStatus(t *testing.T) {
	for status, want := range map[int]error{
		http.StatusServiceUnavailable: engine.ErrUploadUnavailable,
		http.StatusUnauthorized:       formats.ErrCredentialsRefused,
	} {
		rp := &retryingPort{authorize: cutAnswer(status), putter: healthyPut}
		out := rp.port(t).AuthorizeAndUpload(context.Background(), twoObjects)
		if !errors.Is(out[0], want) || errors.Is(out[0], engine.ErrOffline) {
			t.Errorf("HTTP %d with a cut body was classified as %v", status, out[0])
		}
		if a, _ := rp.counts(); a != 1 {
			t.Errorf("HTTP %d with a cut body: sent %d authorizations, want 1", status, a)
		}
	}
}

// The heartbeat's context asks for one attempt: no answer is still offline, but is not resent.
func TestASingleAttemptContextIsNotRetried(t *testing.T) {
	rp := &retryingPort{authorize: func(_ int, w http.ResponseWriter, r *http.Request) { hangUp(w, r) }, putter: healthyPut}
	out := rp.port(t).AuthorizeAndUpload(context.WithValue(context.Background(), singleAttempt{}, true), twoObjects)
	if !errors.Is(out[0], engine.ErrOffline) {
		t.Errorf("want offline, got %v", out[0])
	}
	if a, _ := rp.counts(); a != 1 {
		t.Errorf("sent %d authorizations on a single-attempt context, want 1", a)
	}
}

func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// A refused connection, to the control plane or to the store, gets the same one retry after the
// pause: the pause runs only when a retry follows.
func TestARefusedConnectionIsRetriedAfterThePause(t *testing.T) {
	const pause = 300 * time.Millisecond
	closed := closedAddr(t)

	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cp := portAgainst(t, http.NotFound)
	client, err := controlplane.New(controlplane.Options{Endpoint: "http://" + closed,
		InstallID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301", Organization: "acme", DeviceKey: key})
	if err != nil {
		t.Fatal(err)
	}
	cp.client, cp.retryPause = client, pause

	rp := &retryingPort{authorize: func(_ int, w http.ResponseWriter, r *http.Request) { ticketsFor(w, closed, "0", "1") }, putter: healthyPut}
	store := rp.port(t)
	target, err := upload.NewUploadTarget(upload.TargetSpec{Origin: "http://" + closed, Addressing: upload.PathStyle,
		PathPrefix: "/b", AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	store.targets, store.retryPause = upload.UploadTargetList{target}, pause

	for name, p := range map[string]*vendPort{"authorize": cp, "PUT": store} {
		started := time.Now()
		out := p.AuthorizeAndUpload(context.Background(), twoObjects)
		var op *net.OpError
		if !errors.Is(out[0], engine.ErrOffline) || !errors.As(out[0], &op) || op.Op != "dial" {
			t.Errorf("%s: want offline with the refusal kept, got %v", name, out[0])
		}
		if elapsed := time.Since(started); elapsed < pause {
			t.Errorf("%s: a refused connection was not retried after the pause (%s)", name, elapsed)
		}
	}
}

// Once a PUT got no answer twice, the PUTs still queued behind the group's slots are not sent:
// one retried round, not one per wave.
func TestQueuedPutsAreNotSentAfterASiblingWentOffline(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	ids := []string{"0", "1", "2", "3", "4", "5", "6", "7"}
	batch := make([]engine.PreparedObject, len(ids))
	for i, id := range ids {
		batch[i] = preparedObject(id)
	}
	rp := &retryingPort{
		authorize: func(_ int, w http.ResponseWriter, r *http.Request) { ticketsFor(w, r.Host, ids...) },
		putter:    func(_ int, w http.ResponseWriter, r *http.Request) { hangUp(w, r) },
	}
	out := rp.port(t).AuthorizeAndUpload(context.Background(), batch)
	for i, err := range out {
		if !errors.Is(err, engine.ErrOffline) || !errors.Is(err, io.EOF) {
			t.Errorf("object %d: want offline with a cause kept, got %v", i, err)
		}
	}
	if _, puts := rp.counts(); puts != 8 {
		t.Errorf("sent %d PUTs, want 8: the first wave of 4 and their retries", puts)
	}
}

// Any HTTP answer is final for this run: a 503 is not retried, and stays unavailable, not offline.
func TestAnAnsweredAuthorizationIsNotRetried(t *testing.T) {
	rp := &retryingPort{putter: healthyPut, authorize: func(_ int, w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}}
	out := rp.port(t).AuthorizeAndUpload(context.Background(), twoObjects)
	if !errors.Is(out[0], engine.ErrUploadUnavailable) || errors.Is(out[0], engine.ErrOffline) {
		t.Errorf("a 503 was classified as %v", out[0])
	}
	if a, _ := rp.counts(); a != 1 {
		t.Errorf("sent %d authorizations for one 503, want 1", a)
	}
}

// Shutdown during the pause ends the batch at once, unretried and not offline.
func TestShutdownDuringThePauseSkipsTheRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rp := &retryingPort{putter: healthyPut, authorize: func(_ int, w http.ResponseWriter, r *http.Request) {
		hangUp(w, r)
		time.AfterFunc(50*time.Millisecond, cancel)
	}}
	p := rp.port(t)
	p.retryPause = time.Minute
	started := time.Now()
	out := p.AuthorizeAndUpload(ctx, twoObjects)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("the batch waited out the pause after shutdown: %s", elapsed)
	}
	if errors.Is(out[0], engine.ErrOffline) {
		t.Errorf("a batch cut short by shutdown was classified offline: %v", out[0])
	}
	if a, _ := rp.counts(); a != 1 {
		t.Errorf("sent %d authorizations after shutdown, want 1", a)
	}

	// A caller already gone sends nothing at all.
	rp2 := &retryingPort{authorize: healthyAuthorize, putter: healthyPut}
	if out := rp2.port(t).AuthorizeAndUpload(ctx, twoObjects); errors.Is(out[0], engine.ErrOffline) {
		t.Errorf("a cancelled caller was classified offline: %v", out[0])
	}
	if a, p := rp2.counts(); a != 0 || p != 0 {
		t.Errorf("a cancelled caller sent %d authorizations and %d PUTs", a, p)
	}
}

// A PUT whose body was sent but got no answer goes once more on the same ticket: the key is
// derived from the content, so the retry rewrites the same object onto a versioned bucket.
func TestAnUnansweredPutIsRetriedOnceThenOffline(t *testing.T) {
	rp := &retryingPort{authorize: healthyAuthorize, putter: func(n int, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/b/k0" {
			hangUp(w, r)
			return
		}
		healthyPut(n, w, r)
	}}
	out := rp.port(t).AuthorizeAndUpload(context.Background(), twoObjects)
	if !errors.Is(out[0], engine.ErrOffline) || out[1] != nil {
		t.Fatalf("want the silent PUT offline and its sibling shipped, got %v", out)
	}
	if a, p := rp.counts(); a != 1 || p != 3 {
		t.Errorf("sent %d authorizations and %d PUTs, want 1 and 3", a, p)
	}
}

func TestAPutAnsweredOnTheRetryShips(t *testing.T) {
	rp := &retryingPort{authorize: healthyAuthorize, putter: func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			hangUp(w, r)
			return
		}
		healthyPut(n, w, r)
	}}
	out := rp.port(t).AuthorizeAndUpload(context.Background(), twoObjects)
	if out[0] != nil || out[1] != nil {
		t.Fatalf("the retries did not ship the batch: %v", out)
	}
	if _, p := rp.counts(); p != 4 {
		t.Errorf("sent %d PUTs, want 4", p)
	}
}

func TestAnAnsweredPutIsNotRetried(t *testing.T) {
	rp := &retryingPort{authorize: healthyAuthorize, putter: func(_ int, w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "<Error><Code>SlowDown</Code></Error>", http.StatusServiceUnavailable)
	}}
	out := rp.port(t).AuthorizeAndUpload(context.Background(), twoObjects)
	var status *upload.StatusError
	if !errors.As(out[0], &status) || errors.Is(out[0], engine.ErrOffline) {
		t.Errorf("a 503 PUT was classified as %v", out[0])
	}
	if _, p := rp.counts(); p != 2 {
		t.Errorf("sent %d PUTs for two 503s, want 2", p)
	}
}

// A retry that outlives its ticket meets the store's refusal, which is the existing one
// reauthorization, not offline and not a second retry.
func TestARetryOnAnExpiredTicketAsksForAnotherAuthorization(t *testing.T) {
	rp := &retryingPort{authorize: healthyAuthorize, putter: func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			hangUp(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "<Error><Code>AccessDenied</Code><Message>Request has expired</Message></Error>", http.StatusForbidden)
	}}
	p := rp.port(t)
	p.now = func() time.Time { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) }
	out := p.AuthorizeAndUpload(context.Background(), twoObjects)
	for i, err := range out {
		if !errors.Is(err, engine.ErrTicketExpired) || errors.Is(err, engine.ErrOffline) {
			t.Errorf("object %d: want an expired ticket, got %v", i, err)
		}
	}
	if _, puts := rp.counts(); puts != 4 {
		t.Errorf("sent %d PUTs, want 4", puts)
	}
}
