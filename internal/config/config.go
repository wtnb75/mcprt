package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

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
	URL       string            `yaml:"url"`
	Headers   map[string]string `yaml:"headers"`
	Prefix    string            `yaml:"prefix"`
}

// Load reads and parses the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	return Parse(data)
}

// Parse parses YAML config data, expands ${VAR} references in backend env
// and header values, and validates the result.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}
	if err := mergeEnvFiles(&cfg); err != nil {
		return nil, err
	}
	expandEnvRefs(&cfg)
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
