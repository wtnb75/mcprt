package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/wtnb75/mcprt/internal/config"
)

// defaultInitPath is the output path `mcprt init` writes to when the caller
// doesn't name one.
const defaultInitPath = "config.yaml"

// exampleConfig is marshaled to produce the template `mcprt init` writes.
// Building it from config.Config (rather than a static embedded file) means
// the generated template automatically reflects the config struct's current
// fields.
func exampleConfig() *config.Config {
	return &config.Config{
		Listen: config.ListenConfig{
			Stdio: true,
			HTTP:  "127.0.0.1:8080",
		},
		Backends: []config.BackendConfig{
			{
				Name:      "example-stdio",
				Transport: "stdio",
				Command:   []string{"mcp-server-example"},
			},
			{
				Name:      "example-http",
				Transport: "http",
				URL:       "http://127.0.0.1:8000/mcp",
			},
		},
		Overrides: map[string]string{
			"example_tool": "example-stdio",
		},
	}
}

func newInitCmd() *cobra.Command {
	var force bool

	// SilenceUsage/SilenceErrors: don't dump flag usage on a runtime error
	// (output file already exists, write failure, etc.), and let
	// cli.Execute's caller (main.go) be the one place that prints the
	// error.
	cmd := &cobra.Command{
		Use:           "init [path]",
		Short:         "write a starter gateway config file",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := defaultInitPath
			if len(args) == 1 {
				path = args[0]
			}
			return runInit(cmd, path, force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite the output path if it already exists")
	return cmd
}

func runInit(cmd *cobra.Command, path string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking %s: %w", path, err)
		}
	}

	data, err := yaml.Marshal(exampleConfig())
	if err != nil {
		return fmt.Errorf("rendering config template: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	_, err = fmt.Fprintf(cmd.OutOrStdout(), "wrote config template: %s\n", path)
	return err
}
