package upload

// Local ticket validation: nothing in an authorization response may validate anything else in
// it. Every field is checked against the prepared object or the machine owner's allowlist.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Provider header names. The set is closed: a name outside it invalidates the whole ticket.
const (
	metadataPrefix   = "x-amz-meta-"
	sourceHashHeader = metadataPrefix + "source-hash"
	ticketIDHeader   = metadataPrefix + "ticket-id"
	taggingHeader    = "x-amz-tagging"
)

type headerDialect struct {
	metadataPrefix, sourceHash, ticketID, tagging string
	azure                                         bool
}

var headerDialects = []headerDialect{
	{metadataPrefix: metadataPrefix, sourceHash: sourceHashHeader,
		ticketID: ticketIDHeader, tagging: taggingHeader},
	{metadataPrefix: "x-goog-meta-", sourceHash: "x-goog-meta-source-hash",
		ticketID: "x-goog-meta-ticket-id"},
	{metadataPrefix: "x-ms-meta-", sourceHash: "x-ms-meta-source_hash",
		ticketID: "x-ms-meta-ticket_id", tagging: "x-ms-tags", azure: true},
}

// MetadataNames is the client-declarable metadata allowlist, unprefixed. source-hash and
// ticket-id are absent by design: each is checked against its own source instead.
// A deliberate copy of controlplane's list (this package imports nothing internal); a test in
// app, the one package that may import both, pins the two.
var MetadataNames = []string{
	"manifest-version", "source-id", "shipped-hash", "artifact-class",
	"agent-version", "shape-sniff", "derived", "enrich-status", "kind",
}

// taggingValues is the closed tag set: any other value means the ticket is not from this protocol.
var taggingValues = []string{"class=trajectory", "class=context"}

// maxKeyLength is the object-store key ceiling; a longer key is an upstream bug, never truncated.
const maxKeyLength = 1024

// PreparedUpload is the sealed object this machine holds: the trusted side of every comparison.
type PreparedUpload struct {
	ObjectID string
	Key      string
	Body     []byte
	// SourceHash is the manifest's pre-redaction source digest, not a checksum of Body.
	SourceHash string
	// Metadata holds unprefixed names from the closed request allowlist.
	Metadata map[string]string
}

// Ticket mirrors the issued PUT capability, declared here so this package imports no plane client.
type Ticket struct {
	TicketID string
	ObjectID string
	Method   string
	URL      string
	// ExpiresAt is for the caller's reauthorization decision; the store decides actual expiry.
	ExpiresAt time.Time
	// RequiredHeaders is a map so the closed name set is enforced here; one value per name.
	RequiredHeaders map[string]string
	ContentLength   int64
	// ContentLengthSigned proves the provider signature confines the exact body size.
	ContentLengthSigned bool
}

// ValidateTicket refuses everything that is not exactly this prepared object at this target.
// Errors name what disagreed and never the URL, its path, or its query.
func ValidateTicket(target UploadTarget, prepared PreparedUpload, ticket Ticket) error {
	if prepared.ObjectID == "" {
		return errors.New("upload: prepared object carries no object id")
	}
	if ticket.ObjectID != prepared.ObjectID {
		return fmt.Errorf("upload: ticket names object %q, prepared object is %q", ticket.ObjectID, prepared.ObjectID)
	}
	if ticket.Method != "PUT" {
		return fmt.Errorf("upload: ticket for object %q authorizes method %q", prepared.ObjectID, ticket.Method)
	}
	if ticket.ContentLength != int64(len(prepared.Body)) {
		return fmt.Errorf("upload: ticket for object %q authorizes %d bytes, prepared object is %d",
			prepared.ObjectID, ticket.ContentLength, len(prepared.Body))
	}
	if !ticket.ContentLengthSigned {
		return fmt.Errorf("upload: ticket for object %q does not sign its content length", prepared.ObjectID)
	}

	u, err := parseTicketURL(ticket.URL)
	if err != nil {
		return err
	}
	if !target.matchesOrigin(u) {
		return fmt.Errorf("%w: %s is not %s", ErrNoTarget, originOf(u), target.Origin())
	}
	if err := validatePath(target, prepared, u.EscapedPath()); err != nil {
		return err
	}
	return validateHeaders(prepared, ticket)
}

// validatePath is the exact-key check: the right origin still permits another install's key.
func validatePath(target UploadTarget, prepared PreparedUpload, escaped string) error {
	if err := validateKey(prepared.Key); err != nil {
		return fmt.Errorf("upload: prepared object %q: %w", prepared.ObjectID, err)
	}
	// Named refusals first: the byte comparison catches these too, but says only "not the key".
	lower := strings.ToLower(escaped)
	switch {
	case strings.Contains(escaped, "//"):
		return fmt.Errorf("upload: ticket path for object %q has an empty segment", prepared.ObjectID)
	case hasDotSegment(escaped):
		return fmt.Errorf("upload: ticket path for object %q has a dot segment", prepared.ObjectID)
	case strings.Contains(lower, "%2f"):
		return fmt.Errorf("upload: ticket path for object %q encodes a slash", prepared.ObjectID)
	case strings.Contains(lower, "%5c"):
		return fmt.Errorf("upload: ticket path for object %q encodes a backslash", prepared.ObjectID)
	}

	// Compared as bytes, never normalized first: normalizing turns a hostile path plausible.
	want := target.pathPrefix + "/" + canonicalPath(prepared.Key)
	switch target.addressing {
	case VirtualHosted, PathStyle:
		if escaped != want {
			return fmt.Errorf("upload: ticket path does not name the prepared key for object %q", prepared.ObjectID)
		}
	case addressingUnpinned:
		// No declared layout, so accept the one spelling either form produces: the key at the
		// root, or below exactly one bucket segment. The refusals above leave only counting.
		bucket, ok := strings.CutSuffix(escaped, want)
		if !ok || strings.Count(bucket, "/") > 1 {
			return fmt.Errorf("upload: ticket path does not name the prepared key for object %q", prepared.ObjectID)
		}
	default:
		return fmt.Errorf("upload: target %s declares unknown addressing %q", target.Origin(), string(target.addressing))
	}
	return nil
}

// validateHeaders enforces the closed header set; a header the server invented is a refusal.
func validateHeaders(prepared PreparedUpload, ticket Ticket) error {
	h := ticket.RequiredHeaders
	dialect, err := ticketHeaderDialect(h)
	if err != nil {
		return fmt.Errorf("upload: ticket for object %q: %w", prepared.ObjectID, err)
	}
	if prepared.SourceHash == "" {
		return fmt.Errorf("upload: prepared object %q carries no source hash", prepared.ObjectID)
	}
	if ticket.TicketID == "" {
		return fmt.Errorf("upload: ticket for object %q carries no ticket id", prepared.ObjectID)
	}
	switch got, ok := h[dialect.sourceHash]; {
	case !ok:
		return fmt.Errorf("upload: ticket for object %q requires no %s header", prepared.ObjectID, dialect.sourceHash)
	case got != prepared.SourceHash:
		return fmt.Errorf("upload: ticket for object %q requires source hash %q, prepared object hashed %q",
			prepared.ObjectID, got, prepared.SourceHash)
	}
	switch got, ok := h[dialect.ticketID]; {
	case !ok:
		return fmt.Errorf("upload: ticket for object %q requires no %s header", prepared.ObjectID, dialect.ticketID)
	case got != ticket.TicketID:
		return fmt.Errorf("upload: ticket %q for object %q requires ticket-id header %q",
			ticket.TicketID, prepared.ObjectID, got)
	}

	for name, want := range prepared.Metadata {
		if !slices.Contains(MetadataNames, name) {
			return fmt.Errorf("upload: prepared object %q declares metadata %q outside the closed set",
				prepared.ObjectID, name)
		}
		header := dialect.metadataHeader(name)
		switch got, ok := h[header]; {
		case !ok:
			return fmt.Errorf("upload: ticket for object %q requires no %s header, which the prepared object declared",
				prepared.ObjectID, header)
		case got != want:
			return fmt.Errorf("upload: ticket for object %q requires %s=%q, prepared object declared %q",
				prepared.ObjectID, header, got, want)
		}
	}
	if dialect.azure && h["x-ms-blob-type"] != "BlockBlob" {
		return fmt.Errorf("upload: ticket for object %q does not require x-ms-blob-type=BlockBlob", prepared.ObjectID)
	}

	for name, got := range h {
		if got == "" {
			return fmt.Errorf("upload: ticket for object %q requires header %q with an empty value",
				prepared.ObjectID, name)
		}
		switch {
		case name == dialect.sourceHash, name == dialect.ticketID:
		case name == dialect.tagging && dialect.tagging != "":
			if !slices.Contains(taggingValues, got) {
				return fmt.Errorf("upload: ticket for object %q requires tag %q", prepared.ObjectID, got)
			}
		case dialect.azure && name == "x-ms-blob-type":
		case strings.HasPrefix(name, dialect.metadataPrefix) &&
			slices.Contains(MetadataNames, dialect.metadataName(name)):
			// Present here but absent above: the server derived it from nothing we sent.
			if prepared.Metadata[dialect.metadataName(name)] != got {
				return fmt.Errorf("upload: ticket for object %q requires %s=%q, which the prepared object did not declare",
					prepared.ObjectID, name, got)
			}
		default:
			return fmt.Errorf("upload: ticket for object %q requires header %q outside the closed provider set",
				prepared.ObjectID, name)
		}
	}
	return nil
}

func ticketHeaderDialect(headers map[string]string) (headerDialect, error) {
	var found *headerDialect
	for i := range headerDialects {
		if _, ok := headers[headerDialects[i].sourceHash]; !ok {
			continue
		}
		if found != nil {
			return headerDialect{}, errors.New("mixes provider header dialects")
		}
		found = &headerDialects[i]
	}
	if found == nil {
		return headerDialect{}, errors.New("carries no recognized source-hash header")
	}
	return *found, nil
}

func (d headerDialect) metadataHeader(name string) string {
	if d.azure {
		name = strings.ReplaceAll(name, "-", "_")
	}
	return d.metadataPrefix + name
}

func (d headerDialect) metadataName(header string) string {
	name := strings.TrimPrefix(header, d.metadataPrefix)
	if d.azure {
		name = strings.ReplaceAll(name, "_", "-")
	}
	return name
}

func validateKey(key string) error {
	switch {
	case key == "":
		return errors.New("key is empty")
	case len(key) > maxKeyLength:
		return fmt.Errorf("key is %d bytes, over the %d ceiling", len(key), maxKeyLength)
	case strings.HasPrefix(key, "/"), strings.HasSuffix(key, "/"):
		return errors.New("key starts or ends with a slash")
	case strings.Contains(key, `\`):
		return errors.New("key contains a backslash")
	}
	if i := strings.IndexFunc(key, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
		return fmt.Errorf("key contains a control byte at offset %d", i)
	}
	return validateSegments(key)
}

func validateSegments(path string) error {
	for _, seg := range strings.Split(path, "/") {
		switch seg {
		case "":
			return errors.New("has an empty segment")
		case ".", "..":
			return errors.New("has a dot segment")
		}
	}
	return nil
}

func hasDotSegment(escaped string) bool {
	segments := strings.Split(escaped, "/")
	return slices.Contains(segments, ".") || slices.Contains(segments, "..")
}

const upperhex = "0123456789ABCDEF"

// canonicalPath renders the one AWS-style escaped spelling of a key: uppercase percent-hex outside
// the unreserved set, "/" kept. Hand-rolled: net/url leaves "=" unescaped and keys are full of them.
func canonicalPath(key string) string {
	var b strings.Builder
	b.Grow(len(key) + 16)
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0f])
		}
	}
	return b.String()
}
