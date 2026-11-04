package controlplane

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

var ErrManagedEnrollmentTargetChanged = errors.New("pending managed enrollment cannot change server or organization; ask an administrator to recover the original enrollment")

// EnrollManaged persists policy changes before sending; the caller holds LockEnrollment.
func EnrollManaged(ctx context.Context, stateDir, endpoint string, req EnrollRequest) (*EnrollResponse, string, ed25519.PrivateKey, error) {
	savedEndpoint, body, priv, err := ManagedEnrollmentAttempt(stateDir, endpoint, req)
	if err != nil {
		return nil, "", nil, err
	}
	c, err := New(Options{Endpoint: savedEndpoint})
	if err != nil {
		return nil, "", nil, err
	}
	resp, err := c.EnrollJSON(ctx, body)
	return resp, savedEndpoint, priv, err
}

// ManagedEnrollmentAttempt keeps identity and request bytes across retries, replacing only a
// rotated grant for the same destination. The caller holds LockEnrollment.
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
		if saved.Endpoint != endpoint {
			return "", nil, nil, ErrManagedEnrollmentTargetChanged
		}
		if previous.Grant != req.Grant {
			// Token routing is only a migration guard; the server authenticates the grant.
			oldParts, newParts := strings.Split(previous.Grant, "."), strings.Split(req.Grant, ".")
			if len(oldParts) != 4 || len(newParts) != 4 || oldParts[0] != "fmi2" || newParts[0] != "fmi2" ||
				oldParts[1] == "" || oldParts[1] != newParts[1] {
				return "", nil, nil, ErrManagedEnrollmentTargetChanged
			}
			previous.Grant = req.Grant
			saved.Request, err = json.Marshal(previous)
			if err != nil {
				return "", nil, nil, err
			}
			if err := saveManagedEnrollmentAttempt(stateDir, saved.Endpoint, saved.Request, priv); err != nil {
				return "", nil, nil, err
			}
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
