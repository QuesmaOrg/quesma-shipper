package packaging

import (
	"os"

	"github.com/QuesmaOrg/quesma-shipper/packaging/macos"
)

func ManagedRunLog(stateDir string) (*os.File, error) { return macos.ManagedRunLog(stateDir) }

func ManagedEnrollment() (server, grant string, err error) { return macos.ManagedEnrollment() }
func SystemManaged() bool                                  { return macos.SystemManaged() }
func ValidateUserUninstall() error                         { return macos.ValidateUserUninstall() }
func ValidateRun() error                                   { return macos.ValidateRun() }
