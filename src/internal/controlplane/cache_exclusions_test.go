package controlplane_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	protocol "github.com/QuesmaOrg/quesma-shipper/shipper-protocol"
	"gopkg.in/yaml.v3"
)

func resolveExclusionCache(t *testing.T, doc *config.Document) (*config.Effective, error) {
	t.Helper()
	catalog, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	local, err := config.ParseDocument([]byte("sources: [{id: claude-code-transcripts, exclude: ['projects/local/**']}]"))
	if err != nil {
		t.Fatal(err)
	}
	return config.Resolve(config.Input{Catalog: catalog, Env: sources.Env{Home: home, Lookup: func(string) (string, bool) { return "", false }}, StateDir: t.TempDir(), Layers: []config.LayeredDocument{{Layer: config.LayerUser, Doc: local}, {Layer: config.LayerRemote, Doc: doc}}})
}

func exclusionCacheSource(t *testing.T, eff *config.Effective) config.ResolvedSource {
	t.Helper()
	for _, source := range eff.Sources {
		if source.ID == "claude-code-transcripts" {
			return source
		}
	}
	t.Fatal("source missing")
	return config.ResolvedSource{}
}

func TestCacheKeepsAdditionsOrPausesSourceAfterOfflineDowngrade(t *testing.T) {
	p := newPlane(t)
	p.config = "config_version: 1\nsources: [{id: claude-code-transcripts, exclude_add: ['projects/private/**']}]\n"
	srv := p.start()
	dir := t.TempDir()
	e := p.enrolled(t, srv.URL, installID)
	options := controlplane.RefreshOptions{Enrollment: e, StateDir: dir, Now: time.Now(), Validate: func(doc *config.Document) (*config.Effective, error) { return resolveExclusionCache(t, doc) }}
	fetched := controlplane.Refresh(context.Background(), options)
	if fetched.Origin != controlplane.OriginFetched {
		t.Fatalf("fetch: %+v", fetched)
	}
	srv.Close()
	raw, _, err := platform.ReadWhole(filepath.Join(dir, controlplane.CacheFile), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	// This is schema 3's old tolerant reader: it knows no config_full field.
	var old struct {
		CacheSchema int       `json:"cache_schema"`
		Config      []byte    `json:"config"`
		FetchedAt   time.Time `json:"fetched_at"`
		ExpiresAt   time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	if old.CacheSchema != 3 {
		t.Fatalf("old reader would reject schema %d", old.CacheSchema)
	}
	legacy, err := config.ParseServedDocument(old.Config)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy.Sources[0].ExcludeAdd) != 0 {
		t.Fatal("old reader still needs the new field")
	}
	eff, err := resolveExclusionCache(t, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if exclusionCacheSource(t, eff).Enabled {
		t.Fatal("offline downgraded reader collected without additions")
	}
	options.Offline = true
	current := controlplane.Refresh(context.Background(), options)
	if current.Origin != controlplane.OriginCached {
		t.Fatalf("new offline reader: %+v", current)
	}
	source := exclusionCacheSource(t, current.Effective)
	if !source.Enabled || !slices.Contains(source.Exclude, "projects/private/**") || !slices.Contains(source.Exclude, "projects/local/**") {
		t.Fatalf("new reader lost additive/local exclusions: %+v", source)
	}
	oldRaw, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, controlplane.CacheFile), oldRaw, 0600); err != nil {
		t.Fatal(err)
	}
	upgraded := controlplane.Refresh(context.Background(), options)
	if upgraded.Origin != controlplane.OriginCached || exclusionCacheSource(t, upgraded.Effective).Enabled {
		t.Fatal("re-upgrade weakened an older writer's reduced cache")
	}
}

func TestCacheReductionMatchesProtocolFixtures(t *testing.T) {
	full, err := protocol.FS.ReadFile("fixtures/v1/exclusion-reduction/full.json")
	if err != nil {
		t.Fatal(err)
	}
	reduced, err := protocol.FS.ReadFile("fixtures/v1/exclusion-reduction/reduced.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := controlplane.SaveCache(dir, controlplane.Cached{Config: full}); err != nil {
		t.Fatal(err)
	}
	cache, err := controlplane.LoadCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := controlplane.SaveCache(dir, *cache); err != nil {
		t.Fatal(err)
	}
	cache, err = controlplane.LoadCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(cache.Document()) != string(full) {
		t.Fatal("full served bytes changed")
	}
	if !slices.Equal(cache.ConfigFullFeatures, []string{protocol.ExcludeAddFeature}) {
		t.Fatalf("full copy lists %v", cache.ConfigFullFeatures)
	}
	var got, want any
	if err := yaml.Unmarshal(cache.Config, &got); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(reduced, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reduced cache differs:\n%s", cache.Config)
	}
}

func TestCacheListsTheFeaturesTheReducerRemoved(t *testing.T) {
	for _, tc := range []struct {
		name     string
		config   string
		features []string
	}{
		{"additions", "config_version: 1\nsources: [{id: claude-code-transcripts, exclude_add: ['projects/private/**']}]\n", []string{"sources.exclude_add"}},
		{"multiple sources share a feature", "config_version: 1\nsources: [{id: claude-code-transcripts, exclude_add: ['projects/private/**']}, {id: codex-transcripts, exclude_add: ['private/**']}]\n", []string{"sources.exclude_add"}},
		{"nothing to reduce", "config_version: 1\nsources: [{id: claude-code-transcripts, exclude: ['projects/private/**']}]\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := controlplane.SaveCache(dir, controlplane.Cached{Config: []byte(tc.config)}); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, controlplane.CacheFile))
			if err != nil {
				t.Fatal(err)
			}
			var cache controlplane.Cached
			if err := json.Unmarshal(raw, &cache); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cache.ConfigFullFeatures, tc.features) {
				t.Fatalf("config_full_features %v, want %v", cache.ConfigFullFeatures, tc.features)
			}
			if hasFull := strings.Contains(string(raw), `"config_full"`); hasFull != (tc.features != nil) {
				t.Fatalf("config_full written: %v\n%s", hasFull, raw)
			}
			if tc.features != nil && string(cache.ConfigFull) != tc.config {
				t.Fatalf("full copy %q", cache.ConfigFull)
			}
		})
	}
}

func TestTwoCopyCacheNeverExceedsOldReaderLimit(t *testing.T) {
	p := newPlane(t)
	p.config = "config_version: 1\nsources: [{id: claude-code-transcripts, exclude_add: ['projects/private/**']}]\nfuture: " + strings.Repeat("x", 2<<20) + "\n"
	srv := p.start()
	dir := t.TempDir()
	e := p.enrolled(t, srv.URL, installID)
	options := controlplane.RefreshOptions{Enrollment: e, StateDir: dir, Now: time.Now(), Validate: func(doc *config.Document) (*config.Effective, error) { return resolveExclusionCache(t, doc) }}
	fetched := controlplane.Refresh(context.Background(), options)
	if fetched.Origin != controlplane.OriginFetched || !exclusionCacheSource(t, fetched.Effective).Enabled {
		t.Fatalf("current fetch should use full config: %+v", fetched)
	}
	cache, err := controlplane.LoadCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cache.ConfigFull) != 0 || cache.ConfigFullFeatures != nil {
		t.Fatal("oversized duplicate still persisted")
	}
	options.Offline = true
	offline := controlplane.Refresh(context.Background(), options)
	if offline.Origin != controlplane.OriginCached || exclusionCacheSource(t, offline.Effective).Enabled {
		t.Fatal("large cache did not keep the affected source off")
	}
}

func TestUnpersistableReductionCannotActivateNewConfig(t *testing.T) {
	p := newPlane(t)
	p.config = "config_version: 1\nmax_files_per_run: 7\n"
	srv := p.start()
	dir := t.TempDir()
	e := p.enrolled(t, srv.URL, installID)
	options := controlplane.RefreshOptions{Enrollment: e, StateDir: dir, Now: time.Now()}
	if got := controlplane.Refresh(context.Background(), options); got.Origin != controlplane.OriginFetched {
		t.Fatalf("initial fetch: %+v", got)
	}
	before, err := os.ReadFile(filepath.Join(dir, controlplane.CacheFile))
	if err != nil {
		t.Fatal(err)
	}
	p.config = "config_version: 1\nmax_files_per_run: 8\nsources: [{id: claude-code-transcripts, exclude_add: ['private/**']}]\nfuture: [" + strings.Repeat(strings.Repeat("x", 300)+",", 10300) + "]\n"
	got := controlplane.Refresh(context.Background(), options)
	if got.Origin != controlplane.OriginCached || got.Err == nil || !strings.Contains(got.Err.Error(), "reduced cache exceeds") {
		t.Fatalf("candidate activated or wrong rejection: %+v", got)
	}
	if got.Doc == nil || got.Doc.MaxFilesPerRun == nil || *got.Doc.MaxFilesPerRun != 7 {
		t.Fatal("unpersisted candidate became active")
	}
	after, err := os.ReadFile(filepath.Join(dir, controlplane.CacheFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("failed persistence changed accepted cache")
	}
}

func TestCachedFullCopyIsUsedOnlyWhenThisBuildCanEnforceIt(t *testing.T) {
	const reduced = "config_version: 1\nmax_files_per_run: 7\nsources: [{id: claude-code-transcripts, enabled: false}]\n"
	const full = "config_version: 1\nmax_files_per_run: 7\nsources: [{id: claude-code-transcripts, exclude_add: ['projects/private/**']}]\n"
	for _, tc := range []struct {
		name     string
		full     string
		features []string
		enforced bool
	}{
		{"supported", full, []string{protocol.ExcludeAddFeature}, true},
		{"written without the list", full, nil, true},
		{"needs an unknown feature", full, []string{protocol.ExcludeAddFeature, "sources.future"}, false},
		// A stricter validator in a later build refuses a full copy an earlier build accepted.
		{"rejected by validation", strings.Replace(full, "projects/private", "projects//private", 1), []string{protocol.ExcludeAddFeature}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			raw, err := json.Marshal(controlplane.Cached{CacheSchema: 3, Config: []byte(reduced), ConfigFull: []byte(tc.full), ConfigFullFeatures: tc.features, FetchedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, controlplane.CacheFile), raw, 0600); err != nil {
				t.Fatal(err)
			}
			got := controlplane.Refresh(context.Background(), controlplane.RefreshOptions{
				Enrollment: &controlplane.Enrollment{Endpoint: "https://fleet.invalid"}, StateDir: dir, Now: time.Now(), Offline: true,
				Validate: func(doc *config.Document) (*config.Effective, error) { return resolveExclusionCache(t, doc) },
			})
			if got.Origin != controlplane.OriginCached || got.Doc.MaxFilesPerRun == nil || *got.Doc.MaxFilesPerRun != 7 {
				t.Fatalf("remote layer lost: %+v", got)
			}
			source := exclusionCacheSource(t, got.Effective)
			if source.Enabled != tc.enforced || tc.enforced && !slices.Contains(source.Exclude, "projects/private/**") {
				t.Fatalf("source %+v, want full copy enforced: %v", source, tc.enforced)
			}
			if rejected := errors.Is(got.Err, controlplane.ErrConfigRejected); rejected != (tc.name == "rejected by validation") {
				t.Fatalf("rejection reported %v: %v", rejected, got.Err)
			}
		})
	}
}
