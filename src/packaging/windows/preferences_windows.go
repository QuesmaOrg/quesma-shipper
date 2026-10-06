//go:build windows

package windows

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func readRegistryString(key registry.Key, name string) (string, error) {
	buf := make([]byte, 2*managedStringLimit)
	n, kind, err := key.GetValue(name, buf)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if errors.Is(err, registry.ErrShortBuffer) || n > len(buf) {
		return "", fmt.Errorf("managed setting %s exceeds its size limit", name)
	}
	if err != nil {
		return "", fmt.Errorf("read managed setting %s: %w", name, err)
	}
	return registryString(name, kind, buf[:n])
}

func ManagedEnrollment() (server, grant string, err error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, ManagedPolicyKey, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("open managed enrollment policy: %w", err)
	}
	defer key.Close()
	for _, path := range []string{`MACHINE\Software\Policies\Quesma`, `MACHINE\` + ManagedPolicyKey} {
		if err := protectedObject(path, windows.SE_REGISTRY_KEY, managedRegistryWriteMask); err != nil {
			return "", "", fmt.Errorf("managed enrollment policy permissions: %w", err)
		}
	}
	if err := validateUserIdentity(); err != nil {
		return "", "", err
	}
	return managedEnrollment(func(name string) (string, error) { return readRegistryString(key, name) })
}
