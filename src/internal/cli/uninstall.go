package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/QuesmaOrg/quesma-shipper/app"
)

func uninstallCmd(b app.Build) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the service and program, keeping local state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			p := paletteFor(w)
			st, err := app.CurrentStatus(b)
			if err != nil {
				return err
			}
			printHeader(w, p, st, time.Now())
			to := "your organisation"
			if st.Organization != "" {
				to = st.Organization
			}
			effect := "deletes the program. Local state is kept\nso a later reinstall can resume this machine."
			question := "Remove " + app.Title + "?"
			if purge {
				effect = "deletes the program and its local state.\nWhat was already sent stays with your organisation."
				question = "Remove " + app.Title + " and purge its local state?"
			}
			fmt.Fprintf(w, "\n%s collects your coding-agent sessions on this machine and sends them\n"+
				"to %s. Removing it stops that and %s\n\n", app.Title, to, effect)
			agreed, err := confirm(cmd, question)
			if err != nil {
				return usage(fmt.Errorf("uninstall asks first, run it in a terminal"))
			}
			if !agreed {
				fmt.Fprintln(w, "Not removed.")
				return nil
			}
			fmt.Fprintln(w)
			deferred, err := app.Uninstall(purge, func(s app.UninstallStep) {
				switch {
				case s.Err != nil:
					fmt.Fprintf(w, "  %s  %s  %s: %v\n", p.glyph(app.SevFail), s.Done, app.HomeTilde(s.Detail), s.Err)
				case s.Skip != "":
					detail := ""
					if s.Detail != "" {
						detail = "  " + app.HomeTilde(s.Detail)
					}
					fmt.Fprintf(w, "  %s\n", styled(p.dim, "-  "+s.Skip+detail, p.reset))
				default:
					fmt.Fprintf(w, "  %s  %s  %s\n", p.glyph(app.SevOK), s.Done, styled(p.dim, app.HomeTilde(s.Detail), p.reset))
				}
			})
			fmt.Fprintln(w)
			if err != nil {
				banner(w, p, p.red, "not fully removed", "")
				return errSilent{code: 1}
			}
			if deferred {
				banner(w, p, p.red, "removal finishing", "the Windows uninstaller is running in the background")
			} else {
				banner(w, p, p.red, "removed", "")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "Also delete enrollment, pending data, logs and other local state")
	return cmd
}
