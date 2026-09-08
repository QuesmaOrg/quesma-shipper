package platform

import (
	"time"

	"golang.org/x/sys/unix"
)

// kern.osproductversion is the marketing version ("26.5.1"), unlike kern.osrelease (kernel).
func readOSVersion() string {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil || v == "" {
		return ""
	}
	return "macOS " + v
}

func readBootTime() string {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil || tv.Sec <= 0 {
		return ""
	}
	return time.Unix(tv.Sec, 0).UTC().Format(time.RFC3339)
}
