package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	uploadMaxObjects     = 32
	uploadMaxObjectBytes = int64(1 << 30)
	uploadMaxBatchBytes  = int64(1 << 30)
	uploadIssuedAtMaxAge = 5 * time.Minute
	uploadIssuedAtSkew   = time.Minute
	uploadTicketLifetime = 5 * time.Minute
)

type UploadSigner interface {
	Authorize(context.Context, InstallScope, UploadBatch) (TicketBatch, error)
}

type InstallScope struct{ Organization, InstallID string }
type UploadBatch struct {
	IssuedAt, ExpiresAt time.Time
	Objects             []UploadObjectRequest
}
type UploadObjectRequest struct {
	ObjectID, TicketID, Key string
	Size                    int64
	Metadata                map[string]string
	Tagging                 string
	Mirror                  bool
}
type TicketBatch struct{ Tickets []uploadTicket }

type uploadAuthorizeRequest struct {
	WriterID string         `json:"writer_id"`
	IssuedAt time.Time      `json:"issued_at"`
	Objects  []uploadObject `json:"objects"`
}
type uploadObject struct {
	ObjectID   string            `json:"object_id"`
	Key        string            `json:"key"`
	Size       int64             `json:"size"`
	SourceHash string            `json:"source_hash"`
	Metadata   map[string]string `json:"metadata"`
}
type uploadAuthorizeResponse struct {
	Tickets []uploadTicket `json:"tickets"`
}
type uploadTicket struct {
	TicketID            string            `json:"ticket_id"`
	ObjectID            string            `json:"object_id"`
	Method              string            `json:"method"`
	URL                 string            `json:"url"`
	ExpiresAt           time.Time         `json:"expires_at"`
	RequiredHeaders     map[string]string `json:"required_headers"`
	ContentLength       int64             `json:"content_length"`
	ContentLengthSigned bool              `json:"content_length_signed"`
	AlreadyPresent      bool              `json:"already_present,omitempty"`
}

// The already-present answer is a distinct wire shape rather than a ticket with zeroed fields:
// a client must not be able to read an expiry or a method out of an answer that carries no
// capability.
type alreadyPresentTicket struct {
	TicketID       string `json:"ticket_id"`
	ObjectID       string `json:"object_id"`
	AlreadyPresent bool   `json:"already_present"`
}

func (t uploadTicket) MarshalJSON() ([]byte, error) {
	if t.AlreadyPresent {
		return json.Marshal(alreadyPresentTicket{TicketID: t.TicketID, ObjectID: t.ObjectID, AlreadyPresent: true})
	}
	type wire uploadTicket
	return json.Marshal(wire(t))
}

var (
	uploadObjectIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	uploadHashRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	uploadIdentRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	uploadVersionRe  = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)
	uploadAgentRe    = regexp.MustCompile(`^[\x20-\x7e]{1,128}$`)
	mirrorKeyRe      = regexp.MustCompile(`^v1/organization=([a-z0-9][a-z0-9._-]{0,63})/install=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/mirror/source=([a-z0-9][a-z0-9._-]{0,63})/[0-9a-f]{64}\.age$`)
	heartbeatKeyRe   = regexp.MustCompile(`^v1/organization=([a-z0-9][a-z0-9._-]{0,63})/install=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/state/heartbeat\.json\.age$`)
)

type uploadMetadataRule struct {
	required bool
	valid    func(string) bool
}

var uploadMirrorMetadata = map[string]uploadMetadataRule{
	"manifest-version": {true, uploadVersionRe.MatchString}, "source-id": {true, uploadIdentRe.MatchString},
	"shipped-hash": {true, uploadHashRe.MatchString}, "artifact-class": {true, uploadIdentRe.MatchString},
	"agent-version": {false, uploadAgentRe.MatchString}, "shape-sniff": {false, uploadIdentRe.MatchString},
	"derived": {false, func(v string) bool { return v == "true" }}, "enrich-status": {false, uploadIdentRe.MatchString},
}
var uploadHeartbeatMetadata = map[string]uploadMetadataRule{"kind": {true, func(v string) bool { return v == "heartbeat" }}}
var uploadServerDerived = map[string]bool{"source-hash": true, "ticket-id": true}

func validateUploadRequest(req uploadAuthorizeRequest, rec InstallRecord, now time.Time) ([]UploadObjectRequest, error) {
	if _, err := uuidParse(req.WriterID); err != nil {
		return nil, fmt.Errorf("writer_id %q is not a UUID", req.WriterID)
	}
	if req.IssuedAt.IsZero() {
		return nil, fmt.Errorf("issued_at is required")
	}
	age := now.Sub(req.IssuedAt)
	if age > uploadIssuedAtMaxAge {
		return nil, fmt.Errorf("issued_at is more than %s old", uploadIssuedAtMaxAge)
	}
	if age < -uploadIssuedAtSkew {
		return nil, fmt.Errorf("issued_at is more than %s ahead of server time", uploadIssuedAtSkew)
	}
	if len(req.Objects) == 0 || len(req.Objects) > uploadMaxObjects {
		return nil, fmt.Errorf("objects carries %d descriptors, want 1 to %d", len(req.Objects), uploadMaxObjects)
	}
	seenID, seenKey := map[string]bool{}, map[string]bool{}
	out := make([]UploadObjectRequest, 0, len(req.Objects))
	var total int64
	for _, obj := range req.Objects {
		if !uploadObjectIDRe.MatchString(obj.ObjectID) {
			return nil, fmt.Errorf("object_id %q is invalid", obj.ObjectID)
		}
		if seenID[obj.ObjectID] {
			return nil, fmt.Errorf("object %q: object_id appears twice", obj.ObjectID)
		}
		if seenKey[obj.Key] {
			return nil, fmt.Errorf("object %q: key appears twice", obj.ObjectID)
		}
		seenID[obj.ObjectID], seenKey[obj.Key] = true, true
		valid, err := validateUploadObject(obj, rec)
		if err != nil {
			return nil, fmt.Errorf("object %q: %w", obj.ObjectID, err)
		}
		total += obj.Size
		if total > uploadMaxBatchBytes {
			return nil, fmt.Errorf("batch declares more than %d bytes", uploadMaxBatchBytes)
		}
		out = append(out, valid)
	}
	return out, nil
}

func validateUploadObject(obj uploadObject, rec InstallRecord) (UploadObjectRequest, error) {
	if obj.Size <= 0 || obj.Size > uploadMaxObjectBytes {
		return UploadObjectRequest{}, fmt.Errorf("size %d is outside 1..%d", obj.Size, uploadMaxObjectBytes)
	}
	if !uploadHashRe.MatchString(obj.SourceHash) {
		return UploadObjectRequest{}, fmt.Errorf("source_hash is not 64 lowercase hexadecimal characters")
	}
	canonical, err := uuidParse(rec.InstallID)
	if err != nil {
		return UploadObjectRequest{}, errors.New("stored install id is invalid")
	}
	v := UploadObjectRequest{ObjectID: obj.ObjectID, Key: obj.Key, Size: obj.Size}
	var allowed map[string]uploadMetadataRule
	if match := mirrorKeyRe.FindStringSubmatch(obj.Key); match != nil {
		if match[1] != rec.Organization || match[2] != canonical {
			return UploadObjectRequest{}, fmt.Errorf("key is outside this install's root")
		}
		allowed, v.Tagging, v.Mirror = uploadMirrorMetadata, "class=trajectory", true
		if obj.Metadata["source-id"] != match[3] {
			return UploadObjectRequest{}, fmt.Errorf("key source does not equal metadata source-id")
		}
	} else if match := heartbeatKeyRe.FindStringSubmatch(obj.Key); match != nil {
		if match[1] != rec.Organization || match[2] != canonical {
			return UploadObjectRequest{}, fmt.Errorf("key is outside this install's root")
		}
		allowed, v.Tagging = uploadHeartbeatMetadata, "class=context"
	} else {
		return UploadObjectRequest{}, fmt.Errorf("key names no object v2 authorizes")
	}
	meta, err := validateUploadMetadata(obj.Metadata, allowed)
	if err != nil {
		return UploadObjectRequest{}, err
	}
	meta["source-hash"] = obj.SourceHash
	v.Metadata = meta
	return v, nil
}

func validateUploadMetadata(meta map[string]string, allowed map[string]uploadMetadataRule) (map[string]string, error) {
	out := make(map[string]string, len(meta)+2)
	for _, name := range slices.Sorted(maps.Keys(meta)) {
		if uploadServerDerived[name] {
			return nil, fmt.Errorf("metadata %q is server-derived", name)
		}
		rule, ok := allowed[name]
		if !ok {
			return nil, fmt.Errorf("metadata %q is not allowlisted", name)
		}
		if !rule.valid(meta[name]) {
			return nil, fmt.Errorf("metadata %q has an invalid value", name)
		}
		out[name] = meta[name]
	}
	for _, name := range slices.Sorted(maps.Keys(allowed)) {
		if allowed[name].required && out[name] == "" {
			return nil, fmt.Errorf("metadata %q is required", name)
		}
	}
	return out, nil
}

// uploadProbeTimeout bounds the whole deduplication probe. A shipper is waiting on this
// authorization, so a slow store costs one re-upload rather than a stalled request.
const uploadProbeTimeout = 3 * time.Second

// splitAlreadyStored partitions a minted batch into the objects that still need a PUT ticket and
// the ones the store already holds under the same source hash. Any doubt keeps an object in the
// first half: a missed match costs one upload, a wrong match loses the object. Only mirror
// objects are asked about: a state object such as the heartbeat is rewritten every tick, so the
// archive never holds the bytes on offer.
func splitAlreadyStored(ctx context.Context, store ObjectStore, logger *log.Logger, installID string, objects []UploadObjectRequest) ([]UploadObjectRequest, []uploadTicket) {
	var mirrors []UploadObjectRequest
	for _, object := range objects {
		if object.Mirror {
			mirrors = append(mirrors, object)
		}
	}
	if len(mirrors) == 0 {
		return objects, nil
	}
	stored := map[string]bool{}
	for i, held := range probeStored(ctx, store, logger, installID, mirrors) {
		stored[mirrors[i].ObjectID] = held
	}
	needed := make([]UploadObjectRequest, 0, len(objects))
	var settled []uploadTicket
	for _, object := range objects {
		if stored[object.ObjectID] {
			settled = append(settled, uploadTicket{TicketID: object.TicketID, ObjectID: object.ObjectID, AlreadyPresent: true})
			continue
		}
		needed = append(needed, object)
	}
	return needed, settled
}

// probeStored reads one key per object, all in flight at once: validation caps a batch at 32
// members, and a serial walk pays the store's latency per object.
func probeStored(ctx context.Context, store ObjectStore, logger *log.Logger, installID string, objects []UploadObjectRequest) []bool {
	ctx, cancel := context.WithTimeout(ctx, uploadProbeTimeout)
	defer cancel()

	stored := make([]bool, len(objects))
	errs := make([]error, len(objects))
	var wg sync.WaitGroup
	for i, object := range objects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hash, err := store.SourceHash(ctx, object.Key)
			switch {
			case errors.Is(err, ErrNotFound):
			case err != nil:
				errs[i] = err
			default:
				stored[i] = hash == object.Metadata["source-hash"]
			}
		}()
	}
	wg.Wait()

	failed, detail := 0, ""
	for _, err := range errs {
		if err != nil {
			failed++
			if detail == "" {
				detail = scrubURLs(err.Error())
			}
		}
	}
	if failed > 0 {
		logger.Printf("deduplication probe for install %s: %d of %d reads failed (%s); those objects are authorized as new",
			installID, failed, len(objects), detail)
	}
	return stored
}

func scrubURLs(message string) string {
	parts := strings.Fields(message)
	for i, part := range parts {
		lower := strings.ToLower(part)
		if strings.Contains(part, "://") || strings.Contains(lower, "signature=") || strings.Contains(lower, "sig=") {
			parts[i] = "[redacted]"
		}
	}
	return strings.Join(parts, " ")
}
