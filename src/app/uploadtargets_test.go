package app

import (
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/upload"
)

func TestUploadTargetsBuildsTheAllowlist(t *testing.T) {
	eff := &config.Effective{UploadTargets: []config.UploadTarget{
		{Origin: "https://acme.s3.example.com", Addressing: "virtual-hosted"},
		{Origin: "http://127.0.0.1:9000", Addressing: "path-style", PathPrefix: "/acme", AllowLoopbackHTTP: true},
	}}
	list, err := uploadTargets(eff)
	if err != nil {
		t.Fatalf("uploadTargets: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("built %d targets, want 2", len(list))
	}
	if got := list[0].Origin(); got != "https://acme.s3.example.com:443" {
		t.Errorf("first origin is %q", got)
	}
	if _, err := list.Match("https://acme.s3.example.com/organization%3Dacme/object.age"); err != nil {
		t.Errorf("allowlist does not match its own origin: %v", err)
	}
	if _, err := list.Match("https://elsewhere.example.com/object.age"); err == nil {
		t.Error("an unlisted origin matched")
	}
}

func TestUploadTargetsRefusesTheWholeListOnOneBadEntry(t *testing.T) {
	eff := &config.Effective{UploadTargets: []config.UploadTarget{
		{Origin: "https://acme.s3.example.com", Addressing: "virtual-hosted"},
		{Origin: "https://acme.s3.example.com", Addressing: "bucket-in-the-query"},
	}}
	list, err := uploadTargets(eff)
	if err == nil {
		t.Fatalf("bad addressing accepted: %+v", list)
	}
	if !strings.Contains(err.Error(), "entry 1") {
		t.Errorf("refusal does not name the offending entry: %v", err)
	}
	if list != nil {
		t.Errorf("a refused list still returned %d targets", len(list))
	}
}

// The bridge is only correct if a ticket that survives it also survives validation, so this runs
// the real validator rather than comparing maps.
func TestToUploadTicketFeedsValidation(t *testing.T) {
	target, err := upload.NewUploadTarget(upload.TargetSpec{
		Origin:     "https://acme.s3.example.com",
		Addressing: upload.VirtualHosted,
	})
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	prepared := upload.PreparedUpload{
		ObjectID:   "01J0000000000000000000000A",
		Key:        "organization=acme/source=claude-code/object.age",
		Body:       []byte("sealed"),
		SourceHash: "sha256:abc",
		Metadata: map[string]string{
			"manifest-version": "3",
			"artifact-class":   "trajectory",
			"kind":             "mirror",
		},
	}
	issued := controlplane.Ticket{
		TicketID:  "ticket-1",
		ObjectID:  prepared.ObjectID,
		Method:    "PUT",
		URL:       "https://acme.s3.example.com/organization%3Dacme/source%3Dclaude-code/object.age?X-Amz-Signature=deadbeef",
		ExpiresAt: time.Unix(1750000000, 0).UTC(),
		RequiredHeaders: controlplane.TicketHeaders{
			"x-amz-meta-source-hash":      prepared.SourceHash,
			"x-amz-meta-ticket-id":        "ticket-1",
			"x-amz-meta-manifest-version": "3",
			"x-amz-meta-artifact-class":   "trajectory",
			"x-amz-meta-kind":             "mirror",
			"x-amz-tagging":               "class=trajectory",
		},
		ContentLength:       int64(len(prepared.Body)),
		ContentLengthSigned: true,
	}

	ticket := toUploadTicket(issued)
	if err := upload.ValidateTicket(target, prepared, ticket); err != nil {
		t.Fatalf("bridged ticket failed validation: %v", err)
	}
	if _, ok := ticket.RequiredHeaders["x-amz-meta-source-id"]; ok {
		t.Error("an unset optional header reached the map")
	}
	if got := ticket.RequiredHeaders["x-amz-tagging"]; got != "class=trajectory" {
		t.Errorf("tagging header is %q", got)
	}
	if !ticket.ContentLengthSigned || ticket.ExpiresAt != issued.ExpiresAt {
		t.Errorf("bridge dropped a field: %+v", ticket)
	}
}

// A field the bridge forgot must fail loudly, not upload a half-derived object.
func TestToUploadTicketDroppedFieldIsRefused(t *testing.T) {
	target, err := upload.NewUploadTarget(upload.TargetSpec{
		Origin:     "https://acme.s3.example.com",
		Addressing: upload.VirtualHosted,
	})
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	prepared := upload.PreparedUpload{
		ObjectID:   "01J0000000000000000000000A",
		Key:        "object.age",
		Body:       []byte("sealed"),
		SourceHash: "sha256:abc",
		Metadata:   map[string]string{"source-id": "laptop"},
	}
	ticket := toUploadTicket(controlplane.Ticket{
		TicketID: "ticket-1",
		ObjectID: prepared.ObjectID,
		Method:   "PUT",
		URL:      "https://acme.s3.example.com/object.age",
		RequiredHeaders: controlplane.TicketHeaders{
			"x-amz-meta-source-hash": prepared.SourceHash,
			"x-amz-meta-ticket-id":   "ticket-1",
		},
		ContentLength: int64(len(prepared.Body)),
	})
	if err := upload.ValidateTicket(target, prepared, ticket); err == nil {
		t.Fatal("a ticket missing the declared source-id header validated")
	}
}
