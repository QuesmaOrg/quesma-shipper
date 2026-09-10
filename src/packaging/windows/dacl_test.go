package windows

import (
	"strings"
	"testing"
)

const (
	sidInstaller          = "S-1-5-21-2214759596-1652326523-2275886453-1001"
	sidUsers              = "S-1-5-32-545"
	sidAuthenticatedUsers = "S-1-5-11"
	sidInteractive        = "S-1-5-4"
)

// The masks an ACL listing reports: ReadAndExecute, the generic read/execute an inherit-only entry
// carries, Modify and FullControl.
const (
	maskReadExecute        = 0x001200A9
	maskGenericReadExecute = 0xA0000000
	maskModify             = 0x001301BF
	maskFullControl        = 0x001F01FF
)

// The fixtures below are the DACLs of real directories, so a rule change has to argue with the
// paths users actually pick.
func TestUntrustedWritersJudgesRealDirectoryLayouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		aces []ace
		want []string
	}{
		{
			name: `%LOCALAPPDATA%\Programs\Quesma Shipper`,
			aces: []ace{
				{SID: sidLocalSystem, Mask: maskFullControl, Allow: true},
				{SID: sidAdministrators, Mask: maskFullControl, Allow: true},
				{SID: sidInstaller, Mask: maskFullControl, Allow: true},
			},
		},
		{
			// CREATOR OWNER holds GENERIC_ALL here, but inherit-only and on a directory only an
			// elevated administrator can create in: neither half is a finding.
			name: `C:\Program Files`,
			aces: []ace{
				{SID: sidUsers, Mask: maskGenericReadExecute, Allow: true, InheritOnly: true},
				{SID: sidUsers, Mask: maskReadExecute, Allow: true},
				{SID: sidCreatorOwner, Mask: genericAll, Allow: true, InheritOnly: true},
				{SID: sidAdministrators, Mask: maskFullControl, Allow: true},
			},
		},
		{
			// AppendData on a directory is CreateDirectories: this is why any user can mkdir at C:\,
			// and why a hand-made shared folder there is the wrong install target.
			name: `C:\`,
			aces: []ace{
				{SID: sidAuthenticatedUsers, Mask: fileAppendData, Allow: true},
				{SID: sidAuthenticatedUsers, Mask: genericWrite | standardDelete, Allow: true, InheritOnly: true},
				{SID: sidAdministrators, Mask: maskFullControl, Allow: true},
			},
			want: []string{sidAuthenticatedUsers},
		},
		{
			// INTERACTIVE matches every logged-on account, so Modify here is write access for
			// whoever else uses the machine.
			name: `C:\Users\Public`,
			aces: []ace{
				{SID: sidCreatorOwner, Mask: maskFullControl, Allow: true},
				{SID: sidInteractive, Mask: maskModify, Allow: true},
				{SID: sidInteractive, Mask: maskReadExecute, Allow: true},
			},
			want: []string{sidInteractive},
		},
		{
			name: "NULL DACL, which grants everyone full access",
			aces: []ace{{SID: sidEveryone, Mask: genericAll, Allow: true}},
			want: []string{sidEveryone},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := untrustedWriters(tc.aces, sidInstaller)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("untrustedWriters() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUntrustedWritersIgnoresDeniedAndReadOnlyAccess(t *testing.T) {
	aces := []ace{
		{SID: sidUsers, Mask: maskFullControl, Allow: false},
		{SID: sidUsers, Mask: maskReadExecute, Allow: true},
		{SID: sidInteractive, Mask: maskGenericReadExecute, Allow: true},
	}
	if got := untrustedWriters(aces, sidInstaller); len(got) != 0 {
		t.Fatalf("untrustedWriters() = %v, want none", got)
	}
}

// A SID string comparison that is not case-insensitive would report the installing user as a
// threat to their own directory.
func TestUntrustedWritersAcceptsTheInstallerInEitherCase(t *testing.T) {
	aces := []ace{{SID: strings.ToLower(sidInstaller), Mask: maskFullControl, Allow: true}}
	if got := untrustedWriters(aces, sidInstaller); len(got) != 0 {
		t.Fatalf("untrustedWriters() = %v, want none", got)
	}
}

func TestUntrustedWritersReportsEachTrusteeOnce(t *testing.T) {
	aces := []ace{
		{SID: sidUsers, Mask: fileWriteData, Allow: true},
		{SID: sidUsers, Mask: standardWriteOwner, Allow: true},
		{SID: sidInteractive, Mask: maskModify, Allow: true},
	}
	want := []string{sidUsers, sidInteractive}
	got := untrustedWriters(aces, sidInstaller)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("untrustedWriters() = %v, want %v", got, want)
	}
}

// Every composite right a user sees in an ACL listing has to intersect the atomic write bits.
func TestWriteMaskCoversTheCompositeRights(t *testing.T) {
	for name, mask := range map[string]uint32{
		"Modify":         maskModify,
		"FullControl":    maskFullControl,
		"GenericWrite":   genericWrite,
		"GenericAll":     genericAll,
		"TakeOwnership":  standardWriteOwner,
		"ChangePermsDAC": standardWriteDAC,
	} {
		if mask&writeMask == 0 {
			t.Errorf("%s (%#08x) does not intersect writeMask", name, mask)
		}
	}
	if maskReadExecute&writeMask != 0 {
		t.Errorf("ReadAndExecute (%#08x) intersects writeMask", maskReadExecute)
	}
	if maskGenericReadExecute&writeMask != 0 {
		t.Errorf("GENERIC_READ|GENERIC_EXECUTE (%#08x) intersects writeMask", maskGenericReadExecute)
	}
}
