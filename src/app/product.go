package app

import (
	"fmt"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

const (
	Name    = "quesma-shipper"
	AuthEnv = "SHIPPER_AUTH_KEY"
)

// Build carries the values a release injects at link time.
type Build struct {
	// Version is DERIVED, not configured: NewBuild fills it from the binary's own build stamp.
	Version string

	// Release says the version above is a corroborated release stamp; it gates self-update, so a
	// build that cannot prove which release it is never decides it is out of date.
	Release bool
}

// NewBuild describes this binary. The version comes from what the toolchain stamped rather than
// from a flag, so there is exactly one answer and no way for a caller to supply a different one.
func NewBuild() Build {
	return Build{Version: platform.Current().String(), Release: platform.Current().Release}
}

func VersionLine(b Build) string {
	i := platform.Current()
	v := strings.TrimPrefix(b.Version, "v")
	if v == "" || v == "unknown" {
		v = i.Version
	}
	if !b.Release {
		v = "dev"
		if rev := platform.ShortRev(i.Revision); rev != "" {
			v += " " + rev
		}
	}
	return fmt.Sprintf("%s %s (%s %s)", Name, v, osName(i.OS), i.Arch)
}

func osName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	}
	return goos
}
