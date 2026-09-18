package common

import (
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// scopeMarker is the file a machine-scope installer drops beside the program. While it is there the
// program neither updates nor removes itself: the package that wrote it owns both.
const scopeMarker = "install-scope"

func ManagedInstall(executable string) bool {
	raw, _, err := platform.ReadWhole(filepath.Join(filepath.Dir(executable), scopeMarker), 64)
	return err == nil && strings.TrimSpace(string(raw)) == "machine"
}
