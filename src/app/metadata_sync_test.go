package app

// Pins upload's deliberate copy of the metadata allowlist to controlplane's tags; only app may
// import both packages.

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/upload"
)

func TestMetadataNamesMatchControlPlane(t *testing.T) {
	var tags []string
	for _, f := range reflect.VisibleFields(reflect.TypeOf(controlplane.UploadMetadata{})) {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		tags = append(tags, name)
	}
	if !slices.Equal(tags, upload.MetadataNames) {
		t.Fatalf("upload.MetadataNames drifted from controlplane.UploadMetadata tags:\n  tags:  %v\n  names: %v", tags, upload.MetadataNames)
	}
}
