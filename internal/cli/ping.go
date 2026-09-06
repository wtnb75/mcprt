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
		Use:           "ping [backend]",
		Short:         "connect to a configured backend (or, with no argument, every configured backend) and print the tools it reports",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var backendName string
			if len(args) > 0 {
				backendName = args[0]
			}
			return runPing(cmd.Context(), cmd, configPath, backendName, jsonOutput)
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

	if backendName == "" {
		return runPingAll(ctx, cmd, cfg.Backends, jsonOutput)
	}

	bc, err := findBackendConfig(cfg.Backends, backendName)
	if err != nil {
		return err
	}

	// Unlike list's connectBackends, a single named backend is the whole
	// point of ping, so a connect or list-tools failure is returned as the
	// command's own error instead of being logged and skipped.
	tools, err := pingBackend(ctx, bc)
	if err != nil {
		return err
	}

	if jsonOutput {
		return printPingJSON(cmd, backendName, tools)
	}
	return printPingText(cmd, backendName, tools)
}

// pingResult is one backend's outcome from runPingAll: either Tools is set
// (connect + ListTools succeeded) or Err is set (connect or ListTools
// failed), never both.
type pingResult struct {
	Backend string
	Tools   []*mcp.Tool
	Err     error
}

// runPingAll pings every configured backend in turn, continuing past a
// failed backend so the rest are still tried -- unlike the single-backend
// path, one unreachable backend shouldn't hide whether the others are up.
// It still reports overall failure (a non-nil error) if any backend failed,
// after printing the full per-backend report.
func runPingAll(ctx context.Context, cmd *cobra.Command, backends []config.BackendConfig, jsonOutput bool) error {
	if len(backends) == 0 {
		return fmt.Errorf("no backends configured")
	}

	results := make([]pingResult, len(backends))
	failed := 0
	for i, bc := range backends {
		tools, err := pingBackend(ctx, bc)
		results[i] = pingResult{Backend: bc.Name, Tools: tools, Err: err}
		if err != nil {
			failed++
		}
	}

	if jsonOutput {
		if err := printPingAllJSON(cmd, results); err != nil {
			return err
		}
	} else if err := printPingAllText(cmd, results); err != nil {
		return err
	}

	if failed > 0 {
		return fmt.Errorf("ping failed for %d of %d backend(s)", failed, len(results))
	}
	return nil
}

// pingBackend connects to bc, lists its tools, and disconnects again,
// bounded by backendConnectTimeout -- the same per-attempt timeout
// connectBackends applies, but without its retry/backoff loop: ping is a
// one-shot check, not a long-lived supervisor.
func pingBackend(ctx context.Context, bc config.BackendConfig) ([]*mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
	defer cancel()

	b, err := backend.Connect(ctx, bc, backend.ChangeCallbacks{})
	if err != nil {
		return nil, fmt.Errorf("connecting to backend %q: %w", bc.Name, err)
	}
	defer func() { _ = b.Close() }()

	tools, err := b.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing tools on backend %q: %w", bc.Name, err)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
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

// pingAllEntry is one backend's result in the shape printed by
// `mcprt ping --json` with no backend argument.
type pingAllEntry struct {
	Backend string      `json:"backend"`
	OK      bool        `json:"ok"`
	Tools   []*mcp.Tool `json:"tools,omitempty"`
	Error   string      `json:"error,omitempty"`
}

func printPingAllJSON(cmd *cobra.Command, results []pingResult) error {
	entries := make([]pingAllEntry, 0, len(results))
	for _, r := range results {
		entry := pingAllEntry{Backend: r.Backend, OK: r.Err == nil, Tools: r.Tools}
		if r.Err != nil {
			entry.Error = r.Err.Error()
		}
		entries = append(entries, entry)
	}

	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Backends []pingAllEntry `json:"backends"`
	}{Backends: entries})
}

func printPingAllText(cmd *cobra.Command, results []pingResult) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "BACKEND\tSTATUS\tTOOLS\tDETAIL"); err != nil {
		return err
	}
	for _, r := range results {
		status, detail := "ok", ""
		if r.Err != nil {
			status, detail = "error", r.Err.Error()
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", r.Backend, status, len(r.Tools), detail); err != nil {
			return err
		}
	}
	return w.Flush()
}
