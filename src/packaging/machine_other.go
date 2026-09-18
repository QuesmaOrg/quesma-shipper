//go:build !windows

package packaging

import (
	"os"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func SupersededByMachineInstall() bool { return false }

// No installer writes a provisioning file on this OS yet.
func machineProvisioning() (common.Provisioning, error) { return common.Provisioning{}, os.ErrNotExist }
