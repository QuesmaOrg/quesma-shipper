package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

func clearSelfUpdateHop() {
	os.Unsetenv(app.ReexecGuardEnv) // clean up guards inherited from pre-file releases
	if stateDir, err := app.StateDirWithoutConfig(); err == nil {
		_ = packaging.ClearSelfUpdateHop(stateDir)
	}
}

func updateCmd(build app.Build) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Install the newest version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			p := paletteFor(w)
			opts := packaging.UpdateOptions{Current: build.Version, Out: cmd.ErrOrStderr()}
			if !build.Release {
				return fmt.Errorf("this is a dev build, it does not update itself, `make build` replaces it")
			}
			res, err := packaging.Update(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if !res.Updated {
				fmt.Fprintf(w, "Up to date (%s)\n", styled(p.cyan, res.From, p.reset))
				return nil
			}
			fmt.Fprintf(w, "Updated %s → %s\n", styled(p.cyan, res.From, p.reset), styled(p.cyan, res.To, p.reset))
			return restartService(cmd.Context(), w)
		},
	}
	return cmd
}

func restartService(ctx context.Context, w io.Writer) error {
	eff, paths, err := app.ResolveEffective()
	if err != nil {
		return nil
	}
	// The wait is the agent's own SIGTERM drain, not a stalled supervisor, so it gets the drain
	// window rather than a short guard; the bound only matters when the supervisor itself hangs.
	budget := packaging.RestartBudget(eff.DrainDeadline)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if !packaging.ServiceStateContext(ctx, paths.StateDir).Loaded {
		if ctx.Err() != nil {
			printWarning(w, restartTimeoutWarning(budget))
		}
		return nil
	}
	p := paletteFor(w)
	fmt.Fprintf(w, "Restarting background service; it ships a final slice before exiting, up to %s…\n", budget)
	if err := packaging.RestartService(ctx); err != nil {
		if ctx.Err() != nil {
			printWarning(w, restartTimeoutWarning(budget))
			return nil
		}
		return fmt.Errorf("the background service did not restart onto the new version: %w; it keeps the previous version until its next restart", err)
	}
	banner(w, p, p.green, "on", "background service restarted")
	return nil
}

// restartTimeoutWarning is not an error: the binary is swapped and the supervisor restarts the
// agent onto it as soon as the drain ends. Only the wait for confirmation gave up.
func restartTimeoutWarning(budget time.Duration) string {
	msg := fmt.Sprintf("the background service was still shutting down after %s; the update is installed and the service starts on the new version when its final slice is shipped", budget)
	if cmd := packaging.RestartCommand(); cmd != "" {
		msg += "; to force it now: " + cmd
	}
	return msg
}

// selfUpdateGate blocks the hop that did not land: we updated to persistedHop, restarted, and are
// still not running it. A hop that matches this build has done its job and the caller clears it.
func selfUpdateGate(build app.Build, getenv func(string) string, persistedHop string) (run bool, why string) {
	if !build.Release {
		return false, ""
	}
	if getenv(app.NoSelfUpdateEnv) != "" {
		return false, "disabled by " + app.NoSelfUpdateEnv
	}
	if to := getenv(app.ReexecGuardEnv); to != "" {
		return false, "already updated to " + to + " this boot"
	}
	if persistedHop != "" && persistedHop != build.Version {
		return false, "already updated to " + persistedHop + " but still running " + build.Version
	}
	return true, ""
}

func maybeSelfUpdate(ctx context.Context, build app.Build, errOut io.Writer) {
	stateDir, stateErr := app.StateDirWithoutConfig()
	persistedHop := ""
	if stateErr == nil {
		persistedHop = packaging.ReadSelfUpdateHop(stateDir)
		if persistedHop == build.Version {
			_ = packaging.ClearSelfUpdateHop(stateDir)
		}
	}
	run, why := selfUpdateGate(build, os.Getenv, persistedHop)
	if !run {
		if why != "" {
			fmt.Fprintf(errOut, "self-update: %s\n", why)
		}
		return
	}
	if eff, _, err := app.ResolveEffective(); err == nil && !eff.AutoupdateEnabled {
		fmt.Fprintf(errOut, "self-update: disabled by autoupdate.enabled\n")
		return
	}
	res, err := packaging.Update(ctx, packaging.UpdateOptions{Current: build.Version, Out: errOut})
	if err != nil {
		fmt.Fprintf(errOut, "self-update: skipped: %v\n", err)
		app.RecordUpdateFailure(fmt.Sprintf("self-update from %s did not happen: %v", build.Version, err))
		return
	}
	if !res.Updated {
		return
	}
	fmt.Fprintf(errOut, "self-update: %s -> %s, restarting\n", res.From, res.To)
	if stateErr != nil {
		fmt.Fprintf(errOut, "self-update: cannot persist the restart guard (%v); the new version runs from the next supervised restart\n", stateErr)
		app.RecordUpdateFailure(fmt.Sprintf("updated to %s but could not persist the restart guard: %v", res.To, stateErr))
		return
	}
	if err := packaging.WriteSelfUpdateHop(stateDir, res.To); err != nil {
		fmt.Fprintf(errOut, "self-update: cannot persist the restart guard (%v); the new version runs from the next supervised restart\n", err)
		app.RecordUpdateFailure(fmt.Sprintf("updated to %s but could not persist the restart guard: %v", res.To, err))
		return
	}
	os.Setenv(app.ReexecGuardEnv, res.To) // keeps compatibility with an older Unix binary on the hop
	if err := packaging.ReExec(); err != nil {
		fmt.Fprintf(errOut, "self-update: restart failed (%v); the new version runs from the next restart\n", err)
		// The binary IS updated; only the restart failed. Recorded because a supervisor that never
		// restarts leaves the install running the old code with nothing saying so.
		app.RecordUpdateFailure(fmt.Sprintf("updated to %s but the restart failed: %v", res.To, err))
	}
}
