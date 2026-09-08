package app

// The write path, assembled: the authorization client, the upload-target allowlist and the
// presigned uploader behind the engine's one upload port. The engine gets prepared objects and
// verdicts; every wire type, every ticket and every URL stops here.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/upload"
)

// vendPort is the engine's UploadPort. One instance per process, because the writer id is one per
// process: a second port would report two writers from one machine.
type vendPort struct {
	client   *controlplane.Client
	uploader *upload.Uploader
	targets  upload.UploadTargetList
	writerID string

	// now is the signing clock and the ticket-expiry clock. A field so a test can pin it.
	now func() time.Time
}

// newUploadPort assembles the write path. The enrollment record is mandatory, the allowlist is not:
// with no upload_targets the tickets decide the destination, https only and exact key enforced.
func newUploadPort(stateDir string, eff *config.Effective) (*vendPort, error) {
	enrollment, err := controlplane.LoadEnrollment(stateDir)
	if err != nil {
		return nil, fmt.Errorf("%w\n\nEvery upload is authorized by the control plane named in "+
			"the enrollment record. Fix it or log in again with `shipper login`", err)
	}
	if enrollment == nil || enrollment.Endpoint == "" {
		return nil, errors.New("uploading needs an enrolled control plane to authorize every object; " +
			"`shipper login` first (`shipper preview` works without it)")
	}
	targets, err := uploadTargets(eff)
	if err != nil {
		return nil, err
	}
	deviceKey, err := enrollment.PrivateKey()
	if err != nil {
		return nil, err
	}
	client, err := controlplane.New(controlplane.Options{
		Endpoint:     enrollment.Endpoint,
		InstallID:    enrollment.InstallID,
		Organization: enrollment.Organization,
		DeviceKey:    deviceKey,
	})
	if err != nil {
		return nil, err
	}
	return &vendPort{
		client:   client,
		uploader: upload.New(),
		targets:  targets,
		writerID: controlplane.NewWriterID(),
		now:      time.Now,
	}, nil
}

// AuthorizeAndUpload spends one bounded group: one authorization, then one PUT per ticket. The
// batch is authorized whole or not at all; past that point the objects succeed or fail alone.
func (p *vendPort) AuthorizeAndUpload(ctx context.Context, batch []engine.PreparedObject) []error {
	out := make([]error, len(batch))
	req, err := p.request(batch)
	if err != nil {
		return sameOutcome(out, err)
	}
	resp, err := p.client.AuthorizeUploads(ctx, req)
	if err != nil {
		return sameOutcome(out, classifyAuthorize(err))
	}

	tickets := make(map[string]controlplane.Ticket, len(resp.Tickets))
	for _, t := range resp.Tickets {
		if _, dup := tickets[t.ObjectID]; dup {
			return sameOutcome(out, fmt.Errorf(
				"upload: the control plane issued two tickets naming object %q", t.ObjectID))
		}
		tickets[t.ObjectID] = t
	}

	// Bounds the PUTs one authorization group has in flight; without it a group of 32 sends 32 at once.
	slots := make(chan struct{}, 4*runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for i, obj := range batch {
		issued, ok := tickets[obj.ObjectID]
		if !ok {
			out[i] = fmt.Errorf(
				"upload: the control plane issued no ticket for object %q", obj.ObjectID)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			out[i] = p.send(ctx, obj, issued)
		}()
	}
	wg.Wait()
	return out
}

// send validates one ticket against the object this machine actually prepared, then spends it.
// Both checks come before any byte leaves: origin, then exact key, then the closed header set.
func (p *vendPort) send(ctx context.Context, obj engine.PreparedObject, issued controlplane.Ticket) error {
	if issued.AlreadyPresent {
		return nil
	}
	prepared := upload.PreparedUpload{
		ObjectID:   obj.ObjectID,
		Key:        obj.Key,
		Body:       obj.Body,
		SourceHash: obj.SourceHash,
		Metadata:   obj.Metadata,
	}
	ticket := toUploadTicket(issued)
	target, err := p.targets.Match(ticket.URL)
	if err != nil {
		return err
	}
	if err := upload.ValidateTicket(target, prepared, ticket); err != nil {
		return err
	}
	// Nothing the store said crosses back: the local fingerprint document is the sole progress authority.
	if err := p.uploader.Upload(ctx, ticket, obj.Body); err != nil {
		return p.classifyPut(err, ticket)
	}
	return nil
}

// request turns prepared objects into one signed authorization batch.
func (p *vendPort) request(batch []engine.PreparedObject) (controlplane.AuthorizeRequest, error) {
	objects := make([]controlplane.UploadObject, len(batch))
	for i, o := range batch {
		md, dropped, err := uploadMetadata(o.Metadata)
		if err != nil {
			return controlplane.AuthorizeRequest{}, fmt.Errorf("upload: object %q: %w", o.ObjectID, err)
		}
		for _, d := range dropped {
			fmt.Fprintf(os.Stderr, "warning: object %s: %s\n", o.Key, d)
		}
		objects[i] = controlplane.UploadObject{
			ObjectID:   o.ObjectID,
			Key:        o.Key,
			Size:       int64(len(o.Body)),
			SourceHash: o.SourceHash,
			Metadata:   md,
		}
	}
	return controlplane.AuthorizeRequest{
		WriterID: p.writerID,
		IssuedAt: p.now().UTC(),
		Objects:  objects,
	}, nil
}

// uploadMetadata maps manifest metadata onto the closed request set, name by name. An unknown name
// fails the whole batch: dropping it would ship plaintext metadata disagreeing with the manifest.
func uploadMetadata(md map[string]string) (controlplane.UploadMetadata, []string, error) {
	var out controlplane.UploadMetadata
	var dropped []string
	for name, value := range md {
		switch name {
		case "manifest-version":
			out.ManifestVersion = value
		case "source-id":
			out.SourceID = value
		case "shipped-hash":
			out.ShippedHash = value
		case "artifact-class":
			out.ArtifactClass = value
		case "agent-version":
			// The one value an agent's own file supplies verbatim: out of grammar it would fail the
			// WHOLE batch for as long as that file exists, so the value goes and the object ships.
			if !printableASCII(value, 128) {
				dropped = append(dropped, fmt.Sprintf(
					"agent-version %q is not printable ASCII within 128 bytes; "+
						"shipping without it (the sealed manifest keeps it)", value))
				break
			}
			out.AgentVersion = value
		case "shape-sniff":
			out.ShapeSniff = value
		case "derived":
			out.Derived = value
		case "enrich-status":
			out.EnrichStatus = value
		case "kind":
			out.Kind = value
		case "source-hash", "ticket-id":
			return controlplane.UploadMetadata{}, nil, fmt.Errorf(
				"metadata %q is derived by the server and may not be requested", name)
		default:
			return controlplane.UploadMetadata{}, nil, fmt.Errorf(
				"metadata %q is outside the closed request set", name)
		}
	}
	return out, dropped, nil
}

// printableASCII is the plaintext-metadata grammar the control plane enforces: up to max bytes of
// printable ASCII. Empty passes; an absent value is simply not sent.
func printableASCII(v string, max int) bool {
	return len(v) <= max && !strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r > 0x7e })
}

// classifyAuthorize picks the sentinel the engine reads: refused credentials kill the install, an
// outage stops one run, and the unavailable wrapping drops the chain so neither can find the other.
func classifyAuthorize(err error) error {
	switch {
	case errors.Is(err, formats.ErrCredentialsRefused):
		return err
	case errors.Is(err, controlplane.ErrAuthorizeUnavailable):
		return fmt.Errorf("%w: %v", engine.ErrUploadUnavailable, err)
	}
	return err
}

// classifyPut names the one PUT failure worth a second authorization inside a run: a store refusal
// on a ticket whose own expiry has passed. Every other refusal waits for the next run.
func (p *vendPort) classifyPut(err error, ticket upload.Ticket) error {
	var status *upload.StatusError
	if !errors.As(err, &status) {
		return err
	}
	switch status.Status {
	case http.StatusForbidden, http.StatusBadRequest:
		if !ticket.ExpiresAt.IsZero() && p.now().After(ticket.ExpiresAt) {
			return fmt.Errorf("%w: %v", engine.ErrTicketExpired, err)
		}
	}
	return err
}

// sameOutcome is one verdict for the whole group, for the failures that leave no per-object information.
func sameOutcome(out []error, err error) []error {
	for i := range out {
		out[i] = err
	}
	return out
}

// DescribeDestination names where objects go for the verbs that print it before a runtime exists:
// a ticket path or query authorizes the write, so it never reaches a printed line.
func DescribeDestination(eff *config.Effective) string {
	if len(eff.UploadTargets) == 0 {
		// Worded to hold on a local-dev install too, where no tickets exist and nothing uploads.
		return "presigned upload (no pinned origins; set upload_targets to pin)"
	}
	origins := make([]string, 0, len(eff.UploadTargets))
	for _, t := range eff.UploadTargets {
		origins = append(origins, t.Origin)
	}
	return "presigned upload -> " + strings.Join(origins, ", ")
}

func DestinationHosts(destination, endpoint string) string {
	if _, origins, ok := strings.Cut(destination, "-> "); ok {
		return origins
	}
	return strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
}
