package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/QuesmaOrg/quesma-shipper/app"
)

func statusCmd(b app.Build) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Is it on, what is collected, what is waiting to be sent",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return showStatus(cmd, b, asJSON) },
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print JSON")
	_ = cmd.Flags().MarkHidden("json")
	return cmd
}

func showStatus(cmd *cobra.Command, b app.Build, asJSON bool) error {
	out := cmd.OutOrStdout()
	st, err := app.CurrentStatus(b)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	p := paletteFor(out)
	now := time.Now()

	if !st.LoggedIn {
		banner(out, p, p.yellow, "not logged in", "your admin has the token")
		fmt.Fprintf(out, "\n  %s <token>  %s\n", styled(p.cyan, app.Name+" login", p.reset), styled(p.dim, "to join your organisation", p.reset))
		return errSilent{code: 3}
	}

	printHeader(out, p, st, now)
	fmt.Fprintln(out)

	if len(st.Agents) == 0 {
		fmt.Fprintln(out, "  nothing found to collect")
	} else {
		rows := make([][]string, len(st.Agents))
		styles := make([]string, len(st.Agents))
		for i, a := range st.Agents {
			rows[i] = []string{"  " + a.Name, app.CountNoun(a.Files, "file"), bytesCell(a.PendingBytes)}
		}
		fmt.Fprint(out, table([]string{"", "tracked", "to send"}, []bool{false, true, true}, 0, rows, styles, p))
	}
	for _, dir := range st.Off {
		fmt.Fprintf(out, "  %s\n", styled(p.dim, "not tracked  "+dir, p.reset))
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s %s\n", styled(p.dim, "More:", p.reset), styled(p.cyan, app.Name+" --help", p.reset))
	return nil
}

func printHeader(out io.Writer, p palette, st app.Status, now time.Time) {
	state, colour, why := "on", p.green, "last sent "+app.Ago(deref(st.LastSent), now)
	switch {
	case st.Paused:
		state, colour, why = "paused", p.yellow, "until "+app.FormatUntil(deref(st.PausedUntil), now)
	case st.Organization == "":
		state, colour, why = "local", p.yellow, "nothing is sent"
	case !st.ServiceOK:
		state, colour, why = "off", p.red, "service "+st.Service
	}
	banner(out, p, colour, state, why)
	if st.Organization != "" {
		fmt.Fprintf(out, "Sending to %s at %s\n", styled(p.cyan, st.Organization, p.reset), styled(p.cyan, app.DestinationHosts(st.Destination, st.Endpoint), p.reset))
	}
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
