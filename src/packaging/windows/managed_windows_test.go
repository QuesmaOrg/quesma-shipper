//go:build windows

package windows

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
		{"inherited administrator rights", "O:BAG:BAD:P(A;;FA;;;SY)(A;OICIIO;GA;;;BA)(A;;FR;;;BU)", false},
		{"inherited read and execute", "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;OICIIO;GRGX;;;BU)", false},
		{"inherit-only without propagation", "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;IO;GA;;;BU)", false},
		{"future file writes", "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;BU)(A;OIIO;FW;;;BU)", true},
		{"future directory writes", "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;BU)(A;CIIO;FW;;;BU)", true},
		{"future immediate file writes", "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;OINPIO;FW;;;BU)", true},
		{"future immediate directory writes", "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;CINPIO;FW;;;BU)", true},
		{"future inherited generic writes", "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;OICIIOID;GW;;;BU)", true},
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
	for _, rights := range []string{"KR", "KW", "KA"} {
		t.Run(rights, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;;KA;;;SY)(A;;KA;;;BA)(A;;" + rights + ";;;BU)")
			if err != nil {
				t.Fatal(err)
			}
			if err := validateManagedACL(sd, managedRegistryWriteMask); (err != nil) != (rights != "KR") {
				t.Fatalf("managed registry permissions = %v", err)
			}
		})
	}
}

func TestManagedRegistryValidatesFutureSubkeyPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, template string
		wantErr        bool
	}{
		{"creator owner default", "(A;CIIO;KA;;;CO)", false},
		{"inherited read", "(A;CIIO;KR;;;BU)", false},
		{"future subkey writes", "(A;CIIO;KW;;;BU)", true},
		{"future subkey full control", "(A;CIIO;KA;;;BU)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;;KA;;;SY)(A;;KA;;;BA)(A;;KR;;;BU)" + tc.template)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateManagedACL(sd, managedRegistryWriteMask); (err != nil) != tc.wantErr {
				t.Fatalf("managed registry inheritance = %v", err)
			}
		})
	}
}

func TestManagedMissingDestinationRequiresProtectedParentInheritance(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("creating an administrator-owned installation parent requires elevation")
	}
	for _, tc := range []struct {
		name, template string
		wantErr        bool
	}{
		{"safe default inheritance", "", false},
		{"unsafe file inheritance", "(A;OIIO;FW;;;BU)", true},
		{"unsafe directory inheritance", "(A;CIIO;FW;;;BU)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			sd, err := windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FRFX;;;BU)(A;OICIIO;GA;;;CO)" + tc.template)
			if err != nil {
				t.Fatal(err)
			}
			owner, _, err := sd.Owner()
			if err != nil {
				t.Fatal(err)
			}
			acl, _, err := sd.DACL()
			if err != nil {
				t.Fatal(err)
			}
			if err := windows.SetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT,
				windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
				owner, nil, acl, nil); err != nil {
				t.Fatal(err)
			}
			err = validateManagedProgramPaths(filepath.Join(parent, "Quesma Shipper"), false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("preflight with missing destination = %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "modified by non-administrator "+sidUsers) {
				t.Fatalf("expected inherited Users write permission rejection, got %v", err)
			}
		})
	}
}

func TestManagedInstallerRejectsUserProfileTargets(t *testing.T) {
	if err := ValidateManagedInstallDir(t.TempDir(), false); err == nil {
		t.Fatal("managed installation accepted a user-owned profile location")
	}
}

func TestPersonalSetupIgnoresEmptyManagedDirectoryOwner(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("creating a Program Files fixture requires elevation")
	}
	if SystemManaged() {
		t.Skip("requires no managed installation")
	}
	if err := checkNoManagedTask(); err != nil {
		t.Fatal(err)
	}
	dir, err := DefaultManagedInstallDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); errors.Is(err, os.ErrExist) {
		t.Skip("requires an absent managed directory")
	} else if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(dir); err != nil {
			t.Error(err)
		}
	})
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateManagedInstallDir(dir, false); err == nil {
		t.Fatal("fixture unexpectedly satisfies managed ownership requirements")
	}
	if err := PrepareUserInstall(t.TempDir()); err != nil {
		t.Fatalf("unrelated empty machine directory blocked personal setup: %v", err)
	}
}
