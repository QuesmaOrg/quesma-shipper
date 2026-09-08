package platform

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// PRETTY_NAME from os-release(5); containers occasionally ship only the /usr/lib copy.
func readOSVersion() string {
	for _, path := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				return strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
	}
	return ""
}

// btime in /proc/stat is absolute epoch seconds, immune to clock steps unlike /proc/uptime.
func readBootTime() string {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil || sec <= 0 {
				return ""
			}
			return time.Unix(sec, 0).UTC().Format(time.RFC3339)
		}
	}
	return ""
}
