package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// seenWriteInterval collapses bursts into one write. Every write leaves a permanent noncurrent
// version in a versioned bucket, so unthrottled telemetry would be the fleet's largest churn.
const seenWriteInterval = time.Minute

const seenWriteTimeout = 3 * time.Second

type clientFacts struct{ Version, OS, BootedAt string }

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
	if recorded := eventTime(rec, event); unchanged(rec, facts) && recorded != nil && now.Sub(*recorded) < seenWriteInterval {
		return nil
	}
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

func (s *Server) touch(r *http.Request, rec InstallRecord, event seenEvent) {
	scoped, err := s.manager.ForOrganization(rec.Organization)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), seenWriteTimeout)
	defer cancel()
	if err := scoped.Touch(ctx, rec.InstallID, event, factsFromHeaders(r)); err != nil {
		s.logger.Printf("recording client facts for install %s: %v", rec.InstallID, err)
	}
}

// ListSeen drops unusable details so their install rows can still render.
func (m *Manager) ListSeen(ctx context.Context) ([]SeenRecord, error) {
	objects, err := m.store.List(ctx, controlPrefix(m.org)+"seen/")
	if err != nil {
		return nil, err
	}
	return readRecords(ctx, m.store, objectKeys(objects), validSeenRecord), nil
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
