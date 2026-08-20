package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/router"
)

func newCallCmd() *cobra.Command {
	var configPath string
	var argsJSON string
	var jsonOutput bool

	// SilenceUsage/SilenceErrors: don't dump flag usage on a runtime error
	// (bad config, backend failure, a tool result with IsError, etc.), and
	// let cli.Execute's caller (main.go) be the one place that prints the
	// error.
	cmd := &cobra.Command{
		Use:           "call <tool-name>",
		Short:         "resolve a tool's exposed name through the gateway routing table and call it",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCall(cmd.Context(), cmd, configPath, args[0], argsJSON, jsonOutput)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to the gateway config file (required)")
	cmd.Flags().StringVar(&argsJSON, "args", "", "tool arguments as a JSON object (default: no arguments)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the raw CallToolResult as JSON instead of a text rendering")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err) // programmer error: "config" flag name must match Flags().StringVar above
	}

	return cmd
}

func runCall(ctx context.Context, cmd *cobra.Command, configPath, toolName, argsJSON string, jsonOutput bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	var arguments any
	if argsJSON != "" {
		if !json.Valid([]byte(argsJSON)) {
			return fmt.Errorf("--args: invalid JSON: %s", argsJSON)
		}
		arguments = json.RawMessage(argsJSON)
	}

	// call goes through the same router resolution the real gateway uses
	// (prefix/overrides applied), so it exercises the exact path a client
	// would.
	logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
	backends, entries := connectBackends(ctx, logger, cfg.Backends)
	defer func() {
		for _, b := range backends {
			_ = b.Close()
		}
	}()

	table := router.Resolve(entries, cfg.Overrides)
	resolved, ok := table.Tools[toolName]
	if !ok {
		return fmt.Errorf("unknown tool %q", toolName)
	}
	b := backends[resolved.BackendName]

	result, err := b.Session.CallTool(ctx, &mcp.CallToolParams{Name: resolved.OriginalName, Arguments: arguments})
	if err != nil {
		return fmt.Errorf("calling tool %q: %w", toolName, err)
	}

	if jsonOutput {
		if err := printCallJSON(cmd, result); err != nil {
			return err
		}
	} else if err := printCallText(cmd, result); err != nil {
		return err
	}

	if result.IsError {
		return fmt.Errorf("tool %q returned an error result", toolName)
	}
	return nil
}

func printCallJSON(cmd *cobra.Command, result *mcp.CallToolResult) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

func printCallText(cmd *cobra.Command, result *mcp.CallToolResult) error {
	out := cmd.OutOrStdout()
	for _, c := range result.Content {
		var line string
		if text, ok := c.(*mcp.TextContent); ok {
			line = text.Text
		} else {
			line = fmt.Sprintf("[%T content]", c)
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}

	if result.StructuredContent != nil {
		structured, err := json.MarshalIndent(result.StructuredContent, "", "  ")
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, string(structured)); err != nil {
			return err
		}
	}

	if result.IsError {
		if _, err := fmt.Fprintln(out, "(tool returned an error result)"); err != nil {
			return err
		}
	}
	return nil
}
