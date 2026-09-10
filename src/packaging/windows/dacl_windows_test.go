//go:build windows

package windows

import (
	"os/exec"
	"os/user"
	"strings"
	"testing"
)

func currentSID(t *testing.T) string {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return current.Uid
}

// A directory created by this user carries the profile's own ACL: nobody else can change it.
func TestVerifyInstallDirAcceptsADirectoryOnlyThisUserCanChange(t *testing.T) {
	if err := verifyInstallDir(t.TempDir(), currentSID(t)); err != nil {
		t.Fatalf("verifyInstallDir() = %v, want nil", err)
	}
}

func TestVerifyInstallDirRejectsADirectoryAnotherAccountCanChange(t *testing.T) {
	dir := t.TempDir()
	// BUILTIN\Users by SID, so the grant does not depend on the system's display language.
	out, err := exec.Command("icacls.exe", dir, "/grant", "*"+sidUsers+":(M)").CombinedOutput()
	if err != nil {
		t.Skipf("cannot grant Users write access to %s: %v: %s", dir, err, out)
	}

	err = verifyInstallDir(dir, currentSID(t))
	if err == nil {
		t.Fatal("verifyInstallDir() = nil, want an error naming the trustee")
	}
	if !strings.Contains(err.Error(), "Users") {
		t.Fatalf("verifyInstallDir() = %v, want the message to name BUILTIN\\Users", err)
	}
}

// The installing user's own FullControl must not read as a finding against their own directory.
func TestDirectoryACEsReadsTheInstallersOwnEntry(t *testing.T) {
	dir := t.TempDir()
	aces, err := directoryACEs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(aces) == 0 {
		t.Fatal("directoryACEs() returned no entries for a directory that has a DACL")
	}
	sid := currentSID(t)
	for _, a := range aces {
		if strings.EqualFold(a.SID, sid) && a.Allow && a.Mask&writeMask != 0 {
			return
		}
	}
	t.Fatalf("no allow-write entry for the installing user %s in %+v", sid, aces)
}
