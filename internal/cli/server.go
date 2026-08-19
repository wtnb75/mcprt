package cli

import "github.com/spf13/cobra"

func newServerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "run the mcprt gateway server",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
}
