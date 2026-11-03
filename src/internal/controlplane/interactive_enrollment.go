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

// EnrollInteractive keeps the exact request across response loss, including an invite's single use.
func EnrollInteractive(ctx context.Context, stateDir, endpoint, token string, req EnrollRequest) (*EnrollResponse, string, ed25519.PrivateKey, error) {
	const attemptFile = "interactive-enrollment-attempt.json"
	path := filepath.Join(stateDir, attemptFile)
	var attempt managedAttempt
	raw, err := platform.ReadPrivate(path, 32768)
	if err == nil {
		if err := json.Unmarshal(raw, &attempt); err != nil {
			return nil, "", nil, err
		}
		var previous EnrollRequest
		if err := json.Unmarshal(attempt.Request, &previous); err != nil {
			return nil, "", nil, err
		}
		if previous.InstallID != req.InstallID || previous.AgeRecipient != req.AgeRecipient {
			return nil, "", nil, fmt.Errorf("pending enrollment belongs to another identity")
		}
		req = previous
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", nil, err
	} else {
		pub, priv, err := NewDeviceKey()
		if err != nil {
			return nil, "", nil, err
		}
		req.DevicePublicKey, req.Invite = EncodeKey(pub), token
		attempt.Endpoint, attempt.DeviceKey = endpoint, EncodeKey(priv)
	}
	priv, err := (&Enrollment{DeviceKey: attempt.DeviceKey}).PrivateKey()
	if err != nil {
		return nil, "", nil, err
	}
	if req.DevicePublicKey != EncodeKey(priv.Public().(ed25519.PublicKey)) {
		return nil, "", nil, fmt.Errorf("pending enrollment signing key does not match")
	}
	for {
		attempt.Request, err = json.Marshal(req)
		if err != nil {
			return nil, "", nil, err
		}
		if err = platform.WriteJSON(path, attempt, 0o600); err != nil {
			return nil, "", nil, err
		}
		client, err := New(Options{Endpoint: attempt.Endpoint})
		if err != nil {
			return nil, "", nil, err
		}
		resp, err := client.EnrollJSON(ctx, attempt.Request)
		if !errors.Is(err, formats.ErrCredentialsRefused) {
			return resp, attempt.Endpoint, priv, err
		}
		if req.Invite != "" {
			req.Grant, req.Invite = req.Invite, ""
		} else if req.Grant != token || attempt.Endpoint != endpoint {
			req.Invite, req.Grant, attempt.Endpoint = token, "", endpoint
		} else {
			return nil, "", nil, err
		}
	}
}
