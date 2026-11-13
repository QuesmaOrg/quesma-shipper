package controlplane

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

const interactiveAttemptFile = "interactive-enrollment-attempt.json"

// EnrollInteractive keeps the exact request across response loss, including an invite's single use.
func EnrollInteractive(ctx context.Context, stateDir, endpoint, token string, req EnrollRequest) (response *EnrollResponse, usedEndpoint string, key ed25519.PrivateKey, err error) {
	path := filepath.Join(stateDir, interactiveAttemptFile)
	defer func() {
		if err != nil {
			err = fmt.Errorf("pending enrollment %s: %w", path, err)
		}
	}()
	req.Invite, req.Grant = token, ""
	savedEndpoint, body, priv, err := enrollmentAttempt(path, endpoint, req)
	if err != nil {
		return nil, "", nil, err
	}
	if strings.TrimRight(savedEndpoint, "/") != strings.TrimRight(endpoint, "/") {
		// A corrected server cannot have consumed the request sent to the previous one.
		if err := platform.RemoveFile(path); err != nil {
			return nil, "", nil, err
		}
		savedEndpoint, body, priv, err = enrollmentAttempt(path, endpoint, req)
		if err != nil {
			return nil, "", nil, err
		}
	}
	req = EnrollRequest{}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", nil, err
	}
	client, err := New(Options{Endpoint: savedEndpoint})
	if err != nil {
		return nil, "", nil, err
	}
	for {
		resp, err := client.EnrollJSON(ctx, body)
		if !errors.Is(err, formats.ErrCredentialsRefused) {
			return resp, savedEndpoint, priv, err
		}
		if req.Invite != "" {
			req.Grant, req.Invite = req.Invite, ""
		} else if req.Grant != token {
			req.Invite, req.Grant = token, ""
		} else {
			return nil, "", nil, err
		}
		body, err = json.Marshal(req)
		if err != nil {
			return nil, "", nil, err
		}
		if err := platform.WriteJSON(path, managedAttempt{Endpoint: savedEndpoint, DeviceKey: EncodeKey(priv), Request: body}, 0o600); err != nil {
			return nil, "", nil, err
		}
	}
}
