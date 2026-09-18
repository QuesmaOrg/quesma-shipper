package packaging

import (
	"errors"
	"sync"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

// ErrManagedInstall is what update and removal return when a machine-scope package owns this program.
var ErrManagedInstall = errors.New("this install is managed by a machine package, which updates and " +
	"removes it (Settings > Apps, or device management)")

var managedOnce = sync.OnceValue(func() bool {
	exe, err := common.CurrentExecutable()
	return err == nil && common.ManagedInstall(exe)
})

// ManagedInstall reports whether a machine-scope package put this program here. Read once: the
// marker cannot change under a running process.
func ManagedInstall() bool { return managedOnce() }

// MachineProvisioning reads the enrollment an administrator's installer left for every user of this
// machine. A missing file is os.ErrNotExist; one anybody else could have written is an error.
func MachineProvisioning() (common.Provisioning, error) { return machineProvisioning() }
