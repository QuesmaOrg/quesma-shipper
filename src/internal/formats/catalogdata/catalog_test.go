package catalogdata_test

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats/catalogdata"
)

// decodeYAML converts a catalog file to the any-shaped value the JSON Schema validator works on.
func decodeYAML(t *testing.T, name string) any {
	t.Helper()
	raw, err := catalogdata.Read(name)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: not valid YAML: %v", name, err)
	}
	return doc
}

// Every bundled catalog file must validate: the catalog is edited by people who do not read Go.
func TestBundledCatalogValidates(t *testing.T) {
	files, err := catalogdata.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no catalog files embedded")
	}
	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			if err := formats.Validate(formats.SourceSpec, decodeYAML(t, name)); err != nil {
				t.Errorf("%v", err)
			}
		})
	}
}

// Without these refusals, TestBundledCatalogValidates could be green with the validator inert.
func TestSchemaRejectsBadCatalogFiles(t *testing.T) {
	cases := map[string]string{
		"unknown gather name": `
spec_version: 1
family: demo
sources:
  - id: demo-src
    gather: sqlite_rows
    artifact_class: trajectory
    roots: ["~/.demo"]
    include: ["**/*.jsonl"]
`,
		"reserved gather name": `
spec_version: 1
family: demo
sources:
  - id: demo-src
    gather: acp
    artifact_class: trajectory
    roots: ["~/.demo"]
    include: ["**/*.jsonl"]
`,
		"missing artifact_class": `
spec_version: 1
family: demo
sources:
  - id: demo-src
    gather: file_glob
    roots: ["~/.demo"]
    include: ["**/*.jsonl"]
`,
		"walking primitive without roots": `
spec_version: 1
family: demo
sources:
  - id: demo-src
    gather: file_glob
    artifact_class: trajectory
    include: ["**/*.jsonl"]
`,
		"unknown source field": `
spec_version: 1
family: demo
sources:
  - id: demo-src
    gather: file_glob
    artifact_class: trajectory
    roots: ["~/.demo"]
    include: ["**/*.jsonl"]
    container: mirror
`,
		"undeclared change_detection strategy": `
spec_version: 1
family: demo
sources:
  - id: demo-src
    gather: file_glob
    artifact_class: trajectory
    change_detection: append_offset
    roots: ["~/.demo"]
    include: ["**/*.jsonl"]
`,
		"unknown spec_version": `
spec_version: 2
family: demo
sources:
  - id: demo-src
    gather: file_glob
    artifact_class: trajectory
    roots: ["~/.demo"]
    include: ["**/*.jsonl"]
`,
		"sidecar without a bounded probe": `
spec_version: 1
family: demo
sources:
  - id: demo-src
    gather: sidecar
    artifact_class: context
    emit: git_project_map
`,
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			var doc any
			if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
				t.Fatalf("fixture is not valid YAML: %v", err)
			}
			if err := formats.Validate(formats.SourceSpec, doc); err == nil {
				t.Error("must be rejected by the source-spec schema")
			}
		})
	}
}

// Group 1 is the v1 scope ceiling: Claude Code, Codex, Cursor. This list is the ceiling itself.
func TestGroupOneCoverage(t *testing.T) {
	files, err := catalogdata.Files()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"claude-code.yaml": false,
		"codex.yaml":       false,
		"cursor.yaml":      false,
	}
	for _, f := range files {
		if _, ok := want[f]; ok {
			want[f] = true
		}
	}
	for f, found := range want {
		if !found {
			t.Errorf("Group 1 catalog file missing: %s", f)
		}
	}
}

// Source ids key both object keys and fingerprint state, so a collision merges two sources.
func TestSourceIDsAreUniqueAcrossFiles(t *testing.T) {
	files, err := catalogdata.Files()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, name := range files {
		raw, err := catalogdata.Read(name)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Family  string `yaml:"family"`
			Sources []struct {
				ID         string          `yaml:"id"`
				Gather     string          `yaml:"gather"`
				Class      string          `yaml:"artifact_class"`
				Roots      []string        `yaml:"roots"`
				RequireSub string          `yaml:"require_subdir"`
				Include    []string        `yaml:"include"`
				Enrichers  map[string]bool `yaml:"enrichers"`
			} `yaml:"sources"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, s := range doc.Sources {
			if prev, dup := seen[s.ID]; dup {
				t.Errorf("source id %q appears in both %s and %s", s.ID, prev, name)
			}
			seen[s.ID] = name
		}
	}
}

// SQLite is enricher input only: no catalog entry may name a database as a shipping source.
func TestNoDatabaseShippingSources(t *testing.T) {
	files, err := catalogdata.Files()
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{"sqlite_rows", "sqlite_query", "sqlite_kv", "sqlite_snapshot"}
	dbGlobs := []string{".vscdb", ".sqlite", ".db"}

	for _, name := range files {
		raw, err := catalogdata.Read(name)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Sources []struct {
				ID      string   `yaml:"id"`
				Gather  string   `yaml:"gather"`
				Include []string `yaml:"include"`
			} `yaml:"sources"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, s := range doc.Sources {
			for _, b := range banned {
				if s.Gather == b {
					t.Errorf("%s/%s: gather %q is not a primitive — SQLite is enricher input only", name, s.ID, b)
				}
			}
			for _, g := range s.Include {
				for _, ext := range dbGlobs {
					if strings.Contains(g, ext) {
						t.Errorf("%s/%s: include %q would ship database bytes; no rows ship", name, s.ID, g)
					}
				}
			}
		}
	}
}

// Cursor's DB-side fidelity reaches the sink only through the enricher, so it must stay enabled.
func TestCursorTranscriptsDeclareTheEnricher(t *testing.T) {
	raw, err := catalogdata.Read("cursor.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sources []struct {
			ID        string          `yaml:"id"`
			Enrichers map[string]bool `yaml:"enrichers"`
		} `yaml:"sources"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, s := range doc.Sources {
		if s.ID == "cursor-transcripts" {
			if !s.Enrichers["cursor-transcript-join"] {
				t.Error("cursor-transcripts must enable cursor-transcript-join")
			}
			return
		}
	}
	t.Error("cursor-transcripts source not found")
}

// Every source declares its artifact class; an undeclared one would default to the wrong class.
func TestEverySourceDeclaresAClass(t *testing.T) {
	files, err := catalogdata.Files()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		raw, err := catalogdata.Read(name)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Sources []struct {
				ID    string `yaml:"id"`
				Class string `yaml:"artifact_class"`
			} `yaml:"sources"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, s := range doc.Sources {
			if s.Class != "trajectory" && s.Class != "context" {
				t.Errorf("%s/%s: artifact_class must be trajectory or context, got %q", name, s.ID, s.Class)
			}
		}
	}
}

// Every source that reads whole files into memory must declare a ceiling: the pipeline holds a
// payload several times over, so one runaway file is an OOM. Metadata-only sources are exempt.
func TestEveryFileReadingSourceDeclaresASizeCap(t *testing.T) {
	files, err := catalogdata.Files()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		raw, err := catalogdata.Read(name)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Sources []struct {
				ID           string `yaml:"id"`
				Gather       string `yaml:"gather"`
				MaxFileBytes int64  `yaml:"max_file_bytes"`
			} `yaml:"sources"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, s := range doc.Sources {
			if s.Gather != "file_glob" {
				continue
			}
			if s.MaxFileBytes <= 0 {
				t.Errorf("%s/%s reads whole files and declares no max_file_bytes", name, s.ID)
			}
		}
	}
}
