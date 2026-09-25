package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const telemetryPath = "/v1/telemetry"

// telemetryCollectorURL is the collector this organization forwards to, and "" is no forwarding.
// Unset is no forwarding too. The server resolves an unset setting to the deployment's default
// collector before asking, and that default is itself empty unless the deployment names one.
func (c FleetConfig) telemetryCollectorURL() string {
	if c.TelemetryCollectorURL == nil {
		return ""
	}
	return *c.TelemetryCollectorURL
}

func normalizeTelemetryCollectorURL(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	original := value
	if !strings.Contains(value, "://") {
		if strings.ContainsAny(value, "/?#@") {
			return "", errors.New("telemetry_collector_url must be a hostname or HTTPS URL")
		}
		value = "https://" + value + telemetryPath
	}
	invalid := errors.New("telemetry_collector_url must be an HTTPS URL without credentials, query or fragment")
	u, err := url.Parse(value)
	if err != nil {
		return "", invalid
	}
	switch {
	case u.Scheme != "https", u.Hostname() == "", u.Opaque != "":
		return "", invalid
	case u.User != nil:
		return "", invalid
	case u.RawQuery != "", u.ForceQuery:
		return "", invalid
	case u.Fragment != "", strings.Contains(original, "#"):
		return "", invalid
	case strings.TrimSpace(original) != original:
		return "", invalid
	}
	if u.Path == "" {
		u.Path = telemetryPath
	}
	return u.String(), nil
}

type telemetryIdentity struct {
	FleetManagerID string `json:"fleet_manager_id"`
	Seed           string `json:"seed"`
}

type telemetryRegistration struct {
	FleetManagerID string `json:"fleet_manager_id"`
	KeyID          string `json:"key_id"`
	PublicKey      string `json:"public_key"`
}

func (i telemetryIdentity) key() (ed25519.PrivateKey, error) {
	id, err := uuid.Parse(i.FleetManagerID)
	if err != nil || id.String() != i.FleetManagerID || id == uuid.Nil {
		return nil, errors.New("telemetry identity requires a canonical nonzero fleet_manager_id UUID")
	}
	seed, err := base64.StdEncoding.DecodeString(i.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("telemetry identity seed must be a base64 32-byte Ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func registration(id string, key ed25519.PrivateKey) telemetryRegistration {
	pub := key.Public().(ed25519.PublicKey)
	digest := sha256.Sum256(pub)
	return telemetryRegistration{id, hex.EncodeToString(digest[:]), base64.StdEncoding.EncodeToString(pub)}
}

func readTelemetryIdentity(raw []byte) (telemetryIdentity, ed25519.PrivateKey, error) {
	var identity telemetryIdentity
	if err := strictDecode(raw, &identity); err != nil {
		return identity, nil, errors.New("invalid telemetry identity JSON")
	}
	key, err := identity.key()
	return identity, key, err
}

// Keep the signing seed outside v1/, which archive readers may be permitted to read.
const telemetryIdentityKey = "private/fleet-manager/telemetry-identity.json"

func (m *Manager) loadTelemetryIdentity(ctx context.Context) (telemetryIdentity, ed25519.PrivateKey, error) {
	raw, _, err := m.store.Get(ctx, telemetryIdentityKey)
	if errors.Is(err, ErrNotFound) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return telemetryIdentity{}, nil, err
		}
		candidate := telemetryIdentity{FleetManagerID: uuid.NewString(), Seed: base64.StdEncoding.EncodeToString(seed)}
		raw, err = json.Marshal(candidate)
		if err != nil {
			return telemetryIdentity{}, nil, err
		}
		if err = m.store.Create(ctx, telemetryIdentityKey, raw); errors.Is(err, ErrConflict) {
			// Another replica created it first; use that one.
			raw, _, err = m.store.Get(ctx, telemetryIdentityKey)
		}
	}
	if err != nil {
		return telemetryIdentity{}, nil, fmt.Errorf("load persistent telemetry identity: %w", err)
	}
	return readTelemetryIdentity(raw)
}

func telemetryForManager(ctx context.Context, manager *Manager) (*telemetryProxy, error) {
	limits := []int{32, 60, 10}
	names := []string{"FLEET_MANAGER_TELEMETRY_CONCURRENCY", "FLEET_MANAGER_TELEMETRY_RATE", "FLEET_MANAGER_TELEMETRY_BURST"}
	for index, name := range names {
		if value := os.Getenv(name); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 10000 {
				return nil, fmt.Errorf("%s must be between 1 and 10000", name)
			}
			limits[index] = parsed
		}
	}
	identity, key, err := manager.loadTelemetryIdentity(ctx)
	if err != nil {
		return nil, err
	}
	return newTelemetryProxy(identity, key, limits[0], limits[1], limits[2]), nil
}

// Public registration material is safe to export; the private seed never enters this response.
func (s *Server) handleTelemetryPublicKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, s.telemetry.identity)
}
