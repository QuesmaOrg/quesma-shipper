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
	if req.Grant == "" || req.Invite != "" {
		return "", nil, nil, fmt.Errorf("managed enrollment requires a grant")
	}
	endpoint = normalizeEndpoint(endpoint)
	savedEndpoint, body, priv, err := enrollmentAttempt(filepath.Join(stateDir, managedAttemptFile), endpoint, req)
	if err != nil {
		return "", nil, nil, err
	}
	if normalizeEndpoint(savedEndpoint) != endpoint {
		return "", nil, nil, ErrManagedEnrollmentTargetChanged
	}
	var previous EnrollRequest
	if err := json.Unmarshal(body, &previous); err != nil {
		return "", nil, nil, err
	}
	if previous.Grant != req.Grant {
		// Token routing is only a migration guard; the server authenticates the grant.
		if org := grantOrganization(previous.Grant); org == "" || org != grantOrganization(req.Grant) {
			return "", nil, nil, ErrManagedEnrollmentTargetChanged
		}
		previous.Grant = req.Grant
		if body, err = json.Marshal(previous); err != nil {
			return "", nil, nil, err
		}
		if err := saveManagedEnrollmentAttempt(stateDir, savedEndpoint, body, priv); err != nil {
			return "", nil, nil, err
		}
	}
	return savedEndpoint, body, priv, nil
}

func enrollmentAttempt(path, endpoint string, req EnrollRequest) (savedEndpoint string, body []byte, key ed25519.PrivateKey, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("pending enrollment %s: %w", path, err)
		}
	}()
	if endpoint == "" || req.InstallID == "" || req.AgeRecipient == "" || (req.Grant == "") == (req.Invite == "") {
		return "", nil, nil, fmt.Errorf("enrollment request is incomplete")
	}
	raw, err := platform.ReadPrivate(path, 32768)
	if err == nil {
		var saved managedAttempt
		if err := json.Unmarshal(raw, &saved); err != nil {
			return "", nil, nil, err
		}
		var previous EnrollRequest
		if err := json.Unmarshal(saved.Request, &previous); err != nil {
			return "", nil, nil, err
		}
		priv, err := (&Enrollment{DeviceKey: saved.DeviceKey}).PrivateKey()
		if err != nil {
			return "", nil, nil, err
		}
		if previous.InstallID != req.InstallID || previous.AgeRecipient != req.AgeRecipient ||
			previous.DevicePublicKey != EncodeKey(priv.Public().(ed25519.PublicKey)) ||
			(previous.Grant == "") == (previous.Invite == "") || (req.Grant != "" && previous.Grant == "") || saved.Endpoint == "" {
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
	body, err = json.Marshal(req)
	if err != nil {
		return "", nil, nil, err
	}
	if err := platform.WriteJSON(path, managedAttempt{Endpoint: endpoint, DeviceKey: EncodeKey(priv), Request: body}, 0o600); err != nil {
		return "", nil, nil, err
	}
	return endpoint, body, priv, nil
}

// grantOrganization reads a grant from the right, as the server does: organizations may contain dots.
func grantOrganization(grant string) string {
	rest, ok := strings.CutPrefix(grant, "fmi2.")
	secret := strings.LastIndexByte(rest, '.')
	if !ok || secret < 0 {
		return ""
	}
	id := strings.LastIndexByte(rest[:secret], '.')
	if id < 0 {
		return ""
	}
	return rest[:id]
}

func saveManagedEnrollmentAttempt(stateDir, endpoint string, body []byte, priv ed25519.PrivateKey) error {
	return platform.WriteJSON(filepath.Join(stateDir, managedAttemptFile),
		managedAttempt{Endpoint: endpoint, DeviceKey: EncodeKey(priv), Request: body}, 0o600)
}

func ClearManagedEnrollmentAttempt(stateDir string) error {
	return platform.RemoveFile(filepath.Join(stateDir, managedAttemptFile))
}
