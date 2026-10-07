// A build reports its compiled source catalog on every config fetch: the sources it can collect,
// its scrub rule packs, and the served-document features it reads. This service keeps each
// install's latest one, renders the served document for the build asking, and refuses writes no
// reporting build could execute. The catalog is build metadata -- the same on every machine
// running that build, with root templates unexpanded -- and never collected data.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// featureExcludeAdd is the one served-document feature defined so far. A build that does not list
// it ignores sources[].exclude_add, which would widen collection, so it is folded into exclude.
const featureExcludeAdd = "sources.exclude_add"

// sourceCatalog is this service's own copy of the config request's catalog; the protocol ships
// schemas, not types. Fields it does not know are dropped on decode, so they are never stored.
type sourceCatalog struct {
	Sources   []catalogSource `json:"sources"`
	RulePacks []string        `json:"rule_packs"`
	Features  []string        `json:"features"`
}

type catalogSource struct {
	ID            string   `json:"id"`
	Family        string   `json:"family"`
	FamilyName    string   `json:"family_name,omitempty"`
	Description   string   `json:"description,omitempty"`
	ArtifactClass string   `json:"artifact_class,omitempty"`
	Enabled       bool     `json:"enabled"`
	Roots         []string `json:"roots"`
	Include       []string `json:"include,omitempty"`
	Exclude       []string `json:"exclude,omitempty"`
	MaxFileBytes  *int64   `json:"max_file_bytes,omitempty"`
	Enrichers     []string `json:"enrichers,omitempty"`
}

// CatalogRecord is one catalog as stored. It is content-addressed: the key is the SHA-256 of the
// catalog's canonical encoding, so every install running a build shares one immutable object.
type CatalogRecord struct {
	Schema  int           `json:"schema"`
	Catalog sourceCatalog `json:"catalog"`
}

// reportedCatalog is a catalog from a request, ready to store: its digest and the record bytes.
type reportedCatalog struct {
	Digest string
	Record []byte
}

// usable is the shape the schema requires of what this service acts on. A catalog that fails it is
// treated as absent rather than failing the fetch: the build still resolves the document itself.
func (c *sourceCatalog) usable() error {
	seen := map[string]bool{}
	for i, source := range c.Sources {
		if source.ID == "" || source.Family == "" || source.Roots == nil {
			return fmt.Errorf("sources[%d] lacks an id, a family or roots", i)
		}
		if seen[source.ID] {
			return fmt.Errorf("sources[%d] repeats id %q", i, source.ID)
		}
		seen[source.ID] = true
	}
	return nil
}

func (c *sourceCatalog) source(id string) (catalogSource, bool) {
	for _, source := range c.Sources {
		if source.ID == id {
			return source, true
		}
	}
	return catalogSource{}, false
}

// reported encodes a catalog for storage. The digest is over the compact encoding of this
// service's struct, so field order and unknown fields cannot make one build two objects.
func (c *sourceCatalog) reported() (*reportedCatalog, error) {
	canonical, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	record, err := encodeRecord(CatalogRecord{Schema: schemaVersion, Catalog: *c})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	return &reportedCatalog{Digest: hex.EncodeToString(digest[:]), Record: record}, nil
}

// errUnusableCatalog is a stored catalog this service cannot read. Its install counts as one that
// reported none, rather than failing every read that would have included it.
var errUnusableCatalog = errors.New("unusable catalog")

func (m *Manager) loadCatalog(ctx context.Context, digest string) (sourceCatalog, error) {
	raw, _, err := m.store.Get(ctx, catalogKey(m.org, digest))
	if err != nil {
		return sourceCatalog{}, err
	}
	var rec CatalogRecord
	if err := strictDecode(raw, &rec); err != nil || rec.Schema != schemaVersion {
		return sourceCatalog{}, fmt.Errorf("%w %s", errUnusableCatalog, digest)
	}
	return rec.Catalog, nil
}

// reportedCatalogs returns the catalog each install's latest config fetch carried, one per install,
// newest report first. active limits it to installs in that set. A catalog that is
// missing or unreadable leaves its install out, as one that reported none.
func (m *Manager) reportedCatalogs(ctx context.Context, active map[string]bool) ([]*sourceCatalog, error) {
	seen, err := m.ListSeen(ctx)
	if err != nil {
		return nil, err
	}
	seen = slices.DeleteFunc(seen, func(rec SeenRecord) bool {
		return rec.CatalogDigest == "" || rec.LastConfigAt == nil || (active != nil && !active[rec.InstallID])
	})
	slices.SortFunc(seen, func(a, b SeenRecord) int { return b.LastConfigAt.Compare(*a.LastConfigAt) })
	loaded := map[string]*sourceCatalog{}
	out := make([]*sourceCatalog, 0, len(seen))
	for _, rec := range seen {
		catalog, ok := loaded[rec.CatalogDigest]
		if !ok {
			c, err := m.loadCatalog(ctx, rec.CatalogDigest)
			switch {
			case err == nil:
				catalog = &c
			case errors.Is(err, ErrNotFound), errors.Is(err, errUnusableCatalog):
			default:
				return nil, err
			}
			loaded[rec.CatalogDigest] = catalog
		}
		if catalog != nil {
			out = append(out, catalog)
		}
	}
	return out, nil
}

// fleetCatalog is what the organization's active installs say they can execute.
type fleetCatalog struct {
	Active int
	// Reporting holds one catalog per active install whose latest config fetch carried one,
	// newest report first.
	Reporting []*sourceCatalog
}

func (m *Manager) FleetCatalog(ctx context.Context) (fleetCatalog, error) {
	installs, err := m.ListInstalls(ctx)
	if err != nil {
		return fleetCatalog{}, err
	}
	active := map[string]bool{}
	for _, install := range installs {
		if install.Status == InstallActive {
			active[install.InstallID] = true
		}
	}
	reporting, err := m.reportedCatalogs(ctx, active)
	if err != nil {
		return fleetCatalog{}, err
	}
	return fleetCatalog{Active: len(active), Reporting: reporting}, nil
}

// sourceExcludes answers, for each id, every exclude glob any catalog of the organization has for
// that source: the union, so the base a catalog-less fold extends can only grow, and no one
// install's report can shrink what another build excludes. An id no catalog has is absent.
func (m *Manager) sourceExcludes(ctx context.Context, ids []string) (map[string][]string, error) {
	reported, err := m.reportedCatalogs(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, id := range ids {
		for _, catalog := range reported {
			source, ok := catalog.source(id)
			if !ok {
				continue
			}
			union := out[id]
			if union == nil {
				union = []string{}
			}
			for _, glob := range source.Exclude {
				if !slices.Contains(union, glob) {
					union = append(union, glob)
				}
			}
			out[id] = union
		}
	}
	return out, nil
}

// collectionForInstall renders the collection for the build asking, and says what it changed.
//
// With the build's catalog: a source or rule pack the build lacks is left out, so the build does
// not refuse the whole document over one entry another build needs; exclude_add is served as is to
// a build that reads it and folded into exclude for one that does not. Without a catalog the
// document is served as before, except exclude_add, which an older build would ignore: it is folded
// over every catalog the organization has for that source, and dropped if there is none.
func (m *Manager) collectionForInstall(ctx context.Context, collection *CollectionConfig, catalog *sourceCatalog) (*CollectionConfig, []string, error) {
	if collection == nil {
		return nil, nil, nil
	}
	out := *collection
	var notes []string
	servesExcludeAdd := catalog != nil && slices.Contains(catalog.Features, featureExcludeAdd)

	var excludes map[string][]string
	if catalog == nil {
		var needed []string
		for _, source := range collection.Sources {
			if len(source.ExcludeAdd) > 0 && len(source.Exclude) == 0 {
				needed = append(needed, source.ID)
			}
		}
		if len(needed) > 0 {
			var err error
			if excludes, err = m.sourceExcludes(ctx, needed); err != nil {
				return nil, nil, err
			}
		}
	}

	out.Sources = make([]CollectionSource, 0, len(collection.Sources))
	for i, source := range collection.Sources {
		// compiled is the build's own exclude list for the source, the base a fold extends.
		compiled, known := excludes[source.ID]
		if catalog != nil {
			reported, ok := catalog.source(source.ID)
			if !ok {
				notes = append(notes, fmt.Sprintf("left out sources[%d] %q, which its catalog lacks", i, source.ID))
				continue
			}
			compiled, known = reported.Exclude, true
		}
		if len(source.ExcludeAdd) > 0 && !servesExcludeAdd {
			folded := fmt.Sprintf("folded sources[%d] %q exclude_add into exclude", i, source.ID)
			switch {
			case len(source.Exclude) > 0:
				source.Exclude = append(slices.Clone(source.Exclude), source.ExcludeAdd...)
				notes = append(notes, folded)
			case known:
				source.Exclude = append(slices.Clone(compiled), source.ExcludeAdd...)
				notes = append(notes, folded)
			default:
				notes = append(notes, fmt.Sprintf("dropped sources[%d] %q exclude_add: no catalog in the organization has the source to fold it over", i, source.ID))
			}
			source.ExcludeAdd = nil
		}
		out.Sources = append(out.Sources, source)
	}

	if catalog != nil && collection.Scrub != nil {
		scrub := *collection.Scrub
		scrub.RulePacks = nil
		for i, pack := range collection.Scrub.RulePacks {
			if !slices.Contains(catalog.RulePacks, pack) {
				notes = append(notes, fmt.Sprintf("left out scrub.rule_packs[%d] %q, which its catalog lacks", i, pack))
				continue
			}
			scrub.RulePacks = append(scrub.RulePacks, pack)
		}
		out.Scrub = &scrub
	}
	return &out, notes, nil
}

// validateCollectionAgainstFleet refuses a collection no reporting build could execute, and an
// exclude_add that an install without a catalog could not have folded for it. A zero status means
// the collection is acceptable.
func validateCollectionAgainstFleet(collection *CollectionConfig, fleet fleetCatalog) (int, string) {
	if collection == nil {
		return 0, ""
	}
	if reporting := len(fleet.Reporting); reporting > 0 {
		held := func(has func(*sourceCatalog) bool) bool {
			return slices.ContainsFunc(fleet.Reporting, has)
		}
		for i, source := range collection.Sources {
			if !held(func(c *sourceCatalog) bool { _, ok := c.source(source.ID); return ok }) {
				return http.StatusBadRequest, fmt.Sprintf("collection: sources[%d].id %q is not in any reporting install's catalog", i, source.ID)
			}
		}
		if collection.Scrub != nil {
			for i, pack := range collection.Scrub.RulePacks {
				if !held(func(c *sourceCatalog) bool { return slices.Contains(c.RulePacks, pack) }) {
					return http.StatusBadRequest, fmt.Sprintf("collection: scrub.rule_packs[%d] %q is not in any reporting install's catalog", i, pack)
				}
			}
		}
	}
	if missing := fleet.Active - len(fleet.Reporting); missing > 0 &&
		slices.ContainsFunc(collection.Sources, func(s CollectionSource) bool { return len(s.ExcludeAdd) > 0 }) {
		return http.StatusConflict, fmt.Sprintf("collection: sources[].exclude_add needs every active install to report its catalog; %d of %d do not yet. Update their shippers first.", missing, fleet.Active)
	}
	return 0, ""
}

// The admin view of the fleet's catalogs: what the active installs can collect, and how many of
// the reporting ones can.
type adminSourcesResponse struct {
	Installs  adminSourcesInstalls `json:"installs"`
	Sources   []adminCatalogSource `json:"sources"`
	RulePacks []adminNameCount     `json:"rule_packs"`
	Features  []adminNameCount     `json:"features"`
}

type adminSourcesInstalls struct {
	Active    int `json:"active"`
	Reporting int `json:"reporting"`
}

// adminCatalogSource always carries its lists, empty rather than absent, so a reader need not
// tell a missing field from an empty one. max_file_bytes is absent when no catalog states one.
type adminCatalogSource struct {
	ID            string   `json:"id"`
	Family        string   `json:"family"`
	FamilyName    string   `json:"family_name"`
	Description   string   `json:"description"`
	ArtifactClass string   `json:"artifact_class"`
	Enabled       bool     `json:"enabled"`
	Roots         []string `json:"roots"`
	Include       []string `json:"include"`
	Exclude       []string `json:"exclude"`
	MaxFileBytes  *int64   `json:"max_file_bytes,omitempty"`
	Enrichers     []string `json:"enrichers"`
	Installs      int      `json:"installs"`
}

type adminNameCount struct {
	Name     string `json:"name"`
	Installs int    `json:"installs"`
}

func summarizeFleetCatalog(fleet fleetCatalog) adminSourcesResponse {
	out := adminSourcesResponse{
		Installs:  adminSourcesInstalls{Active: fleet.Active, Reporting: len(fleet.Reporting)},
		Sources:   []adminCatalogSource{},
		RulePacks: []adminNameCount{},
		Features:  []adminNameCount{},
	}
	sources := map[string]int{}
	packs, features := map[string]int{}, map[string]int{}
	// Newest first, so the first catalog to name a source is the one its metadata comes from.
	for _, catalog := range fleet.Reporting {
		for _, source := range catalog.Sources {
			if i, ok := sources[source.ID]; ok {
				out.Sources[i].Installs++
				continue
			}
			sources[source.ID] = len(out.Sources)
			out.Sources = append(out.Sources, adminCatalogSource{ID: source.ID, Family: source.Family,
				FamilyName: source.FamilyName, Description: source.Description, ArtifactClass: source.ArtifactClass,
				Enabled: source.Enabled, Roots: nonNil(source.Roots), Include: nonNil(source.Include),
				Exclude: nonNil(source.Exclude), MaxFileBytes: source.MaxFileBytes, Enrichers: nonNil(source.Enrichers), Installs: 1})
		}
		for _, pack := range uniqueNames(catalog.RulePacks) {
			packs[pack]++
		}
		for _, feature := range uniqueNames(catalog.Features) {
			features[feature]++
		}
	}
	slices.SortFunc(out.Sources, func(a, b adminCatalogSource) int {
		if c := strings.Compare(a.FamilyName, b.FamilyName); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	out.RulePacks, out.Features = nameCounts(packs), nameCounts(features)
	return out
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func uniqueNames(names []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(names)))
}

func nameCounts(counts map[string]int) []adminNameCount {
	out := make([]adminNameCount, 0, len(counts))
	for name, installs := range counts {
		out = append(out, adminNameCount{Name: name, Installs: installs})
	}
	slices.SortFunc(out, func(a, b adminNameCount) int { return strings.Compare(a.Name, b.Name) })
	return out
}
