package protocol

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

const ExcludeAddFeature = "sources.exclude_add"

// ValidateExcludeAdd requires portable globs relative to a source's resolved root.
func ValidateExcludeAdd(pattern string) error {
	if pattern == "" || strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, `\`) ||
		(len(pattern) >= 2 && pattern[1] == ':') || strings.ContainsRune(pattern, 0) {
		return fmt.Errorf("exclude_add must be a non-empty relative glob")
	}
	for _, part := range strings.Split(strings.ReplaceAll(pattern, `\`, "/"), "/") {
		if part == ".." {
			return fmt.Errorf("exclude_add must not contain ..")
		}
	}
	if !doublestar.ValidatePattern(pattern) {
		return fmt.Errorf("exclude_add is not a valid doublestar glob")
	}
	return nil
}

type SourceExclusions struct {
	ID         string   `json:"id" yaml:"id"`
	Enabled    *bool    `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	ExcludeAdd []string `json:"exclude_add,omitempty" yaml:"exclude_add,omitempty"`
}

// ReduceSourceExclusions pauses every entry for an affected ID, including duplicate overrides.
func ReduceSourceExclusions(sources []SourceExclusions) []SourceExclusions {
	paused := map[string]bool{}
	for _, source := range sources {
		if len(source.ExcludeAdd) > 0 {
			paused[source.ID] = true
		}
	}
	out := slices.Clone(sources)
	for i := range out {
		if paused[out[i].ID] {
			off := false
			out[i].Enabled = &off
		}
		out[i].ExcludeAdd = nil
	}
	return out
}
