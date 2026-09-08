// Catalog parsing: the compiled tier in typed form. A root in no compiled spec cannot be collected, and
// SpecFingerprint covers only the fields that affect how bytes are read, so an unrelated config push
// cannot re-ship full history fleet-wide.

package sources

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	catalogdata "github.com/QuesmaOrg/quesma-shipper/internal/formats/catalogdata"
)

// Spec is one catalog file.
type Spec struct {
	SpecVersion int      `yaml:"spec_version"`
	Family      string   `yaml:"family"`
	DisplayName string   `yaml:"display_name"`
	Sources     []Source `yaml:"sources"`
}

// Source is one compiled source spec: what may be collected, and how.
type Source struct {
	ID            string          `yaml:"id"`
	Description   string          `yaml:"description"`
	Enabled       *bool           `yaml:"enabled"`
	Gather        string          `yaml:"gather"`
	ArtifactClass string          `yaml:"artifact_class"`
	Roots         []string        `yaml:"roots"`
	RequireSubdir string          `yaml:"require_subdir"`
	Include       []string        `yaml:"include"`
	Exclude       []string        `yaml:"exclude"`
	MaxFileBytes  int64           `yaml:"max_file_bytes"`
	Sniff         *Sniff          `yaml:"sniff"`
	Enrichers     map[string]bool `yaml:"enrichers"`

	// Emit, CWDProbe and GitRead belong to the sidecar primitive. Remote normalisation is not
	// configurable: NormaliseRemote strips userinfo unconditionally.
	Emit     string    `yaml:"emit"`
	CWDProbe *CWDProbe `yaml:"cwd_probe"`
	GitRead  *GitRead  `yaml:"git_read"`

	// Family is copied down from the containing spec so a Source travels alone.
	Family string `yaml:"-"`
}

// Sniff is the declarative shape assertion, living in the catalog so a drifted format is a data fix.
type Sniff struct {
	Kind         string `yaml:"kind"`
	MagicHex     string `yaml:"magic_hex"`
	MaxScanBytes int64  `yaml:"max_scan_bytes"`
}

// IsEnabledByDefault reports the spec's own default; only the compiled tier decides what exists at all.
func (s Source) IsEnabledByDefault() bool {
	return s.Enabled == nil || *s.Enabled
}

// CWDProbe is the bounded head-of-file probe. Field names live here, not in code, so a rename is a data fix.
type CWDProbe struct {
	From      []string `yaml:"from"`
	Fields    []string `yaml:"fields"`
	ScanBytes int64    `yaml:"scan_bytes"`
}

// GitRead is the equally bounded .git handling.
type GitRead struct {
	WalkUp           bool     `yaml:"walk_up"`
	FollowGitdirFile bool     `yaml:"follow_gitdir_file"`
	Take             []string `yaml:"take"`
}

// Compiled is the whole parsed catalog.
type Compiled struct {
	Specs   []Spec
	byID    map[string]Source
	sources []Source
}

// Load parses and validates every embedded catalog file at process start, so a build whose catalog does not satisfy its own schema fails loudly.
func Load() (*Compiled, error) {
	names, err := catalogdata.Files()
	if err != nil {
		return nil, err
	}

	c := &Compiled{byID: map[string]Source{}}
	for _, name := range names {
		raw, err := catalogdata.Read(name)
		if err != nil {
			return nil, err
		}

		var asAny any
		if err := yaml.Unmarshal(raw, &asAny); err != nil {
			return nil, fmt.Errorf("catalog: %s: %w", name, err)
		}
		if err := formats.Validate(formats.SourceSpec, asAny); err != nil {
			return nil, fmt.Errorf("catalog: %s: %w", name, err)
		}

		var spec Spec
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&spec); err != nil {
			return nil, fmt.Errorf("catalog: %s: %w", name, err)
		}

		for i := range spec.Sources {
			spec.Sources[i].Family = spec.Family
			s := spec.Sources[i]
			if prev, dup := c.byID[s.ID]; dup {
				return nil, fmt.Errorf("catalog: source id %q declared twice (family %s and %s)",
					s.ID, prev.Family, s.Family)
			}
			c.byID[s.ID] = s
			c.sources = append(c.sources, s)
		}
		c.Specs = append(c.Specs, spec)
	}

	slices.SortFunc(c.sources, func(a, b Source) int { return cmp.Compare(a.ID, b.ID) })
	return c, nil
}

// Sources returns every compiled source, ordered by id.
func (c *Compiled) Sources() []Source {
	return slices.Clone(c.sources)
}

// Source looks up a compiled source by id. An id absent here is refused rather than invented, which would mean collecting outside the ceiling.
func (c *Compiled) Source(id string) (Source, bool) {
	s, ok := c.byID[id]
	return s, ok
}

// SpecFingerprint hashes only the fields that affect how bytes are read: it keys local upload state, so widening it re-ships the fleet.
func SpecFingerprint(s Source) string {
	h := sha256.New()
	// Length-prefix every component so no other combination of values can hash the same.
	write := func(label string, values ...string) {
		fmt.Fprintf(h, "%s:%d:", label, len(values))
		for _, v := range values {
			fmt.Fprintf(h, "%d:%s", len(v), v)
		}
	}
	write("gather", s.Gather)
	write("roots", s.Roots...)
	write("require_subdir", s.RequireSubdir)
	write("include", s.Include...)
	write("exclude", s.Exclude...)
	write("emit", s.Emit)
	return hex.EncodeToString(h.Sum(nil))
}

// Resolved is a compiled source with config applied and roots expanded. Defined here rather than in the
// config package that builds it, so discovery and the loop never import the layered merge.
type Resolved struct {
	Source

	// Root is the first candidate that expanded, exists and satisfied require_subdir; empty means the agent is not installed here.
	Root string

	// RootUnresolvedReason explains an empty Root, so agent_absent stays distinguishable from a misconfiguration.
	RootUnresolvedReason string

	Enabled         bool
	SpecFingerprint string
}
