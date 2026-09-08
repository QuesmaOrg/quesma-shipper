package app

import (
	"os"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

type AgentSummary struct {
	Name         string `json:"name"`
	Files        int    `json:"files"`
	PendingBytes int64  `json:"pending_bytes"`
}

type Status struct {
	Version      string         `json:"version"`
	Organization string         `json:"organization,omitempty"`
	Machine      string         `json:"machine,omitempty"`
	LoggedIn     bool           `json:"logged_in"`
	Service      string         `json:"service"`
	ServiceOK    bool           `json:"service_ok"`
	Paused       bool           `json:"paused"`
	PausedUntil  *time.Time     `json:"paused_until,omitempty"`
	LastSent     *time.Time     `json:"last_sent,omitempty"`
	Endpoint     string         `json:"endpoint,omitempty"`
	Agents       []AgentSummary `json:"agents"`
	PendingBytes int64          `json:"pending_bytes"`
	Off          []string       `json:"off,omitempty"`
	Destination  string         `json:"destination"`
	StateDir     string         `json:"state_dir"`
}

func CurrentStatus(build Build) (Status, error) {
	st := Status{Version: build.Version}
	eff, paths, err := ResolveEffective()
	if err != nil {
		return st, err
	}
	st.StateDir = paths.StateDir
	st.Destination = DescribeDestination(eff)
	if _, err := identity.Load(paths.StateDir); err == nil {
		st.LoggedIn = true
	}
	if enr, err := controlplane.LoadEnrollment(paths.StateDir); err == nil {
		st.Organization, st.Endpoint = enr.Organization, enr.Endpoint
	}
	st.Machine, _ = os.Hostname()

	sv := packaging.ServiceState(paths.StateDir)
	if !sv.LastRun.IsZero() {
		st.LastSent = &sv.LastRun
	}
	switch {
	case sv.Loaded:
		st.Service, st.ServiceOK = string(sv.Kind), true
	case sv.Installed:
		st.Service = "installed but not running"
	default:
		st.Service = "not installed"
	}
	if p := platform.Read(paths.StateDir); p.Paused {
		st.Paused = true
		if u := p.UntilTime(); !u.IsZero() {
			st.PausedUntil = &u
		}
	}

	compiled, _ := sources.Load()
	if compiled == nil {
		return st, nil
	}
	rows := Survey(eff, paths, compiled.RepoFilter(), FamilyNames(compiled))
	doc, docErr := engine.Peek(paths.StateDir)
	registry := sources.NewRegistry()
	for _, a := range rows {
		if len(a.Repos) == 0 {
			continue
		}
		sum := AgentSummary{Name: a.Display}
		for _, src := range eff.Sources {
			if src.Family != a.Family || src.Root == "" || !src.Enabled {
				continue
			}
			prim, err := registry.For(src.Gather)
			if err != nil {
				continue
			}
			d, err := prim.Discover(sources.Request{Source: src, All: eff.Sources, Deny: eff.Deny,
				Ignore: compiled.RepoFilter(), StateDir: paths.StateDir})
			if err != nil {
				continue
			}
			sum.Files += len(d.Candidates)
			if docErr == nil {
				_, bytes := pending(doc, src, d)
				sum.PendingBytes += bytes
			}
		}
		st.PendingBytes += sum.PendingBytes
		st.Agents = append(st.Agents, sum)
		for _, r := range a.Repos {
			if r.Off && r.Dir != "" {
				st.Off = append(st.Off, r.Dir)
			}
		}
	}
	return st, nil
}
