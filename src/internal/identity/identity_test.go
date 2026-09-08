package identity_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
)

func TestMintThenLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()

	minted, err := identity.Mint(dir)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	loaded, err := identity.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.InstallID != minted.InstallID {
		t.Errorf("install_id: got %s want %s", loaded.InstallID, minted.InstallID)
	}
	if loaded.Identity.String() != minted.Identity.String() {
		t.Error("age identity did not survive the round trip")
	}
	if string(loaded.NameKey) != string(minted.NameKey) {
		t.Error("name_key did not survive the round trip")
	}
	if !loaded.CreatedAt.Equal(minted.CreatedAt) {
		t.Errorf("created_at: got %s want %s", loaded.CreatedAt, minted.CreatedAt)
	}
}

// The whole unit is one file so that on an ephemeral host it goes on the durable volume or none
// of it does: a fresh name_key re-ships every file, a fresh age identity strands every archive.
func TestUnitIsOneFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := identity.Mint(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != identity.FileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected exactly %s, got %v", identity.FileName, names)
	}
}

func TestMintIsSecretByDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	dir := t.TempDir()
	if _, err := identity.Mint(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, identity.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("unit file mode is %#o, want 0600", perm)
	}
}

// Minting over an existing unit would orphan every object the previous one named and every
// archive it could decrypt, so it has to be refused.
func TestMintRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := identity.Mint(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Mint(dir); err == nil {
		t.Fatal("a second mint into the same dir must be refused")
	}
}

// A unit readable by group or world is a finding, not something to repair silently.
func TestLoadRefusesLooseMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	dir := t.TempDir()
	if _, err := identity.Mint(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, identity.FileName)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := identity.Load(dir)
	if err == nil {
		t.Fatal("a group/world-readable unit must be refused")
	}
	if !strings.Contains(err.Error(), "world-accessible") {
		t.Errorf("error should name the permission problem, got: %v", err)
	}
}

func TestLoadRejectsForeignSchema(t *testing.T) {
	dir := t.TempDir()
	if _, err := identity.Mint(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, identity.FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bumped := strings.Replace(string(raw), `"identity_schema": 1`, `"identity_schema": 2`, 1)
	if bumped == string(raw) {
		t.Fatal("test could not bump identity_schema; file shape changed")
	}
	if err := os.WriteFile(path, []byte(bumped), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Load(dir); err == nil {
		t.Fatal("an unknown identity_schema must be rejected, never guessed at")
	}
}

// A recipient that no longer matches its identity means the naming and encryption halves may no
// longer belong together.
func TestLoadRejectsMismatchedRecipient(t *testing.T) {
	dir := t.TempDir()
	if _, err := identity.Mint(dir); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	stranger, err := identity.Mint(other)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, identity.FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mine, err := identity.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	swapped := strings.Replace(string(raw),
		mine.Recipient().String(), stranger.Recipient().String(), 1)
	if swapped == string(raw) {
		t.Fatal("test could not swap the recipient; file shape changed")
	}
	if err := os.WriteFile(path, []byte(swapped), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Load(dir); err == nil {
		t.Fatal("a recipient that disagrees with the identity must be rejected")
	}
}

// The private age identity and the name_key must not reach a log line by accident, which is the
// likeliest way either escapes.
func TestUnitRedactsWhenFormatted(t *testing.T) {
	dir := t.TempDir()
	u, err := identity.Mint(dir)
	if err != nil {
		t.Fatal(err)
	}

	secret := u.Identity.String()
	for _, rendered := range []string{
		fmt.Sprintf("%v", *u),
		fmt.Sprintf("%s", *u),
		fmt.Sprintf("%+v", *u),
		fmt.Sprintf("%#v", *u),
		(*u).String(),
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("formatted unit leaked the private age identity: %s", rendered)
		}
		if strings.Contains(rendered, hexOf(u.NameKey)) {
			t.Errorf("formatted unit leaked the name_key: %s", rendered)
		}
		if !strings.Contains(rendered, "REDACTED") {
			t.Errorf("formatted unit should say REDACTED, got: %s", rendered)
		}
	}
}

func hexOf(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, d[c>>4], d[c&0x0f])
	}
	return string(out)
}

// Two installs must not collide on either the naming secret or the identity.
func TestMintIsUniquePerInstall(t *testing.T) {
	a, err := identity.Mint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := identity.Mint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.InstallID == b.InstallID {
		t.Error("two installs share an install_id")
	}
	if string(a.NameKey) == string(b.NameKey) {
		t.Error("two installs share a name_key")
	}
	if a.Identity.String() == b.Identity.String() {
		t.Error("two installs share an age identity")
	}
}
