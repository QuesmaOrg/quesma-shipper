package engine

import (
	"slices"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

// A summary reports what the others discovered, so it runs after them however ids sort.
func TestSummariesRunLast(t *testing.T) {
	src := func(id, gather string) sources.Resolved {
		return sources.Resolved{Source: sources.Source{ID: id, Gather: gather}}
	}
	o := Options{Registry: sources.NewRegistry(), Plan: Plan{Sources: []sources.Resolved{
		src("a", "file_glob"), src("repositories", "repositories"), src("z", "sidecar"), src("bogus", "nope"),
	}}}
	var got []string
	for _, s := range o.runOrder() {
		got = append(got, s.ID)
	}
	if want := []string{"a", "z", "bogus", "repositories"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
