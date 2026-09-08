package controlplane

// V2 upload authorization: POST /v2/uploads/authorize exchanges a bounded batch of prepared object
// descriptors for one short-lived PUT ticket each. Authorization, not acknowledgment: the local
// fingerprint document stays the only upload-progress authority, and no ticket URL is ever logged.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

const uploadAuthorizePath = "/v2/uploads/authorize"

// uploadAuthorizePreamble domain-separates the v2 signature: the signed bytes are this exact prefix
// followed immediately by the body, with no canonicalization, byte-identical to the protocol fixture.
const uploadAuthorizePreamble = "trajectory-shipper-upload-authorize-v2\nPOST\n/v2/uploads/authorize\n"

// AuthorizeRequest is one bounded authorization batch. The caller stamps IssuedAt, the freshness
// the server checks, so the signed bytes and the sent bytes are the same object.
type AuthorizeRequest struct {
	WriterID string         `json:"writer_id"`
	IssuedAt time.Time      `json:"issued_at"`
	Objects  []UploadObject `json:"objects"`
}

// UploadObject is one prepared object's descriptor. Size is the exact ciphertext byte count, and
// SourceHash is the manifest's pre-redaction digest, never a checksum of the encrypted PUT body.
type UploadObject struct {
	ObjectID   string         `json:"object_id"`
	Key        string         `json:"key"`
	Size       int64          `json:"size"`
	SourceHash string         `json:"source_hash"`
	Metadata   UploadMetadata `json:"metadata"`
}

// metadataNames is the closed plaintext-metadata allowlist, and the UploadMetadata json tags below
// spell exactly these names in order. source-hash and ticket-id are absent by design: the server
// derives both, so a client cannot spell them itself.
var metadataNames = []string{
	"manifest-version", "source-id", "shipped-hash", "artifact-class",
	"agent-version", "shape-sniff", "derived", "enrich-status", "kind",
}

// UploadMetadata carries the allowlisted names. A mirror object sets the first four; a heartbeat
// sets Kind alone. The server refuses any other combination.
type UploadMetadata struct {
	ManifestVersion string `json:"manifest-version,omitempty"`
	SourceID        string `json:"source-id,omitempty"`
	ShippedHash     string `json:"shipped-hash,omitempty"`
	ArtifactClass   string `json:"artifact-class,omitempty"`
	AgentVersion    string `json:"agent-version,omitempty"`
	ShapeSniff      string `json:"shape-sniff,omitempty"`
	Derived         string `json:"derived,omitempty"`
	EnrichStatus    string `json:"enrich-status,omitempty"`
	Kind            string `json:"kind,omitempty"`
}

// AuthorizeResponse is one issued ticket batch.
type AuthorizeResponse struct {
	Tickets []Ticket `json:"tickets"`
}

// Ticket is one bounded PUT capability, usable only after the caller matches it back to the
// prepared object and validates its origin and exact key; this package does neither.
type Ticket struct {
	TicketID       string    `json:"ticket_id"`
	ObjectID       string    `json:"object_id"`
	AlreadyPresent bool      `json:"already_present,omitempty"`
	Method         string    `json:"method,omitempty"`
	URL            string    `json:"url,omitempty"`
	ExpiresAt      time.Time `json:"expires_at,omitzero"`

	RequiredHeaders TicketHeaders `json:"required_headers,omitempty"`

	ContentLength int64 `json:"content_length,omitempty"`

	// ContentLengthSigned reports whether the provider signature covers Content-Length: capability
	// data per ticket, not a global assumption about the store.
	ContentLengthSigned bool `json:"content_length_signed,omitempty"`
}

// TicketHeaders is decoded as a map because each store has its own namespace. UnmarshalJSON keeps
// the combined name set closed before a ticket reaches local prepared-object validation.
type TicketHeaders map[string]string

func (h *TicketHeaders) UnmarshalJSON(raw []byte) error {
	var decoded map[string]string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	for name := range decoded {
		if !allowedTicketHeaderName(name) {
			return fmt.Errorf("required header %q is outside the closed provider set", name)
		}
	}
	*h = decoded
	return nil
}

func allowedTicketHeaderName(name string) bool {
	switch name {
	case "x-amz-tagging", "x-ms-tags", "x-ms-blob-type":
		return true
	}
	for _, prefix := range []string{"x-amz-meta-", "x-goog-meta-"} {
		if suffix, ok := strings.CutPrefix(name, prefix); ok {
			return suffix == "source-hash" || suffix == "ticket-id" || allowedMetadataName(suffix)
		}
	}
	if suffix, ok := strings.CutPrefix(name, "x-ms-meta-"); ok {
		suffix = strings.ReplaceAll(suffix, "_", "-")
		return suffix == "source-hash" || suffix == "ticket-id" || allowedMetadataName(suffix)
	}
	return false
}

func allowedMetadataName(name string) bool {
	return slices.Contains(metadataNames, name)
}

// NewWriterID mints this process's writer identity. Audit only, nothing is fenced on it. Random
// per process and never persisted, so two processes on one install never claim the same id.
func NewWriterID() string {
	return uuid.New().String()
}

// AuthorizeUploads exchanges prepared object descriptors for PUT tickets. Its own status mapping:
// 401/403 stops the run, 429/5xx retries later, and conflating them kills an install on a 429.
func (c *Client) AuthorizeUploads(ctx context.Context, req AuthorizeRequest) (resp AuthorizeResponse, err error) {
	if c.installID == "" {
		return AuthorizeResponse{}, ErrNotEnrolled
	}
	switch {
	case req.WriterID == "":
		return AuthorizeResponse{}, errors.New("backend: authorize request carries no writer_id")
	case req.IssuedAt.IsZero():
		return AuthorizeResponse{}, errors.New("backend: authorize request carries no issued_at")
	case len(req.Objects) == 0:
		return AuthorizeResponse{}, errors.New("backend: authorize request carries no objects")
	}

	var payload []byte
	if payload, err = json.Marshal(req); err != nil {
		return AuthorizeResponse{}, fmt.Errorf("backend: encode authorize request: %w", err)
	}

	status, raw, err := c.exchange(ctx, uploadAuthorizePath, uploadAuthorizePreamble, payload, true)
	if err != nil {
		return AuthorizeResponse{}, err
	}
	// Read only on a failure status: a 200 body holds ticket URLs, which never become a diagnostic.
	reason := func() string { return truncate(strings.TrimSpace(string(raw)), 200) }

	switch {
	case status == http.StatusOK:
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return AuthorizeResponse{}, fmt.Errorf("backend: %s refused this install's credentials (HTTP %d): %w",
			uploadAuthorizePath, status, formats.ErrCredentialsRefused)
	case status == http.StatusTooManyRequests, status >= 500:
		return AuthorizeResponse{}, fmt.Errorf("%w (HTTP %d): %s", ErrAuthorizeUnavailable, status, reason())
	default:
		return AuthorizeResponse{}, fmt.Errorf("backend: %s returned HTTP %d: %s", uploadAuthorizePath, status, reason())
	}

	// Strict decoding, scoped to this response only: a ticket half-understood is worse than none.
	// The shared v1 decode stays tolerant on purpose.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&resp); err != nil {
		return AuthorizeResponse{}, fmt.Errorf("backend: decode %s response: %w", uploadAuthorizePath, err)
	}
	if dec.More() {
		return AuthorizeResponse{}, fmt.Errorf("backend: %s response carries content after the JSON document", uploadAuthorizePath)
	}

	if len(resp.Tickets) != len(req.Objects) {
		// The server refuses a batch whole or issues one ticket per object; anything else is a
		// partial authorization this client will not guess its way through.
		return AuthorizeResponse{}, fmt.Errorf("backend: %s issued %d tickets for %d objects",
			uploadAuthorizePath, len(resp.Tickets), len(req.Objects))
	}
	for _, ticket := range resp.Tickets {
		if ticket.AlreadyPresent && (ticket.TicketID == "" || ticket.ObjectID == "" ||
			ticket.Method != "" || ticket.URL != "" || !ticket.ExpiresAt.IsZero() ||
			ticket.RequiredHeaders != nil || ticket.ContentLength != 0 || ticket.ContentLengthSigned) {
			return AuthorizeResponse{}, fmt.Errorf(
				"backend: %s returned an invalid already-present answer for object %q",
				uploadAuthorizePath, ticket.ObjectID)
		}
	}
	return resp, nil
}
