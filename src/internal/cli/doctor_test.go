package cli

import (
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/app"
)

func TestUpdateLinesGiveManagedPackageRemedyAsProse(t *testing.T) {
	lines := updateLines(app.Build{}, app.UpdateStatus{
		State: "available", Detail: "1.1.0 available", Fix: "ask an administrator to deploy the newer macOS package through MDM",
	}, palette{}, false)
	if len(lines) != 2 || !strings.Contains(lines[1], "administrator") || strings.Contains(lines[1], "`") {
		t.Fatalf("managed update guidance: %q", lines)
	}
}
