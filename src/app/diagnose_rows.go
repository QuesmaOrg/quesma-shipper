package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

const (
	updateCheckTimeout = 5 * time.Second
	NoSelfUpdateEnv    = "SHIPPER_NO_SELFUPDATE"
	ReexecGuardEnv     = "SHIPPER_SELFUPDATE_REEXEC"
)

type UpdateStatus struct {
	State     string // disabled | skipped | failed | available | current
	Latest    string
	Published time.Time
	Detail    string
	Fix       string // the exact command, rendered as an arrow line under the header
}

func checkUpdate(ctx context.Context, build Build, autoupdate bool, getenv func(string) string,
	check func(context.Context, packaging.UpdateOptions) (string, time.Time, bool, error)) UpdateStatus {
	if getenv(NoSelfUpdateEnv) != "" {
		return UpdateStatus{State: "disabled", Detail: "check disabled by " + NoSelfUpdateEnv}
	}
	if !autoupdate {
		return UpdateStatus{State: "disabled", Detail: "check disabled by autoupdate.enabled: false"}
	}
	// Only a release build self-updates, and it does so through TUF. A dev build reports its own
	// stamp and stops: no network, no comparison against a source repository.
	if !build.Release {
		return UpdateStatus{State: "skipped", Detail: "dev build; self-update installs only in release builds"}
	}
	latest, published, available, err := check(ctx, packaging.UpdateOptions{Current: build.Version, Timeout: updateCheckTimeout})
	when := ""
	if !published.IsZero() {
		when = ", " + published.Format("2006-01-02")
	}
	switch {
	case err != nil:
		return UpdateStatus{State: "failed", Detail: fmt.Sprintf("check failed - %v", err)}
	case available:
		return UpdateStatus{State: "available", Latest: latest, Published: published,
			Detail: fmt.Sprintf("%s available (%s)", latest, published.Format("2006-01-02")),
			Fix:    "quesma-shipper update"}
	default:
		return UpdateStatus{State: "current", Latest: latest, Published: published,
			Detail: fmt.Sprintf("up to date (newest release is %s%s)", latest, when)}
	}
}

type sourceProbe struct {
	src     config.ResolvedSource
	d       sources.Discovery
	err     error
	pending int
}

func pending(doc engine.Document, src config.ResolvedSource, d sources.Discovery) (files int, bytes int64) {
	for _, c := range d.Candidates {
		if pendingFile(doc, src, c) {
			files++
			bytes += c.Size
		}
	}
	return files, bytes
}

func pendingFile(doc engine.Document, src config.ResolvedSource, c sources.Candidate) bool {
	if stored, known := doc.SourceSpecs[src.ID]; known && stored != src.SpecFingerprint {
		return true
	}
	fp, seen := doc.Entries[engine.Key{SourceID: src.ID, NativePath: c.Path}]
	return !seen || fp.SourceSize != c.Size || fp.SourceMTime.UnixNano() != c.MTime.UnixNano()
}

type lastUpload struct {
	at       time.Time
	bySource map[string]engine.SourceHealth
}

func loadLastUpload(stateDir string) lastUpload {
	raw, err := os.ReadFile(filepath.Join(stateDir, engine.Name))
	if err != nil {
		return lastUpload{}
	}
	var hb engine.Heartbeat
	if err := json.Unmarshal(raw, &hb); err != nil {
		return lastUpload{}
	}
	at, err := time.Parse(time.RFC3339, hb.At)
	if err != nil {
		return lastUpload{}
	}
	up := lastUpload{at: at, bySource: map[string]engine.SourceHealth{}}
	for _, s := range hb.Sources {
		up.bySource[s.SourceID] = s
	}
	return up
}

const sidecarFamily = "project-map"

func agentRows(probes []sourceProbe, names map[string]string, up lastUpload, now time.Time, verbose bool) (rows []Row, agents, files int) {
	var order []string
	byFam := map[string][]sourceProbe{}
	for _, pr := range probes {
		f := pr.src.Family
		if f == "" {
			f = pr.src.ID
		}
		if _, seen := byFam[f]; !seen {
			order = append(order, f)
		}
		byFam[f] = append(byFam[f], pr)
	}
	for _, f := range order {
		if f == sidecarFamily && !verbose {
			continue
		}
		name := names[f]
		if name == "" {
			name = f
		}
		famRows, collecting, famFiles := familyRows(name, byFam[f], familyUploadFor(byFam[f], up), now, verbose)
		rows = append(rows, famRows...)
		if collecting && f != sidecarFamily {
			agents++
		}
		files += famFiles
	}
	return rows, agents, files
}

type familyUpload struct {
	recorded        bool
	at              time.Time
	shipped, failed int
	pending         int
}

func familyUploadFor(probes []sourceProbe, up lastUpload) familyUpload {
	agg := familyUpload{at: up.at}
	for _, pr := range probes {
		agg.pending += pr.pending
		if s, ok := up.bySource[pr.src.ID]; ok {
			agg.recorded = true
			agg.shipped += s.Shipped
			agg.failed += s.Failed
		}
	}
	return agg
}

func familyRows(name string, probes []sourceProbe, up familyUpload, now time.Time, verbose bool) (rows []Row, collecting bool, files int) {
	var parts []string
	var issues []Row
	var disabled []Row
	sniffed := ""
	absent := 0
	var noMatch []config.ResolvedSource

	// Every per-part warning is the same row shape; short is what the rollup line says went wrong.
	warn := func(part, short, detail, fix string) Row {
		return Row{Sev: SevWarn, Sub: true, Label: "  " + part,
			Brief: name + " " + part + ": " + short, Detail: detail, Fix: fix}
	}

	for _, pr := range probes {
		src, d := pr.src, pr.d
		part := partName(src)
		if !src.Enabled {
			disabled = append(disabled, Row{Sev: SevDim, Label: "  " + part, Detail: "disabled by configuration"})
			continue
		}
		if pr.err != nil {
			issues = append(issues, warn(part, "discovery error",
				fmt.Sprintf("error: %v", pr.err),
				fmt.Sprintf("check sources[%s] in config", src.ID)))
			continue
		}
		switch d.Health {
		case sources.Collected:
			collecting = true
			files += len(d.Candidates)
			parts = append(parts, part)
			if d.AgentVersion != "" {
				sniffed = d.AgentVersion
			}
			if d.Sniff != "" && d.Sniff != sources.SniffOK {
				issues = append(issues, warn(part, "unexpected format",
					"content does not look like the expected format",
					"the agent may have changed formats, `quesma-shipper preview` shows what would ship"))
			}
		case sources.AgentAbsent:
			absent++
		case sources.RootPresentNoMatch:
			if d.Ignored {
				disabled = append(disabled, Row{Sev: SevDim, Label: "  " + part, Detail: d.Reason})
				break
			}
			noMatch = append(noMatch, src)
		case sources.MatchPresentUnreadable:
			issues = append(issues, warn(part, "unreadable",
				"found but unreadable, "+d.Reason,
				"fix permissions on the store"))
		default:
			issues = append(issues, warn(part, string(d.Health),
				string(d.Health)+", "+d.Reason, ""))
		}

		if d.Unreadable > 0 && d.Health != sources.MatchPresentUnreadable {
			issues = append(issues, warn(part, "some files unreadable",
				fmt.Sprintf("%d files could not be read - %s", d.Unreadable, d.UnreadableReason),
				"first failing path: "+d.UnreadableExample))
		}
		if len(d.Oversize) > 0 {
			largest := largestOversize(d.Oversize)
			issues = append(issues, warn(part, "files over the size cap",
				fmt.Sprintf("%d files over the %s cap are never collected (largest %s)",
					len(d.Oversize), HumanBytes(largest.Limit), HumanBytes(largest.Size)),
				fmt.Sprintf("raise sources[%s].max_file_bytes to collect them", src.ID)))
		}
		issues = append(issues, enricherIssues(name, src)...)
	}

	for _, src := range noMatch {
		if collecting {
			if verbose {
				disabled = append(disabled, Row{Sev: SevDim, Sub: true, Detail: "no " + partName(src) + " files yet"})
			}
			continue
		}
		issues = append(issues, Row{Sev: SevWarn, Sub: true,
			Brief:  name + ": nothing found in " + HomeTilde(src.Root),
			Detail: "nothing found in " + HomeTilde(src.Root) + ", " + name + " may have changed where it writes"})
	}

	if !collecting && len(issues) == 0 {
		if len(disabled) > 0 {
			return append([]Row{{Sev: SevDim, Label: name, Detail: "disabled by configuration"}}, disabled...), false, 0
		}
		if absent > 0 {
			return []Row{{Sev: SevDim, Label: name, Name: true, Detail: "not installed"}}, false, 0
		}
	}

	head := Row{Sev: SevOK, Label: name, Name: true, Tag: sniffed}
	switch {
	case collecting:
		head.Detail = CountNoun(files, "file")
		if verbose && len(parts) > 1 {
			head.Detail += " (" + strings.Join(parts, ", ") + ")"
		}
		if h := claudeHeadline(probes, verbose); h != "" {
			head.Detail = h
		}
	default:
		head.Sev = SevWarn
		head.Detail = "present but nothing is collected"
		head.Brief = name + ": nothing collected"
	}

	if collecting && (up.recorded || up.pending > 0) && (verbose || up.failed > 0) {
		detail := ""
		switch {
		case up.recorded && up.shipped > 0:
			detail = fmt.Sprintf("%s, %s sent", Ago(up.at, now), CountNoun(up.shipped, "file"))
		case up.recorded:
			detail = fmt.Sprintf("%s, nothing new", Ago(up.at, now))
		default:
			detail = "none yet"
		}
		if up.failed > 0 {
			detail += fmt.Sprintf(", %d failed", up.failed)
		}
		if up.pending > 0 {
			detail += fmt.Sprintf(", %s changed since", CountNoun(up.pending, "file"))
		}
		row := Row{Sev: SevDim, Sub: true, Label: "  last upload", Detail: detail}
		if up.failed > 0 {
			row.Sev = SevWarn
			row.Brief = name + ": upload failures"
			row.Fix = "`quesma-shipper log` shows each file's outcome"
		}
		issues = append([]Row{row}, issues...)
	}
	for _, issue := range issues {
		if issue.Sev == SevWarn {
			head.Sev = SevWarn
			head.Rollup = true
			break
		}
	}
	return append(append([]Row{head}, issues...), disabled...), collecting, files
}

func claudeHeadline(probes []sourceProbe, verbose bool) string {
	sessions := 0
	projects := map[string]bool{}
	var extras []string
	for _, pr := range probes {
		if pr.src.Family != "claude-code" {
			return ""
		}
		if !pr.src.Enabled || pr.err != nil || pr.d.Health != sources.Collected {
			continue
		}
		if pr.src.ID != "claude-code-transcripts" {
			extras = append(extras, partName(pr.src))
			continue
		}
		for _, c := range pr.d.Candidates {
			if strings.HasSuffix(c.RelPath, ".jsonl") {
				sessions++
			}
			if rest, ok := strings.CutPrefix(c.RelPath, "projects/"); ok {
				if proj, _, found := strings.Cut(rest, "/"); found {
					projects[proj] = true
				}
			}
		}
	}
	if sessions == 0 || len(projects) == 0 {
		return ""
	}
	h := fmt.Sprintf("%s in %s", CountNoun(sessions, "session"), CountNoun(len(projects), "project"))
	if verbose && len(extras) > 0 {
		h += ", plus " + strings.Join(extras, " and ")
	}
	return h
}

func HomeTilde(path string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, home) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

func partName(src config.ResolvedSource) string {
	if src.Family != "" {
		if p := strings.TrimPrefix(src.ID, src.Family+"-"); p != src.ID && p != "" {
			return p
		}
	}
	return src.ID
}

type enricherProbe struct {
	id  string
	e   transforms.Enricher
	on  bool
	db  string // resolved database path; empty means not found
	err error  // declared but not in this build
}

func probeEnrichers(src config.ResolvedSource) []enricherProbe {
	if len(src.Enrichers) == 0 {
		return nil
	}
	compiled := Enrichers()
	var out []enricherProbe
	for _, id := range slices.Sorted(maps.Keys(src.Enrichers)) {
		p := enricherProbe{id: id, on: src.Enrichers[id]}
		if p.e, p.err = compiled.For(id); p.err == nil && p.on {
			p.db = firstExistingDB(p.e)
		}
		out = append(out, p)
	}
	return out
}

func enricherIssues(name string, src config.ResolvedSource) []Row {
	var rows []Row
	for _, p := range probeEnrichers(src) {
		switch {
		case p.err != nil:
			rows = append(rows, Row{Sev: SevWarn, Sub: true, Label: "  database",
				Brief:  name + ": enricher missing from this build",
				Detail: "tool results and timestamps are not captured - enricher " + p.id + " is not in this build",
				Fix:    "`quesma-shipper update` may carry it"})
		case !p.on:
			rows = append(rows, Row{Sev: SevDim, Label: "  database",
				Detail: "enrichment disabled - no tool results, call ids or timestamps"})
		case p.db == "":
			rows = append(rows, Row{Sev: SevWarn, Sub: true, Label: "  database",
				Brief:  name + ": enrichment database not found",
				Detail: "not found, raw files only, no tool results or timestamps",
				Fix:    "looked for " + strings.Join(p.e.DBCandidates(), ", ")})
		}
	}
	return rows
}

func technicalSourceRows(pr sourceProbe) []Row {
	src := pr.src
	if !src.Enabled {
		return []Row{{Sev: SevDim, Label: src.ID, Detail: "disabled by configuration"}}
	}
	if pr.err != nil {
		return []Row{{Sev: SevWarn, Label: src.ID, Detail: fmt.Sprintf("error: %v", pr.err)}}
	}
	return append(discoveryRows(src, pr.d), enricherRows(src)...)
}

func discoveryRows(src config.ResolvedSource, d sources.Discovery) []Row {
	var rows []Row
	state := string(d.Health)
	switch d.Health {
	case sources.Collected:
		detail := fmt.Sprintf("%s - %d candidate(s)", state, len(d.Candidates))
		if d.AgentVersion != "" {
			detail += ", agent " + d.AgentVersion
		}
		if d.Sniff != "" && d.Sniff != sources.SniffOK {
			rows = append(rows, Row{Sev: SevWarn, Label: src.ID,
				Detail: detail + ", sniff " + string(d.Sniff),
				Fix:    "the agent's format may have changed under this build, `quesma-shipper preview` shows what would ship"})
		} else {
			rows = append(rows, Row{Sev: SevOK, Label: src.ID, Detail: detail})
		}
	default:
		// Every health other than Collected reports the same line and differs only in severity and fix.
		sev, fix := SevWarn, ""
		switch {
		case d.Health == sources.AgentAbsent, d.Health == sources.RootPresentNoMatch && d.Ignored:
			sev = SevDim
		case d.Health == sources.RootPresentNoMatch:
			fix = "the store may have moved, files there are not collected"
		case d.Health == sources.MatchPresentUnreadable:
			fix = "fix permissions on the store, or the agent changed its format under this build"
		}
		rows = append(rows, Row{Sev: sev, Label: src.ID, Detail: state + ", " + d.Reason, Fix: fix})
	}

	if d.Unreadable > 0 && d.Health != sources.MatchPresentUnreadable {
		rows = append(rows, Row{Sev: SevWarn, Label: "  unreadable",
			Detail: fmt.Sprintf("%d path(s) unreadable during discovery - %s", d.Unreadable, d.UnreadableReason),
			Fix:    "first failing path: " + d.UnreadableExample})
	}
	if len(d.Oversize) > 0 {
		largest := largestOversize(d.Oversize)
		rows = append(rows, Row{Sev: SevWarn, Label: "  size cap",
			Detail: fmt.Sprintf("%d file(s) over the %s cap, never collected - largest %s",
				len(d.Oversize), HumanBytes(largest.Limit), HumanBytes(largest.Size)),
			Fix: fmt.Sprintf("raise sources[%s].max_file_bytes to collect them", src.ID)})
	}
	return rows
}

func controlPlaneRows(enr *controlplane.Enrollment, enrErr error,
	eff *config.Effective, remote controlplane.Remote) []Row {
	if enr == nil {
		if enrErr != nil && !errors.Is(enrErr, os.ErrNotExist) {
			return []Row{{Sev: SevWarn, Label: "server",
				Detail: fmt.Sprintf("enrollment record unreadable: %v", enrErr),
				Fix:    "the install behaves as standalone until this is fixed"}}
		}
		return nil
	}
	var rows []Row
	if remote.Err != nil {
		rows = append(rows, Row{Sev: SevWarn, Label: "server", Brief: "server unreachable",
			Detail: fmt.Sprintf("unreachable, running on cached settings: %v", remote.Err),
			Fix:    "collecting continues, settings changes wait until it answers"})
	} else {
		rows = append(rows, Row{Sev: SevOK, Label: "server",
			Detail: "connected, settings current"})
	}
	if remote.Expired || eff.ConfigExpired {
		rows = append(rows, Row{Sev: SevWarn, Label: "server", Brief: "cached settings expired",
			Detail: "cached settings are past their expiry, still collecting",
			Fix:    "check that the server is reachable"})
	}
	return rows
}

func scheduleRows(stateDir string, now time.Time) []Row {
	var rows []Row
	if p := platform.Read(stateDir); p.Paused {
		rows = append(rows, Row{Sev: SevWarn, Label: "collecting", Brief: "paused",
			Detail: "paused until " + FormatUntil(p.UntilTime(), now),
			Fix:    "`quesma-shipper resume`"})
	}

	st := packaging.ServiceState(stateDir)
	switch {
	case st.Loaded && st.LastRun.IsZero():
		rows = append(rows, Row{Sev: SevWarn, Label: "background service", Brief: "background service never ran",
			Detail: "loaded but it has never completed a run",
			Fix:    "`quesma-shipper log` shows what each run did"})
	case st.Loaded:
		rows = append(rows, Row{Sev: SevOK, Label: "background service",
			Detail: "running"})
	case st.Installed:
		rows = append(rows, Row{Sev: SevWarn, Label: "background service", Brief: "background service not loaded",
			Detail: "installed but not loaded",
			Fix:    "re-run the Shipper installer"})
	default:
		rows = append(rows, Row{Sev: SevWarn, Label: "background service", Brief: "no background service",
			Detail: "not installed, nothing is sent on its own",
			Fix:    "re-run the Shipper installer"})
	}
	return rows
}

func doctorReport(probes []sourceProbe) formats.Report {
	var rep formats.Report
	for _, pr := range probes {
		if !pr.src.Enabled || pr.err != nil {
			continue
		}
		rep.Sources = append(rep.Sources, formats.SourceOutcome{
			SourceID:     pr.src.ID,
			Family:       pr.src.Family,
			Health:       pr.d.Health,
			Sniff:        pr.d.Sniff,
			AgentVersion: pr.d.AgentVersion,
			Reason:       pr.d.Reason,
			Oversize:     len(pr.d.Oversize),
			Unreadable:   pr.d.Unreadable,
		})
	}
	return rep
}

func enrollmentRows(stateDir string, enr *controlplane.Enrollment, enrErr error,
	eff *config.Effective, remote controlplane.Remote) []Row {
	if enr == nil {
		if enrErr == nil || errors.Is(enrErr, os.ErrNotExist) {
			return []Row{{Sev: SevDim, Label: "enrollment",
				Detail: "standalone - no endpoint configured; no control-plane calls"}}
		}
		return []Row{{Sev: SevWarn, Label: "enrollment", Detail: fmt.Sprintf("unreadable: %v", enrErr)}}
	}

	rows := []Row{
		{Sev: SevDim, Label: "enrollment", Detail: fmt.Sprintf("%s - organization %s", enr.Endpoint, enr.Organization)},
	}
	if root, err := formats.InstallRoot(enr.Organization, enr.InstallID); err == nil {
		rows = append(rows, Row{Sev: SevDim, Label: "install_key_root", Detail: root})
	}

	if c, err := controlplane.LoadCache(stateDir); err == nil {
		if c.Expired(time.Now()) {
			rows = append(rows, Row{Sev: SevWarn, Label: "cached_config",
				Detail: fmt.Sprintf("expired - fetched %s, expired %s",
					c.FetchedAt.Format(time.RFC3339), c.ExpiresAt.Format(time.RFC3339)),
				Fix: "collecting under it and stamping config_expired; check that the control plane is reachable"})
		} else {
			rows = append(rows, Row{Sev: SevOK, Label: "cached_config",
				Detail: fmt.Sprintf("current - fetched %s, expires %s",
					c.FetchedAt.Format(time.RFC3339), c.ExpiresAt.Format(time.RFC3339))})
		}
	} else {
		rows = append(rows, Row{Sev: SevDim, Label: "cached_config",
			Detail: "none - no remote config has verified yet"})
	}

	rows = append(rows, Row{Sev: SevDim, Label: "config_source", Detail: string(remote.Origin)})
	if remote.Err != nil {
		rows = append(rows, Row{Sev: SevWarn, Label: "config_fetch",
			Detail: fmt.Sprintf("failed: %v", remote.Err),
			Fix:    "the run still collects under cached/local layers; a pushed policy change has not taken effect"})
	}
	if eff.ConfigExpired {
		rows = append(rows, Row{Sev: SevWarn, Label: "config_expired",
			Detail: "true - every manifest this run carries the stamp"})
	}
	return rows
}

func enricherRows(src config.ResolvedSource) []Row {
	var rows []Row
	for _, p := range probeEnrichers(src) {
		switch {
		case p.err != nil:
			rows = append(rows, Row{Sev: SevWarn, Label: "  enricher " + p.id,
				Detail: fmt.Sprintf("declared but not in this build: %v", p.err)})
		case !p.on:
			rows = append(rows, Row{Sev: SevDim, Label: "  enricher " + p.id,
				Detail: "disabled - no tool results, call ids or timestamps will be collected"})
		case p.db == "":
			rows = append(rows, Row{Sev: SevWarn, Label: fmt.Sprintf("  enricher %s@%d", p.id, p.e.Version()),
				Detail: strings.Join(p.e.DBCandidates(), ", ") + " not found, raw files only"})
		default:
			rows = append(rows, Row{Sev: SevOK, Label: fmt.Sprintf("  enricher %s@%d", p.id, p.e.Version()),
				Detail: fmt.Sprintf("reads %s (%s, keyspaces %v)", p.db, p.e.Table(), p.e.Keyspaces())})
		}
	}
	return rows
}

func firstExistingDB(e transforms.Enricher) string {
	env, err := sources.OSEnv()
	if err != nil {
		return ""
	}
	return env.FirstExistingFile(e.DBCandidates())
}

func largestOversize(o []sources.Oversize) sources.Oversize {
	return slices.MaxFunc(o, func(a, b sources.Oversize) int { return cmp.Compare(a.Size, b.Size) })
}
