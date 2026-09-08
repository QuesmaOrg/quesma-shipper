package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/QuesmaOrg/quesma-shipper/app"
)

func doctorCmd(build app.Build) *cobra.Command {
	var asJSON, verbose bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check collecting and sending, explain anything wrong",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			rep := app.Diagnose(cmd.Context(), build, verbose || asJSON)
			issues, fails := rep.Issues()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(toDoctorJSON(build, rep, fails, issues)); err != nil {
					return err
				}
			} else {
				p := paletteFor(out)
				if st, err := app.CurrentStatus(build); err == nil {
					printHeader(out, p, st, time.Now())
				}
				for _, line := range updateLines(build, rep.Update, p, verbose) {
					fmt.Fprintln(out, line)
				}
				fmt.Fprintln(out)
				renderSections(out, p, rep.Sections)
				renderVerdict(out, p, fails, rep.AgentsCollecting, issues)
				if !verbose {
					fmt.Fprintf(out, "\n%s %s\n", styled(p.dim, "More:", p.reset), styled(p.cyan, app.Name+" doctor --all", p.reset))
				}
			}
			if fails > 0 {
				return errSilent{code: 1}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print JSON, always with the technical sections")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().BoolVar(&verbose, "all", false, "Add the technical sections")
	return cmd
}

func updateLines(build app.Build, upd app.UpdateStatus, p palette, verbose bool) []string {
	var lines []string
	if verbose {
		lines = append(lines, styled(p.dim, "Version "+build.Version, p.reset))
	}
	if upd.State == "available" {
		lines = append(lines, styled(p.yellow+p.bold, "Update", p.reset)+" "+upd.Detail)
		if upd.Fix != "" {
			lines = append(lines, "  → "+p.names("`"+upd.Fix+"`", ""))
		}
	} else if verbose {
		lines = append(lines, styled(p.dim, "Update "+upd.Detail, p.reset))
	}
	return lines
}

func renderVerdict(w io.Writer, p palette, fails, agents int, issues []string) {
	fmt.Fprintln(w)
	if agents == 0 {
		fmt.Fprintf(w, "%s\n", styled(p.red+p.bold, "Nothing is being collected", p.reset))
	}
	if len(issues) == 0 {
		fmt.Fprintf(w, "%s\n", styled(p.green+p.bold, "Everything is being collected", p.reset))
		return
	}
	color := p.yellow
	if fails > 0 {
		color = p.red
	}
	fmt.Fprintf(w, "%s\n", styled(color+p.bold, strings.TrimSuffix(verdict(fails, len(issues)-fails), "."), p.reset))
	for _, b := range issues {
		b = p.paint(b, "")
		if who, rest, ok := strings.Cut(b, ": "); ok {
			b = styled(p.bold, who, p.reset) + ": " + rest
		}
		fmt.Fprintf(w, "  %s\n", p.names(b, ""))
	}
}

type doctorJSON struct {
	SchemaVersion    int             `json:"doctor_schema_version"`
	ClientVersion    string          `json:"client_version"`
	Enrollment       *enrollmentJSON `json:"enrollment,omitempty"`
	Update           updateJSON      `json:"update"`
	Sections         []sectionJSON   `json:"sections"`
	AgentsCollecting int             `json:"agents_collecting"`
	FilesFound       int             `json:"files_found"`
	Problems         int             `json:"problems"`
	Attention        int             `json:"attention"`
	Issues           []string        `json:"issues,omitempty"`
	Verdict          string          `json:"verdict"`
}

type enrollmentJSON struct {
	Organization string `json:"organization"`
	Endpoint     string `json:"endpoint"`
}

type updateJSON struct {
	Status    string `json:"status"`
	Latest    string `json:"latest,omitempty"`
	Published string `json:"published,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type sectionJSON struct {
	Title string    `json:"title"`
	Rows  []rowJSON `json:"rows"`
}

type rowJSON struct {
	Severity string `json:"severity"`
	Label    string `json:"label"`
	Detail   string `json:"detail,omitempty"`
	Fix      string `json:"fix,omitempty"`
}

func toDoctorJSON(build app.Build, rep *app.Report, fails int, issues []string) doctorJSON {
	out := doctorJSON{
		SchemaVersion: 1, ClientVersion: build.Version,
		Update:           updateJSON{Status: rep.Update.State, Latest: rep.Update.Latest, Detail: rep.Update.Detail},
		AgentsCollecting: rep.AgentsCollecting, FilesFound: rep.FilesFound,
		Problems: fails, Attention: len(issues) - fails, Issues: plainAll(issues),
		Verdict: verdict(fails, len(issues)-fails),
	}
	if !rep.Update.Published.IsZero() {
		out.Update.Published = rep.Update.Published.Format(time.RFC3339)
	}
	if rep.Organization != "" {
		out.Enrollment = &enrollmentJSON{Organization: rep.Organization, Endpoint: rep.Endpoint}
	}
	for _, sec := range rep.Sections {
		s := sectionJSON{Title: sec.Title}
		for _, r := range sec.Rows {
			s.Rows = append(s.Rows, rowJSON{Severity: severityName(r.Sev), Label: strings.TrimSpace(r.Head()), Detail: r.Detail, Fix: app.Plain(r.Fix)})
		}
		out.Sections = append(out.Sections, s)
	}
	return out
}

func plainAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = app.Plain(s)
	}
	return out
}

func severityName(s app.Severity) string {
	switch s {
	case app.SevOK:
		return "ok"
	case app.SevWarn:
		return "warn"
	case app.SevFail:
		return "fail"
	}
	return "info"
}
