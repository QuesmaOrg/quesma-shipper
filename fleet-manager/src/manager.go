package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

type Manager struct {
	store ObjectStore
	org   string
	now   func() time.Time
	// transition is test-only fault injection; production leaves it nil.
	transition func(string) error
}

func NewManager(store ObjectStore) (*Manager, error) {
	if store == nil {
		return nil, errors.New("object store is required")
	}
	return &Manager{store: store, now: time.Now}, nil
}

func (m *Manager) ForOrganization(org string) (*Manager, error) {
	if !orgPattern.MatchString(org) {
		return nil, errors.New("organization must be a lowercase slug of at most 63 characters")
	}
	return &Manager{store: m.store, org: org, now: m.now, transition: m.transition}, nil
}

func enrollmentOrganization(token string) (string, error) {
	org, _, err := organizationToken(token)
	if err != nil {
		return "", ErrForbidden
	}
	return org, nil
}

func deviceOrganization(org string) (string, error) {
	if !orgPattern.MatchString(org) {
		return "", ErrUnknownInstall
	}
	return org, nil
}

func (m *Manager) time() time.Time { return m.now().UTC() }

func (m *Manager) after(name string) error {
	if m.transition != nil {
		return m.transition(name)
	}
	return nil
}

func getRecord[T any](ctx context.Context, store ObjectStore, key string) (T, string, error) {
	var out T
	raw, version, err := store.Get(ctx, key)
	if err != nil {
		return out, "", err
	}
	if err := strictDecode(raw, &out); err != nil {
		return out, "", fmt.Errorf("decode %s: %w", key, err)
	}
	return out, version, nil
}

const recordReadConcurrency = 16

// readRecords reads keys concurrently, in order, dropping what fails to read or to keep: a list
// that feeds an install table must still render when one record is unusable.
func readRecords[T any](ctx context.Context, store ObjectStore, keys []string, keep func(rec T, key string) bool) []T {
	records := make([]T, len(keys))
	found := make([]bool, len(keys))
	var group errgroup.Group
	group.SetLimit(recordReadConcurrency)
	for i, key := range keys {
		group.Go(func() error {
			rec, _, err := getRecord[T](ctx, store, key)
			if err == nil && keep(rec, key) {
				records[i], found[i] = rec, true
			}
			return nil
		})
	}
	_ = group.Wait()
	out := make([]T, 0, len(keys))
	for i, ok := range found {
		if ok {
			out = append(out, records[i])
		}
	}
	return out
}

func createRecord(ctx context.Context, store ObjectStore, key string, value any) error {
	raw, err := encodeRecord(value)
	if err != nil {
		return err
	}
	return store.Create(ctx, key, raw)
}

func replaceRecord(ctx context.Context, store ObjectStore, key, version string, value any) error {
	raw, err := encodeRecord(value)
	if err != nil {
		return err
	}
	return store.Replace(ctx, key, version, raw)
}

func (m *Manager) LoadConfig(ctx context.Context) (FleetConfig, string, error) {
	cfg, version, err := getRecord[FleetConfig](ctx, m.store, configKey(m.org))
	if err != nil {
		return FleetConfig{}, "", err
	}
	if err := validateConfig(cfg); err != nil {
		return FleetConfig{}, "", fmt.Errorf("stored config is invalid: %w", err)
	}
	if cfg.Organization != m.org {
		return FleetConfig{}, "", errors.New("stored config names another organization")
	}
	cfg.DisplayName = cmp.Or(cfg.DisplayName, cfg.Organization)
	return cfg, version, nil
}

func (m *Manager) Init(ctx context.Context, cfg FleetConfig) error {
	cfg.Schema, cfg.Organization, cfg.UpdatedAt = schemaVersion, m.org, m.time()
	cfg.DisplayName = cmp.Or(cfg.DisplayName, m.org)
	if err := validateConfigForWrite(cfg); err != nil {
		return err
	}
	enabled, err := m.store.VersioningEnabled(ctx)
	if err != nil {
		return fmt.Errorf("verify object versioning: %w", err)
	}
	if !enabled {
		return errors.New("object versioning is required")
	}
	return createRecord(ctx, m.store, configKey(m.org), cfg)
}

func (m *Manager) ApplyConfig(ctx context.Context, cfg FleetConfig, expectedVersion string) error {
	cfg.Schema, cfg.Organization, cfg.UpdatedAt = schemaVersion, m.org, m.time()
	if err := validateConfigForWrite(cfg); err != nil {
		return err
	}
	return replaceRecord(ctx, m.store, configKey(m.org), expectedVersion, cfg)
}

// credential is a grant or an invite, through a pointer so the shared fields can be set in place.
type credential[T any] interface {
	*T
	credential() *credentialRecord
}

func createCredential[T any, P credential[T]](ctx context.Context, m *Manager, keyOf func(org, id string) string, expires time.Time) (string, error) {
	token, id, digest, err := mintToken(tokenPrefix(m.org))
	if err != nil {
		return "", err
	}
	now := m.time()
	if !expires.After(now) {
		return "", invalidRequest("expiry must be in the future")
	}
	var rec T
	*P(&rec).credential() = credentialRecord{Schema: schemaVersion, ID: id, SecretDigest: digest, ExpiresAt: expires.UTC(), CreatedAt: now}
	if err := createRecord(ctx, m.store, keyOf(m.org, id), rec); err != nil {
		return "", err
	}
	return token, nil
}

func (m *Manager) CreateGrant(ctx context.Context, expires time.Time) (string, error) {
	return createCredential[GrantRecord](ctx, m, grantKey, expires)
}

func (m *Manager) CreateInvite(ctx context.Context, expires time.Time) (string, error) {
	return createCredential[InviteRecord](ctx, m, inviteKey, expires)
}

// uuidParse accepts the hyphenated form only, in either case, and returns it lowercased.
func uuidParse(value string) (string, error) {
	if len(value) != 36 {
		return "", errors.New("not UUID")
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func enrollmentDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func (m *Manager) EnrollGrant(ctx context.Context, token string, in InstallRecord) error {
	org, id, err := organizationToken(token)
	if err != nil || org != m.org {
		return ErrForbidden
	}
	grant, _, err := getRecord[GrantRecord](ctx, m.store, grantKey(m.org, id))
	if errors.Is(err, ErrNotFound) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if grant.Schema != schemaVersion || !verifyToken(token, tokenPrefix(m.org), grant.ID, grant.SecretDigest) ||
		grant.RevokedAt != nil || !m.time().Before(grant.ExpiresAt) {
		return ErrForbidden
	}
	in.Schema, in.Organization, in.Status = schemaVersion, m.org, InstallActive
	in.CreatedAt, in.UpdatedAt = m.time(), m.time()
	if err := createRecord(ctx, m.store, installKey(m.org, in.InstallID), in); errors.Is(err, ErrConflict) {
		current, _, readErr := getRecord[InstallRecord](ctx, m.store, installKey(m.org, in.InstallID))
		if readErr != nil {
			return readErr
		}
		if current.Status == InstallActive && sameEnrollment(current, in) {
			return nil
		}
		return ErrEnrollmentConflict
	} else {
		return err
	}
}

// EnrollInvite is a four-write transaction whose durable intermediate states are recoverable.
// A reservation never times out: only an administrator can prove it safe to release.
func (m *Manager) EnrollInvite(ctx context.Context, token string, in InstallRecord) error {
	org, id, err := organizationToken(token)
	if err != nil || org != m.org {
		return ErrForbidden
	}
	key := inviteKey(m.org, id)
	inv, version, err := getRecord[InviteRecord](ctx, m.store, key)
	if errors.Is(err, ErrNotFound) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	now := m.time()
	if inv.Schema != schemaVersion || !verifyToken(token, tokenPrefix(m.org), inv.ID, inv.SecretDigest) || inv.RevokedAt != nil {
		return ErrForbidden
	}
	in.Schema, in.Organization = schemaVersion, m.org
	if inv.ReservedInstallID == "" && !now.Before(inv.ExpiresAt) {
		return ErrForbidden
	}
	if inv.SpentAt != nil && (inv.ReservedInstallID != in.InstallID || inv.ReservedEnrollmentDigest != in.EnrollmentDigest) {
		return ErrForbidden
	}
	if inv.ReservedInstallID == "" {
		inv.ReservedInstallID, inv.ReservedEnrollmentDigest = in.InstallID, in.EnrollmentDigest
		inv.ReservedAt = &now
		if err := replaceRecord(ctx, m.store, key, version, inv); err != nil {
			if errors.Is(err, ErrConflict) {
				return m.EnrollInvite(ctx, token, in)
			}
			return err
		}
		if err := m.after("reserved"); err != nil {
			return err
		}
	} else if inv.ReservedInstallID != in.InstallID || inv.ReservedEnrollmentDigest != in.EnrollmentDigest {
		return ErrForbidden
	}

	installObjectKey := installKey(m.org, in.InstallID)
	current, _, getErr := getRecord[InstallRecord](ctx, m.store, installObjectKey)
	if errors.Is(getErr, ErrNotFound) {
		in.Status = InstallPending
		in.CreatedAt, in.UpdatedAt = now, now
		if err := createRecord(ctx, m.store, installObjectKey, in); err != nil {
			if errors.Is(err, ErrConflict) {
				return m.EnrollInvite(ctx, token, in)
			}
			return err
		}
		if err := m.after("install-created"); err != nil {
			return err
		}
	} else if getErr != nil {
		return getErr
	} else if !sameEnrollment(current, in) || current.Status == InstallRevoked {
		return ErrEnrollmentConflict
	} else if current.Status == InstallActive {
		return nil
	}

	inv, version, err = getRecord[InviteRecord](ctx, m.store, key)
	if err != nil {
		return err
	}
	if inv.SpentAt == nil {
		spent := m.time()
		inv.SpentAt = &spent
		if err := replaceRecord(ctx, m.store, key, version, inv); err != nil {
			if errors.Is(err, ErrConflict) {
				return m.EnrollInvite(ctx, token, in)
			}
			return err
		}
		if err := m.after("spent"); err != nil {
			return err
		}
	}

	current, installVersion, err := getRecord[InstallRecord](ctx, m.store, installObjectKey)
	if err != nil {
		return err
	}
	if current.Status == InstallActive {
		if sameEnrollment(current, in) {
			return nil
		}
		return ErrEnrollmentConflict
	}
	if current.Status != InstallPending || !sameEnrollment(current, in) {
		return ErrEnrollmentConflict
	}
	current.Status, current.UpdatedAt = InstallActive, m.time()
	if err := replaceRecord(ctx, m.store, installObjectKey, installVersion, current); err != nil {
		if errors.Is(err, ErrConflict) {
			return m.EnrollInvite(ctx, token, in)
		}
		return err
	}
	return m.after("activated")
}

func sameEnrollment(current, requested InstallRecord) bool {
	return current.Schema == schemaVersion && current.Organization == requested.Organization &&
		current.InstallID == requested.InstallID && current.EnrollmentDigest == requested.EnrollmentDigest &&
		current.DevicePublicKey == requested.DevicePublicKey && current.AgeRecipient == requested.AgeRecipient
}

func (m *Manager) LoadActiveInstall(ctx context.Context, id string) (InstallRecord, error) {
	rec, _, err := getRecord[InstallRecord](ctx, m.store, installKey(m.org, id))
	if errors.Is(err, ErrNotFound) {
		return InstallRecord{}, ErrUnknownInstall
	}
	if err != nil {
		return InstallRecord{}, err
	}
	if rec.Schema != schemaVersion || rec.Organization != m.org {
		return InstallRecord{}, errors.New("invalid install record")
	}
	if rec.Status == InstallRevoked {
		return InstallRecord{}, ErrRevokedInstall
	}
	if rec.Status != InstallActive {
		return InstallRecord{}, ErrUnknownInstall
	}
	return rec, nil
}

var (
	ErrForbidden          = errors.New("enrollment credential refused")
	ErrEnrollmentConflict = errors.New("enrollment conflicts with an existing install")
	ErrUnknownInstall     = errors.New("unknown install")
	ErrRevokedInstall     = errors.New("this install is revoked")
)

// Refusals the admin API answers with the error's own text, so a kind never shows in the message.
var (
	errInvalidRequest = errors.New("invalid request")
	errStateConflict  = errors.New("state conflict")
)

type refusal struct{ kind, err error }

func (e refusal) Error() string   { return e.err.Error() }
func (e refusal) Unwrap() []error { return []error{e.kind, e.err} }

func invalidRequest(message string) error { return refusal{errInvalidRequest, errors.New(message)} }
func stateConflict(message string) error  { return refusal{errStateConflict, errors.New(message)} }
