package windows

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestManagedSupervisorLockPreventsDuplicateCollectorsAndReleasesOnExit(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireSupervisorLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := acquireSupervisorLock(dir)
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		t.Fatalf("duplicate supervisor lock = %v, want ERROR_LOCK_VIOLATION", err)
	}
	otherUser, err := acquireSupervisorLock(t.TempDir())
	if err != nil {
		t.Fatalf("another user's lock was blocked: %v", err)
	}
	otherUser.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := acquireSupervisorLock(dir)
	if err != nil {
		t.Fatalf("supervisor cannot restart after exit: %v", err)
	}
	restarted.Close()
}

func TestManagedTaskRecognizesTheLocalizedUsersGroup(t *testing.T) {
	sid, err := windows.StringToSid("S-1-5-32-545")
	if err != nil {
		t.Fatal(err)
	}
	account, domain, _, err := sid.LookupAccount("")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{sid.String(), domain + `\` + account} {
		if !isUsersGroup(value) {
			t.Fatalf("Users group %q was not recognized", value)
		}
	}
	for _, value := range []string{"S-1-5-18", "S-1-5-32-544", "S-1-1-0", ""} {
		if isUsersGroup(value) {
			t.Fatalf("unexpected managed principal %q was accepted", value)
		}
	}
}
