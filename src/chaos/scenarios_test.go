//go:build chaos

// Each scenario reproduces one field fault: warm up clean, stage new transcripts, arm the fault, run
// one tick and read what the install recorded, then lift it and prove a clean tick ships everything.
// The expectations pin today's behaviour; a hardening change flips them on purpose.
package chaos

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

const smallTranscript = 6 << 10

type expectation struct {
	// The newest event after the fault tick, "" for none, and the streak it leaves.
	kind        string
	consecutive int
	contains    []string

	// Whether the fault tick's own install_health reached the control plane.
	faultTelemetry bool

	// Only where the question is whether the tick gets stuck: a loose ceiling, never a timing claim.
	maxTick time.Duration
}

type scenario struct {
	slug  string
	title string
	field string
	files int
	size  int
	arm   func(t *testing.T, s *stage)
	want  expectation

	// Anything the record cannot say; the note lands in the matrix.
	check func(t *testing.T, s *stage) string
}

var nxdomain = dnsFault{Rcode: "NXDOMAIN"}

var scenarios = []scenario{
	{
		slug: "put-reset", field: "F1", files: 3,
		title: "1 store resets the PUT after the body, before the response",
		arm: func(t *testing.T, s *stage) {
			s.armResets()
			s.in.setStore(storeFault{resetAfterBody: true})
		},
		want: expectation{kind: formats.FailureTick, consecutive: 1, faultTelemetry: true,
			contains: []string{"the run shipped nothing: all 3 attempted uploads failed", "upload: PUT object", "read: connection reset by peer"}},
	},
	{
		slug: "put-reset-mid-body", field: "F1", files: 1, size: 64 << 20,
		// Past every buffer on the path, so the reset lands while the client writes (2.7 MB sealed raced).
		title: "2 store resets the PUT mid-body",
		arm: func(t *testing.T, s *stage) {
			s.armResets()
			s.in.setStore(storeFault{resetMidBody: 64 << 10})
		},
		want: expectation{kind: formats.FailureTick, consecutive: 1, faultTelemetry: true,
			contains: []string{"the run shipped nothing: all 1 attempted uploads failed", "upload: PUT object", "write: connection reset by peer"}},
		check: func(t *testing.T, s *stage) string {
			return fmt.Sprintf("sealed object %d bytes; the store reads 64 KiB of it, then resets", s.in.largestObject())
		},
	},
	{
		slug: "tls-stall", field: "F4", files: 3,
		title: "3 TLS handshake stalls on the control-plane link",
		arm: func(t *testing.T, s *stage) {
			if _, err := s.cp.AddToxic("stall", "timeout", "downstream", 1, toxiAttrs{"timeout": 0}); err != nil {
				t.Fatal(err)
			}
		},
		want: expectation{kind: formats.FailureTick, consecutive: 1, maxTick: 2 * time.Minute,
			contains: []string{"TLS handshake timeout"}},
	},
	{
		// Two groups of four in one wave; more files form a second wave whose size depends on scheduling.
		slug: "header-stall", field: "F5", files: 8,
		title: "4 authorize accepts the request and never answers",
		arm: func(t *testing.T, s *stage) {
			s.in.setAuthorize(authorizeFault{stall: true})
		},
		want: expectation{kind: formats.FailureTick, consecutive: 1, faultTelemetry: true, maxTick: 4 * time.Minute,
			contains: []string{"Client.Timeout exceeded while awaiting headers"}},
		check: func(t *testing.T, s *stage) string {
			mirror, heartbeat, shortest, longest := countStalls(s.in.stalled())
			if mirror+heartbeat > 0 && shortest < 25*time.Second {
				t.Errorf("an authorize stall ended after %s: the client gave up before its 30s timeout", shortest)
			}
			if mirror != 2 || heartbeat != 1 {
				t.Errorf("authorize stalled for %d mirror groups and %d heartbeats, want 2 and 1", mirror, heartbeat)
			}
			return fmt.Sprintf("authorize stalled for %d mirror group(s) and %d heartbeat(s), each waited %s-%s",
				mirror, heartbeat, shortest.Round(time.Second), longest.Round(time.Second))
		},
	},
	{
		slug: "cp-blackhole", field: "F5", files: 3,
		title: "5 control plane blackholed: connect never completes",
		arm: func(t *testing.T, s *stage) {
			setDNS(t, s.cpHost, dnsFault{Address: blackholeIP})
		},
		want: expectation{kind: formats.FailureTick, consecutive: 1, maxTick: 4 * time.Minute,
			contains: []string{"backend: /v2/uploads/authorize", "(Client.Timeout exceeded while awaiting headers)"}},
		// The 30s dial and client deadlines expire together, so net/http picks one of two exact spellings.
		check: func(t *testing.T, s *stage) string {
			msg := s.faultMessage
			dial := "dial tcp " + blackholeIP + ":"
			switch {
			case strings.Contains(msg, dial) && strings.HasSuffix(msg, ": i/o timeout (Client.Timeout exceeded while awaiting headers)"):
				return "worded as the dial timeout"
			case strings.HasSuffix(msg, "/v2/uploads/authorize\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)"):
				return "worded as the client deadline, identical to F5"
			}
			t.Errorf("blackhole message is neither the dial timeout to %s nor the client deadline:\n%s", blackholeIP, msg)
			return "unexpected wording"
		},
	},
	// DNS wording below is Linux pure-Go ("lookup <host> on <server>"); field F2 has no server, so a system resolver (macOS, Windows).
	{
		slug: "cp-nxdomain", field: "F2", files: 3,
		title: "6 control-plane name is NXDOMAIN",
		arm: func(t *testing.T, s *stage) {
			setDNS(t, s.cpHost, nxdomain)
		},
		want: expectation{kind: formats.FailureOffline, consecutive: 0,
			contains: []string{"engine: the control plane could not be reached", "no such host"}},
	},
	{
		slug: "store-nxdomain", field: "F2", files: 3,
		title: "7a control-plane name resolves, store name is NXDOMAIN",
		arm: func(t *testing.T, s *stage) {
			setDNS(t, s.store, nxdomain)
		},
		want: expectation{kind: formats.FailureTick, consecutive: 1, faultTelemetry: true,
			contains: []string{"the run shipped nothing: all 3 attempted uploads failed", "upload: PUT object", "no such host"}},
	},
	{
		slug: "dns-flap", field: "F2", files: 8,
		title: "7b both names stop resolving after the config fetch",
		arm: func(t *testing.T, s *stage) {
			// The first group reuses the config fetch's connection and closes it, so the second must resolve.
			s.in.setAuthorize(authorizeFault{closeConn: true})
			s.in.setOnConfig(func() {
				setDNS(t, s.cpHost, nxdomain)
				setDNS(t, s.store, nxdomain)
			})
		},
		want: expectation{kind: formats.FailureTick, consecutive: 1,
			contains: []string{"engine: the control plane could not be reached", "no such host"}},
	},
	{
		slug: "cp-servfail", field: "F2", files: 3,
		title: "8a control-plane name answers SERVFAIL",
		arm: func(t *testing.T, s *stage) {
			setDNS(t, s.cpHost, dnsFault{Rcode: "SERVFAIL"})
		},
		want: expectation{kind: formats.FailureOffline, consecutive: 0,
			contains: []string{"engine: the control plane could not be reached", "server misbehaving"}},
	},
	{
		slug: "cp-dns-timeout", field: "F2", files: 3,
		title: "8b control-plane name gets no DNS answer",
		arm: func(t *testing.T, s *stage) {
			setDNS(t, s.cpHost, dnsFault{Drop: true})
		},
		want: expectation{kind: formats.FailureOffline, consecutive: 0, maxTick: 2 * time.Minute,
			contains: []string{"engine: the control plane could not be reached", "i/o timeout"}},
	},
	{
		slug: "cp-refused", field: "-", files: 3,
		title: "9 control-plane connection refused",
		arm: func(t *testing.T, s *stage) {
			if err := s.cp.Disable(); err != nil {
				t.Fatal(err)
			}
		},
		want: expectation{kind: formats.FailureOffline, consecutive: 0,
			contains: []string{"engine: the control plane could not be reached", "connection refused"}},
	},
	{
		slug: "store-slowdown", field: "-", files: 3,
		title: "10a store answers 503 SlowDown",
		arm: func(t *testing.T, s *stage) {
			s.in.setStore(storeFault{status: 503, body: "<Error><Code>SlowDown</Code><Message>Please reduce your request rate.</Message></Error>"})
		},
		want: expectation{kind: formats.FailureTick, consecutive: 1, faultTelemetry: true,
			contains: []string{"the run shipped nothing: all 3 attempted uploads failed", "HTTP 503", "SlowDown"}},
	},
	{
		slug: "authorize-503", field: "-", files: 3,
		title: "10b authorize answers 503",
		arm: func(t *testing.T, s *stage) {
			s.in.setAuthorize(authorizeFault{status: 503, body: "upstream unavailable"})
		},
		want: expectation{kind: formats.FailureTick, consecutive: 1, faultTelemetry: true,
			contains: []string{"upload authorization is unavailable", "HTTP 503"}},
	},
	{
		slug: "authorize-429", field: "-", files: 3,
		title: "10c authorize answers 429",
		arm: func(t *testing.T, s *stage) {
			s.in.setAuthorize(authorizeFault{status: 429, body: "slow down"})
		},
		want: expectation{kind: formats.FailureTick, consecutive: 1, faultTelemetry: true,
			contains: []string{"upload authorization is unavailable", "HTTP 429"}},
	},
	{
		slug: "authorize-truncated", field: "-", files: 3,
		title: "11 authorize response body truncated",
		arm: func(t *testing.T, s *stage) {
			s.in.setAuthorize(authorizeFault{truncate: true})
		},
		want: expectation{kind: formats.FailureTick, consecutive: 1, faultTelemetry: true,
			contains: []string{"unexpected EOF"}},
	},
	{
		slug: "link-cut", field: "-", files: 4,
		title: "12 link cut while every PUT is in flight",
		arm: func(t *testing.T, s *stage) {
			hold := &putHold{arrived: make(chan struct{}, 16), release: make(chan struct{})}
			s.in.setStore(storeFault{hold: hold})
			stop, result := make(chan struct{}), make(chan error, 1)
			go func() {
				defer close(hold.release)
				for n := range 4 {
					select {
					case <-hold.arrived:
					case <-stop:
						result <- fmt.Errorf("link-cut: only %d of 4 PUTs reached the store during the fault tick", n)
						return
					}
				}
				result <- s.cutLinks()
			}()
			s.afterFaultTick = func() error { close(stop); return <-result }
		},
		want: expectation{kind: formats.FailureTick, consecutive: 1, maxTick: 4 * time.Minute,
			contains: []string{"the run shipped nothing: all 4 attempted uploads failed", "upload: PUT object", "timeout awaiting response headers"}},
	},
	{
		slug: "partial", field: "F1 (partial)", files: 4,
		title: "13 two PUTs land, two are reset",
		arm: func(t *testing.T, s *stage) {
			s.armResets()
			s.in.setStore(storeFault{resetAfterBody: true, passFirst: 2})
		},
		want: expectation{kind: "", consecutive: 0, faultTelemetry: true},
		check: func(t *testing.T, s *stage) string {
			if want := 3 * s.perFile; s.afterFault != want {
				t.Errorf("the fault tick left %d mirror objects in the store, want %d", s.afterFault, want)
			}
			return "the fault tick shipped 2 of 4 new files and exited 0"
		},
	},
}

func TestChaos(t *testing.T) {
	t.Run("scenario", func(t *testing.T) {
		for i, sc := range scenarios {
			t.Run(sc.slug, func(t *testing.T) {
				t.Parallel()
				runScenario(t, i, sc)
			})
		}
	})
	writeMatrix(t)
}

func runScenario(t *testing.T, order int, sc scenario) {
	s := newStage(t, sc.slug)
	s.stageTranscripts(1, smallTranscript)
	if warm := s.tick("warm-up"); warm.exit != 0 {
		t.Fatalf("the warm-up tick failed with exit %d", warm.exit)
	}
	if rec := s.record(); rec.ConsecutiveFailures != 0 || len(rec.Recent) != 0 {
		t.Fatalf("the warm-up tick left a failure record: %+v", rec)
	}
	s.perFile = s.in.mirrorObjects()

	size := sc.size
	if size == 0 {
		size = smallTranscript
	}
	s.stageTranscripts(sc.files, size)
	sc.arm(t, s)
	fault := s.tick("fault")
	if s.afterFaultTick != nil {
		if err := s.afterFaultTick(); err != nil {
			t.Errorf("%v", err)
		}
	}
	s.afterFault = s.in.mirrorObjects()
	rec := s.record()
	latest := rec.Latest()
	if latest != nil {
		s.faultMessage = latest.Message
	}
	s.lift()
	recovery := s.tick("recovery")
	faultHealth := s.in.telemetryBetween(fault.started, recovery.started)
	recoveryHealth := s.in.telemetryBetween(recovery.started, time.Now().Add(time.Hour))

	r := row{
		order: order, Scenario: sc.title, Field: sc.field,
		Consecutive: rec.ConsecutiveFailures, TickSeconds: fault.elapsed.Seconds(), Exit: fault.exit,
		FaultTelemetry: len(faultHealth) > 0,
	}
	if latest != nil {
		r.Kind, r.Message, r.Counted = latest.Kind, latest.Message, latest.Counted()
	}
	if sc.check != nil {
		r.Notes = sc.check(t, s)
	}
	if len(recoveryHealth) > 0 {
		r.RecoveryHealth = recoveryHealth[len(recoveryHealth)-1]
	}
	if len(faultHealth) > 0 {
		r.FaultHealth = faultHealth[len(faultHealth)-1]
	}
	recordRow(r)

	want := sc.want
	if r.Kind != want.kind {
		t.Errorf("newest event kind = %q, want %q (message %q)", r.Kind, want.kind, r.Message)
	}
	if rec.ConsecutiveFailures != want.consecutive {
		t.Errorf("consecutive_failures = %d, want %d", rec.ConsecutiveFailures, want.consecutive)
	}
	for _, sub := range want.contains {
		if !strings.Contains(r.Message, sub) {
			t.Errorf("newest event message lacks %q:\n%s", sub, r.Message)
		}
	}
	if want.maxTick > 0 && fault.elapsed > want.maxTick {
		t.Errorf("the fault tick took %s, past the %s that would mean it is stuck", fault.elapsed, want.maxTick)
	}
	// Today a tick exits 1 exactly when it records an event, offline included.
	wantExit := 1
	if want.kind == "" {
		wantExit = 0
	}
	if fault.exit != wantExit {
		t.Errorf("the fault tick exited %d, want %d", fault.exit, wantExit)
	}
	if r.FaultTelemetry != want.faultTelemetry {
		t.Errorf("fault-tick install_health delivered = %v, want %v", r.FaultTelemetry, want.faultTelemetry)
	}
	if r.FaultTelemetry {
		checkHealth(t, "fault", r.FaultHealth, rec)
	}

	if recovery.exit != 0 {
		t.Errorf("the recovery tick failed with exit %d", recovery.exit)
	}
	if got, want := s.in.mirrorObjects(), s.perFile*(1+sc.files); got != want {
		t.Errorf("after recovery the store holds %d mirror objects, want %d", got, want)
	}
	after := s.record()
	if after.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures after recovery = %d, want 0", after.ConsecutiveFailures)
	}
	if len(recoveryHealth) != 1 {
		t.Fatalf("the recovery tick delivered %d install_health events, want 1", len(recoveryHealth))
	}
	checkHealth(t, "recovery", r.RecoveryHealth, after)
}

type healthEvent struct {
	Event       string `json:"event"`
	Consecutive int    `json:"consecutive_failures"`
	Faults      []struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"faults"`
}

// The event the control plane forwards must say what the record says: same streak, same kinds.
func checkHealth(t *testing.T, label string, raw json.RawMessage, rec formats.FailureRecord) {
	t.Helper()
	var ev healthEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Errorf("%s install_health does not parse: %v\n%s", label, err, raw)
		return
	}
	var kinds, want []string
	for _, f := range ev.Faults {
		kinds = append(kinds, f.Kind)
	}
	for _, f := range rec.Recent {
		want = append(want, f.Kind)
	}
	if ev.Event != "install_health" || ev.Consecutive != rec.ConsecutiveFailures || !slices.Equal(kinds, want) {
		t.Errorf("%s install_health = %s, want consecutive %d and kinds %v", label, raw, rec.ConsecutiveFailures, want)
	}
}

func countStalls(stalls []stalledAuthorize) (mirror, heartbeat int, shortest, longest time.Duration) {
	for i, s := range stalls {
		if s.mirror {
			mirror++
		} else {
			heartbeat++
		}
		if i == 0 || s.waited < shortest {
			shortest = s.waited
		}
		longest = max(longest, s.waited)
	}
	return mirror, heartbeat, shortest, longest
}
