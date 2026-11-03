package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

type configAuthor string

const (
	configAuthorAdministrator configAuthor = "administrator"
	configAuthorPartner       configAuthor = "partner"
	configAuthorUnknown       configAuthor = "unknown"
	configAuthorUnavailable   configAuthor = "unavailable"
)

// The record versions itself, so a FleetConfig schema change leaves existing records readable.
const configAuthorshipSchema = 1

type configAuthorship struct {
	Schema    int          `json:"schema"`
	Author    configAuthor `json:"author"`
	UpdatedAt time.Time    `json:"updated_at"`
}

func configAuthorOf(r *http.Request) configAuthor {
	switch callerOf(r).who {
	case callerAdmin:
		return configAuthorAdministrator
	case callerPartner:
		return configAuthorPartner
	default:
		return configAuthorUnknown
	}
}

func configAuthorshipKey(org string, raw []byte) string {
	digest := sha256.Sum256(raw)
	return controlPrefix(org) + "config-authorship/" + hex.EncodeToString(digest[:]) + ".json"
}

func (m *Manager) prepareConfigWrite(ctx context.Context, author configAuthor, cfg FleetConfig) ([]byte, error) {
	if author != configAuthorAdministrator && author != configAuthorPartner {
		return nil, errors.New("configuration author must be administrator or partner")
	}
	raw, err := encodeRecord(cfg)
	if err != nil {
		return nil, err
	}
	key := configAuthorshipKey(m.org, raw)
	record := configAuthorship{Schema: configAuthorshipSchema, Author: author, UpdatedAt: cfg.UpdatedAt}
	// Prepare attribution before committing; unused records never describe the active configuration.
	if err := createRecord(ctx, m.store, key, record); errors.Is(err, ErrConflict) {
		existing, _, err := getRecord[configAuthorship](ctx, m.store, key)
		if err != nil {
			return nil, fmt.Errorf("read configuration authorship: %w", err)
		}
		if existing.Schema != record.Schema || existing.Author != record.Author || !existing.UpdatedAt.Equal(record.UpdatedAt) {
			return nil, errors.New("configuration authorship conflicts with an existing record")
		}
	} else if err != nil {
		return nil, fmt.Errorf("record configuration authorship: %w", err)
	}
	return raw, nil
}

// A bad authorship record never fails the read, since GET /config issues the ETag every edit needs.
func (m *Manager) LoadConfigWithAuthor(ctx context.Context, logger *log.Logger) (FleetConfig, string, configAuthor, error) {
	cfg, version, raw, err := m.loadConfig(ctx)
	if err != nil {
		return FleetConfig{}, "", "", err
	}
	record, _, err := getRecord[configAuthorship](ctx, m.store, configAuthorshipKey(m.org, raw))
	if errors.Is(err, ErrNotFound) {
		return cfg, version, configAuthorUnknown, nil
	}
	if err == nil && (record.Schema != configAuthorshipSchema || (record.Author != configAuthorAdministrator && record.Author != configAuthorPartner) || !record.UpdatedAt.Equal(cfg.UpdatedAt)) {
		err = errors.New("stored configuration authorship is invalid")
	}
	if err != nil {
		logger.Printf("organization %s: configuration authorship is unavailable: %v", m.org, err)
		return cfg, version, configAuthorUnavailable, nil
	}
	return cfg, version, record.Author, nil
}
