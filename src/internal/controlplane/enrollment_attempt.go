package controlplane

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
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

// EnrollManaged retries saved requests exactly until a definitive credential refusal permits
// adopting a replacement policy grant. The caller holds LockEnrollment.
func EnrollManaged(ctx context.Context, stateDir, endpoint string, req EnrollRequest) (*EnrollResponse, string, ed25519.PrivateKey, error) {
	savedEndpoint, body, priv, err := ManagedEnrollmentAttempt(stateDir, endpoint, req)
	if err != nil {
		return nil, "", nil, err
	}
	send := func(endpoint string, body []byte) (*EnrollResponse, error) {
		c, err := New(Options{Endpoint: endpoint})
		if err != nil {
			return nil, err
		}
		return c.EnrollJSON(ctx, body)
	}
	resp, err := send(savedEndpoint, body)
	if !errors.Is(err, formats.ErrCredentialsRefused) {
		return resp, savedEndpoint, priv, err
	}
	var replacement EnrollRequest
	if decodeErr := json.Unmarshal(body, &replacement); decodeErr != nil {
		return nil, "", nil, decodeErr
	}
	if replacement.Grant == req.Grant {
		return nil, savedEndpoint, priv, err
	}
	replacement.Grant = req.Grant
	candidate, err := json.Marshal(replacement)
	if err != nil {
		return nil, "", nil, err
	}
	if err := saveManagedEnrollmentAttempt(stateDir, endpoint, candidate, priv); err != nil {
		return nil, "", nil, err
	}
	resp, err = send(endpoint, candidate)
	if errors.Is(err, ErrEnrollmentConflict) {
		// Enrollment's 409 can mean the original request already created this install.
		if restoreErr := saveManagedEnrollmentAttempt(stateDir, savedEndpoint, body, priv); restoreErr != nil {
			return nil, "", nil, errors.Join(err, restoreErr)
		}
	}
	return resp, endpoint, priv, err
}

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
	if err := saveManagedEnrollmentAttempt(stateDir, endpoint, body, priv); err != nil {
		return "", nil, nil, err
	}
	return endpoint, body, priv, nil
}

func saveManagedEnrollmentAttempt(stateDir, endpoint string, body []byte, priv ed25519.PrivateKey) error {
	return platform.WriteJSON(filepath.Join(stateDir, managedAttemptFile),
		managedAttempt{Endpoint: endpoint, DeviceKey: EncodeKey(priv), Request: body}, 0o600)
}

func ClearManagedEnrollmentAttempt(stateDir string) error {
	return platform.RemoveFile(filepath.Join(stateDir, managedAttemptFile))
}
