// A build reports its compiled source catalog on every config fetch: the sources it can collect,
// its scrub rule packs, and the served-document features it reads. This service keeps each
// install's latest one, leaves out of the served document the sources the asking build lacks, and
// offers administrators the sources the fleet reports. The reported sources are the ones discovered
// so far, not an allowlist: a newer or custom build may carry a source no other install reports, and
// an install without a catalog has unknown support, not none. The catalog is build metadata -- the
// same on every machine running that build, with root templates unexpanded -- and never collected
// data.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

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

// wireCatalog reads the request's catalog with every field the schema requires as a pointer, so a
// missing or null one is told apart from an empty one. Fields it does not know are ignored: the
// request grows client-first.
type wireCatalog struct {
	Sources   *[]*wireSource `json:"sources"`
	RulePacks *[]*string     `json:"rule_packs"`
	Features  *[]*string     `json:"features"`
}

type wireSource struct {
	ID            *string    `json:"id"`
	Family        *string    `json:"family"`
	FamilyName    string     `json:"family_name"`
	Description   string     `json:"description"`
	ArtifactClass string     `json:"artifact_class"`
	Enabled       *bool      `json:"enabled"`
	Roots         *[]*string `json:"roots"`
	Include       []*string  `json:"include"`
	Exclude       []*string  `json:"exclude"`
	MaxFileBytes  *int64     `json:"max_file_bytes"`
	Enrichers     []*string  `json:"enrichers"`
}

// decodeCatalog reads the config request's optional catalog. One this service cannot act on --
// not an object, a required list missing or null, a known field of the wrong type -- comes back as
// an error, and the fetch treats it as absent: the build still resolves whatever it is served, so
// a fault in its report must neither cost it the document nor stand for an empty catalog, which
// would leave every source out of it. No catalog at all is nil without an error.
func decodeCatalog(raw json.RawMessage) (*sourceCatalog, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var wire wireCatalog
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	if wire.Sources == nil || wire.RulePacks == nil || wire.Features == nil {
		return nil, errors.New("sources, rule_packs or features is missing or null")
	}
	catalog := &sourceCatalog{Sources: make([]catalogSource, 0, len(*wire.Sources))}
	var err error
	if catalog.RulePacks, err = names("rule_packs", *wire.RulePacks); err != nil {
		return nil, err
	}
	if catalog.Features, err = names("features", *wire.Features); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i, w := range *wire.Sources {
		if w == nil || w.ID == nil || *w.ID == "" || w.Family == nil || *w.Family == "" || w.Enabled == nil || w.Roots == nil {
			return nil, fmt.Errorf("sources[%d] lacks an id, a family, enabled or roots", i)
		}
		if seen[*w.ID] {
			return nil, fmt.Errorf("sources[%d] repeats id %q", i, *w.ID)
		}
		seen[*w.ID] = true
		if w.MaxFileBytes != nil && *w.MaxFileBytes < 0 {
			return nil, fmt.Errorf("sources[%d].max_file_bytes is negative", i)
		}
		source := catalogSource{ID: *w.ID, Family: *w.Family, FamilyName: w.FamilyName, Description: w.Description,
			ArtifactClass: w.ArtifactClass, Enabled: *w.Enabled, MaxFileBytes: w.MaxFileBytes}
		prefix := fmt.Sprintf("sources[%d].", i)
		if source.Roots, err = strs(prefix+"roots", *w.Roots); err != nil {
			return nil, err
		}
		if source.Include, err = strs(prefix+"include", w.Include); err != nil {
			return nil, err
		}
		if source.Exclude, err = strs(prefix+"exclude", w.Exclude); err != nil {
			return nil, err
		}
		if source.Enrichers, err = names(prefix+"enrichers", w.Enrichers); err != nil {
			return nil, err
		}
		catalog.Sources = append(catalog.Sources, source)
	}
	return catalog, nil
}

// strs copies a list of strings, refusing a null item.
func strs(field string, items []*string) ([]string, error) {
	if items == nil {
		return nil, nil
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		if item == nil {
			return nil, fmt.Errorf("%s[%d] is null", field, i)
		}
		out = append(out, *item)
	}
	return out, nil
}

// names is strs for a list of names, which are never empty.
func names(field string, items []*string) ([]string, error) {
	out, err := strs(field, items)
	for i, name := range out {
		if name == "" {
			return nil, fmt.Errorf("%s[%d] is empty", field, i)
		}
	}
	return out, err
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
// service's struct, so field order and unknown fields cannot make one build two objects. A record
// the stores could not read back is refused: the request limit does not bound it, because encoding
// adds indentation and escapes, and a stored catalog nobody can read would fail every read of the
// fleet's catalogs.
func (c *sourceCatalog) reported() (*reportedCatalog, error) {
	canonical, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	record, err := encodeRecord(CatalogRecord{Schema: schemaVersion, Catalog: *c})
	if err != nil {
		return nil, err
	}
	if len(record) > stateObjectLimit {
		return nil, fmt.Errorf("its record is %d bytes, above the %d a store reads", len(record), stateObjectLimit)
	}
	digest := sha256.Sum256(canonical)
	return &reportedCatalog{Digest: hex.EncodeToString(digest[:]), Record: record}, nil
}

// errUnusableCatalog is a stored catalog this service cannot read. Its install counts as one that
// reported none, rather than failing every read that would have included it.
var errUnusableCatalog = errors.New("unusable catalog")

func (m *Manager) loadCatalog(ctx context.Context, digest string) (sourceCatalog, error) {
	raw, _, err := m.store.Get(ctx, catalogKey(m.org, digest))
	if errors.Is(err, ErrTooLarge) {
		return sourceCatalog{}, fmt.Errorf("%w %s: %v", errUnusableCatalog, digest, err)
	}
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
// newest report first, for the installs in active. A catalog that is missing or unreadable leaves
// its install out, as one that reported none.
func (m *Manager) reportedCatalogs(ctx context.Context, active map[string]bool) ([]*sourceCatalog, error) {
	seen, err := m.ListSeen(ctx)
	if err != nil {
		return nil, err
	}
	seen = slices.DeleteFunc(seen, func(rec SeenRecord) bool {
		return rec.CatalogDigest == "" || rec.LastConfigAt == nil || !active[rec.InstallID]
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

// collectionForBuild renders the collection for a build that reported its catalog: a sources[]
// entry the build lacks is left out, so the build does not refuse the whole document over an entry
// another build needs -- it could not collect that source either way. Nothing else changes. A
// requested rule pack in particular is never left out: that would scrub less than was asked for,
// and a build refusing the document keeps its last working one. It reports what it left out.
func collectionForBuild(collection *CollectionConfig, catalog *sourceCatalog) (*CollectionConfig, []string) {
	if collection == nil || catalog == nil {
		return collection, nil
	}
	out := *collection
	var notes []string
	out.Sources = make([]CollectionSource, 0, len(collection.Sources))
	for i, source := range collection.Sources {
		if _, ok := catalog.source(source.ID); !ok {
			notes = append(notes, fmt.Sprintf("left out sources[%d] %q, which its catalog lacks", i, source.ID))
			continue
		}
		out.Sources = append(out.Sources, source)
	}
	return &out, notes
}

// validateCollectionAgainstFleet refuses a source ID nobody has confirmed, and a rule pack no
// reporting build has. A source ID passes when a reporting active install reports it, when the
// stored collection already has it, or when the administrator acknowledged it in unverified: the
// reported set is what has been discovered, so an ID beyond it is a warning to confirm, not an
// error -- a build nobody has run yet may carry it. A rule pack passes when a reporting build has
// it or the stored collection already asks for it; a new one stays strict while installs report,
// because a pack a build lacks makes that build refuse the document and it is never left out.
// What is stored is never refused again, so a write that changes something else -- recipients, one
// source -- is not blocked by a fleet that moved since the collection was saved. An empty message
// means the collection is acceptable.
func validateCollectionAgainstFleet(collection, stored *CollectionConfig, unverified []string, fleet fleetCatalog) string {
	if collection == nil {
		return ""
	}
	held := func(has func(*sourceCatalog) bool) bool { return slices.ContainsFunc(fleet.Reporting, has) }
	for i, source := range collection.Sources {
		if held(func(c *sourceCatalog) bool { _, ok := c.source(source.ID); return ok }) ||
			slices.Contains(unverified, source.ID) ||
			(stored != nil && slices.ContainsFunc(stored.Sources, func(s CollectionSource) bool { return s.ID == source.ID })) {
			continue
		}
		return fmt.Sprintf("collection: sources[%d].id %q is not reported by any install; send it in unverified_sources to save it anyway", i, source.ID)
	}
	if len(fleet.Reporting) > 0 && collection.Scrub != nil {
		for i, pack := range collection.Scrub.RulePacks {
			if held(func(c *sourceCatalog) bool { return slices.Contains(c.RulePacks, pack) }) ||
				(stored != nil && stored.Scrub != nil && slices.Contains(stored.Scrub.RulePacks, pack)) {
				continue
			}
			return fmt.Sprintf("collection: scrub.rule_packs[%d] %q is not in any reporting install's catalog", i, pack)
		}
	}
	return ""
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
