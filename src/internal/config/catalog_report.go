// The build's compiled catalog as the config request reports it, so a control plane can serve and
// validate against what this install can execute. Templates and globs are the compiled strings,
// never expanded: the report is identical on every machine running this build.

package config

import (
	"maps"
	"slices"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/packs"
)

// Features are the served-document fields this resolver reads beyond its accepted config_version. None
// is defined yet; the list exists so a later field can be served only to builds that read it.
var Features = []string{}

// CatalogReport is the config request's `catalog`. It lives here, beside the resolver, because only
// the resolver knows which sources, packs and features a served document may name.
type CatalogReport struct {
	Sources   []CatalogSource `json:"sources"`
	RulePacks []string        `json:"rule_packs"`
	Features  []string        `json:"features"`
}

// CatalogSource is one compiled source; optional fields are omitted when the catalog leaves them empty.
type CatalogSource struct {
	ID            string `json:"id"`
	Family        string `json:"family"`
	FamilyName    string `json:"family_name,omitempty"`
	Description   string `json:"description,omitempty"`
	ArtifactClass string `json:"artifact_class,omitempty"`

	// Enabled is the build's default when no layer says otherwise.
	Enabled bool `json:"enabled"`

	Roots        []string `json:"roots"`
	Include      []string `json:"include,omitempty"`
	Exclude      []string `json:"exclude,omitempty"`
	MaxFileBytes int64    `json:"max_file_bytes,omitempty"`

	// Enrichers are the attached enricher names, the only ones a served document may turn off.
	Enrichers []string `json:"enrichers,omitempty"`
}

// CompiledCatalogReport reports the embedded catalog, not a resolved one: no layer, environment or file reaches it.
func CompiledCatalogReport() (*CatalogReport, error) {
	c, err := sources.Load()
	if err != nil {
		return nil, err
	}
	familyNames := map[string]string{}
	for _, spec := range c.Specs {
		familyNames[spec.Family] = spec.DisplayName
	}

	report := &CatalogReport{
		Sources:   []CatalogSource{},
		RulePacks: append([]string{}, packs.Available()...),
		Features:  append([]string{}, Features...),
	}
	for _, s := range c.Sources() {
		report.Sources = append(report.Sources, CatalogSource{
			ID:            s.ID,
			Family:        s.Family,
			FamilyName:    familyNames[s.Family],
			Description:   s.Description,
			ArtifactClass: s.ArtifactClass,
			Enabled:       s.IsEnabledByDefault(),
			// Roots is required on the wire, so a source without one reports [] rather than null.
			Roots:        append([]string{}, s.Roots...),
			Include:      slices.Clone(s.Include),
			Exclude:      slices.Clone(s.Exclude),
			MaxFileBytes: s.MaxFileBytes,
			Enrichers:    slices.Sorted(maps.Keys(s.Enrichers)),
		})
	}
	return report, nil
}
