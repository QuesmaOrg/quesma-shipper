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
	objects, err := m.store.List(ctx, controlPrefix(m.org)+"grants/")
	if err != nil {
		return nil, err
	}
	out := make([]GrantRecord, 0, len(objects))
	for _, object := range objects {
		rec, _, err := getRecord[GrantRecord](ctx, m.store, object.Key)
		if err != nil {
			return nil, err
		}
		if rec.Schema != schemaVersion {
			return nil, fmt.Errorf("grant %s has unsupported schema", rec.ID)
		}
		rec.SecretDigest = ""
		out = append(out, rec)
	}
	return out, nil
}

func (m *Manager) ListInvites(ctx context.Context) ([]InviteRecord, error) {
	objects, err := m.store.List(ctx, controlPrefix(m.org)+"invites/")
	if err != nil {
		return nil, err
	}
	out := make([]InviteRecord, 0, len(objects))
	for _, object := range objects {
		rec, _, err := getRecord[InviteRecord](ctx, m.store, object.Key)
		if err != nil {
			return nil, err
		}
		if rec.Schema != schemaVersion {
			return nil, fmt.Errorf("invite %s has unsupported schema", rec.ID)
		}
		rec.SecretDigest = ""
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
	if _, err := uuidParse(id); err != nil {
		return errors.New("grant id is not a UUID")
	}
	key := grantKey(m.org, id)
	rec, version, err := getRecord[GrantRecord](ctx, m.store, key)
	if err != nil {
		return err
	}
	if rec.RevokedAt != nil {
		return nil
	}
	now := m.time()
	rec.RevokedAt = &now
	return replaceRecord(ctx, m.store, key, version, rec)
}

func (m *Manager) RevokeInvite(ctx context.Context, id string) error {
	if _, err := uuidParse(id); err != nil {
		return errors.New("invite id is not a UUID")
	}
	key := inviteKey(m.org, id)
	rec, version, err := getRecord[InviteRecord](ctx, m.store, key)
	if err != nil {
		return err
	}
	if rec.RevokedAt != nil {
		return nil
	}
	now := m.time()
	rec.RevokedAt = &now
	return replaceRecord(ctx, m.store, key, version, rec)
}

func (m *Manager) RevokeInstall(ctx context.Context, id string) error {
	if _, err := uuidParse(id); err != nil {
		return errors.New("install id is not a UUID")
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
		return errors.New("invite id is not a UUID")
	}
	key := inviteKey(m.org, id)
	rec, version, err := getRecord[InviteRecord](ctx, m.store, key)
	if err != nil {
		return err
	}
	if rec.SpentAt != nil {
		return errors.New("a spent invite cannot be released")
	}
	if rec.ReservedInstallID == "" {
		return nil
	}
	install, _, installErr := getRecord[InstallRecord](ctx, m.store, installKey(m.org, rec.ReservedInstallID))
	if installErr == nil && install.Status != InstallRevoked {
		return errors.New("reservation install exists and is not revoked")
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
