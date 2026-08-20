package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// NewRootCmd builds the mcprt root command. Subcommands are attached to it
// (mcprt is subcommand-based from v1, even though "server" is currently the
// only one).
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "mcprt",
		Short: "mcprt aggregates multiple MCP servers behind a single gateway",
	}
	root.AddCommand(newServerCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newListCmd())
	root.AddCommand(newPingCmd())
	root.AddCommand(newCallCmd())
	return root
}

// Execute runs the mcprt CLI with the given arguments (typically
// os.Args[1:]) and returns its error, if any.
func Execute(ctx context.Context, args []string) error {
	root := NewRootCmd()
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}
