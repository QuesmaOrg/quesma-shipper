package platform

import (
	"fmt"
	"strconv"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// GetVersionEx reports Windows 8 to binaries without a compatibility manifest; RtlGetVersion reports the real version.
func readOSVersion() string {
	v := windows.RtlGetVersion()
	return fmt.Sprintf("Windows %d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
}

func readBootTime() string {
	return time.Now().Add(-windows.DurationSinceBoot()).UTC().Format(time.RFC3339)
}

// The kernel increments BootId on every boot.
func readBootID() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Session Manager\Memory Management\PrefetchParameters`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	n, _, err := k.GetIntegerValue("BootId")
	if err != nil {
		return ""
	}
	return strconv.FormatUint(n, 10)
}
