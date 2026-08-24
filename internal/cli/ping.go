package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
)

func newPingCmd() *cobra.Command {
	var configPath string
	var jsonOutput bool

	// SilenceUsage/SilenceErrors: don't dump flag usage on a runtime error
	// (bad config, backend failure, etc.), and let cli.Execute's caller
	// (main.go) be the one place that prints the error.
	cmd := &cobra.Command{
		Use:           "ping <backend>",
		Short:         "connect to a single configured backend and print the tools it reports",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPing(cmd.Context(), cmd, configPath, args[0], jsonOutput)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to the gateway config file (required)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print machine-readable JSON instead of a text table")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err) // programmer error: "config" flag name must match Flags().StringVar above
	}

	return cmd
}

func runPing(ctx context.Context, cmd *cobra.Command, configPath, backendName string, jsonOutput bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	bc, err := findBackendConfig(cfg.Backends, backendName)
	if err != nil {
		return err
	}

	// Unlike list's connectBackends, a single named backend is the whole
	// point of ping, so a connect or list-tools failure is returned as the
	// command's own error instead of being logged and skipped.
	ctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
	defer cancel()

	b, err := backend.Connect(ctx, bc, backend.ChangeCallbacks{})
	if err != nil {
		return fmt.Errorf("connecting to backend %q: %w", backendName, err)
	}
	defer func() { _ = b.Close() }()

	tools, err := b.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("listing tools on backend %q: %w", backendName, err)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	if jsonOutput {
		return printPingJSON(cmd, backendName, tools)
	}
	return printPingText(cmd, backendName, tools)
}

func findBackendConfig(backends []config.BackendConfig, name string) (config.BackendConfig, error) {
	for _, bc := range backends {
		if bc.Name == name {
			return bc, nil
		}
	}
	return config.BackendConfig{}, fmt.Errorf("unknown backend %q", name)
}

func printPingJSON(cmd *cobra.Command, backendName string, tools []*mcp.Tool) error {
	out := struct {
		Backend string      `json:"backend"`
		Tools   []*mcp.Tool `json:"tools"`
	}{Backend: backendName, Tools: tools}

	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printPingText(cmd *cobra.Command, backendName string, tools []*mcp.Tool) error {
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "backend %q: connected, %d tool(s)\n\n", backendName, len(tools)); err != nil {
		return err
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tDESCRIPTION"); err != nil {
		return err
	}
	for _, t := range tools {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", t.Name, t.Description); err != nil {
			return err
		}
	}
	return w.Flush()
}
