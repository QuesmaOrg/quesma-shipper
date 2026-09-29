package app

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

type LoginResult struct {
	Organization string
	Machine      string
}

var ErrAlreadyLoggedIn = errors.New("already logged in")

func Login(ctx context.Context, server, token string) (LoginResult, error) {
	return login(ctx, server, token, false)
}

// ManagedLogin only enrolls an unconfigured user; profile rotation never replaces device identity.
func ManagedLogin(ctx context.Context) (bool, error) {
	if _, ok := LoggedIn(); ok {
		if _, paths, err := ResolveEffective(); err == nil {
			_ = controlplane.ClearManagedEnrollmentAttempt(paths.StateDir)
		}
		return true, nil
	}
	server, grant, err := packaging.ManagedEnrollment()
	if err != nil || server == "" {
		return false, err
	}
	_, err = login(ctx, server, grant, true)
	if errors.Is(err, ErrAlreadyLoggedIn) {
		return true, nil
	}
	if err != nil {
		return false, managedEnrollmentError(err)
	}
	return true, nil
}

func managedEnrollmentError(err error) error {
	// Response bodies may echo a grant, so only report structured failure details.
	if errors.Is(err, formats.ErrCredentialsRefused) {
		return fmt.Errorf("managed enrollment grant was refused; ask an administrator to replace an expired or revoked grant")
	}
	var httpErr *controlplane.HTTPStatusError
	if errors.As(err, &httpErr) {
		return fmt.Errorf("managed enrollment failed: server returned HTTP %d", httpErr.Status)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Errorf("managed enrollment failed: DNS lookup failed")
	}
	var authorityErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certErr x509.CertificateInvalidError
	if errors.As(err, &authorityErr) || errors.As(err, &hostnameErr) || errors.As(err, &certErr) {
		return fmt.Errorf("managed enrollment failed: TLS certificate validation failed")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("managed enrollment failed: connection timed out")
	}
	return fmt.Errorf("managed enrollment did not complete; check server reachability and the Server/Grant profile")
}

func login(ctx context.Context, server, token string, managed bool) (LoginResult, error) {
	return loginWithRunCheck(ctx, server, token, managed, packaging.ValidateRun)
}

func loginWithRunCheck(ctx context.Context, server, token string, managed bool, validateRun func() error) (LoginResult, error) {
	if err := validateRun(); err != nil {
		return LoginResult{}, err
	}
	_, paths, err := ResolveEffective()
	if err != nil {
		return LoginResult{}, err
	}
	unlock, err := controlplane.LockEnrollment(paths.StateDir)
	if err != nil {
		return LoginResult{}, err
	}
	defer unlock()
	if existing, err := controlplane.LoadEnrollment(paths.StateDir); err == nil {
		_ = controlplane.ClearManagedEnrollmentAttempt(paths.StateDir)
		return LoginResult{Organization: existing.Organization}, ErrAlreadyLoggedIn
	} else if !errors.Is(err, os.ErrNotExist) {
		return LoginResult{}, err
	}
	unit, err := loadOrMintIdentity(paths.StateDir)
	if err != nil {
		return LoginResult{}, err
	}
	hostname, _ := os.Hostname()
	req := controlplane.EnrollRequest{
		InstallID:    unit.InstallID.String(),
		AgeRecipient: unit.Recipient().String(),
		Hostname:     hostname,
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
	}
	endpoint := server
	var body []byte
	var priv []byte
	if managed {
		req.Grant = token
		endpoint, body, priv, err = controlplane.ManagedEnrollmentAttempt(paths.StateDir, server, req)
	} else {
		var pub []byte
		pub, priv, err = controlplane.NewDeviceKey()
		req.DevicePublicKey = controlplane.EncodeKey(pub)
		req.Invite = token
	}
	if err != nil {
		return LoginResult{}, err
	}
	c, err := controlplane.New(controlplane.Options{Endpoint: endpoint})
	if err != nil {
		return LoginResult{}, err
	}
	var resp *controlplane.EnrollResponse
	if managed {
		resp, err = c.EnrollJSON(ctx, body)
	} else {
		resp, err = c.Enroll(ctx, req)
	}
	if !managed && errors.Is(err, formats.ErrCredentialsRefused) {
		req.Invite, req.Grant = "", token
		resp, err = c.Enroll(ctx, req)
	}
	if err != nil {
		return LoginResult{}, err
	}
	rec := controlplane.Enrollment{
		InstallID:    unit.InstallID.String(),
		Organization: resp.Organization,
		Endpoint:     endpoint,
		DeviceKey:    controlplane.EncodeKey(priv),
		EnrolledAt:   controlplane.Now(),
	}
	if err := rec.Save(paths.StateDir); err != nil {
		return LoginResult{}, err
	}
	_ = controlplane.ClearManagedEnrollmentAttempt(paths.StateDir)
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
