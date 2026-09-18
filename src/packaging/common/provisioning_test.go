package common

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProvisioning(t *testing.T) {
	p, err := parseProvisioning([]byte(`{"provisioning_schema":1,"server":" https://cp.example.com ","token":"tok\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Server != "https://cp.example.com" || p.Token != "tok" {
		t.Fatalf("parseProvisioning() = %+v", p)
	}
	for name, raw := range map[string]string{
		"newer schema": `{"provisioning_schema":2,"server":"https://cp","token":"tok"}`,
		"no schema":    `{"server":"https://cp","token":"tok"}`,
		"no token":     `{"provisioning_schema":1,"server":"https://cp","token":" "}`,
		"no server":    `{"provisioning_schema":1,"token":"tok"}`,
		"not JSON":     `server=https://cp`,
	} {
		if _, err := parseProvisioning([]byte(raw)); err == nil {
			t.Errorf("%s: parseProvisioning() accepted %s", name, raw)
		}
	}
}

func TestReadProvisioningAsksTrustedFirstAndReportsAbsence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ProvisioningFile)
	trustedAsked := false
	trusted := func(string) error { trustedAsked = true; return nil }

	if _, err := ReadProvisioning(path, trusted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want os.ErrNotExist", err)
	}
	if trustedAsked {
		t.Fatal("trusted was consulted about a file that does not exist")
	}

	if err := os.WriteFile(path, []byte(`{"provisioning_schema":1,"server":"https://cp","token":"tok"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("owned by someone else")
	if _, err := ReadProvisioning(path, func(string) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("untrusted file: err = %v, want %v", err, refused)
	}
	p, err := ReadProvisioning(path, trusted)
	if err != nil || p.Server != "https://cp" {
		t.Fatalf("ReadProvisioning() = %+v, %v", p, err)
	}
}

func TestReadProvisioningRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"provisioning_schema":1,"server":"https://cp","token":"tok"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ProvisioningFile)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	_, err := ReadProvisioning(link, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("ReadProvisioning() on a symlink = %v, want a refusal", err)
	}
}
