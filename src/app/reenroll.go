package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// The enrollment record commits the replacement last; an interrupted recovery remains revoked.
func reenroll(ctx context.Context, stateDir, server, token string, old *controlplane.Enrollment) (LoginResult, error) {
	store, err := engine.Open(stateDir, old.InstallID)
	if err != nil {
		return LoginResult{}, err
	}
	defer store.Close()
	if err := archiveRevokedState(stateDir, old.InstallID); err != nil {
		return LoginResult{}, err
	}
	pending := filepath.Join(stateDir, "re-enrollment", old.InstallID)
	unit, err := loadOrMintIdentity(pending)
	if err != nil {
		return LoginResult{}, err
	}
	hostname, _ := os.Hostname()
	resp, endpoint, priv, err := controlplane.EnrollInteractive(ctx, pending, server, token, controlplane.EnrollRequest{
		InstallID: unit.InstallID.String(), AgeRecipient: unit.Recipient().String(), Hostname: hostname, Platform: runtime.GOOS + "/" + runtime.GOARCH,
	})
	if err != nil {
		return LoginResult{}, err
	}
	raw, err := platform.ReadPrivate(filepath.Join(pending, identity.FileName), 1<<20)
	if err != nil {
		return LoginResult{}, err
	}
	if err := platform.WriteAtomic(filepath.Join(stateDir, identity.FileName), raw, 0o600); err != nil {
		return LoginResult{}, err
	}
	if err := platform.RemoveFile(filepath.Join(stateDir, controlplane.CacheFile)); err != nil {
		return LoginResult{}, err
	}
	rec := controlplane.Enrollment{InstallID: unit.InstallID.String(), Organization: resp.Organization, Endpoint: endpoint, DeviceKey: controlplane.EncodeKey(priv), EnrolledAt: controlplane.Now()}
	if err := rec.Save(stateDir); err != nil {
		return LoginResult{}, err
	}
	_ = platform.RemoveFile(filepath.Join(pending, "interactive-enrollment-attempt.json"))
	return LoginResult{Organization: resp.Organization, Machine: hostname}, nil
}

func archiveRevokedState(stateDir, installID string) error {
	if filepath.Base(installID) != installID || installID == "." || installID == ".." {
		return fmt.Errorf("invalid revoked install ID")
	}
	archive := filepath.Join(stateDir, "revoked-installs", installID)
	if err := platform.EnsureDir(archive, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}
		destination := filepath.Join(archive, entry.Name())
		if _, err := os.Stat(destination); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("cannot archive non-regular state artifact %s", entry.Name())
		}
		raw, err := os.ReadFile(filepath.Join(stateDir, entry.Name()))
		if err != nil {
			return err
		}
		if err := platform.WriteAtomic(destination, raw, 0o600); err != nil {
			return err
		}
	}
	return nil
}
