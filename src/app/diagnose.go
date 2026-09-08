package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

func Diagnose(ctx context.Context, build Build, verbose bool) *Report {
	rep := &Report{}
	eff, paths, remote, err := ResolveOnline(ctx)
	if err != nil {
		rep.Sections = []Section{{Title: "Configuration", Rows: []Row{{
			Sev: SevFail, Label: "config", Detail: fmt.Sprintf("refused: %v", err),
			Fix: "nothing runs until this resolves, `shipper config` walks the layers",
		}}}}
		rep.Update = UpdateStatus{State: "skipped", Detail: "not checked - config did not resolve"}
		return rep
	}

	unit, unitErr := identity.Load(paths.StateDir)
	enr, enrErr := controlplane.LoadEnrollment(paths.StateDir)
	if enrErr != nil {
		enr = nil
	}
	if enr != nil {
		rep.Organization, rep.Endpoint = enr.Organization, enr.Endpoint
	}
	updCh := make(chan UpdateStatus, 1)
	go func() {
		updCh <- checkUpdate(ctx, build, eff.AutoupdateEnabled, os.Getenv, packaging.CheckUpdate)
	}()

	username := ""
	if unitErr == nil {
		username = engine.UsernameFromStateDir(paths.StateDir)
	}
	doc, docErr := engine.Peek(paths.StateDir)
	registry := sources.NewRegistry()
	compiled, _ := sources.Load()
	var trackFilter *sources.RepoFilter
	if compiled != nil {
		trackFilter = compiled.RepoFilter()
	}
	var probes []sourceProbe
	for _, src := range eff.Sources {
		pr := sourceProbe{src: src}
		if src.Enabled {
			if prim, err := registry.For(src.Gather); err != nil {
				pr.err = err
			} else {
				pr.d, pr.err = prim.Discover(sources.Request{
					Source: src, All: eff.Sources, Deny: eff.Deny, Ignore: trackFilter,
					StateDir: paths.StateDir, Username: username,
				})
			}
			if pr.err == nil && docErr == nil {
				pr.pending, _ = pending(doc, src, pr.d)
			}
		}
		probes = append(probes, pr)
	}
	now := time.Now()
	agents, agentsCollecting, filesFound := agentRows(probes, FamilyNames(compiled), loadLastUpload(paths.StateDir), now, verbose)
	rep.AgentsCollecting, rep.FilesFound = agentsCollecting, filesFound

	var shipping []Row
	if env, err := NewFrom(build, eff, paths, remote); err != nil {
		shipping = append(shipping, Row{Sev: SevFail, Label: "storage", Brief: "cannot send",
			Detail: fmt.Sprintf("unavailable: %v", err),
			Fix:    "nothing is sent until this resolves"})
	} else {
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		where := ""
		if verbose {
			where = env.Destination() + ", "
		}
		if err := env.ProbeHeartbeat(pctx, doctorReport(probes)); err != nil {
			shipping = append(shipping, Row{Sev: SevFail, Label: "storage", Brief: "cannot send",
				Detail: where + "upload check failed: " + err.Error(),
				Fix:    "nothing can be sent until this works"})
		} else {
			shipping = append(shipping, Row{Sev: SevOK, Label: "storage",
				Detail: where + "one test file sent"})
		}
	}
	for _, row := range controlPlaneRows(enr, enrErr, eff, remote) {
		if verbose || row.Sev != SevOK {
			shipping = append(shipping, row)
		}
	}
	shipping = append(shipping, scheduleRows(paths.StateDir, now)...)
	var stateDetail []Row
	for _, row := range stateRowsFrom(doc, docErr) {
		if row.Sev == SevWarn {
			shipping = append(shipping, row)
		} else {
			stateDetail = append(stateDetail, row)
		}
	}
	rep.Sections = []Section{{Title: "Collecting", Rows: agents}, {Title: "Sending", Rows: shipping}}

	if verbose {
		var tech []Row
		for _, pr := range probes {
			tech = append(tech, technicalSourceRows(pr)...)
		}
		conf := []Row{{Sev: SevDim, Label: "build", Detail: VersionLine(build)}}
		if unitErr != nil {
			conf = append(conf, Row{Sev: SevFail, Label: "identity", Detail: fmt.Sprintf("missing: %v", unitErr), Fix: "`shipper login`"})
		} else {
			conf = append(conf, Row{Sev: SevDim, Label: "install_id", Detail: unit.InstallID.String()})
		}
		conf = append(conf,
			Row{Sev: SevDim, Label: "state_dir", Detail: paths.StateDir},
			Row{Sev: SevDim, Label: "config_version", Detail: fmt.Sprintf("%d", eff.ConfigVersion)},
			Row{Sev: SevDim, Label: "rule_packs", Detail: fmt.Sprintf("%v", eff.RulePacks)},
			Row{Sev: SevDim, Label: "deny_patterns", Detail: fmt.Sprintf("%d compiled + served additions", len(eff.Deny.Patterns()))},
		)
		conf = append(conf, enrollmentRows(paths.StateDir, enr, enrErr, eff, remote)...)
		conf = append(conf, stateDetail...)
		if st := packaging.ServiceState(paths.StateDir); st.Path != "" {
			conf = append(conf, Row{Sev: SevDim, Label: "service_entry", Detail: st.Path})
		}
		rep.Sections = append(rep.Sections, Section{Title: "Sources (technical)", Rows: tech}, Section{Title: "Configuration", Rows: conf})
		for si := 2; si < len(rep.Sections); si++ {
			for ri := range rep.Sections[si].Rows {
				if rep.Sections[si].Rows[ri].Sev == SevWarn {
					rep.Sections[si].Rows[ri].Rollup = true
				}
			}
		}
	} else if unitErr != nil {
		rep.Sections[1].Rows = append(rep.Sections[1].Rows, Row{Sev: SevFail, Label: "identity",
			Detail: fmt.Sprintf("missing: %v", unitErr), Fix: "`shipper login`"})
	}
	rep.Update = <-updCh
	return rep
}

func stateRowsFrom(doc engine.Document, err error) []Row {
	if err != nil {
		return []Row{{Sev: SevWarn, Label: "local state", Brief: "local state unreadable",
			Detail: fmt.Sprintf("unreadable: %v", err), Fix: "`shipper state` inspects and repairs it"}}
	}
	rows := []Row{{Sev: SevDim, Label: "tracked_files", Detail: fmt.Sprintf("%d", len(doc.Entries))}}
	parked := 0
	var fixes []string
	for k, fp := range doc.Entries {
		if fp.Parked {
			parked++
			fixes = append(fixes, fmt.Sprintf("%s: %s", k.SourceID, fp.LastError))
		}
	}
	if parked > 0 {
		rows = append(rows, Row{Sev: SevWarn, Label: "set aside", Brief: "files set aside after repeated failures",
			Detail: fmt.Sprintf("%d %s set aside after repeated failures", parked, Plural(parked, "file")),
			Fix:    strings.Join(fixes, "\n")})
	}
	if !doc.UpdatedAt.IsZero() {
		rows = append(rows, Row{Sev: SevDim, Label: "state_updated", Detail: doc.UpdatedAt.Format(time.RFC3339)})
	}
	return rows
}
