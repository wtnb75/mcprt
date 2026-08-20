package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wtnb75/mcprt/internal/config"
)

func newValidateCmd() *cobra.Command {
	var configPath string

	// SilenceUsage/SilenceErrors: don't dump flag usage on a runtime error
	// (bad config), and let cli.Execute's caller (main.go) be the one place
	// that prints the error.
	cmd := &cobra.Command{
		Use:           "validate",
		Short:         "validate a gateway config file without connecting to any backend",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := config.Load(configPath); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "config is valid: %s\n", configPath)
			return err
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to the gateway config file (required)")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err) // programmer error: "config" flag name must match Flags().StringVar above
	}

	return cmd
}
