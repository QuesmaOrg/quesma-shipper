package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
)

type LoginResult struct {
	Organization string
	Machine      string
	AlreadyIn    bool
}

var ErrAlreadyLoggedIn = errors.New("already logged in")

func Login(ctx context.Context, server, token string) (LoginResult, error) {
	_, paths, err := ResolveEffective()
	if err != nil {
		return LoginResult{}, err
	}
	if existing, err := controlplane.LoadEnrollment(paths.StateDir); err == nil {
		return LoginResult{Organization: existing.Organization, AlreadyIn: true}, ErrAlreadyLoggedIn
	}
	unit, err := loadOrMintIdentity(paths.StateDir)
	if err != nil {
		return LoginResult{}, err
	}
	pub, priv, err := controlplane.NewDeviceKey()
	if err != nil {
		return LoginResult{}, err
	}
	c, err := controlplane.New(controlplane.Options{Endpoint: server})
	if err != nil {
		return LoginResult{}, err
	}
	hostname, _ := os.Hostname()
	req := controlplane.EnrollRequest{
		InstallID:       unit.InstallID.String(),
		DevicePublicKey: controlplane.EncodeKey(pub),
		AgeRecipient:    unit.Recipient().String(),
		Hostname:        hostname,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
	}
	req.Invite = token
	resp, err := c.Enroll(ctx, req)
	if errors.Is(err, formats.ErrCredentialsRefused) {
		req.Invite, req.Grant = "", token
		resp, err = c.Enroll(ctx, req)
	}
	if err != nil {
		return LoginResult{}, err
	}
	rec := controlplane.Enrollment{
		InstallID:    unit.InstallID.String(),
		Organization: resp.Organization,
		Endpoint:     server,
		DeviceKey:    controlplane.EncodeKey(priv),
		EnrolledAt:   controlplane.Now(),
	}
	if err := rec.Save(paths.StateDir); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Organization: resp.Organization, Machine: hostname}, nil
}

func LoggedIn() (organization string, ok bool) {
	_, paths, err := ResolveEffective()
	if err != nil {
		return "", false
	}
	enr, err := controlplane.LoadEnrollment(paths.StateDir)
	if err != nil {
		return "", false
	}
	return enr.Organization, true
}

func LocalDev() (config.Paths, *identity.Unit, error) {
	_, paths, err := ResolveEffective()
	if err != nil {
		return paths, nil, err
	}
	if existing, err := controlplane.LoadEnrollment(paths.StateDir); err == nil {
		return paths, nil, fmt.Errorf("%w with %s as %s", ErrAlreadyLoggedIn, existing.Endpoint, existing.Organization)
	}
	unit, err := loadOrMintIdentity(paths.StateDir)
	return paths, unit, err
}

func loadOrMintIdentity(stateDir string) (*identity.Unit, error) {
	unit, err := identity.Load(stateDir)
	if err == nil {
		return unit, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return identity.Mint(stateDir)
}
