package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/QuesmaOrg/quesma-shipper/app"
)

func pauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause [15m | 1h | 6h | 12h | 24h | tomorrow]",
		Short: "Stop collecting for a while",
		Long: "Stop collecting for 15 min, 1 h, 6 h, 12 h, 24 h or until tomorrow 9:00.\n" +
			"`pause` alone shows the choices, `pause 1h` or `pause tomorrow` picks one.",
		RunE: func(cmd *cobra.Command, args []string) error {
			now := time.Now()
			var until time.Time
			if len(args) == 0 {
				i, err := choose(cmd, labels(app.PauseChoices))
				if errors.Is(err, errCancelled) {
					fmt.Fprintln(cmd.OutOrStdout(), "Not paused.")
					return nil
				}
				if err != nil {
					return usage(errors.New(app.PauseUsage))
				}
				until = app.PauseChoices[i].Until(now)
			} else {
				var err error
				if until, err = app.ParsePauseUntil(args, now); err != nil {
					return usage(err)
				}
			}
			warning, err := app.Pause(until)
			if err != nil {
				return err
			}
			printWarning(cmd.ErrOrStderr(), warning)
			p := paletteFor(cmd.OutOrStdout())
			banner(cmd.OutOrStdout(), p, p.yellow, "paused", "until "+app.FormatUntil(until, now))
			return nil
		},
	}
}

func resumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Start collecting again",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			was, warning, err := app.Resume()
			if err != nil {
				return err
			}
			printWarning(cmd.ErrOrStderr(), warning)
			p := paletteFor(cmd.OutOrStdout())
			why := ""
			if !was {
				why = "was not paused"
			}
			banner(cmd.OutOrStdout(), p, p.green, "on", why)
			return nil
		},
	}
}

func labels(choices []app.PauseChoice) []string {
	out := make([]string, len(choices))
	for i, c := range choices {
		out[i] = c.Label
	}
	return out
}

func usage(err error) error { return usageError{err} }

type usageError struct{ error }
