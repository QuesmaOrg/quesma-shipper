package cli

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

func printPreview(out io.Writer, rep formats.Report, env *app.Runtime) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	defer w.Flush()

	fmt.Fprintf(w, "PREVIEW - nothing was uploaded and no state was written.\n\n")
	fmt.Fprintf(w, "destination\t%s\n", env.Destination())
	fmt.Fprintf(w, "recipients\t%s\n\n", strings.Join(env.Recipients(), ", "))

	for _, s := range rep.Sources {
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.SourceID, s.Health, s.Reason)
		if s.Root != "" {
			fmt.Fprintf(w, "  root\t%s\tagent %s\n", s.Root, orDash(s.AgentVersion))
		}
		for _, f := range s.Files {
			fmt.Fprintf(w, "  %s\t%d B in\t%d B sealed\tdensity %.4f\t%s\n",
				f.RelPath, f.BytesIn, f.BytesOut, f.Density, ruleSummary(f.RuleHits))
		}
	}
	fmt.Fprintf(w, "\nwould ship\t%d files\n", rep.Shipped)
	if rep.Truncated {
		fmt.Fprintf(w, "note\tmax_files_per_run reached; the rest would follow next tick\n")
	}
}
func printRunSummary(out io.Writer, rep formats.Report, adviseDrain bool) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	defer w.Flush()

	if rep.Paused {
		fmt.Fprintf(w, "%s\tpaused - nothing collected\t%s\n",
			rep.StartedAt.Local().Format("15:04:05"), rep.PauseReason)
		fmt.Fprintf(w, "  note\tclear it with `quesma-shipper resume`\n")
		return
	}

	fmt.Fprintf(w, "%s\tshipped %d\tunchanged %d\tskipped %d\tparked %d\tfailed %d\n",
		rep.StartedAt.Local().Format("15:04:05"), rep.Shipped, rep.Unchanged, rep.Skipped, rep.Parked, rep.Failed)
	w.Flush()
	if line := statsLine(rep); line != "" {
		fmt.Fprintln(out, line)
	}
	for _, s := range rep.Sources {
		if s.Health != sources.Collected {
			fmt.Fprintf(w, "  %s\t%s\t%s\n", s.SourceID, s.Health, s.Reason)
		}
	}
	if rep.Truncated {
		fmt.Fprintf(w, "  note\tmax_files_per_run reached; %d left, %.0f%% of this tick's work shipped\n",
			rep.Remaining, completeness(rep.Shipped, rep.Remaining))
		if adviseDrain && rep.Shipped > 0 {
			fmt.Fprintf(w, "  \tflush the rest now with `quesma-shipper run --once --drain`\n")
		}
	}
	for _, s := range rep.Sources {
		if s.Unreadable > 0 && s.Health == sources.Collected {
			fmt.Fprintf(w, "  %s\t%d %s not readable during discovery, skipped\n",
				s.SourceID, s.Unreadable, app.Plural(s.Unreadable, "path"))
			fmt.Fprintf(w, "  \t%s\n", s.UnreadableReason)
			fmt.Fprintf(w, "  \tthe rest of this source collected; those files never will\n")
		}
		if s.Oversize == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s\t%d %s over the %s cap, not collected\n",
			s.SourceID, s.Oversize, app.Plural(s.Oversize, "file"), app.HumanBytes(s.OversizeLimit))
		fmt.Fprintf(w, "  \tlargest %s  %s\n",
			app.HumanBytes(s.OversizeLargest), s.OversizeExample)
		fmt.Fprintf(w, "  \tnever collected; raise sources[%s].max_file_bytes to change that\n",
			s.SourceID)
	}
	if rep.Parked > 0 {
		fmt.Fprintf(w, "  note\tparked entries need attention: see `quesma-shipper doctor`\n")
	}
	for _, s := range rep.Sources {
		if s.Enriched > 0 {
			fmt.Fprintf(w, "  %s\tenriched %d via %s@%d\n", s.SourceID, s.Enriched,
				s.EnricherID, s.EnricherVersion)
		}
		if s.EnrichMismatch > 0 {
			fmt.Fprintf(w, "  %s\t%d files sent without their database details\n",
				s.SourceID, s.EnrichMismatch)
		}
		if s.EnrichErrors > 0 {
			fmt.Fprintf(w, "  %s\tenrich errors ×%d\n", s.SourceID, s.EnrichErrors)
		}
		for _, n := range s.EnrichNotes {
			fmt.Fprintf(w, "  \t%s\n", n)
		}
		for _, n := range s.EnrichInfos {
			fmt.Fprintf(w, "  %s\tenrich note (shipped)\t%s\n", s.SourceID, n)
		}
	}
}
func statsLine(rep formats.Report) string {
	if rep.FinishedAt.IsZero() || rep.BytesRead == 0 {
		return ""
	}
	elapsed := rep.FinishedAt.Sub(rep.StartedAt)
	line := fmt.Sprintf("  sent  %s sealed of %s read in %s",
		app.HumanBytes(rep.BytesSealed), app.HumanBytes(rep.BytesRead), app.HumanDuration(elapsed))

	var notes []string
	if elapsed > 0 {
		notes = append(notes, app.HumanBytes(int64(float64(rep.BytesRead)/elapsed.Seconds()))+"/s")
	}
	if rep.MedianFileBytes > 0 {
		notes = append(notes, "median file "+app.HumanBytes(rep.MedianFileBytes))
	}
	if len(notes) > 0 {
		line += "  (" + strings.Join(notes, ", ") + ")"
	}
	return line
}
func progressLine(sourceID string, done, total int, f formats.FileOutcome) string {
	switch f.Decision {
	case formats.DecisionUnchanged:
		return ""
	case formats.DecisionShipped:
		return fmt.Sprintf("[%d/%d] %s  %s  shipped (%s in, %s sealed)",
			done, total, sourceID, f.RelPath, app.HumanBytes(f.BytesIn), app.HumanBytes(f.BytesOut))
	default:
		line := fmt.Sprintf("[%d/%d] %s  %s  %s", done, total, sourceID, f.RelPath, f.Decision)
		if f.Reason != "" {
			line += ": " + f.Reason
		}
		return line
	}
}

func ruleSummary(hits map[string]int) string {
	if len(hits) == 0 {
		return "no redactions"
	}
	out := ""
	for _, id := range slices.Sorted(maps.Keys(hits)) {
		if out != "" {
			out += ", "
		}
		out += fmt.Sprintf("%s×%d", id, hits[id])
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
func completeness(shipped, remaining int) float64 {
	wanted := shipped + remaining
	if wanted <= 0 {
		return 100
	}
	return 100 * float64(shipped) / float64(wanted)
}
