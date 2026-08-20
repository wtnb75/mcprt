package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// envKeyRE matches a POSIX portable environment variable name. Backend env
// keys are interpolated unquoted into a shell "export NAME=..." statement
// when run over ssh (see backend.remoteScript), so a key outside this set
// could inject arbitrary shell syntax.
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Config is the top-level gateway configuration, loaded from a YAML file.
type Config struct {
	Listen    ListenConfig      `yaml:"listen"`
	Backends  []BackendConfig   `yaml:"backends"`
	Overrides map[string]string `yaml:"overrides"`
}

// ListenConfig controls which client-facing transports the gateway serves.
type ListenConfig struct {
	Stdio bool   `yaml:"stdio"`
	HTTP  string `yaml:"http"`
}

// BackendConfig describes one backend MCP server to connect to.
type BackendConfig struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"` // "stdio" or "http"
	Command   []string          `yaml:"command"`
	Dir       string            `yaml:"dir"`      // working directory for the stdio subprocess
	EnvFile   string            `yaml:"env_file"` // .env-format file merged under Env
	Env       map[string]string `yaml:"env"`
	SSH       *SSHConfig        `yaml:"ssh"` // if set, run the stdio Command on a remote host via ssh
	URL       string            `yaml:"url"`
	Headers   map[string]string `yaml:"headers"`
	Proxy     string            `yaml:"proxy"` // proxy URL for http transport; "none" disables proxying even if HTTP_PROXY etc. are set; unset follows HTTP_PROXY/HTTPS_PROXY/NO_PROXY
	Prefix    string            `yaml:"prefix"`
}

// SSHConfig describes how to reach the remote host a stdio backend's
// Command should be run on.
type SSHConfig struct {
	Host         string   `yaml:"host"` // required, e.g. "user@example.com"
	Port         int      `yaml:"port"`
	IdentityFile string   `yaml:"identity_file"` // passed as -i
	Args         []string `yaml:"args"`          // extra ssh arguments, e.g. ["-J", "jumphost"]
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
		for k, v := range b.Env {
			fileEnv[k] = v
		}
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

		if b.Proxy != "" && b.Proxy != "none" {
			if _, err := url.Parse(b.Proxy); err != nil {
				return fmt.Errorf("backend %q: invalid proxy url: %w", b.Name, err)
			}
		}

		switch b.Transport {
		case "stdio":
			if len(b.Command) == 0 {
				return fmt.Errorf("backend %q: stdio transport requires command", b.Name)
			}
		case "http":
			if b.URL == "" {
				return fmt.Errorf("backend %q: http transport requires url", b.Name)
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

	return nil
}
