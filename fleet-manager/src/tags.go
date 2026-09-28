package main

import (
	"context"
	"errors"
	"maps"

	"golang.org/x/sync/errgroup"
)

// Install names and metadata, written to each install's own root. Reads fan out over the installs list rather
// than listing the payload prefix: the runtime identity is granted a listing of control/ and of
// the organization roots, never of an install's objects.

const tagsReadConcurrency = 16

func (m *Manager) LoadTags(ctx context.Context, installID string) (TagsRecord, string, error) {
	if _, err := uuidParse(installID); err != nil {
		return TagsRecord{}, "", errors.New("install id is not a UUID")
	}
	return getRecord[TagsRecord](ctx, m.store, tagsKey(m.org, installID))
}

// SetTag names an install, or clears the name when given an empty one — the way back to showing the
// install id. A revoked install can still be renamed: names are for reading what it already wrote.
func (m *Manager) SetTag(ctx context.Context, installID, name string) error {
	return m.setTag(ctx, installID, name, false)
}

func (m *Manager) setTag(ctx context.Context, installID, name string, ifMissing bool) error {
	if name != "" {
		if err := validateHumanName("install name", name); err != nil {
			return err
		}
	}
	return m.updateTags(ctx, installID, func(rec *TagsRecord) error {
		if !ifMissing || rec.Name == "" {
			rec.Name = name
		}
		return nil
	})
}

// Retry against the latest record so name changes and disjoint metadata edits never erase one another.
func (m *Manager) updateTags(ctx context.Context, installID string, update func(*TagsRecord) error) error {
	if _, err := uuidParse(installID); err != nil {
		return errors.New("install id is not a UUID")
	}
	if _, _, err := getRecord[InstallRecord](ctx, m.store, installKey(m.org, installID)); err != nil {
		return err
	}
	key := tagsKey(m.org, installID)
	for range 8 {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, version, err := getRecord[TagsRecord](ctx, m.store, key)
		missing := errors.Is(err, ErrNotFound)
		if missing {
			current = TagsRecord{Schema: schemaVersion, InstallID: installID}
		} else if err != nil {
			return err
		} else if current.Schema != schemaVersion || current.InstallID != installID {
			return errors.New("invalid install tags record")
		}
		rec := current
		rec.Metadata = maps.Clone(current.Metadata)
		if err := update(&rec); err != nil {
			return err
		}
		if !missing && rec.Name == current.Name && maps.Equal(rec.Metadata, current.Metadata) {
			return nil
		}
		rec.UpdatedAt = m.time()
		if missing {
			err = createRecord(ctx, m.store, key, rec)
		} else {
			err = replaceRecord(ctx, m.store, key, version, rec)
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
	}
	return ErrConflict
}

// ListTags skips unusable records while retaining existing tags with no name.
func (m *Manager) ListTags(ctx context.Context) ([]TagsRecord, error) {
	installs, err := m.ListInstalls(ctx)
	if err != nil {
		return nil, err
	}
	records := make([]TagsRecord, len(installs))
	found := make([]bool, len(installs))
	var group errgroup.Group
	group.SetLimit(tagsReadConcurrency)
	for i, install := range installs {
		group.Go(func() error {
			rec, _, err := getRecord[TagsRecord](ctx, m.store, tagsKey(m.org, install.InstallID))
			if err != nil || rec.InstallID != install.InstallID || rec.Schema != schemaVersion || validateMetadata(rec.Metadata) != nil {
				return nil
			}
			records[i], found[i] = rec, true
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	out := make([]TagsRecord, 0, len(installs))
	for i, ok := range found {
		if ok {
			out = append(out, records[i])
		}
	}
	return out, nil
}
