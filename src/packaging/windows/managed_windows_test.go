//go:build windows

package windows

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func TestManagedRegistryPreflightAcceptsWindowsDefaultParent(t *testing.T) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Quesma`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err == nil {
		key.Close()
		t.Skip("read-only preflight test requires no existing Quesma registry subtree")
	}
	if !errors.Is(err, registry.ErrNotExist) {
		t.Fatal(err)
	}
	if err := validateExistingManagedRegistry(); err != nil {
		t.Fatalf("fresh installation rejected the Windows registry defaults: %v", err)
	}
}

func TestManagedProgramRequiresProtectedOwnersAndWriters(t *testing.T) {
	for _, tc := range []struct {
		name, sddl string
		wantErr    bool
	}{
		{"administrators and SYSTEM", "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;BU)", false},
		{"inherited creator rights", "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;OICIIO;GA;;;CO)(A;;FR;;;BU)", false},
		{"user writes", "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FW;;;BU)", true},
		{"user owns", "O:S-1-5-21-111-222-333-1001G:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;BU)", true},
		{"NULL DACL", "O:BAG:BAD:NO_ACCESS_CONTROL", true},
		{"everyone full control", "O:BAG:BAD:P(A;;FA;;;WD)", true},
		{"effective creator rights", "O:BAG:BAD:P(A;;FA;;;SY)(A;;GA;;;CO)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateManagedACL(sd, writeMask); (err != nil) != tc.wantErr {
				t.Fatalf("managed program permissions = %v", err)
			}
		})
	}
}

func TestManagedRegistryAllowsQueriesButRejectsPolicyWriters(t *testing.T) {
	mask := uint32(registry.SET_VALUE|registry.CREATE_SUB_KEY|registry.CREATE_LINK) | standardDelete | standardWriteDAC | standardWriteOwner | genericWrite | genericAll
	for _, rights := range []string{"KR", "KW", "KA"} {
		t.Run(rights, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;;KA;;;SY)(A;;KA;;;BA)(A;;" + rights + ";;;BU)")
			if err != nil {
				t.Fatal(err)
			}
			if err := validateManagedACL(sd, mask); (err != nil) != (rights != "KR") {
				t.Fatalf("managed registry permissions = %v", err)
			}
		})
	}
}

func TestManagedInstallerRejectsUserProfileTargets(t *testing.T) {
	if err := ValidateManagedInstallDir(t.TempDir(), false); err == nil {
		t.Fatal("managed installation accepted a user-owned profile location")
	}
}
