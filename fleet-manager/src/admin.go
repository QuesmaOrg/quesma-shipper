package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

type OrganizationSummary struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

func (m *Manager) ListOrganizations(ctx context.Context) ([]OrganizationSummary, error) {
	objects, err := m.store.List(ctx, "v1/organization=")
	if err != nil {
		return nil, err
	}
	out := make([]OrganizationSummary, 0)
	for _, object := range objects {
		const suffix = "/control/config.json"
		if !strings.HasSuffix(object.Key, suffix) {
			continue
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(object.Key, "v1/organization="), suffix)
		if !orgPattern.MatchString(slug) {
			continue
		}
		scoped, _ := m.ForOrganization(slug)
		cfg, _, err := scoped.LoadConfig(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, OrganizationSummary{Slug: slug, DisplayName: cfg.DisplayName})
	}
	slices.SortFunc(out, func(a, b OrganizationSummary) int { return strings.Compare(a.Slug, b.Slug) })
	return out, nil
}

func (m *Manager) ListGrants(ctx context.Context) ([]GrantRecord, error) {
	return listCredentials[GrantRecord](ctx, m, "grants/", "grant")
}

func (m *Manager) ListInvites(ctx context.Context) ([]InviteRecord, error) {
	return listCredentials[InviteRecord](ctx, m, "invites/", "invite")
}

func listCredentials[T any, P credential[T]](ctx context.Context, m *Manager, dir, noun string) ([]T, error) {
	objects, err := m.store.List(ctx, controlPrefix(m.org)+dir)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(objects))
	for _, object := range objects {
		rec, _, err := getRecord[T](ctx, m.store, object.Key)
		if err != nil {
			return nil, err
		}
		shared := P(&rec).credential()
		if shared.Schema != schemaVersion {
			return nil, fmt.Errorf("%s %s has unsupported schema", noun, shared.ID)
		}
		shared.SecretDigest = ""
		out = append(out, rec)
	}
	return out, nil
}

func (m *Manager) ListInstalls(ctx context.Context) ([]InstallRecord, error) {
	objects, err := m.store.List(ctx, controlPrefix(m.org)+"installs/")
	if err != nil {
		return nil, err
	}
	out := make([]InstallRecord, 0, len(objects))
	for _, object := range objects {
		rec, _, err := getRecord[InstallRecord](ctx, m.store, object.Key)
		if err != nil {
			return nil, err
		}
		if rec.Schema != schemaVersion {
			return nil, fmt.Errorf("install %s has unsupported schema", rec.InstallID)
		}
		out = append(out, rec)
	}
	return out, nil
}

func (m *Manager) RevokeGrant(ctx context.Context, id string) error {
	return revokeCredential[GrantRecord](ctx, m, grantKey, id, "grant")
}

func (m *Manager) RevokeInvite(ctx context.Context, id string) error {
	return revokeCredential[InviteRecord](ctx, m, inviteKey, id, "invite")
}

func revokeCredential[T any, P credential[T]](ctx context.Context, m *Manager, keyOf func(org, id string) string, id, noun string) error {
	if _, err := uuidParse(id); err != nil {
		return invalidRequest(noun + " id is not a UUID")
	}
	key := keyOf(m.org, id)
	rec, version, err := getRecord[T](ctx, m.store, key)
	if err != nil {
		return err
	}
	shared := P(&rec).credential()
	if shared.RevokedAt != nil {
		return nil
	}
	now := m.time()
	shared.RevokedAt = &now
	return replaceRecord(ctx, m.store, key, version, rec)
}

func (m *Manager) RevokeInstall(ctx context.Context, id string) error {
	if _, err := uuidParse(id); err != nil {
		return invalidRequest("install id is not a UUID")
	}
	key := installKey(m.org, id)
	rec, version, err := getRecord[InstallRecord](ctx, m.store, key)
	if err != nil {
		return err
	}
	if rec.Status == InstallRevoked {
		return nil
	}
	now := m.time()
	rec.Status, rec.RevokedAt, rec.UpdatedAt = InstallRevoked, &now, now
	return replaceRecord(ctx, m.store, key, version, rec)
}

func (m *Manager) ReleaseInvite(ctx context.Context, id string) error {
	if _, err := uuidParse(id); err != nil {
		return invalidRequest("invite id is not a UUID")
	}
	key := inviteKey(m.org, id)
	rec, version, err := getRecord[InviteRecord](ctx, m.store, key)
	if err != nil {
		return err
	}
	if rec.SpentAt != nil {
		return stateConflict("a spent invite cannot be released")
	}
	if rec.ReservedInstallID == "" {
		return nil
	}
	install, _, installErr := getRecord[InstallRecord](ctx, m.store, installKey(m.org, rec.ReservedInstallID))
	if installErr == nil && install.Status != InstallRevoked {
		return stateConflict("reservation install exists and is not revoked")
	}
	if installErr != nil && !errors.Is(installErr, ErrNotFound) {
		return installErr
	}
	rec.ReservedInstallID, rec.ReservedEnrollmentDigest, rec.ReservedAt = "", "", nil
	return replaceRecord(ctx, m.store, key, version, rec)
}

func recordIDFromKey(key string) string {
	return strings.TrimSuffix(path.Base(key), ".json")
}
