package controlplane

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEnrollmentLockRejectsConcurrentLoginAndReleases(t *testing.T) {
	dir := t.TempDir()
	unlock, err := LockEnrollment(dir)
	if err != nil {
		t.Fatal(err)
	}
	if otherUnlock, err := LockEnrollment(dir); err == nil {
		otherUnlock()
		t.Fatal("concurrent enrollment acquired the lock")
	}
	unlock()
	unlock, err = LockEnrollment(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestManagedEnrollmentKeyPersistsAndCannotCrossIdentities(t *testing.T) {
	dir := t.TempDir()
	pub, priv, err := ManagedEnrollmentKey(dir, "one")
	if err != nil {
		t.Fatal(err)
	}
	againPub, againPriv, err := ManagedEnrollmentKey(dir, "one")
	if err != nil || !bytes.Equal(pub, againPub) || !bytes.Equal(priv, againPriv) {
		t.Fatalf("pending device key changed on retry: %v", err)
	}
	if _, _, err := ManagedEnrollmentKey(dir, "two"); err == nil {
		t.Fatal("pending key accepted another identity")
	}
	info, err := os.Stat(filepath.Join(dir, "managed-enrollment-key.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 && runtime.GOOS != "windows" {
		t.Fatal("pending device key is not private")
	}
}
