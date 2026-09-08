package upload

// The golden authorization pair from the shipper-protocol v2 fixtures anchors every validation test: key
// grammar, escaped path and header set come from what the control plane is tested against.

import (
	"encoding/json"
	"io/fs"
	"path"
	"testing"
	"time"

	protocol "github.com/QuesmaOrg/shipper-protocol"
)

type goldenObject struct {
	ObjectID   string            `json:"object_id"`
	Key        string            `json:"key"`
	Size       int64             `json:"size"`
	SourceHash string            `json:"source_hash"`
	Metadata   map[string]string `json:"metadata"`
}

type goldenTicket struct {
	TicketID            string            `json:"ticket_id"`
	ObjectID            string            `json:"object_id"`
	Method              string            `json:"method"`
	URL                 string            `json:"url"`
	ExpiresAt           time.Time         `json:"expires_at"`
	RequiredHeaders     map[string]string `json:"required_headers"`
	ContentLength       int64             `json:"content_length"`
	ContentLengthSigned bool              `json:"content_length_signed"`
}

// goldenPair loads one fixture request and response into this package's types, as app/ does.
func goldenPair(t *testing.T, requestFixture, responseFixture string) (PreparedUpload, Ticket) {
	t.Helper()

	var request struct {
		Objects []goldenObject `json:"objects"`
	}
	var response struct {
		Tickets []goldenTicket `json:"tickets"`
	}
	readFixture(t, requestFixture, &request)
	readFixture(t, responseFixture, &response)
	if len(request.Objects) != 1 || len(response.Tickets) != 1 {
		t.Fatalf("fixture pair %s/%s holds %d objects and %d tickets, want one of each",
			requestFixture, responseFixture, len(request.Objects), len(response.Tickets))
	}

	object, ticket := request.Objects[0], response.Tickets[0]
	prepared := PreparedUpload{
		ObjectID:   object.ObjectID,
		Key:        object.Key,
		Body:       make([]byte, object.Size),
		SourceHash: object.SourceHash,
		Metadata:   object.Metadata,
	}
	return prepared, Ticket{
		TicketID:            ticket.TicketID,
		ObjectID:            ticket.ObjectID,
		Method:              ticket.Method,
		URL:                 ticket.URL,
		ExpiresAt:           ticket.ExpiresAt,
		RequiredHeaders:     ticket.RequiredHeaders,
		ContentLength:       ticket.ContentLength,
		ContentLengthSigned: ticket.ContentLengthSigned,
	}
}

func readFixture(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := fs.ReadFile(protocol.FS, path.Join("fixtures", "v2", "uploads-authorize", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
}

// goldenTarget is the machine-owner allowlist entry the fixture URLs belong to.
func goldenTarget(t *testing.T) UploadTarget {
	t.Helper()
	target, err := NewUploadTarget(TargetSpec{
		Origin:     "https://archive.example.invalid",
		Addressing: VirtualHosted,
	})
	if err != nil {
		t.Fatalf("build golden target: %v", err)
	}
	return target
}
