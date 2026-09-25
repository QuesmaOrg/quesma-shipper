package main

import (
	"context"
	"errors"
)

// Install names, written to each install's own root. Reads fan out over the installs list rather
// than listing the payload prefix: the runtime identity is granted a listing of control/ and of
// the organization roots, never of an install's objects.

// SetTag names an install, or clears the name when given an empty one — the way back to showing the
// install id. A revoked install can still be renamed: names are for reading what it already wrote.
func (m *Manager) SetTag(ctx context.Context, installID, name string) error {
	if _, err := uuidParse(installID); err != nil {
		return invalidRequest("install id is not a UUID")
	}
	if name != "" {
		if err := validateHumanName("install name", name); err != nil {
			return invalidRequest(err.Error())
		}
	}
	// The install record proves the id belongs to this organization; without it any UUID would
	// place a file under a root of its choosing.
	if _, _, err := getRecord[InstallRecord](ctx, m.store, installKey(m.org, installID)); err != nil {
		return err
	}
	rec := TagsRecord{Schema: schemaVersion, InstallID: installID, Name: name, UpdatedAt: m.time()}
	current, version, err := getRecord[TagsRecord](ctx, m.store, tagsKey(m.org, installID))
	if errors.Is(err, ErrNotFound) {
		return createRecord(ctx, m.store, tagsKey(m.org, installID), rec)
	}
	if err != nil {
		return err
	}
	if current.Name == name {
		return nil
	}
	return replaceRecord(ctx, m.store, tagsKey(m.org, installID), version, rec)
}

// ListTags drops names it cannot use so their install rows can still render. An install with no
// name is the normal case, not a failure.
func (m *Manager) ListTags(ctx context.Context) ([]TagsRecord, error) {
	installs, err := m.ListInstalls(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(installs))
	for i, install := range installs {
		keys[i] = tagsKey(m.org, install.InstallID)
	}
	// The key names the install, so matching it is matching the install the record claims.
	return readRecords(ctx, m.store, keys, func(rec TagsRecord, key string) bool {
		return rec.Name != "" && tagsKey(m.org, rec.InstallID) == key
	}), nil
}
