package app

// The classifications the vend port owes the engine: each decides whether a run stops for good,
// stops until the next tick, or asks for one fresh ticket, so each is asserted on its own.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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
	if got := classifyAuthorize(refused); !errors.Is(got, formats.ErrCredentialsRefused) {
		t.Errorf("a refusal was reclassified as %v", got)
	}
	if got := classifyAuthorize(refused); errors.Is(got, engine.ErrUploadUnavailable) {
		t.Error("a refusal also reads as an unavailable control plane")
	}

	unavailable := fmt.Errorf("%w (HTTP 503)", controlplane.ErrAuthorizeUnavailable)
	got := classifyAuthorize(unavailable)
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
		if got := classifyAuthorize(other); got != other {
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

	if got := p.classifyPut(refusal, expired); !errors.Is(got, engine.ErrTicketExpired) {
		t.Errorf("a refusal on an expired ticket was classified as %v", got)
	}
	if got := p.classifyPut(refusal, live); errors.Is(got, engine.ErrTicketExpired) {
		t.Error("a refusal on a live ticket asked for a second authorization")
	}
	notFound := &upload.StatusError{Status: 404}
	if got := p.classifyPut(notFound, expired); errors.Is(got, engine.ErrTicketExpired) {
		t.Error("a 404 on an expired ticket was read as an expiry")
	}
	transport := errors.New("connection reset")
	if got := p.classifyPut(transport, expired); got != transport {
		t.Errorf("a transport failure was rewritten to %v", got)
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
