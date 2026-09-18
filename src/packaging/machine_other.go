//go:build !windows

package packaging

import (
	"fmt"
	"os"
	"runtime"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func InstallMachineService() error {
	return fmt.Errorf("a machine-scope service is not supported on %s", runtime.GOOS)
}

func SupersededByMachineInstall() bool { return false }

// No installer writes a provisioning file on this OS yet.
func machineProvisioning() (common.Provisioning, error) { return common.Provisioning{}, os.ErrNotExist }
