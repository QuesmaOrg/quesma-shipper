package app

import (
	"cmp"
	"slices"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

type RepoRow struct {
	Name, Dir      string
	Bytes, Pending int64
	Last           time.Time
	Markers        []string
	Off            bool
}

type AgentRow struct {
	Family, Display string
	Repos           []RepoRow
	Bytes, Pending  int64
	PendingKnown    bool
	Last            time.Time
}

func Survey(eff *config.Effective, paths config.Paths, attr *sources.RepoFilter, names map[string]string) []AgentRow {
	registry := sources.NewRegistry()
	byFamily := map[string]*AgentRow{}
	var order []string
	doc, docErr := engine.Peek(paths.StateDir)
	markers := map[string]string{}
	markerOf := func(cwd string) string {
		if m, hit := markers[cwd]; hit {
			return m
		}
		m, _ := attr.Marker(cwd)
		markers[cwd] = m
		return m
	}

	for _, src := range eff.Sources {
		if src.ArtifactClass != "trajectory" {
			continue
		}
		family := cmp.Or(src.Family, src.ID)
		row, seen := byFamily[family]
		if !seen {
			row = &AgentRow{Family: family, Display: cmp.Or(names[family], family)}
			byFamily[family] = row
			order = append(order, family)
		}
		if src.Root == "" {
			continue
		}
		prim, err := registry.For(src.Gather)
		if err != nil {
			continue
		}
		d, err := prim.Discover(sources.Request{Source: src, All: eff.Sources, Deny: eff.Deny, StateDir: paths.StateDir})
		if err != nil {
			continue
		}
		row.PendingKnown = docErr == nil
		for _, c := range d.Candidates {
			row.Add(attr.RepoDir(src, c), c, markerOf(attr.CWD(src, c)), docErr == nil && pendingFile(doc, src, c))
		}
	}

	out := make([]AgentRow, 0, len(order))
	for _, family := range order {
		row := byFamily[family]
		SortRepos(row.Repos)
		out = append(out, *row)
	}
	slices.SortStableFunc(out, func(a, b AgentRow) int { return b.Last.Compare(a.Last) })
	return out
}

func SortRepos(repos []RepoRow) {
	slices.SortStableFunc(repos, func(a, b RepoRow) int {
		return cmp.Or(day(b.Last).Compare(day(a.Last)), cmp.Compare(b.Bytes, a.Bytes))
	})
}

func day(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func (a *AgentRow) Add(dir string, c sources.Candidate, marker string, pending bool) {
	a.Bytes += c.Size
	a.Last = maxTime(a.Last, c.MTime)
	i := slices.IndexFunc(a.Repos, func(r RepoRow) bool { return r.Dir == dir })
	if i < 0 {
		a.Repos = append(a.Repos, RepoRow{Name: sources.RepoName(dir), Dir: dir, Off: true})
		i = len(a.Repos) - 1
	}
	r := &a.Repos[i]
	if marker == "" {
		r.Off = false
	} else if !slices.Contains(r.Markers, marker) {
		r.Markers = append(r.Markers, marker)
	}
	r.Bytes += c.Size
	r.Last = maxTime(r.Last, c.MTime)
	if pending {
		a.Pending += c.Size
		r.Pending += c.Size
	}
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func FamilyNames(compiled *sources.Compiled) map[string]string {
	names := map[string]string{}
	if compiled == nil {
		return names
	}
	for _, spec := range compiled.Specs {
		if spec.DisplayName != "" {
			names[spec.Family] = spec.DisplayName
		}
	}
	return names
}
