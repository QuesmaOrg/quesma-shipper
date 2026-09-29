package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"
)

type OrganizationSummary struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

func (m *Manager) ListOrganizations(ctx context.Context) ([]OrganizationSummary, error) {
	const prefix = "v1/organization="
	organizations, err := m.store.ListPrefixes(ctx, prefix, "/")
	if err != nil {
		return nil, err
	}
	out := make([]OrganizationSummary, len(organizations))
	found := make([]bool, len(organizations))
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(16)
	for i, organization := range organizations {
		slug := strings.TrimSuffix(strings.TrimPrefix(organization, prefix), "/")
		if organization != prefix+slug+"/" {
			continue
		}
		if !orgPattern.MatchString(slug) {
			continue
		}
		scoped, _ := m.ForOrganization(slug)
		group.Go(func() error {
			cfg, _, err := scoped.LoadConfig(ctx)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			out[i] = OrganizationSummary{Slug: slug, DisplayName: cfg.DisplayName}
			found[i] = true
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	summaries := make([]OrganizationSummary, 0, len(organizations))
	for i, ok := range found {
		if ok {
			summaries = append(summaries, out[i])
		}
	}
	slices.SortFunc(summaries, func(a, b OrganizationSummary) int { return strings.Compare(a.Slug, b.Slug) })
	return summaries, nil
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
