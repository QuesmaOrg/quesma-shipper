package controlplane

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// LockEnrollment serializes managed retries and interactive login in this user's state directory.
func LockEnrollment(stateDir string) (func(), error) {
	if err := platform.EnsureDir(stateDir, 0o700); err != nil {
		return nil, err
	}
	lock, err := platform.OpenTruncating(filepath.Join(stateDir, "enrollment.lock"), 0o600)
	if err != nil {
		return nil, err
	}
	if err := platform.LockFile(lock); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another enrollment is in progress")
	}
	return func() { platform.UnlockFile(lock); lock.Close() }, nil
}

// ManagedEnrollmentKey is saved before the request so a lost response can be retried after restart.
// The caller holds LockEnrollment; the profile grant is never persisted here.
func ManagedEnrollmentKey(stateDir, installID string) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	path := filepath.Join(stateDir, "managed-enrollment-key.json")
	var key struct {
		InstallID string `json:"install_id"`
		DeviceKey string `json:"device_key"`
	}
	raw, err := platform.ReadPrivate(path, 4096)
	if err == nil {
		if err := json.Unmarshal(raw, &key); err != nil {
			return nil, nil, fmt.Errorf("read pending enrollment key: %w", err)
		}
		if key.InstallID != installID {
			return nil, nil, fmt.Errorf("pending enrollment key belongs to a different installation")
		}
		priv, err := (&Enrollment{DeviceKey: key.DeviceKey}).PrivateKey()
		if err != nil {
			return nil, nil, err
		}
		return priv.Public().(ed25519.PublicKey), priv, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	pub, priv, err := NewDeviceKey()
	if err != nil {
		return nil, nil, err
	}
	key.InstallID, key.DeviceKey = installID, EncodeKey(priv)
	if err := platform.WriteJSON(path, key, 0o600); err != nil {
		return nil, nil, err
	}
	return pub, priv, nil
}
