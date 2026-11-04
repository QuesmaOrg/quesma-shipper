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

func RevocationRecovery() string {
	return revocationRecovery(packaging.SystemManaged())
}

func revocationRecovery(managed bool) string {
	if managed {
		return "Ask your administrator to supply a new enrollment token or remove this managed installation."
	}
	return "Run `quesma-shipper login <token>` to enroll again, or `quesma-shipper uninstall`."
}

func notifyRevocation(stateDir string) {
	notifyRevocationWith(stateDir, packaging.NotifyRevocation)
}

func notifyRevocationWith(stateDir string, notify func(context.Context, string) bool) {
	unlock, err := controlplane.LockEnrollment(stateDir)
	if err != nil {
		return
	}
	defer unlock()
	var revoked *controlplane.Revocation
	if !errors.As(EnrollmentRevocation(stateDir), &revoked) || revoked.Notified {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if notify(ctx, revoked.Error()+" "+RevocationRecovery()) {
		revoked.Notified = true
		_ = revoked.Save(stateDir)
	}
}
