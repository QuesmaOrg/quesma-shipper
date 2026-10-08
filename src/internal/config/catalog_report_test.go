package config_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/packs"
)

func catalogReport(t *testing.T) *config.CatalogReport {
	t.Helper()
	r, err := config.CompiledCatalogReport()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCatalogReportCarriesEveryCompiledSource(t *testing.T) {
	compiled := loadCatalog(t)
	report := catalogReport(t)

	var ids []string
	for _, s := range report.Sources {
		ids = append(ids, s.ID)
		c, _ := compiled.Source(s.ID)
		if s.Family != c.Family || s.FamilyName == "" || s.Description != c.Description || s.ArtifactClass != c.ArtifactClass {
			t.Errorf("%s: family %q (%q), class %q do not match the catalog", s.ID, s.Family, s.FamilyName, s.ArtifactClass)
		}
		if s.Enabled != c.IsEnabledByDefault() {
			t.Errorf("%s: enabled %v, the build's default is %v", s.ID, s.Enabled, c.IsEnabledByDefault())
		}
		if !slices.Equal(s.Include, c.Include) || !slices.Equal(s.Exclude, c.Exclude) || s.MaxFileBytes != c.MaxFileBytes {
			t.Errorf("%s: globs or size cap differ from the catalog", s.ID)
		}
		if want := slices.Sorted(maps.Keys(c.Enrichers)); !slices.Equal(s.Enrichers, want) {
			t.Errorf("%s: enrichers %v, want %v", s.ID, s.Enrichers, want)
		}
	}
	var want []string
	for _, s := range compiled.Sources() {
		want = append(want, s.ID)
	}
	if !slices.Equal(ids, want) {
		t.Errorf("reported sources %v, want every compiled source in id order %v", ids, want)
	}
	if got := report.Sources[slices.Index(ids, "cursor-transcripts")].Enrichers; !slices.Equal(got, []string{"cursor-transcript-join"}) {
		t.Errorf("cursor-transcripts reports enrichers %v", got)
	}
}

// The report is the same on every machine: templates as compiled, whatever this process's environment says.
func TestCatalogReportRootsAreUnexpandedTemplates(t *testing.T) {
	t.Setenv("HOME", "/Users/ada")
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/ada/.claude-work")
	t.Setenv("CODEX_HOME", "/Users/ada/.codex")

	compiled := loadCatalog(t)
	report := catalogReport(t)
	for _, s := range report.Sources {
		c, _ := compiled.Source(s.ID)
		if !slices.Equal(s.Roots, c.Roots) {
			t.Errorf("%s: roots %v, want the compiled templates %v", s.ID, s.Roots, c.Roots)
		}
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("/Users/ada")) {
		t.Errorf("the report carries this machine's environment: %s", raw)
	}
	again, err := json.Marshal(catalogReport(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, again) {
		t.Error("two reports from the same build differ")
	}
	// The server reads at most 1 MiB of request; the report must leave the rest of it alone.
	if len(raw) > 256<<10 {
		t.Errorf("the report is %d bytes, more than a quarter of the 1 MiB request cap", len(raw))
	}
}

func TestCatalogReportListsEveryRulePackAndNoFeatures(t *testing.T) {
	report := catalogReport(t)
	if !slices.Equal(report.RulePacks, packs.Available()) {
		t.Errorf("rule_packs %v, want every pack this build has %v", report.RulePacks, packs.Available())
	}
	// No feature is defined yet, and the wire field is required: [] rather than null.
	if report.Features == nil || len(report.Features) != 0 {
		t.Errorf("features %#v, want an empty list", report.Features)
	}
}
