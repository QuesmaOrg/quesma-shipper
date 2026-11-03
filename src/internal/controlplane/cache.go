package controlplane

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	protocol "github.com/QuesmaOrg/quesma-shipper/shipper-protocol"
	"gopkg.in/yaml.v3"
)

// CacheFile holds the last remote config this install fetched.
const CacheFile = "remote-config.json"

// cacheSchema versions the file. An older cache is refused on load and treated as "no cache":
// the install collects under local config until the next successful fetch rewrites it.
const cacheSchema = 3

// Cached is the last remote config fetched, kept so a run with no reachable backend still has a
// remote layer.
type Cached struct {
	CacheSchema int `json:"cache_schema"`

	// Config is the safe fallback older clients read after a downgrade.
	Config     []byte `json:"config"`
	ConfigFull []byte `json:"config_full,omitempty"`

	// ConfigFullFeatures is what a reader must support before it may use ConfigFull.
	ConfigFullFeatures []string `json:"config_full_features,omitempty"`

	FetchedAt time.Time `json:"fetched_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SaveCache persists a fetched config.
func SaveCache(stateDir string, c Cached) error {
	c.CacheSchema = cacheSchema
	full := c.Document()
	reduced, changed, err := reduceCachedConfig(full)
	if err != nil {
		return err
	}
	c.Config, c.ConfigFull, c.ConfigFullFeatures = reduced, nil, nil
	if changed {
		c.ConfigFull, c.ConfigFullFeatures = full, []string{protocol.ExcludeAddFeature}
	}
	encoded, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// An older reader's size limit must still admit the cache after a downgrade.
	if len(encoded)+1 > maxResponseBytes {
		c.ConfigFull, c.ConfigFullFeatures = nil, nil
		encoded, err = json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		if len(encoded)+1 > maxResponseBytes {
			return fmt.Errorf("backend: reduced cache exceeds %d bytes", maxResponseBytes)
		}
	}
	return platform.WriteAtomic(filepath.Join(stateDir, CacheFile), append(encoded, '\n'), 0o600)
}

// LoadCache reads the cached config. A missing file is an error the caller treats as "no remote
// layer", which is a complete configuration on its own.
func LoadCache(stateDir string) (*Cached, error) {
	raw, _, err := platform.ReadWhole(filepath.Join(stateDir, CacheFile), maxResponseBytes)
	if err != nil {
		return nil, err
	}
	var c Cached
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("backend: parse cached config: %w", err)
	}
	if c.CacheSchema != cacheSchema {
		return nil, fmt.Errorf("backend: cache_schema %d, this client speaks %d",
			c.CacheSchema, cacheSchema)
	}
	return &c, nil
}

// Expired reports whether the cached config is past its expiry. Expiry does NOT stop collection:
// the scope kept was already served and can never widen, while halting would lose data for good.
func (c Cached) Expired(now time.Time) bool {
	return !c.ExpiresAt.IsZero() && now.After(c.ExpiresAt)
}

// Document selects the full layer when this client supports every feature it needs.
func (c Cached) Document() []byte {
	needs := c.ConfigFullFeatures
	if needs == nil {
		// A full copy without the list predates it, from builds whose only reduced feature was exclude_add.
		needs = []string{protocol.ExcludeAddFeature}
	}
	unsupported := func(feature string) bool { return !slices.Contains(config.Features, feature) }
	if len(c.ConfigFull) > 0 && !slices.ContainsFunc(needs, unsupported) {
		return c.ConfigFull
	}
	return c.Config
}

func reduceCachedConfig(raw []byte) ([]byte, bool, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, false, err
	}
	changed := false
	sources, _ := doc["sources"].([]any)
	var records []map[string]any
	var scopes []protocol.SourceExclusions
	for _, item := range sources {
		source, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := source["exclude_add"]; exists {
			changed = true
		}
		records = append(records, source)
	}
	if !changed {
		return raw, false, nil
	}
	for _, source := range records {
		encoded, err := yaml.Marshal(source)
		if err != nil {
			return nil, false, err
		}
		var scope protocol.SourceExclusions
		if err := yaml.Unmarshal(encoded, &scope); err != nil {
			return nil, false, err
		}
		scopes = append(scopes, scope)
	}
	for i, scope := range protocol.ReduceSourceExclusions(scopes) {
		delete(records[i], "exclude_add")
		if scope.Enabled != nil {
			records[i]["enabled"] = *scope.Enabled
		}
	}
	reduced, err := yaml.Marshal(doc)
	return reduced, true, err
}
