package windows

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

const (
	ManagedInstallKey   = `Software\Quesma\Shipper`
	ManagedInstallValue = "InstallDir"
	ManagedPolicyKey    = `Software\Policies\Quesma\Shipper`
	managedStringLimit  = 8192
)

func registryString(name string, kind uint32, raw []byte) (string, error) {
	if kind != 1 {
		return "", fmt.Errorf("managed setting %s must be REG_SZ", name)
	}
	if len(raw) > 2*managedStringLimit {
		return "", fmt.Errorf("managed setting %s exceeds its size limit", name)
	}
	if len(raw) < 2 || len(raw)%2 != 0 || binary.LittleEndian.Uint16(raw[len(raw)-2:]) != 0 {
		return "", fmt.Errorf("managed setting %s must be a terminated UTF-16 string", name)
	}
	units := make([]uint16, len(raw)/2-1)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(raw[2*i:])
		if units[i] == 0 {
			return "", fmt.Errorf("managed setting %s contains an embedded NUL", name)
		}
	}
	for i := 0; i < len(units); i++ {
		if units[i] >= 0xD800 && units[i] <= 0xDBFF {
			if i+1 == len(units) || units[i+1] < 0xDC00 || units[i+1] > 0xDFFF {
				return "", fmt.Errorf("managed setting %s contains invalid UTF-16", name)
			}
			i++
		} else if units[i] >= 0xDC00 && units[i] <= 0xDFFF {
			return "", fmt.Errorf("managed setting %s contains invalid UTF-16", name)
		}
	}
	value := string(utf16.Decode(units))
	if len(value) >= managedStringLimit {
		return "", fmt.Errorf("managed setting %s exceeds its size limit", name)
	}
	return strings.TrimSpace(value), nil
}

func managedEnrollment(read func(string) (string, error)) (string, string, error) {
	server, err := read("Server")
	if err != nil {
		return "", "", err
	}
	grant, err := read("Grant")
	if err != nil {
		return "", "", err
	}
	if (server == "") != (grant == "") {
		return "", "", fmt.Errorf("managed policy requires both Server and Grant")
	}
	return server, grant, nil
}

func validateInstallIdentity(sid string, groups []string) error {
	if sid == sidLocalSystem || sid == "S-1-5-19" || sid == "S-1-5-20" || strings.HasPrefix(sid, "S-1-5-80-") {
		return errors.New("refusing to collect or enroll as SYSTEM or a Windows service account; run as the signed-in user")
	}
	for _, group := range groups {
		if group == "S-1-5-6" {
			return errors.New("refusing to collect or enroll in a Windows service session")
		}
	}
	return nil
}

func validateIdentity(sid string, groups []string) error {
	if err := validateInstallIdentity(sid, groups); err != nil {
		return err
	}
	for _, group := range groups {
		if group == "S-1-5-4" {
			return nil
		}
	}
	return errors.New("Quesma Shipper requires a signed-in user's interactive session")
}

func checkNoPersonalProgram(dir string) error {
	path, err := existingProgram(dir)
	if err != nil {
		return err
	}
	if path != "" {
		return fmt.Errorf("a personal installation exists at %s; uninstall it without purging local state", path)
	}
	return nil
}

func checkNoManagedProgram(dir string) error {
	path, err := existingProgram(dir)
	if err != nil {
		return fmt.Errorf("inspect managed installation: %w", err)
	}
	if path != "" {
		return common.ErrSystemManaged
	}
	return nil
}

func existingProgram(dir string) (string, error) {
	for _, name := range []string{"quesma-shipper.exe", "unins000.exe"} {
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); err == nil {
			return path, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}
