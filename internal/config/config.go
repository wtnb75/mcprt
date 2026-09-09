package config

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// envKeyRE matches a POSIX portable environment variable name, used to
// validate backends[].env and backends[].docker.env keys. It matters most
// for ssh backends: those keys are interpolated unquoted into a shell
// "export NAME=..." statement (see backend.remoteScript), so a key outside
// this set could inject arbitrary shell syntax.
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Config is the top-level gateway configuration, loaded from a YAML file.
type Config struct {
	Listen                    ListenConfig         `yaml:"listen" json:"listen"`
	Backends                  []BackendConfig      `yaml:"backends" json:"backends"`
	Overrides                 map[string]string    `yaml:"overrides,omitempty" json:"overrides,omitempty"`
	ResourceOverrides         map[string]string    `yaml:"resource_overrides,omitempty" json:"resource_overrides,omitempty"`
	ResourceTemplateOverrides map[string]string    `yaml:"resource_template_overrides,omitempty" json:"resource_template_overrides,omitempty"`
	PromptOverrides           map[string]string    `yaml:"prompt_overrides,omitempty" json:"prompt_overrides,omitempty"`
	Prompts                   []StaticPromptConfig `yaml:"prompts,omitempty" json:"prompts,omitempty"`
	Logging                   LoggingConfig        `yaml:"logging,omitempty" json:"logging,omitempty"`
	Timeouts                  TimeoutsConfig       `yaml:"timeouts,omitempty" json:"timeouts,omitempty"`
}

// StaticPromptConfig defines a prompt mcprt serves directly from config,
// without forwarding prompts/get to any backend. See
// internal/gateway.StaticPrompt for the runtime representation built from
// this at gateway-construction time (internal/cli's buildStaticPrompts
// parses Text as a template there; validateStaticPrompts below only checks
// it parses, it doesn't keep the *template.Template around).
//
// SkillFile, when set, is resolved by expandSkillFiles (called from Parse,
// before validation) into Name/Description/Text -- whichever of those three
// fields is still empty after that. A field set here in config.yaml always
// wins over the skill_file's front matter/body, so a single entry can
// override any of them, or add Arguments (which a SKILL.md's front matter
// doesn't carry), without touching the file.
type StaticPromptConfig struct {
	Name        string                 `yaml:"name,omitempty" json:"name,omitempty"`
	Description string                 `yaml:"description,omitempty" json:"description,omitempty"`
	Arguments   []StaticPromptArgument `yaml:"arguments,omitempty" json:"arguments,omitempty"`
	Text        string                 `yaml:"text,omitempty" json:"text,omitempty"`
	SkillFile   string                 `yaml:"skill_file,omitempty" json:"skill_file,omitempty"`
}

// StaticPromptArgument mirrors mcp.PromptArgument's fields (Name,
// Description, Required) -- kept as a separate config-layer type rather
// than importing the mcp package here, matching how BackendConfig etc.
// don't import mcp either.
type StaticPromptArgument struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

// TimeoutsConfig overrides mcprt's built-in timeout and backoff defaults.
// Every field is optional; the zero value (unset in YAML) means "keep the
// built-in default" -- see internal/cli's applyTimeouts, the only reader of
// this struct.
type TimeoutsConfig struct {
	// Shutdown bounds gateway.ServeHTTP's graceful HTTP shutdown. Default 5s.
	Shutdown Duration `yaml:"shutdown,omitempty" json:"shutdown,omitempty"`
	// TelemetryShutdown bounds flushing OpenTelemetry on process shutdown. Default 5s.
	TelemetryShutdown Duration `yaml:"telemetry_shutdown,omitempty" json:"telemetry_shutdown,omitempty"`
	// BackendConnect bounds a single backend connect attempt, and the
	// startup window connectBackends waits for all backends to report in.
	// Default 30s.
	BackendConnect Duration `yaml:"backend_connect,omitempty" json:"backend_connect,omitempty"`
	// ReloadDrain bounds how long a superseded hot-reload generation's
	// backend connections are kept alive before being force-closed. Default 5m.
	ReloadDrain Duration `yaml:"reload_drain,omitempty" json:"reload_drain,omitempty"`
	// Elicit bounds how long the server waits for a human to answer an
	// elicitation request relayed from a backend. Default 5m.
	Elicit Duration `yaml:"elicit,omitempty" json:"elicit,omitempty"`
	// ProgressRelay bounds relaying one progress notification from a
	// backend to the downstream client that requested it. Default 5s.
	ProgressRelay Duration `yaml:"progress_relay,omitempty" json:"progress_relay,omitempty"`
	// BackendBackoffMin/Max bound a disconnected backend's exponential
	// reconnect backoff. Defaults 1s/60s.
	BackendBackoffMin Duration `yaml:"backend_backoff_min,omitempty" json:"backend_backoff_min,omitempty"`
	BackendBackoffMax Duration `yaml:"backend_backoff_max,omitempty" json:"backend_backoff_max,omitempty"`
	// BackendKeepAlive, if non-zero, makes every backend connection send a
	// periodic MCP "ping" at this interval, closing the connection after
	// BackendKeepAliveFailureThreshold consecutive failures -- which
	// superviseBackend's existing disconnect handling then reconnects, the
	// same as any other disconnect. Zero (the default) disables this
	// entirely, matching mcprt's behavior before this field existed.
	BackendKeepAlive Duration `yaml:"backend_keepalive,omitempty" json:"backend_keepalive,omitempty"`
	// BackendKeepAliveFailureThreshold is the number of consecutive
	// keepalive ping failures tolerated before closing the connection. Has
	// no effect unless BackendKeepAlive is non-zero. Zero defers to
	// go-sdk's own default of 1.
	BackendKeepAliveFailureThreshold int `yaml:"backend_keepalive_failure_threshold,omitempty" json:"backend_keepalive_failure_threshold,omitempty"`
	// DownstreamKeepAlive is BackendKeepAlive's counterpart for the other
	// direction: if non-zero, mcprt sends a periodic MCP "ping" to every
	// downstream client at this interval, closing that client's session
	// after DownstreamKeepAliveFailureThreshold consecutive failures. This
	// detects a downstream client that's gone silent (crashed, network
	// drop) without waiting for its next request. Zero (the default)
	// disables this entirely. Unlike BackendKeepAlive, this is applied
	// fresh on every SIGHUP-triggered reload too, not just at process
	// startup -- see internal/cli's buildGateway.
	DownstreamKeepAlive Duration `yaml:"downstream_keepalive,omitempty" json:"downstream_keepalive,omitempty"`
	// DownstreamKeepAliveFailureThreshold is the number of consecutive
	// keepalive ping failures tolerated before closing a downstream
	// client's session. Has no effect unless DownstreamKeepAlive is
	// non-zero. Zero defers to go-sdk's own default of 1.
	DownstreamKeepAliveFailureThreshold int `yaml:"downstream_keepalive_failure_threshold,omitempty" json:"downstream_keepalive_failure_threshold,omitempty"`
}

// ListenConfig controls which client-facing transports the gateway serves.
type ListenConfig struct {
	Stdio bool   `yaml:"stdio,omitempty" json:"stdio,omitempty"`
	HTTP  string `yaml:"http,omitempty" json:"http,omitempty"`
}

// BackendConfig describes one backend MCP server to connect to.
type BackendConfig struct {
	Name      string            `yaml:"name" json:"name"`
	Transport string            `yaml:"transport" json:"transport"` // "stdio" or "http"
	Command   []string          `yaml:"command,omitempty" json:"command,omitempty"`
	Dir       string            `yaml:"dir,omitempty" json:"dir,omitempty"`           // working directory for the stdio subprocess
	EnvFile   string            `yaml:"env_file,omitempty" json:"env_file,omitempty"` // .env-format file merged under Env
	Env       map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	SSH       *SSHConfig        `yaml:"ssh,omitempty" json:"ssh,omitempty"`       // if set, run the stdio Command on a remote host via ssh
	Docker    *DockerConfig     `yaml:"docker,omitempty" json:"docker,omitempty"` // if set, run the stdio Command inside a container
	URL       string            `yaml:"url,omitempty" json:"url,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Proxy     string            `yaml:"proxy,omitempty" json:"proxy,omitempty"` // proxy URL for http transport; "none" disables proxying even if HTTP_PROXY etc. are set; unset follows HTTP_PROXY/HTTPS_PROXY/NO_PROXY
	Prefix    string            `yaml:"prefix,omitempty" json:"prefix,omitempty"`
}

// SSHConfig describes how to reach the remote host a stdio backend's
// Command should be run on.
type SSHConfig struct {
	Host         string   `yaml:"host" json:"host"` // required, e.g. "user@example.com"
	Port         int      `yaml:"port,omitempty" json:"port,omitempty"`
	IdentityFile string   `yaml:"identity_file,omitempty" json:"identity_file,omitempty"` // passed as -i
	Args         []string `yaml:"args,omitempty" json:"args,omitempty"`                   // extra ssh arguments, e.g. ["-J", "jumphost"]
}

// DockerConfig describes how to run a stdio backend's Command inside a
// container via a docker-compatible CLI.
type DockerConfig struct {
	Bin   string            `yaml:"bin,omitempty" json:"bin,omitempty"`   // docker-compatible CLI to invoke; defaults to "docker" (e.g. "podman", "nerdctl")
	Image string            `yaml:"image" json:"image"`                   // required
	Args  []string          `yaml:"args,omitempty" json:"args,omitempty"` // extra arguments appended to "run", e.g. ["-v", "/data:/data"]
	Env   map[string]string `yaml:"env,omitempty" json:"env,omitempty"`   // env vars for the local CLI process itself (e.g. DOCKER_HOST), not the container; backends[].env is the container's env
}

// LoggingConfig controls audit-log behavior beyond what --log-level/--log-format
// (CLI flags) cover.
type LoggingConfig struct {
	// MaskKeys are extra case-insensitive substrings matched against
	// argument key names, in addition to the built-in defaultMaskKeyPatterns
	// ("key", "auth", "pass", "cred", "token") gateway.maskArguments uses.
	MaskKeys []string `yaml:"mask_keys,omitempty" json:"mask_keys,omitempty"`
}

// Load reads and parses the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	return Parse(data)
}

// Parse parses YAML config data, expands ${VAR} references in backend env,
// header and proxy values, merges in each backend's env_file, and validates
// the result.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}
	// Expand ${VAR} refs in the config-declared env/headers/proxy first,
	// then merge in env_file: an env_file's own ${VAR} refs are resolved by
	// godotenv against other vars in that same file (see mergeEnvFiles), not
	// against the host environment, so its values must not go through
	// os.Expand afterward too.
	expandEnvRefs(&cfg)
	if err := mergeEnvFiles(&cfg); err != nil {
		return nil, err
	}
	if err := expandSkillFiles(cfg.Prompts); err != nil {
		return nil, err
	}
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// skillFrontMatter is the subset of a SKILL.md's YAML front matter
// expandSkillFiles understands -- the two fields Anthropic's SKILL.md format
// defines (https://docs.claude.com/en/docs/agents-and-tools/agent-skills).
type skillFrontMatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// expandSkillFiles reads each prompt's SkillFile (if set) and fills in
// Name/Description/Text from it, leaving any field the config.yaml entry
// already set untouched -- config.yaml always wins over the file, per
// StaticPromptConfig's doc comment. It runs from Parse before validate, so a
// missing file, unreadable front matter, or (via validateStaticPrompts,
// unchanged) a still-empty name/text after expansion all fail config
// loading the same way every other misconfiguration here does.
func expandSkillFiles(prompts []StaticPromptConfig) error {
	for i := range prompts {
		p := &prompts[i]
		if p.SkillFile == "" {
			continue
		}
		path, err := expandHome(p.SkillFile)
		if err != nil {
			return fmt.Errorf("prompts: skill_file %q: %w", p.SkillFile, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("prompts: skill_file %q: %w", p.SkillFile, err)
		}
		name, description, body, err := parseSkillFile(data)
		if err != nil {
			return fmt.Errorf("prompts: skill_file %q: %w", p.SkillFile, err)
		}
		if p.Name == "" {
			p.Name = name
		}
		if p.Description == "" {
			p.Description = description
		}
		if p.Text == "" {
			p.Text = body
		}
	}
	return nil
}

// parseSkillFile splits SKILL.md-style content into its YAML front matter
// (delimited by a leading and trailing "---" line) and Markdown body. A file
// with no leading "---" line has no front matter -- its entire content
// becomes the body, so a plain-text prompt file also works as a skill_file.
func parseSkillFile(data []byte) (name, description, body string, err error) {
	const delim = "---\n"
	text := string(data)
	if !strings.HasPrefix(text, delim) {
		return "", "", text, nil
	}
	rest := text[len(delim):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", text, nil
	}
	var fm skillFrontMatter
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return "", "", "", fmt.Errorf("parse front matter: %w", err)
	}
	body = strings.TrimPrefix(rest[end+len("\n---"):], "\n")
	return fm.Name, fm.Description, body, nil
}

// expandHome expands a leading "~" (home-directory shorthand) in path, e.g.
// "~/.claude/skills/foo/SKILL.md" -- unlike a shell, os.ReadFile does not do
// this itself. A path without a leading "~" is returned unchanged and
// resolves against the process's current working directory, same as every
// other file path config.Parse reads (env_file, ssh identity_file, ...).
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[len("~/"):]), nil
}

// mergeEnvFiles reads each backend's EnvFile (if set) and merges it into
// Env, with existing Env entries taking precedence over the file.
func mergeEnvFiles(cfg *Config) error {
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		if b.EnvFile == "" {
			continue
		}
		fileEnv, err := godotenv.Read(b.EnvFile)
		if err != nil {
			return fmt.Errorf("backend %q: env_file: %w", b.Name, err)
		}
		maps.Copy(fileEnv, b.Env)
		b.Env = fileEnv
	}
	return nil
}

func expandEnvRefs(cfg *Config) {
	for i := range cfg.Backends {
		for k, v := range cfg.Backends[i].Env {
			cfg.Backends[i].Env[k] = os.Expand(v, os.Getenv)
		}
		for k, v := range cfg.Backends[i].Headers {
			cfg.Backends[i].Headers[k] = os.Expand(v, os.Getenv)
		}
		cfg.Backends[i].Proxy = os.Expand(cfg.Backends[i].Proxy, os.Getenv)
		if cfg.Backends[i].Docker != nil {
			for k, v := range cfg.Backends[i].Docker.Env {
				cfg.Backends[i].Docker.Env[k] = os.Expand(v, os.Getenv)
			}
		}
	}
}

func validate(cfg *Config) error {
	names := make(map[string]bool, len(cfg.Backends))
	for _, b := range cfg.Backends {
		if b.Name == "" {
			return fmt.Errorf("backend has empty name")
		}
		if names[b.Name] {
			return fmt.Errorf("duplicate backend name: %q", b.Name)
		}
		names[b.Name] = true

		for k := range b.Env {
			if !envKeyRE.MatchString(k) {
				return fmt.Errorf("backend %q: invalid env key %q (must match %s)", b.Name, k, envKeyRE.String())
			}
		}

		if b.SSH != nil {
			if b.Transport != "stdio" {
				return fmt.Errorf("backend %q: ssh is only valid for stdio transport", b.Name)
			}
			if b.SSH.Host == "" {
				return fmt.Errorf("backend %q: ssh requires host", b.Name)
			}
			if b.SSH.Port < 0 || b.SSH.Port > 65535 {
				return fmt.Errorf("backend %q: ssh port %d out of range", b.Name, b.SSH.Port)
			}
		}

		if b.Docker != nil {
			if b.Transport != "stdio" {
				return fmt.Errorf("backend %q: docker is only valid for stdio transport", b.Name)
			}
			if b.SSH != nil {
				return fmt.Errorf("backend %q: ssh and docker are mutually exclusive", b.Name)
			}
			if b.Docker.Image == "" {
				return fmt.Errorf("backend %q: docker requires image", b.Name)
			}
			for k := range b.Docker.Env {
				if !envKeyRE.MatchString(k) {
					return fmt.Errorf("backend %q: invalid docker env key %q (must match %s)", b.Name, k, envKeyRE.String())
				}
			}
		}

		if b.Proxy != "" && b.Proxy != "none" {
			if _, err := url.Parse(b.Proxy); err != nil {
				return fmt.Errorf("backend %q: invalid proxy url: %w", b.Name, err)
			}
		}

		switch b.Transport {
		case "stdio":
			// command may be omitted when docker is set: the container image's
			// own ENTRYPOINT/CMD can supply it. ssh has no such fallback (it
			// execs cfg.Command directly on the remote shell), so it still
			// requires a non-empty command.
			if len(b.Command) == 0 && b.Docker == nil {
				return fmt.Errorf("backend %q: stdio transport requires command", b.Name)
			}
			if b.URL != "" {
				return fmt.Errorf("backend %q: url is only valid for http transport", b.Name)
			}
			if len(b.Headers) > 0 {
				return fmt.Errorf("backend %q: headers is only valid for http transport", b.Name)
			}
			if b.Proxy != "" {
				return fmt.Errorf("backend %q: proxy is only valid for http transport", b.Name)
			}
		case "http":
			if b.URL == "" {
				return fmt.Errorf("backend %q: http transport requires url", b.Name)
			}
			if len(b.Command) > 0 {
				return fmt.Errorf("backend %q: command is only valid for stdio transport", b.Name)
			}
			if b.Dir != "" {
				return fmt.Errorf("backend %q: dir is only valid for stdio transport", b.Name)
			}
			if len(b.Env) > 0 {
				return fmt.Errorf("backend %q: env is only valid for stdio transport", b.Name)
			}
		default:
			return fmt.Errorf("backend %q: unknown transport %q (must be \"stdio\" or \"http\")", b.Name, b.Transport)
		}
	}

	for toolName, backendName := range cfg.Overrides {
		if !names[backendName] {
			return fmt.Errorf("override %q references unknown backend %q", toolName, backendName)
		}
	}
	for uri, backendName := range cfg.ResourceOverrides {
		if !names[backendName] {
			return fmt.Errorf("resource_overrides %q references unknown backend %q", uri, backendName)
		}
	}
	for uriTemplate, backendName := range cfg.ResourceTemplateOverrides {
		if !names[backendName] {
			return fmt.Errorf("resource_template_overrides %q references unknown backend %q", uriTemplate, backendName)
		}
	}
	for promptName, backendName := range cfg.PromptOverrides {
		if !names[backendName] {
			return fmt.Errorf("prompt_overrides %q references unknown backend %q", promptName, backendName)
		}
	}

	if err := validateStaticPrompts(cfg.Prompts); err != nil {
		return err
	}

	if err := validateTimeouts(cfg.Timeouts); err != nil {
		return err
	}

	return nil
}

// validateStaticPrompts rejects an empty/duplicate prompt or argument name,
// an empty text body, and a text body that doesn't parse as a Go
// text/template -- so a broken prompts: entry fails fast at config-load
// time (mcprt validate/server startup/SIGHUP reload), the same as every
// other misconfiguration this file checks.
func validateStaticPrompts(prompts []StaticPromptConfig) error {
	seen := make(map[string]bool, len(prompts))
	for _, p := range prompts {
		if p.Name == "" {
			return fmt.Errorf("prompts: name is required")
		}
		if seen[p.Name] {
			return fmt.Errorf("prompts %q: duplicate name", p.Name)
		}
		seen[p.Name] = true
		if p.Text == "" {
			return fmt.Errorf("prompts %q: text is required", p.Name)
		}
		if _, err := template.New(p.Name).Parse(p.Text); err != nil {
			return fmt.Errorf("prompts %q: parse text template: %w", p.Name, err)
		}
		argSeen := make(map[string]bool, len(p.Arguments))
		for _, a := range p.Arguments {
			if a.Name == "" {
				return fmt.Errorf("prompts %q: argument name is required", p.Name)
			}
			if argSeen[a.Name] {
				return fmt.Errorf("prompts %q: duplicate argument %q", p.Name, a.Name)
			}
			argSeen[a.Name] = true
		}
	}
	return nil
}

// validateTimeouts rejects negative durations (time.ParseDuration itself
// accepts "-5s") and a backoff range with min above max -- both would
// otherwise silently misbehave wherever they're used (see
// internal/cli's applyTimeouts).
func validateTimeouts(t TimeoutsConfig) error {
	fields := map[string]Duration{
		"shutdown":             t.Shutdown,
		"telemetry_shutdown":   t.TelemetryShutdown,
		"backend_connect":      t.BackendConnect,
		"reload_drain":         t.ReloadDrain,
		"elicit":               t.Elicit,
		"progress_relay":       t.ProgressRelay,
		"backend_backoff_min":  t.BackendBackoffMin,
		"backend_backoff_max":  t.BackendBackoffMax,
		"backend_keepalive":    t.BackendKeepAlive,
		"downstream_keepalive": t.DownstreamKeepAlive,
	}
	for name, d := range fields {
		if d < 0 {
			return fmt.Errorf("timeouts.%s: must not be negative, got %s", name, time.Duration(d))
		}
	}
	if t.BackendBackoffMin > 0 && t.BackendBackoffMax > 0 && t.BackendBackoffMin > t.BackendBackoffMax {
		return fmt.Errorf("timeouts.backend_backoff_min (%s) must not exceed timeouts.backend_backoff_max (%s)",
			time.Duration(t.BackendBackoffMin), time.Duration(t.BackendBackoffMax))
	}
	if t.BackendKeepAliveFailureThreshold < 0 {
		return fmt.Errorf("timeouts.backend_keepalive_failure_threshold: must not be negative, got %d", t.BackendKeepAliveFailureThreshold)
	}
	if t.DownstreamKeepAliveFailureThreshold < 0 {
		return fmt.Errorf("timeouts.downstream_keepalive_failure_threshold: must not be negative, got %d", t.DownstreamKeepAliveFailureThreshold)
	}
	return nil
}
