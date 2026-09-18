//go:build windows

package packaging

import (
	"os"
	"path/filepath"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
	windowspkg "github.com/QuesmaOrg/quesma-shipper/packaging/windows"
)

func InstallMachineService() error {
	exe, err := common.CurrentExecutable()
	if err != nil {
		return err
	}
	return windowspkg.InstallMachineService(exe)
}

// SupersededByMachineInstall reports a per-user copy that its own task started while a machine-scope
// install already serves this user: two shippers would spend their lives fighting over the state lock.
func SupersededByMachineInstall() bool {
	if ManagedInstall() || os.Getenv(common.SupervisedEnv) == "" {
		return false
	}
	return common.ManagedInstall(filepath.Join(os.Getenv("ProgramFiles"), "Quesma Shipper", "quesma-shipper.exe"))
}

func machineProvisioning() (common.Provisioning, error) {
	return common.ReadProvisioning(windowspkg.ProvisioningPath(), windowspkg.TrustedMachineFile)
}
