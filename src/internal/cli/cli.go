package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/QuesmaOrg/quesma-shipper/app"
)

const (
	groupUser  = "user"
	groupSetup = "setup"
)

func Root(b app.Build, out, errOut io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   app.Name,
		Short: "Collects your AI-agent sessions, scrubs secrets, encrypts them and sends them to your organisation",
		Long: app.Name + " collects the sessions your coding agents (Claude Code, Codex, Cursor)\n" +
			"leave on this machine, scrubs secrets, encrypts every file and sends it to your\n" +
			"organisation. It runs in the background.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if v, _ := cmd.Flags().GetBool("version"); v {
				fmt.Fprintln(cmd.OutOrStdout(), paletteFor(out).paint(app.VersionLine(b), ""))
				return nil
			}
			return showStatus(cmd, b, false)
		},
	}
	root.SetOut(out)
	root.SetErr(errOut)
	pal := paletteFor(out)
	root.AddGroup(&cobra.Group{ID: groupUser, Title: "Commands"}, &cobra.Group{ID: groupSetup, Title: "Setup"})
	root.Flags().BoolP("help", "h", false, "Show this help")
	root.Flags().BoolP("version", "v", false, "Show the version")
	root.SetHelpCommand(&cobra.Command{Hidden: true})
	root.CompletionOptions.HiddenDefaultCmd = true
	root.SetUsageFunc(func(c *cobra.Command) error { return printUsage(c, pal) })
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		for _, prefix := range []string{"unknown flag: ", "unknown shorthand flag: "} {
			if flag, ok := strings.CutPrefix(err.Error(), prefix); ok {
				return usage(fmt.Errorf("unknown flag `%s`", flag))
			}
		}
		return err
	})
	root.SetHelpFunc(func(c *cobra.Command, _ []string) {
		if intro := cmp.Or(c.Long, c.Short); intro != "" {
			fmt.Fprint(c.OutOrStdout(), strings.TrimRightFunc(pal.names(intro, ""), unicode.IsSpace)+"\n\n")
		}
		fmt.Fprint(c.OutOrStdout(), c.UsageString())
	})

	cobra.EnableCommandSorting = false
	for _, c := range []*cobra.Command{
		trackingCmd(), pauseCmd(), resumeCmd(), statusCmd(b), doctorCmd(b), updateCmd(b), uninstallCmd(b), licensesCmd(),
	} {
		c.GroupID = groupUser
		root.AddCommand(c)
	}
	login := loginCmd()
	login.GroupID = groupSetup
	root.AddCommand(login)

	for _, c := range []*cobra.Command{
		preuninstallSystemCmd(), postinstallCmd(), serviceCmd(), runCmd(b), previewCmd(b), logCmd(), configCmd(), stateCmd(), localDevCmd(),
	} {
		c.Hidden = true
		root.AddCommand(c)
	}
	return root
}

func ErrorLine(err error, w io.Writer) string {
	p := paletteFor(w)
	return app.Name + ": " + p.names(err.Error(), "")
}

func HelpPointer(w io.Writer) string {
	p := paletteFor(w)
	return styled(p.dim, "More:", p.reset) + " " + styled(p.cyan, app.Name+" --help", p.reset)
}

type errSilent struct{ code int }

func (e errSilent) Error() string { return fmt.Sprintf("exit %d", e.code) }

func ExitCode(err error) (code int, show bool) {
	if err == nil {
		return 0, false
	}
	var silent errSilent
	if errors.As(err, &silent) {
		return silent.code, false
	}
	var usage usageError
	if errors.As(err, &usage) || strings.HasPrefix(err.Error(), "unknown command") {
		return 2, true
	}
	return 1, true
}

// Renders in plain Go rather than a cobra template: a custom template makes
// text/template's reflect.Value.MethodByName reachable, which turns off the
// linker's dead-code elimination for the whole binary (cmd/quesma-shipper/deps_test.go).
func printUsage(c *cobra.Command, pal palette) error {
	name := func(s string) string { return styled(pal.cyan, s, pal.reset) }
	dim := func(s string) string { return styled(pal.dim, s, pal.reset) }
	var b strings.Builder
	b.WriteString(dim("Usage:") + " " + name(c.CommandPath()))
	if _, args, ok := strings.Cut(c.Use, " "); ok && args != "" {
		b.WriteString(" " + args)
	}
	if c.HasAvailableSubCommands() {
		b.WriteString(" [command]")
	}
	if c.HasAvailableLocalFlags() {
		b.WriteString("\n" + flagTable(c.LocalFlags(), pal))
	}
	if c.HasExample() {
		b.WriteString("\n\n" + dim("Examples:") + "\n" + c.Example)
	}
	if c.HasAvailableSubCommands() {
		for _, g := range c.Groups() {
			b.WriteString("\n\n" + dim(g.Title))
			for _, sub := range c.Commands() {
				if sub.GroupID == g.ID && sub.IsAvailableCommand() {
					b.WriteString("\n  " + name(fmt.Sprintf("%-*s", sub.NamePadding(), sub.Name())) + " " + sub.Short)
				}
			}
		}
		b.WriteString("\n\n" + dim("More:") + " " + name(c.CommandPath()) + " <command> " + name("--help"))
	}
	b.WriteString("\n")
	_, err := fmt.Fprint(c.OutOrStderr(), b.String())
	return err
}

func flagTable(fs *pflag.FlagSet, pal palette) string {
	type row struct{ lead, name, usage string }
	var rows []row
	width := 0
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		name, lead := "--"+f.Name, "    "
		if f.Shorthand != "" {
			name, lead = "-"+f.Shorthand+", --"+f.Name, ""
		}
		if f.Value.Type() != "bool" {
			name += " <" + f.Name + ">"
		}
		width = max(width, len(lead+name))
		rows = append(rows, row{lead, name, f.Usage})
	})
	var b strings.Builder
	for _, r := range rows {
		pad := strings.Repeat(" ", width-len(r.lead+r.name))
		b.WriteString("  " + r.lead + styled(pal.cyan, r.name, pal.reset) + pad + "   " + r.usage + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
