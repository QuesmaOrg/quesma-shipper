package cli

import "testing"

func TestServiceInstallationBelongsToThePackager(t *testing.T) {
	for _, cmd := range serviceCmd().Commands() {
		if cmd.Name() == "install" {
			t.Fatal("service install is exposed through the CLI")
		}
	}
}
