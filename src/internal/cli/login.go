package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/QuesmaOrg/quesma-shipper/app"
)

func loginCmd() *cobra.Command {
	var server string
	cmd := &cobra.Command{
		Use:                "login <token>",
		Short:              "Join your organisation with the token from your admin",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			args, err := parseFlagsKeepingTokens(cmd, args)
			if err != nil {
				return usage(err)
			}
			if args == nil {
				return cmd.Help()
			}
			if len(args) > 1 {
				return usage(fmt.Errorf("login takes one token, got %d arguments", len(args)))
			}
			token := os.Getenv(app.AuthEnv)
			if len(args) == 1 {
				token = args[0]
			}
			p := paletteFor(w)
			if token == "" {
				if org, ok := app.LoggedIn(); ok {
					fmt.Fprintf(w, "Already logged in to %s\n", styled(p.cyan, org, p.reset))
					return nil
				}
				token = askToken(cmd)
			}
			if token == "" {
				return usage(fmt.Errorf("login needs the token from your admin, for example `%s login <token>`", app.Name))
			}
			if server == "" {
				return usage(errors.New("no server for this build, pass --server"))
			}
			res, err := app.Login(cmd.Context(), server, token)
			if errors.Is(err, app.ErrAlreadyLoggedIn) {
				fmt.Fprintf(w, "Already logged in to %s\n", styled(p.cyan, res.Organization, p.reset))
			} else if err != nil {
				return err
			} else {
				fmt.Fprintf(w, "Logged in to %s as %s\n", styled(p.cyan, res.Organization, p.reset), styled(p.cyan, res.Machine, p.reset))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "Server URL")
	return cmd
}

func askToken(cmd *cobra.Command) string {
	stdin, ok := cmd.InOrStdin().(*os.File)
	out := cmd.OutOrStdout()
	if !ok || !isTerminal(stdin) || !isTerminal(out) {
		return ""
	}
	fmt.Fprint(out, "Paste the token from your admin: ")
	raw, err := term.ReadPassword(int(stdin.Fd()))
	fmt.Fprintln(out)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func parseFlagsKeepingTokens(cmd *cobra.Command, args []string) (tokens []string, err error) {
	tokens = []string{}
	var flags []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" {
			return nil, nil
		}
		if !strings.HasPrefix(a, "--") {
			tokens = append(tokens, a)
			continue
		}
		flags = append(flags, a)
		name, _, hasValue := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if f := cmd.Flags().Lookup(name); f != nil && f.NoOptDefVal == "" && !hasValue && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return tokens, cmd.Flags().Parse(flags)
}
