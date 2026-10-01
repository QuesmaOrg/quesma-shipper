package windows

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func policyString(value string) []byte {
	units := append(utf16.Encode([]rune(value)), 0)
	raw := make([]byte, 2*len(units))
	for i, unit := range units {
		binary.LittleEndian.PutUint16(raw[2*i:], unit)
	}
	return raw
}

func TestPolicyRejectsMalformedOrOversizedValuesWithoutEchoingGrant(t *testing.T) {
	const secret = "sensitive-grant-contents"
	for _, tc := range []struct {
		name string
		kind uint32
		raw  []byte
	}{
		{"expandable string", 2, policyString(secret)},
		{"binary", 3, policyString(secret)},
		{"unterminated", 1, policyString(secret)[:2*len(secret)]},
		{"odd bytes", 1, append(policyString(secret), 0)},
		{"embedded NUL", 1, policyString(secret + "\x00hidden")},
		{"unpaired high surrogate", 1, []byte{0, 0xd8, 0, 0}},
		{"unpaired low surrogate", 1, []byte{0, 0xdc, 0, 0}},
		{"oversized", 1, policyString(secret + strings.Repeat("x", managedStringLimit))},
		{"UTF-8 expansion", 1, policyString(strings.Repeat("界", managedStringLimit/2))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := registryString("Grant", tc.kind, tc.raw)
			if err == nil || value != "" {
				t.Fatalf("malformed policy accepted: value length %d, error %v", len(value), err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("policy error disclosed grant")
			}
		})
	}
}

func TestManagedEnrollmentRequiresAPairAndAcceptsPolicyRotation(t *testing.T) {
	for _, tc := range []struct {
		name, server, grant string
		wantErr             bool
	}{
		{name: "policy absent"},
		{name: "server only", server: "https://fleet.example", wantErr: true},
		{name: "grant only", grant: "old-grant", wantErr: true},
		{name: "complete", server: "https://fleet.example", grant: "old-grant"},
		{name: "rotated", server: "https://fleet.example", grant: "new-grant"},
		{name: "unicode", server: "https://管理.example", grant: "🔑"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, grant, err := managedEnrollment(func(key string) (string, error) {
				value := tc.server
				if key == "Grant" {
					value = tc.grant
				}
				return registryString(key, 1, policyString("  "+value+"  "))
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if err == nil && (server != tc.server || grant != tc.grant) {
				t.Fatal("managed policy was not preserved")
			}
			if err != nil && (server != "" || grant != "") {
				t.Fatal("incomplete policy leaked a partial result")
			}
		})
	}
}

func TestManagedCollectorIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, sid string
		groups    []string
		wantErr   bool
	}{
		{"SYSTEM", sidLocalSystem, []string{"S-1-5-4"}, true},
		{"LocalService", "S-1-5-19", []string{"S-1-5-4"}, true},
		{"NetworkService", "S-1-5-20", []string{"S-1-5-4"}, true},
		{"virtual service", "S-1-5-80-1234", []string{"S-1-5-4"}, true},
		{"domain service logon", "S-1-5-21-1234", []string{"S-1-5-6"}, true},
		{"batch logon", "S-1-5-21-1234", []string{"S-1-5-3"}, true},
		{"local interactive", "S-1-5-21-1234", []string{"S-1-5-4", "S-1-5-32-545"}, false},
		{"Entra interactive", "S-1-12-1-1234", []string{"S-1-5-4"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateIdentity(tc.sid, tc.groups); (err != nil) != tc.wantErr {
				t.Fatalf("identity validation = %v", err)
			}
		})
	}
}

func TestMigrationAllowsUninstallLeftoversButRejectsInstalledPrograms(t *testing.T) {
	for _, name := range []string{"notes.txt", "quesma-shipper.exe", "unins000.exe"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := checkNoPersonalProgram(dir); err != nil {
				t.Fatalf("empty directory blocks migration: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := checkNoPersonalProgram(dir); (err != nil) != (name != "notes.txt") {
				t.Fatalf("migration conflict = %v", err)
			}
		})
	}
}
