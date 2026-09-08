package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

func serviceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "service", Short: "Remove or show how to restart the background service", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	uninstall := &cobra.Command{Use: "uninstall", Short: "Unload and delete the service entry", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			kind, err := packaging.UninstallService()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s service removed\n", kind)
			return nil
		}}
	restart := &cobra.Command{Use: "restart", Short: "Print the command that restarts the service", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if c := packaging.RestartCommand(); c != "" {
				fmt.Fprintln(cmd.OutOrStdout(), c)
			}
			return nil
		}}
	cmd.AddCommand(uninstall, restart)
	return cmd
}

func postinstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:  "postinstall",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			warning, err := app.PostInstallPackage()
			printWarning(cmd.ErrOrStderr(), warning)
			return err
		},
	}
}

func reportRemote(w io.Writer, r *app.Runtime) {
	rem := r.Remote()
	if rem.Err != nil && rem.Origin == controlplane.OriginCached {
		fmt.Fprintf(w, "warning: using the cached config: %v\n", rem.Err)
	}
	if rem.Err != nil && rem.Origin == controlplane.OriginNone {
		fmt.Fprintf(w, "warning: no remote config in force: %v\n", rem.Err)
	}
	if rem.Expired {
		fmt.Fprintf(w, "warning: the cached config is past its expiry; collecting under it and stamping config_expired\n")
	}
}

func previewCmd(build app.Build) *cobra.Command {
	var sourceFilter string
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Show what would be sent, without sending or recording anything",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := app.New(build)
			if err != nil {
				return err
			}
			if sourceFilter != "" {
				env.FilterSources(sourceFilter)
			}
			rep, err := env.Flush(cmd.Context(), true)
			if err != nil {
				return err
			}
			printPreview(cmd.OutOrStdout(), rep, env)
			return nil
		},
	}
	cmd.Flags().StringVar(&sourceFilter, "source", "", "one source only")
	return cmd
}

func logCmd() *cobra.Command {
	var lines int
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Recent audit entries: what was decided about each file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, paths, err := app.ResolveEffective()
			if err != nil {
				return err
			}
			log, err := auditlog.Open(paths.StateDir)
			if err != nil {
				return err
			}
			entries, err := auditlog.Tail(log.Path(), lines)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no entries yet")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			defer w.Flush()
			fmt.Fprintf(w, "WHEN\tDECISION\tSOURCE\tIN\tOUT\tDENSITY\tDETAIL\n")
			for _, e := range entries {
				detail := e.Reason
				if detail == "" {
					detail = e.File
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%.4f\t%s\n",
					e.At.Local().Format("15:04:05"), e.Decision, e.SourceID, e.BytesIn, e.BytesOut, e.RedactionDensity, detail)
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "how many entries")
	return cmd
}

func configCmd() *cobra.Command {
	var withProvenance bool
	cmd := &cobra.Command{
		Use:   "config",
		Short: "The effective configuration and where each value came from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			eff, paths, err := app.ResolveEffective()
			if err != nil {
				return err
			}
			printEffective(cmd.OutOrStdout(), eff, paths, withProvenance)
			return nil
		},
	}
	cmd.Flags().BoolVar(&withProvenance, "with-provenance", false, "name the layer that set each value")
	return cmd
}

func printEffective(out io.Writer, eff *config.Effective, paths config.Paths, withProvenance bool) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	defer w.Flush()
	row := func(field, value string) {
		if !withProvenance {
			fmt.Fprintf(w, "%s\t%s\n", field, value)
			return
		}
		origin := "(not set)"
		if o, ok := eff.Provenance[field]; ok {
			if o.Derived {
				origin = "derived"
			} else {
				origin = o.Layer.String()
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", field, value, origin)
	}
	if withProvenance {
		fmt.Fprintf(w, "FIELD\tVALUE\tSET BY\n")
	} else {
		fmt.Fprintf(w, "FIELD\tVALUE\n")
	}
	if path, ok := config.UserConfigFound(paths); ok {
		fmt.Fprintf(w, "(config file)\t%s\t%s\n", path, config.LayerUser)
	} else {
		fmt.Fprintf(w, "(config files)\tnone - compiled defaults and bundled catalog only\n")
	}
	row("config_version", fmt.Sprint(eff.ConfigVersion))
	row("organization", eff.OrganizationID)
	row("mode.schedule", eff.Schedule)
	row("max_files_per_run", fmt.Sprint(eff.MaxFilesPerRun))
	row("state_dir", eff.StateDir)
	row("send.sink", config.SinkAdapter)
	for i, t := range eff.UploadTargets {
		row(fmt.Sprintf("upload_targets[%d]", i), t.Origin+" ("+t.Addressing+t.PathPrefix+")")
	}
	row("scrub.rule_packs", strings.Join(eff.RulePacks, ", "))
	row("encryption.include_install_recipient", fmt.Sprint(eff.IncludeInstallRecipient))
	if len(eff.AdditionalRecipients) > 0 {
		row("encryption.additional_recipients", strings.Join(eff.AdditionalRecipients, ", "))
	}
	row("autoupdate.enabled", fmt.Sprint(eff.AutoupdateEnabled))
	for _, s := range eff.Sources {
		state := "enabled"
		if !s.Enabled {
			state = "disabled"
		}
		row("sources."+s.ID+".enabled", state)
		if s.Root != "" {
			row("sources."+s.ID+".root", s.Root)
		} else {
			reason := s.RootUnresolvedReason
			if reason == "" {
				reason = "no candidate root resolved"
			}
			row("sources."+s.ID+".root", "(unresolved: "+reason+")")
		}
		row("sources."+s.ID+".include", strings.Join(s.Include, ", "))
		row("sources."+s.ID+".artifact_class", s.ArtifactClass)
		row("sources."+s.ID+".spec_fingerprint", s.SpecFingerprint[:16]+"...")
	}
	fmt.Fprintf(w, "\t\t\n")
	fmt.Fprintf(w, "(deny patterns)\t%d compiled + served additions, applied post-expansion and post-symlink\n", len(eff.Deny.Patterns()))
}

func stateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "state", Short: "Inspect and repair the local record of what was sent", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	var apply bool
	reset := &cobra.Command{Use: "reset", Short: "Forget everything so the next run re-sends every file", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, paths, err := app.ResolveEffective()
			if err != nil {
				return err
			}
			unit, err := identity.Load(paths.StateDir)
			if err != nil {
				return err
			}
			removed, err := engine.Reset(paths.StateDir, unit.InstallID.String(), !apply)
			if err != nil {
				return err
			}
			reportStateChange(cmd.OutOrStdout(), removed, 0, apply, "forgotten")
			return nil
		}}
	prune := &cobra.Command{Use: "prune", Short: "Forget files that no longer exist", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, paths, err := app.ResolveEffective()
			if err != nil {
				return err
			}
			unit, err := identity.Load(paths.StateDir)
			if err != nil {
				return err
			}
			removed, kept, err := engine.Prune(paths.StateDir, unit.InstallID.String(), !apply)
			if err != nil {
				return err
			}
			reportStateChange(cmd.OutOrStdout(), removed, kept, apply, "pruned")
			return nil
		}}
	for _, c := range []*cobra.Command{reset, prune} {
		c.Flags().BoolVar(&apply, "apply", false, "write the change instead of reporting it")
	}
	cmd.AddCommand(reset, prune)
	return cmd
}

func reportStateChange(w io.Writer, removed, kept int, apply bool, verb string) {
	switch {
	case removed == 0:
		fmt.Fprintf(w, "nothing to do (%d entries)\n", kept)
	case apply:
		fmt.Fprintf(w, "%d entries %s, %d remain\n", removed, verb, kept)
	default:
		fmt.Fprintf(w, "%d entries would be %s; re-run with --apply\n", removed, verb)
	}
}

func localDevCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "local-dev",
		Short: "Set up without a control plane: preview works, nothing can be sent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, unit, err := app.LocalDev()
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "identity %s\nstate    %s\nconfig   %s\n", unit.InstallID, paths.StateDir, paths.User)
			return nil
		},
	}
}
