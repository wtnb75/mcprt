package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"text/tabwriter"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/router"
)

// listEntry is one resolved tool, in the shape printed by the text table.
type listEntry struct {
	Name         string `json:"name"`
	Backend      string `json:"backend"`
	OriginalName string `json:"original_name"`
}

// listToolDetail is one resolved tool with its full backend-reported
// definition (description, input/output schema, annotations, ...), in the
// shape printed by `mcprt list --json`.
type listToolDetail struct {
	Backend      string    `json:"backend"`
	OriginalName string    `json:"original_name"`
	Tool         *mcp.Tool `json:"tool"`
}

// listConflict is one exposed-name conflict, in the shape printed by
// `mcprt list`.
type listConflict struct {
	Name   string   `json:"name"`
	Winner string   `json:"winner"`
	Hidden []string `json:"hidden"`
}

func newListCmd() *cobra.Command {
	var configPath string
	var jsonOutput bool

	// SilenceUsage/SilenceErrors: don't dump flag usage on a runtime error
	// (bad config, backend failure, etc.), and let cli.Execute's caller
	// (main.go) be the one place that prints the error.
	cmd := &cobra.Command{
		Use:           "list",
		Short:         "connect to every backend and print the resolved tool routing table",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd.Context(), cmd, configPath, jsonOutput)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to the gateway config file (required)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print machine-readable JSON instead of a text table")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err) // programmer error: "config" flag name must match Flags().StringVar above
	}

	return cmd
}

func runList(ctx context.Context, cmd *cobra.Command, configPath string, jsonOutput bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// list only needs to know each backend's tools long enough to resolve
	// and print the routing table; it never starts a listener, so every
	// connection is closed again before returning.
	logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
	backends, entries := connectBackends(ctx, logger, cfg.Backends)
	defer func() {
		for _, b := range backends {
			_ = b.Close()
		}
	}()

	table := router.Resolve(entries, toolNameOf, toolRename, cfg.Overrides)

	if jsonOutput {
		return printListJSON(cmd, listToolDetails(table), listConflicts(table))
	}
	entriesOut, conflictsOut := listTable(table)
	return printListText(cmd, entriesOut, conflictsOut)
}

// listTable converts table into the sorted, text-friendly shapes printed by
// `mcprt list`.
func listTable(table *router.Table[*mcp.Tool]) ([]listEntry, []listConflict) {
	entries := make([]listEntry, 0, len(table.Items))
	for name, resolved := range table.Items {
		entries = append(entries, listEntry{Name: name, Backend: resolved.BackendName, OriginalName: resolved.OriginalName})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	return entries, listConflicts(table)
}

// listToolDetails converts table into the sorted, full-detail shape printed
// by `mcprt list --json`.
func listToolDetails(table *router.Table[*mcp.Tool]) []listToolDetail {
	details := make([]listToolDetail, 0, len(table.Items))
	for _, resolved := range table.Items {
		details = append(details, listToolDetail{Backend: resolved.BackendName, OriginalName: resolved.OriginalName, Tool: resolved.Item})
	}
	sort.Slice(details, func(i, j int) bool { return details[i].Tool.Name < details[j].Tool.Name })
	return details
}

func listConflicts(table *router.Table[*mcp.Tool]) []listConflict {
	conflicts := make([]listConflict, 0, len(table.Conflicts))
	for _, c := range table.Conflicts {
		conflicts = append(conflicts, listConflict{Name: c.ExposedName, Winner: c.Winner, Hidden: c.Losers})
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Name < conflicts[j].Name })
	return conflicts
}

func printListJSON(cmd *cobra.Command, tools []listToolDetail, conflicts []listConflict) error {
	out := struct {
		Tools     []listToolDetail `json:"tools"`
		Conflicts []listConflict   `json:"conflicts"`
	}{Tools: tools, Conflicts: conflicts}

	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printListText(cmd *cobra.Command, entries []listEntry, conflicts []listConflict) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tBACKEND\tORIGINAL"); err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", e.Name, e.Backend, e.OriginalName); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if len(conflicts) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), "\nconflicts:"); err != nil {
		return err
	}
	for _, c := range conflicts {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %s: winner=%s hidden=%v\n", c.Name, c.Winner, c.Hidden); err != nil {
			return err
		}
	}
	return nil
}
