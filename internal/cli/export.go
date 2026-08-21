package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/wtnb75/mcprt/internal/config"
)

// mcprtVarRE matches mcprt's own "${NAME}" env-reference syntax (see
// config.expandEnvRefs), so it can be rewritten to VS Code's "${env:NAME}"
// syntax for the exported mcp.json.
var mcprtVarRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// defaultExportPath is the output path `mcprt export` writes to when the
// caller doesn't name one.
const defaultExportPath = "mcp.json"

func newExportCmd() *cobra.Command {
	var configPath string
	var force bool

	// SilenceUsage/SilenceErrors: don't dump flag usage on a runtime error
	// (bad config, output file already exists, etc.), and let cli.Execute's
	// caller (main.go) be the one place that prints the error.
	cmd := &cobra.Command{
		Use:           "export [output-path]",
		Short:         "convert an mcprt config file into a generic mcp.json-style client config",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			outPath := defaultExportPath
			if len(args) == 1 {
				outPath = args[0]
			}
			return runExport(cmd, configPath, outPath, force)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to the gateway config file (required)")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err) // programmer error: "config" flag name must match Flags().StringVar above
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite the output path if it already exists")
	return cmd
}

func runExport(cmd *cobra.Command, configPath, outPath string, force bool) error {
	if !force {
		if _, err := os.Stat(outPath); err == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", outPath)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking %s: %w", outPath, err)
		}
	}

	// Unmarshal the raw YAML directly, bypassing config.Load/config.Parse:
	// those expand "${VAR}" against the live process environment and merge
	// in env_file, which would bake already-resolved secret values into the
	// exported file instead of preserving the "${VAR}" reference.
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", configPath, err)
	}
	var src config.Config
	if err := yaml.Unmarshal(data, &src); err != nil {
		return fmt.Errorf("parsing %s: %w", configPath, err)
	}

	var warnings []string
	warn := func(msg string) { warnings = append(warnings, msg) }

	if len(src.Overrides) > 0 {
		warn("overrides: dropped (mcp.json has no equivalent for resolving exposed-name conflicts)")
	}
	if len(src.ResourceOverrides) > 0 {
		warn("resource_overrides: dropped (mcp.json has no equivalent for resolving conflicting resource URIs)")
	}
	if len(src.ResourceTemplateOverrides) > 0 {
		warn("resource_template_overrides: dropped (mcp.json has no equivalent for resolving conflicting URI templates)")
	}
	if len(src.PromptOverrides) > 0 {
		warn("prompt_overrides: dropped (mcp.json has no equivalent for resolving conflicting prompt names)")
	}

	servers := make(map[string]mcpJSONServer, len(src.Backends))
	for _, b := range src.Backends {
		s, ok := convertBackend(b, warn)
		if !ok {
			continue
		}
		servers[b.Name] = s
	}

	for _, w := range warnings {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w); err != nil {
			return err
		}
	}
	if len(servers) == 0 {
		return fmt.Errorf("no backend could be exported from %s", configPath)
	}

	out, err := json.MarshalIndent(mcpJSONFile{MCPServers: servers}, "", "  ")
	if err != nil {
		return fmt.Errorf("rendering mcp.json: %w", err)
	}
	if err := os.WriteFile(outPath, out, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}

	_, err = fmt.Fprintf(cmd.OutOrStdout(), "exported %d backend(s) from %s to %s\n", len(servers), configPath, outPath)
	return err
}

// convertBackend maps one mcprt BackendConfig to an mcp.json entry. It
// reports false (after calling warn) when the entry can't be represented in
// mcp.json at all (currently: ssh backends), rather than failing the whole
// export.
func convertBackend(b config.BackendConfig, warn func(string)) (mcpJSONServer, bool) {
	if b.SSH != nil {
		warn(fmt.Sprintf("backend %q: skipped (ssh backends have no mcp.json equivalent)", b.Name))
		return mcpJSONServer{}, false
	}
	if b.EnvFile != "" {
		warn(fmt.Sprintf("backend %q: env_file %q is not included in the export (mcp.json has no file-reference concept); only its explicit env entries are exported", b.Name, b.EnvFile))
	}
	if b.Proxy != "" {
		warn(fmt.Sprintf("backend %q: proxy is dropped (mcp.json has no equivalent)", b.Name))
	}
	if b.Prefix != "" {
		warn(fmt.Sprintf("backend %q: prefix is dropped (mcp.json has no equivalent)", b.Name))
	}

	switch b.Transport {
	case "stdio":
		s := mcpJSONServer{Env: translateVarsToEnvPlaceholders(b.Env)}
		if len(b.Command) > 0 {
			s.Command = b.Command[0]
			s.Args = b.Command[1:]
		}
		s.Cwd = b.Dir
		return s, true
	case "http":
		return mcpJSONServer{
			Type:    "streamable-http",
			URL:     b.URL,
			Headers: translateVarsToEnvPlaceholders(b.Headers),
		}, true
	default:
		warn(fmt.Sprintf("backend %q: skipped (unknown transport %q)", b.Name, b.Transport))
		return mcpJSONServer{}, false
	}
}

// translateVarsToEnvPlaceholders rewrites mcprt's own "${NAME}" syntax to
// VS Code's "${env:NAME}" syntax, the reverse of translatePlaceholders.
func translateVarsToEnvPlaceholders(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = mcprtVarRE.ReplaceAllString(v, `${env:$1}`)
	}
	return out
}
