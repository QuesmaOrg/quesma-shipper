package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sync/errgroup"
)

// seenWriteInterval collapses bursts into one write. Every write leaves a permanent noncurrent
// version in a versioned bucket, so unthrottled telemetry would be the fleet's largest churn.
const seenWriteInterval = time.Minute

const seenReadConcurrency = 16
const seenWriteTimeout = 3 * time.Second

type clientFacts struct {
	Version, OS, BootedAt string
	// Catalog is what a config fetch reported; nil for a fetch without one and for other events.
	Catalog *reportedCatalog
}

type seenEvent uint8

const (
	seenEnrollment seenEvent = iota
	seenConfig
	seenVend
)

func factsFromHeaders(r *http.Request) clientFacts {
	return clientFacts{
		Version:  clientFact(r.Header.Get("X-Shipper-Version")),
		OS:       clientFact(r.Header.Get("X-Shipper-OS")),
		BootedAt: clientFact(r.Header.Get("X-Shipper-Boot")),
	}
}

// Touch accepts lost concurrent stamps rather than putting shipper requests on a conflict retry loop.
// A config fetch also records which catalog it carried. A changed catalog is written inside the
// throttle window too: rendering and write validation read it, and a minute late is a wrong answer.
func (m *Manager) Touch(ctx context.Context, installID string, event seenEvent, facts clientFacts) error {
	now, key := m.time(), seenKey(m.org, installID)
	var rec SeenRecord
	raw, _, err := m.store.Get(ctx, key)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		// Overwriting after a failed read could erase the other event's timestamp.
		return err
	default:
		if strictDecode(raw, &rec) != nil || !validSeenRecord(rec, key) {
			rec = SeenRecord{}
		}
	}
	digest := rec.CatalogDigest
	if event == seenConfig {
		digest = ""
		if facts.Catalog != nil {
			digest = facts.Catalog.Digest
		}
	}
	if recorded := eventTime(rec, event); unchanged(rec, facts) && digest == rec.CatalogDigest && recorded != nil && now.Sub(*recorded) < seenWriteInterval {
		return nil
	}
	if digest != rec.CatalogDigest && digest != "" {
		// Content-addressed, so an object already there holds these bytes. Stored before the seen
		// record names it, so a digest on record always resolves.
		if err := m.store.Create(ctx, catalogKey(m.org, digest), facts.Catalog.Record); err != nil && !errors.Is(err, ErrConflict) {
			return err
		}
	}
	rec.CatalogDigest = digest
	rec.Schema, rec.InstallID, rec.LastSeenAt = schemaVersion, installID, now
	rec.OS, rec.BootedAt, rec.ClientVersion = facts.OS, facts.BootedAt, facts.Version
	switch event {
	case seenConfig:
		rec.LastConfigAt = &now
	case seenVend:
		rec.LastVendAt = &now
	}
	encoded, err := encodeRecord(rec)
	if err != nil {
		return err
	}
	return m.store.Put(ctx, key, encoded)
}

func unchanged(rec SeenRecord, facts clientFacts) bool {
	return rec.OS == facts.OS && rec.BootedAt == facts.BootedAt && rec.ClientVersion == facts.Version
}

func eventTime(rec SeenRecord, event seenEvent) *time.Time {
	switch event {
	case seenConfig:
		return rec.LastConfigAt
	case seenVend:
		return rec.LastVendAt
	}
	return nil
}

func validSeenRecord(rec SeenRecord, key string) bool {
	return rec.Schema == schemaVersion && rec.InstallID == recordIDFromKey(key)
}

func (s *Server) touch(r *http.Request, rec InstallRecord, event seenEvent, catalog *reportedCatalog) {
	scoped, err := s.manager.ForOrganization(rec.Organization)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), seenWriteTimeout)
	defer cancel()
	facts := factsFromHeaders(r)
	facts.Catalog = catalog
	if err := scoped.Touch(ctx, rec.InstallID, event, facts); err != nil {
		s.logger.Printf("recording client facts for install %s: %v", rec.InstallID, err)
	}
}

// ListSeen drops unusable details so their install rows can still render.
func (m *Manager) ListSeen(ctx context.Context) ([]SeenRecord, error) {
	objects, err := m.store.List(ctx, controlPrefix(m.org)+"seen/")
	if err != nil {
		return nil, err
	}
	records := make([]SeenRecord, len(objects))
	found := make([]bool, len(objects))
	var group errgroup.Group
	group.SetLimit(seenReadConcurrency)
	for i, object := range objects {
		group.Go(func() error {
			rec, _, err := getRecord[SeenRecord](ctx, m.store, object.Key)
			if err != nil || !validSeenRecord(rec, object.Key) {
				return nil
			}
			records[i], found[i] = rec, true
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	out := make([]SeenRecord, 0, len(objects))
	for i, ok := range found {
		if ok {
			out = append(out, records[i])
		}
	}
	return out, nil
}

// clientFact bounds device-chosen text before it reaches an administrator's table.
func clientFact(value string) string { return boundedText(value, 200) }

// boundedText is the shared bound: trimmed, control characters removed, cut to max runes. The limit
// is a parameter because a version string and a failure message are not the same length of useful --
// an error's cause is at the END of the line, so the shorter bound removes the part worth reading.
func boundedText(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > max {
		runes = runes[:max]
	}
	out := runes[:0]
	for _, r := range runes {
		if !unicode.IsControl(r) {
			out = append(out, r)
		}
	}
	return string(out)
}
