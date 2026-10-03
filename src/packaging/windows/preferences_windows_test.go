//go:build windows

package windows

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func TestRegistryPolicyReaderBoundsDataBeforeAllocating(t *testing.T) {
	path := fmt.Sprintf(`Software\QuesmaShipperTests\%d-%d`, os.Getpid(), time.Now().UnixNano())
	key, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { key.Close(); _ = registry.DeleteKey(registry.CURRENT_USER, path) })
	if value, err := readRegistryString(key, "Grant"); err != nil || value != "" {
		t.Fatalf("absent policy: value length %d, error %v", len(value), err)
	}
	for _, value := range []string{"grant", strings.Repeat("secret", managedStringLimit)} {
		if err := key.SetStringValue("Grant", value); err != nil {
			t.Fatal(err)
		}
		got, err := readRegistryString(key, "Grant")
		if len(value) < managedStringLimit {
			if err != nil || got != value {
				t.Fatal("valid registry string was not read")
			}
		} else if err == nil || got != "" || strings.Contains(err.Error(), "secret") {
			t.Fatal("oversized registry string was accepted or disclosed")
		}
	}
	if err := key.SetExpandStringValue("Grant", "%sensitive%"); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegistryString(key, "Grant"); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("expandable string was accepted or disclosed")
	}
}
