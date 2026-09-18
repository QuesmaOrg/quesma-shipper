package app

import (
	"context"

	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

// Provision enrolls from the file an administrator's installer left for every user of this machine,
// so a fleet push needs nobody at the keyboard. os.ErrNotExist means there is no such file.
func Provision(ctx context.Context) (LoginResult, error) {
	p, err := packaging.MachineProvisioning()
	if err != nil {
		return LoginResult{}, err
	}
	return Login(ctx, p.Server, p.Token)
}
