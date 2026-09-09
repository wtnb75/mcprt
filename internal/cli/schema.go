package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/invopop/jsonschema"
	"github.com/spf13/cobra"

	"github.com/wtnb75/mcprt/internal/config"
)

// configSchemaURL is embedded both in the JSON Schema's own self-referential
// $id (configSchema below) and in the `# yaml-language-server: $schema=...`
// modeline `mcprt init` (init.go) prepends to its generated config.yaml --
// one constant so the two can't drift apart.
const configSchemaURL = "https://raw.githubusercontent.com/wtnb75/mcprt/main/config.schema.json"

func newSchemaCmd() *cobra.Command {
	var force bool

	// SilenceUsage/SilenceErrors: don't dump flag usage on a runtime error
	// (output file already exists, write failure, etc.), and let
	// cli.Execute's caller (main.go) be the one place that prints the
	// error.
	cmd := &cobra.Command{
		Use:           "schema [output-path]",
		Short:         "print a JSON Schema for the gateway config file format (defaults to stdout)",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			outPath := ""
			if len(args) == 1 {
				outPath = args[0]
			}
			return runSchema(cmd, outPath, force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite the output path if it already exists")
	return cmd
}

// configSchema reflects config.Config into a JSON Schema document. It is
// the single source both `mcprt schema` and config.schema.json's staleness
// test (internal/cli/schema_test.go) call, so the two can never drift from
// each other -- only from the committed file, which is exactly what that
// test catches.
func configSchema() ([]byte, error) {
	r := &jsonschema.Reflector{
		// Inline Config's own properties at the schema root instead of a
		// $ref into $defs["Config"]: config.schema.json is meant to be
		// pointed at directly (via a `# yaml-language-server: $schema=...`
		// comment), so its root should describe the document being
		// validated, not a pointer to it.
		ExpandedStruct: true,
	}
	schema := r.Reflect(&config.Config{})
	// Self-referential $id: README.md documents pointing a config.yaml's
	// `# yaml-language-server: $schema=...` comment at this exact raw URL,
	// so the schema should identify itself the same way, per JSON Schema's
	// own convention (draft-bhutton-json-schema-00 section 8.2.1).
	schema.ID = configSchemaURL
	return json.MarshalIndent(schema, "", "  ")
}

// runSchema writes configSchema()'s output to outPath, or to cmd's stdout
// when outPath is empty (no path given on the command line) -- the same
// stdout-by-default/force-to-overwrite convention runExport uses.
func runSchema(cmd *cobra.Command, outPath string, force bool) error {
	if outPath != "" && !force {
		if _, err := os.Stat(outPath); err == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", outPath)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking %s: %w", outPath, err)
		}
	}

	data, err := configSchema()
	if err != nil {
		return fmt.Errorf("generating schema: %w", err)
	}
	data = append(data, '\n')

	if outPath == "" {
		_, err := cmd.OutOrStdout().Write(data)
		return err
	}
	// A schema file carries no secrets (unlike an exported mcp.json or a
	// config.yaml, which may embed tokens) -- 0o644 rather than those
	// commands' 0o600, since there's no reason to keep it unreadable to
	// other local users.
	return os.WriteFile(outPath, data, 0o644)
}
