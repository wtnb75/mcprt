package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/wtnb75/mcprt/internal/config"
)

// mcprtVarRE matches mcprt's own "${NAME}" env-reference syntax (see
// config.expandEnvRefs), so it can be rewritten to VS Code's "${env:NAME}"
// syntax for the exported mcp.json.
var mcprtVarRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func newExportCmd() *cobra.Command {
	var configPath string
	var force bool

	// SilenceUsage/SilenceErrors: don't dump flag usage on a runtime error
	// (bad config, output file already exists, etc.), and let cli.Execute's
	// caller (main.go) be the one place that prints the error.
	cmd := &cobra.Command{
		Use:           "export [output-path]",
		Short:         "convert an mcprt config file into a generic mcp.json-style client config (defaults to stdout)",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			outPath := ""
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

// runExport converts the config at configPath into mcp.json content. An
// empty outPath means "no path given on the command line": the content goes
// to cmd.OutOrStdout() instead of a file, and the exists/force check is
// skipped since there is no file to guard.
func runExport(cmd *cobra.Command, configPath, outPath string, force bool) error {
	if outPath != "" && !force {
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

	dest := outPath
	if outPath == "" {
		dest = "stdout"
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(out)); err != nil {
			return err
		}
	} else if err := os.WriteFile(outPath, out, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}

	_, err = fmt.Fprintf(cmd.ErrOrStderr(), "exported %d backend(s) from %s to %s\n", len(servers), configPath, dest)
	return err
}

// convertBackend maps one mcprt BackendConfig to an mcp.json entry. It
// reports false (after calling warn) when the entry can't be represented in
// mcp.json at all, rather than failing the whole export.
func convertBackend(b config.BackendConfig, warn func(string)) (mcpJSONServer, bool) {
	if b.EnvFile != "" {
		warn(fmt.Sprintf("backend %q: env_file %q is not included in the export (mcp.json has no file-reference concept); only its explicit env entries are exported", b.Name, b.EnvFile))
	}
	if b.Proxy != "" {
		warn(fmt.Sprintf("backend %q: proxy is dropped (mcp.json has no equivalent)", b.Name))
	}
	if b.Prefix != "" {
		warn(fmt.Sprintf("backend %q: prefix is dropped (mcp.json has no equivalent)", b.Name))
	}

	switch {
	case b.SSH != nil:
		return convertSSHBackend(b), true
	case b.Docker != nil:
		return convertDockerBackend(b), true
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

// convertSSHBackend mirrors backend.sshCommand/remoteScript, building the
// equivalent "ssh ... <remote-script>" invocation as a plain mcp.json
// command/args entry. b.Env values go through translateVarPlaceholder before
// being embedded in the script text, so a generic mcp.json client expands
// them (via its own "${env:NAME}" substitution) instead of the export baking
// in resolved secret values.
func convertSSHBackend(b config.BackendConfig) mcpJSONServer {
	var args []string
	if b.SSH.Port != 0 {
		args = append(args, "-p", strconv.Itoa(b.SSH.Port))
	}
	if b.SSH.IdentityFile != "" {
		args = append(args, "-i", b.SSH.IdentityFile)
	}
	args = append(args, b.SSH.Args...)
	args = append(args, b.SSH.Host, sshRemoteScript(b))
	return mcpJSONServer{Command: "ssh", Args: args}
}

// sshRemoteScript builds the same "cd ... && export ... && exec ..." shell
// script as backend.remoteScript, except each env value is translated to
// mcp.json's "${env:NAME}" placeholder syntax first (see convertSSHBackend).
func sshRemoteScript(b config.BackendConfig) string {
	var s strings.Builder
	if b.Dir != "" {
		fmt.Fprintf(&s, "cd %s && ", shellQuote(b.Dir))
	}

	keys := make([]string, 0, len(b.Env))
	for k := range b.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&s, "export %s=%s && ", k, shellQuote(translateVarPlaceholder(b.Env[k])))
	}

	quoted := make([]string, len(b.Command))
	for i, arg := range b.Command {
		quoted[i] = shellQuote(arg)
	}
	s.WriteString("exec ")
	s.WriteString(strings.Join(quoted, " "))
	return s.String()
}

// shellQuote wraps s in single quotes for safe use as one word in a POSIX
// shell command, escaping any single quotes it contains. Mirrors
// backend.shellQuote; kept separate since the two packages build shell text
// for different purposes (live exec vs. an exported mcp.json script).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// convertDockerBackend mirrors backend.dockerCommand, building the
// equivalent "<bin> run ... <image> <command>" invocation as a plain
// mcp.json command/args entry. b.Env (the container's env) is translated to
// mcp.json's "${env:NAME}" placeholder syntax and passed as "-e" flags,
// matching how dockerCommand passes it to the container; b.Docker.Env (the
// local CLI process's own env, e.g. DOCKER_HOST) becomes the entry's "env"
// field.
func convertDockerBackend(b config.BackendConfig) mcpJSONServer {
	bin := b.Docker.Bin
	if bin == "" {
		bin = "docker"
	}

	args := []string{"run", "-i", "--rm"}
	keys := make([]string, 0, len(b.Env))
	for k := range b.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+translateVarPlaceholder(b.Env[k]))
	}
	if b.Dir != "" {
		args = append(args, "-w", b.Dir)
	}
	args = append(args, b.Docker.Args...)
	args = append(args, b.Docker.Image)
	args = append(args, b.Command...)

	return mcpJSONServer{Command: bin, Args: args, Env: translateVarsToEnvPlaceholders(b.Docker.Env)}
}

// translateVarPlaceholder rewrites mcprt's own "${NAME}" syntax to VS Code's
// "${env:NAME}" syntax in a single value; see translateVarsToEnvPlaceholders
// for the map version.
func translateVarPlaceholder(v string) string {
	return mcprtVarRE.ReplaceAllString(v, `${env:$1}`)
}

// translateVarsToEnvPlaceholders rewrites mcprt's own "${NAME}" syntax to
// VS Code's "${env:NAME}" syntax, the reverse of translatePlaceholders.
func translateVarsToEnvPlaceholders(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = translateVarPlaceholder(v)
	}
	return out
}
