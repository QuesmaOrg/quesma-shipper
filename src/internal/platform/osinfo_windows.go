package platform

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

// GetVersionEx reports Windows 8 to binaries without a compatibility manifest; RtlGetVersion reports the real version.
func readOSVersion() string {
	v := windows.RtlGetVersion()
	return fmt.Sprintf("Windows %d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
}

func readBootTime() string {
	return time.Now().Add(-windows.DurationSinceBoot()).UTC().Format(time.RFC3339)
}
