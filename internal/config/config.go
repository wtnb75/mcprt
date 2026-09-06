package config

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"

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
	Listen                    ListenConfig      `yaml:"listen"`
	Backends                  []BackendConfig   `yaml:"backends"`
	Overrides                 map[string]string `yaml:"overrides,omitempty"`
	ResourceOverrides         map[string]string `yaml:"resource_overrides,omitempty"`
	ResourceTemplateOverrides map[string]string `yaml:"resource_template_overrides,omitempty"`
	PromptOverrides           map[string]string `yaml:"prompt_overrides,omitempty"`
	Logging                   LoggingConfig     `yaml:"logging,omitempty"`
}

// ListenConfig controls which client-facing transports the gateway serves.
type ListenConfig struct {
	Stdio bool   `yaml:"stdio,omitempty"`
	HTTP  string `yaml:"http,omitempty"`
}

// BackendConfig describes one backend MCP server to connect to.
type BackendConfig struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"` // "stdio" or "http"
	Command   []string          `yaml:"command,omitempty"`
	Dir       string            `yaml:"dir,omitempty"`      // working directory for the stdio subprocess
	EnvFile   string            `yaml:"env_file,omitempty"` // .env-format file merged under Env
	Env       map[string]string `yaml:"env,omitempty"`
	SSH       *SSHConfig        `yaml:"ssh,omitempty"`    // if set, run the stdio Command on a remote host via ssh
	Docker    *DockerConfig     `yaml:"docker,omitempty"` // if set, run the stdio Command inside a container
	URL       string            `yaml:"url,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	Proxy     string            `yaml:"proxy,omitempty"` // proxy URL for http transport; "none" disables proxying even if HTTP_PROXY etc. are set; unset follows HTTP_PROXY/HTTPS_PROXY/NO_PROXY
	Prefix    string            `yaml:"prefix,omitempty"`
}

// SSHConfig describes how to reach the remote host a stdio backend's
// Command should be run on.
type SSHConfig struct {
	Host         string   `yaml:"host"` // required, e.g. "user@example.com"
	Port         int      `yaml:"port,omitempty"`
	IdentityFile string   `yaml:"identity_file,omitempty"` // passed as -i
	Args         []string `yaml:"args,omitempty"`          // extra ssh arguments, e.g. ["-J", "jumphost"]
}

// DockerConfig describes how to run a stdio backend's Command inside a
// container via a docker-compatible CLI.
type DockerConfig struct {
	Bin   string            `yaml:"bin,omitempty"`  // docker-compatible CLI to invoke; defaults to "docker" (e.g. "podman", "nerdctl")
	Image string            `yaml:"image"`          // required
	Args  []string          `yaml:"args,omitempty"` // extra arguments appended to "run", e.g. ["-v", "/data:/data"]
	Env   map[string]string `yaml:"env,omitempty"`  // env vars for the local CLI process itself (e.g. DOCKER_HOST), not the container; backends[].env is the container's env
}

// LoggingConfig controls audit-log behavior beyond what --log-level/--log-format
// (CLI flags) cover.
type LoggingConfig struct {
	// MaskKeys are extra case-insensitive substrings matched against
	// argument key names, in addition to the built-in defaultMaskKeyPatterns
	// ("key", "auth", "pass", "cred", "token") gateway.maskArguments uses.
	MaskKeys []string `yaml:"mask_keys,omitempty"`
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
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
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

	return nil
}
