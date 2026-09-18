//go:build windows

package windows

import (
	"os"
	"path/filepath"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

// ProvisioningPath is where the machine installer leaves the enrollment every user's first run reads.
func ProvisioningPath() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "Quesma Shipper", common.ProvisioningFile)
}
