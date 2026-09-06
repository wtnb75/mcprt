package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
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
	applyTimeouts(cfg.Timeouts)

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
	// connectBackends spawns a supervisor goroutine per backend that keeps
	// watching (and reconnecting to) its backend for as long as ctx lives, so
	// this command owns a context it can end itself.
	ctx, cancel := context.WithCancel(ctx)
	conn := connectBackends(ctx, logger, cfg.Backends, nil)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()
	// Registered last -> runs first (LIFO), before the close loop above: the
	// close is what releases each supervisor from Session.Wait(), and the
	// graceful-disconnect path has no backoff at all, so a supervisor still
	// running then would immediately open one more connection (for a stdio
	// backend, fork one more subprocess) that nobody owns, microseconds
	// before this process exits.
	defer cancel()

	table := router.Resolve(conn.toolEntries, gateway.ToolNameOf, gateway.ToolRename, cfg.Overrides)
	resolved, ok := table.Items[toolName]
	if !ok {
		return fmt.Errorf("unknown tool %q", toolName)
	}
	b := conn.backends[resolved.BackendName]

	start := time.Now()
	result, err := b.Session.CallTool(ctx, &mcp.CallToolParams{Name: resolved.OriginalName, Arguments: arguments})
	logCLICall(logger, resolved.BackendName, toolName, resolved.OriginalName, arguments, cfg.Logging.MaskKeys, start, err)
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

// logCLICall logs one mcprt-call invocation's outcome in the same spirit as
// the gateway's own tools/call audit line (internal/gateway/audit.go's
// logCall) -- backend, tool, masked arguments, duration, success/failure --
// but without the server-side identity fields (session_id, client_name/
// version, remote_addr, trace/span id) that don't exist for a one-shot CLI
// invocation with no downstream MCP session. Uses "cli " message prefixes
// (distinct from logCall's "tool call"/"tool call failed") so a log
// pipeline that also ingests mcprt server's audit log can tell a
// human-operator-invoked call apart from one the gateway relayed.
//
// tool and originalTool can differ: tool is the gateway-exposed name the
// operator typed on the command line (the more intuitive value for a human
// reading this invocation's own log line), while originalTool is the name
// as the backend itself knows it -- the same value logCall logs under its
// own "tool" key. A backend config's prefix or the gateway's overrides
// config can rename a tool between the two, so for a call through such a
// backend these two names diverge for what is otherwise the exact same
// underlying call. Logging both lets an operator grep either name and find
// the matching line in both mcprt call's and mcprt server's audit logs.
func logCLICall(logger *slog.Logger, backendName, tool, originalTool string, arguments any, maskKeys []string, start time.Time, err error) {
	attrs := []any{
		"backend", backendName,
		"tool", tool,
		"original_tool", originalTool,
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if arguments != nil {
		attrs = append(attrs, "arguments", gateway.MaskArguments(arguments, maskKeys))
	}
	if err != nil {
		logger.Error("cli tool call failed", append(attrs, "error", err)...)
		return
	}
	logger.Info("cli tool call", attrs...)
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
