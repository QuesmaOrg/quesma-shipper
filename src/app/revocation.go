package app

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

func EnrollmentRevocation(stateDir string) error {
	enrollment, err := controlplane.LoadEnrollment(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	revoked, err := controlplane.LoadRevocation(stateDir, enrollment)
	if err != nil {
		return err
	}
	if revoked != nil {
		return revoked
	}
	return nil
}

func notifyRevocation(stateDir string) {
	notifyRevocationWith(stateDir, packaging.NotifyRevocation)
}

func notifyRevocationWith(stateDir string, notify func(context.Context, string)) {
	unlock, err := controlplane.LockEnrollment(stateDir)
	if err != nil {
		return
	}
	var revoked *controlplane.Revocation
	if !errors.As(EnrollmentRevocation(stateDir), &revoked) || revoked.Notified {
		unlock()
		return
	}
	// Claim before attempting desktop delivery: unattended services may have no desktop session.
	revoked.Notified = true
	err = revoked.Save(stateDir)
	unlock()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	notify(ctx, revoked.Error())
}
