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

type managedAttempt struct {
	Endpoint  string `json:"endpoint"`
	DeviceKey string `json:"device_key"`
	Request   []byte `json:"request"`
}

const managedAttemptFile = "managed-enrollment-attempt.json"

// ManagedEnrollmentAttempt preserves the exact request after response loss; the grant is private
// state until enrollment succeeds. The caller holds LockEnrollment.
func ManagedEnrollmentAttempt(stateDir, endpoint string, req EnrollRequest) (string, []byte, ed25519.PrivateKey, error) {
	if endpoint == "" || req.InstallID == "" || req.AgeRecipient == "" || req.Grant == "" {
		return "", nil, nil, fmt.Errorf("managed enrollment request is incomplete")
	}
	path := filepath.Join(stateDir, managedAttemptFile)
	raw, err := platform.ReadPrivate(path, 32768)
	if err == nil {
		var saved managedAttempt
		if err := json.Unmarshal(raw, &saved); err != nil {
			return "", nil, nil, fmt.Errorf("read pending enrollment request: %w", err)
		}
		var previous EnrollRequest
		if err := json.Unmarshal(saved.Request, &previous); err != nil {
			return "", nil, nil, fmt.Errorf("read pending enrollment request: %w", err)
		}
		priv, err := (&Enrollment{DeviceKey: saved.DeviceKey}).PrivateKey()
		if err != nil {
			return "", nil, nil, err
		}
		if previous.InstallID != req.InstallID || previous.AgeRecipient != req.AgeRecipient ||
			previous.DevicePublicKey != EncodeKey(priv.Public().(ed25519.PublicKey)) ||
			previous.Grant == "" || saved.Endpoint == "" {
			return "", nil, nil, fmt.Errorf("pending enrollment request does not match this installation")
		}
		return saved.Endpoint, saved.Request, priv, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", nil, nil, err
	}
	pub, priv, err := NewDeviceKey()
	if err != nil {
		return "", nil, nil, err
	}
	req.DevicePublicKey = EncodeKey(pub)
	body, err := json.Marshal(req)
	if err != nil {
		return "", nil, nil, err
	}
	if err := platform.WriteJSON(path, managedAttempt{Endpoint: endpoint, DeviceKey: EncodeKey(priv), Request: body}, 0o600); err != nil {
		return "", nil, nil, err
	}
	return endpoint, body, priv, nil
}

func ClearManagedEnrollmentAttempt(stateDir string) error {
	return platform.RemoveFile(filepath.Join(stateDir, managedAttemptFile))
}
