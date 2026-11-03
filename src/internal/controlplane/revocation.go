package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

const RevocationFile = "revocation.json"

// Revocation records observation time: the server does not disclose when an administrator revoked it.
type Revocation struct {
	InstallID    string    `json:"install_id"`
	Organization string    `json:"organization"`
	Endpoint     string    `json:"endpoint"`
	DetectedAt   time.Time `json:"detected_at"`
	Notified     bool      `json:"notified,omitempty"`
}

func (r *Revocation) Error() string {
	return fmt.Sprintf("This machine's install was revoked from %s (detected %s). Nothing is being collected. Run `quesma-shipper login <token>` to enroll again, or `quesma-shipper uninstall`.", r.Organization, r.DetectedAt.UTC().Format(time.RFC3339))
}

func (r *Revocation) Unwrap() error { return formats.ErrCredentialsRefused }

func (r *Revocation) Save(stateDir string) error {
	return platform.WriteJSON(filepath.Join(stateDir, RevocationFile), r, 0o600)
}

func LoadRevocation(stateDir string, enrollment *Enrollment) (*Revocation, error) {
	if enrollment == nil {
		return nil, nil
	}
	raw, err := platform.ReadPrivate(filepath.Join(stateDir, RevocationFile), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Revocation
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("read revocation: %w", err)
	}
	if r.InstallID == "" || r.Organization == "" || r.Endpoint == "" || r.DetectedAt.IsZero() {
		return nil, errors.New("revocation record is incomplete; collection is stopped")
	}
	if r.InstallID != enrollment.InstallID || r.Organization != enrollment.Organization || strings.TrimSuffix(r.Endpoint, "/") != strings.TrimSuffix(enrollment.Endpoint, "/") {
		return nil, nil
	}
	return &r, nil
}

func (c *Client) Revocation() error {
	if r := c.revoked.Load(); r != nil {
		return r
	}
	if c.stateDir == "" {
		return nil
	}
	r, err := LoadRevocation(c.stateDir, &Enrollment{InstallID: c.installID, Organization: c.organization, Endpoint: c.endpoint})
	if err != nil || r == nil {
		return err
	}
	return r
}

func (c *Client) recordRevocation() error {
	r := &Revocation{InstallID: c.installID, Organization: c.organization, Endpoint: c.endpoint, DetectedAt: time.Now().UTC()}
	c.revoked.Store(r)
	if c.stateDir == "" {
		return r
	}
	unlock, err := LockEnrollment(c.stateDir)
	if err != nil {
		return errors.Join(r, err)
	}
	defer unlock()
	if previous, err := LoadRevocation(c.stateDir, &Enrollment{InstallID: c.installID, Organization: c.organization, Endpoint: c.endpoint}); err != nil {
		return errors.Join(r, err)
	} else if previous != nil {
		c.revoked.Store(previous)
		return previous
	}
	if err := r.Save(c.stateDir); err != nil {
		return errors.Join(r, fmt.Errorf("persist revocation: %w", err))
	}
	return r
}
