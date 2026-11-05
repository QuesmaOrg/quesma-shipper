package engine

import (
	"slices"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

// The repositories source reports what the others discovered, so it runs after them however ids sort.
func TestRepositoriesRunsLast(t *testing.T) {
	src := func(id, gather string) sources.Resolved {
		return sources.Resolved{Source: sources.Source{ID: id, Gather: gather}}
	}
	in := []sources.Resolved{src("a", "file_glob"), src("repositories", sources.RepositoriesGather), src("z", "sidecar")}
	var got []string
	for _, s := range runOrder(in) {
		got = append(got, s.ID)
	}
	if want := []string{"a", "z", "repositories"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
