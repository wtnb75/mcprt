package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/wtnb75/mcprt/internal/config"
)

// mcpJSONServer is one entry under "mcpServers"/"servers" in a generic
// mcp.json-style client config (Claude Desktop, Cursor, VS Code, ...).
// Fields with no mcprt equivalent (headersHelper, VS Code's non-env
// placeholders, ...) are intentionally not modeled here.
type mcpJSONServer struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// mcpJSONFile is the top level of a generic mcp.json-style client config.
// "mcpServers" is used by Claude Desktop and Cursor; VS Code uses "servers".
// A file may reasonably contain either (or, in principle, both).
type mcpJSONFile struct {
	MCPServers map[string]mcpJSONServer `json:"mcpServers,omitempty"`
	Servers    map[string]mcpJSONServer `json:"servers,omitempty"`
}

// envPlaceholderRE matches VS Code's "${env:NAME}" env-reference syntax, so
// it can be rewritten to mcprt's own "${NAME}" (see config.expandEnvRefs).
var envPlaceholderRE = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// anyPlaceholderRE matches any "${...}" left after envPlaceholderRE has run,
// i.e. an editor-specific placeholder (VS Code's "${workspaceFolder}",
// "${input:...}", etc.) that mcprt has no way to resolve.
var anyPlaceholderRE = regexp.MustCompile(`\$\{[^}]*\}`)

func newImportCmd() *cobra.Command {
	var force bool

	// SilenceUsage/SilenceErrors: don't dump flag usage on a runtime error
	// (bad input JSON, output file already exists, etc.), and let
	// cli.Execute's caller (main.go) be the one place that prints the
	// error.
	cmd := &cobra.Command{
		Use:           "import <mcp.json> [output-path]",
		Short:         "convert a generic mcp.json-style client config into an mcprt config file",
		Args:          cobra.RangeArgs(1, 2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			outPath := defaultInitPath
			if len(args) == 2 {
				outPath = args[1]
			}
			return runImport(cmd, args[0], outPath, force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite the output path if it already exists")
	return cmd
}

func runImport(cmd *cobra.Command, inPath, outPath string, force bool) error {
	if !force {
		if _, err := os.Stat(outPath); err == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", outPath)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking %s: %w", outPath, err)
		}
	}

	data, err := os.ReadFile(inPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", inPath, err)
	}
	var src mcpJSONFile
	if err := json.Unmarshal(data, &src); err != nil {
		return fmt.Errorf("parsing %s: %w", inPath, err)
	}
	if len(src.MCPServers) == 0 && len(src.Servers) == 0 {
		return fmt.Errorf("%s has no top-level \"mcpServers\" or \"servers\" object", inPath)
	}

	var warnings []string
	warn := func(msg string) { warnings = append(warnings, msg) }

	seen := make(map[string]bool)
	var backends []config.BackendConfig
	for _, group := range []map[string]mcpJSONServer{src.MCPServers, src.Servers} {
		for _, name := range sortedKeys(group) {
			if seen[name] {
				warn(fmt.Sprintf("backend %q: skipped duplicate name (already imported from an earlier entry)", name))
				continue
			}
			bc, ok := convertServer(name, group[name], warn)
			if !ok {
				continue
			}
			seen[name] = true
			backends = append(backends, bc)
		}
	}

	for _, w := range warnings {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w); err != nil {
			return err
		}
	}
	if len(backends) == 0 {
		return fmt.Errorf("no backend could be imported from %s", inPath)
	}

	cfg := &config.Config{
		Listen:   config.ListenConfig{Stdio: true, HTTP: "127.0.0.1:8080"},
		Backends: backends,
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("rendering config: %w", err)
	}
	if err := os.WriteFile(outPath, out, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}

	_, err = fmt.Fprintf(cmd.OutOrStdout(), "imported %d backend(s) from %s to %s\n", len(backends), inPath, outPath)
	return err
}

func sortedKeys(m map[string]mcpJSONServer) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// convertServer maps one mcp.json entry to an mcprt BackendConfig. It
// reports false (after calling warn) when the entry can't be converted at
// all, rather than failing the whole import.
func convertServer(name string, s mcpJSONServer, warn func(string)) (config.BackendConfig, bool) {
	transport, ok := inferTransport(s)
	if !ok {
		warn(fmt.Sprintf("backend %q: skipped (no command or url, and type is not recognized)", name))
		return config.BackendConfig{}, false
	}

	bc := config.BackendConfig{Name: name, Transport: transport}
	switch transport {
	case "stdio":
		if s.Command == "" {
			warn(fmt.Sprintf("backend %q: skipped (stdio type but no command)", name))
			return config.BackendConfig{}, false
		}
		bc.Command = append([]string{s.Command}, s.Args...)
		bc.Env = translatePlaceholders(name, "env", s.Env, warn)
		bc.Dir = s.Cwd
	case "http":
		if s.URL == "" {
			warn(fmt.Sprintf("backend %q: skipped (http type but no url)", name))
			return config.BackendConfig{}, false
		}
		bc.URL = s.URL
		bc.Headers = translatePlaceholders(name, "headers", s.Headers, warn)
		if strings.EqualFold(strings.TrimSpace(s.Type), "sse") {
			warn(fmt.Sprintf("backend %q: source type is \"sse\" (legacy SSE transport); mcprt's http backend speaks streamable HTTP and may not connect", name))
		}
	}
	return bc, true
}

// inferTransport decides stdio vs. http for one entry: an explicit type
// wins when recognized, otherwise it falls back to which of
// command/url is present.
func inferTransport(s mcpJSONServer) (transport string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s.Type)) {
	case "stdio":
		return "stdio", true
	case "http", "streamable-http", "streamablehttp", "sse":
		return "http", true
	}
	if s.Command != "" {
		return "stdio", true
	}
	if s.URL != "" {
		return "http", true
	}
	return "", false
}

// translatePlaceholders rewrites VS Code's "${env:NAME}" syntax to mcprt's
// own "${NAME}" (both are expanded from the process environment at load
// time), and warns about any placeholder left over that mcprt has no way to
// resolve (e.g. "${workspaceFolder}").
func translatePlaceholders(backendName, field string, m map[string]string, warn func(string)) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		// Check for any placeholder mcprt can't resolve on a copy with the
		// (translatable) "${env:NAME}" occurrences stripped out first — the
		// translated "${NAME}" they become is exactly mcprt's own syntax
		// and must not itself be flagged here.
		if remainder := envPlaceholderRE.ReplaceAllString(v, ""); anyPlaceholderRE.MatchString(remainder) {
			warn(fmt.Sprintf("backend %q: %s[%q] contains a placeholder mcprt cannot resolve: %q", backendName, field, k, v))
		}
		out[k] = envPlaceholderRE.ReplaceAllString(v, `${$1}`)
	}
	return out
}
