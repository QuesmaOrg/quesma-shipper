package cli

import (
	"github.com/spf13/cobra"

	"github.com/QuesmaOrg/quesma-shipper/internal/legal"
)

func licensesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "licenses",
		Short: "Print the license and the third-party notices",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return legal.Write(cmd.OutOrStdout()) },
	}
}
