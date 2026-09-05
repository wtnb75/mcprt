package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wtnb75/mcprt/internal/config"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func TestParse_ValidConfig(t *testing.T) {
	data := []byte(`
listen:
  stdio: true
  http: ":8080"

backends:
  - name: filesystem
    transport: stdio
    command: ["mcp-server-filesystem", "--root", "/data"]
    dir: /data
    env:
      FOO: bar
  - name: github
    transport: http
    url: "http://localhost:9090/mcp"
    headers:
      Authorization: "Bearer ${TEST_GITHUB_TOKEN}"
    prefix: "gh__"

overrides:
  gh__search: github
`)
	t.Setenv("TEST_GITHUB_TOKEN", "secret123")

	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if !cfg.Listen.Stdio || cfg.Listen.HTTP != ":8080" {
		t.Fatalf("Listen = %+v, want stdio=true http=\":8080\"", cfg.Listen)
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("len(Backends) = %d, want 2", len(cfg.Backends))
	}
	fs := cfg.Backends[0]
	if fs.Name != "filesystem" || fs.Transport != "stdio" || len(fs.Command) != 3 || fs.Dir != "/data" || fs.Env["FOO"] != "bar" {
		t.Fatalf("Backends[0] = %+v, unexpected", fs)
	}
	gh := cfg.Backends[1]
	if gh.Prefix != "gh__" || gh.Headers["Authorization"] != "Bearer secret123" {
		t.Fatalf("Backends[1] = %+v, want expanded Authorization header", gh)
	}
	if cfg.Overrides["gh__search"] != "github" {
		t.Fatalf("Overrides[gh__search] = %q, want %q", cfg.Overrides["gh__search"], "github")
	}
}

func TestParse_DuplicateBackendName(t *testing.T) {
	data := []byte(`
backends:
  - name: dup
    transport: stdio
    command: ["a"]
  - name: dup
    transport: stdio
    command: ["b"]
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for duplicate backend name, got nil")
	}
}

func TestParse_UnknownTransport(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: carrier-pigeon
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for unknown transport, got nil")
	}
}

func TestParse_StdioMissingCommand(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: stdio
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for stdio backend with no command, got nil")
	}
}

func TestParse_HTTPMissingURL(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: http
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for http backend with no url, got nil")
	}
}

func TestParse_StdioWithURLSetErrors(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: stdio
    command: ["a"]
    url: "http://localhost:9090/mcp"
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for stdio backend with url set (likely a stale transport left over from an http->stdio migration mistake), got nil")
	}
}

func TestParse_StdioWithHeadersSetErrors(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: stdio
    command: ["a"]
    headers:
      Authorization: "Bearer xyz"
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for stdio backend with headers set, got nil")
	}
}

func TestParse_StdioWithProxySetErrors(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: stdio
    command: ["a"]
    proxy: "http://proxy.example.com:8080"
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for stdio backend with proxy set, got nil")
	}
}

func TestParse_HTTPWithCommandSetErrors(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: http
    url: "http://localhost:9090/mcp"
    command: ["a"]
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for http backend with command set (likely a stale transport left over from a stdio->http migration mistake), got nil")
	}
}

func TestParse_HTTPWithDirSetErrors(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: http
    url: "http://localhost:9090/mcp"
    dir: "/some/dir"
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for http backend with dir set, got nil")
	}
}

func TestParse_HTTPWithEnvSetErrors(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: http
    url: "http://localhost:9090/mcp"
    env:
      FOO: bar
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for http backend with env set, got nil")
	}
}

func TestParse_EnvFile(t *testing.T) {
	// BAR references FOO within the file itself: godotenv expands ${VAR}
	// refs against vars already parsed from the same file, not the host
	// process environment.
	envFile := filepath.Join(t.TempDir(), ".env")
	writeFile(t, envFile, "FOO=from-file\nBAR=${FOO}\n")

	data := fmt.Appendf(nil, `
backends:
  - name: filesystem
    transport: stdio
    command: ["a"]
    env_file: %q
    env:
      FOO: from-config
`, envFile)

	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	env := cfg.Backends[0].Env
	if env["FOO"] != "from-config" {
		t.Fatalf("Env[FOO] = %q, want %q (explicit env: should win over env_file)", env["FOO"], "from-config")
	}
	if env["BAR"] != "from-file" {
		t.Fatalf("Env[BAR] = %q, want %q (env_file's own ${VAR} refs resolve within the file)", env["BAR"], "from-file")
	}
}

func TestParse_EnvFileNotDoubleExpanded(t *testing.T) {
	// godotenv's own escape syntax: "\$HOME" in the file resolves to the
	// literal string "$HOME" (not expanded within the file). That value
	// must not then be expanded a second time against the host process's
	// environment.
	envFile := filepath.Join(t.TempDir(), ".env")
	writeFile(t, envFile, `MSG=\$HOME`+"\n")
	t.Setenv("HOME", "/should/not/leak")

	data := fmt.Appendf(nil, `
backends:
  - name: filesystem
    transport: stdio
    command: ["a"]
    env_file: %q
`, envFile)

	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := cfg.Backends[0].Env["MSG"], "$HOME"; got != want {
		t.Fatalf("Env[MSG] = %q, want %q (env_file value must not be re-expanded against the host env)", got, want)
	}
}

func TestParse_EnvInvalidKey(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: stdio
    command: ["a"]
    env:
      "FOO; rm -rf /": bar
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for invalid env key, got nil")
	}
}

func TestParse_EnvFileMissing(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: stdio
    command: ["a"]
    env_file: /nonexistent/does-not-exist.env
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for missing env_file, got nil")
	}
}

func TestParse_SSH(t *testing.T) {
	data := []byte(`
backends:
  - name: remote
    transport: stdio
    command: ["mcp-server-filesystem"]
    ssh:
      host: user@example.com
      port: 2222
      identity_file: /home/me/.ssh/id_ed25519
      args: ["-o", "StrictHostKeyChecking=no"]
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ssh := cfg.Backends[0].SSH
	if ssh == nil {
		t.Fatal("Backends[0].SSH = nil, want non-nil")
	}
	if ssh.Host != "user@example.com" || ssh.Port != 2222 || ssh.IdentityFile != "/home/me/.ssh/id_ed25519" || len(ssh.Args) != 2 {
		t.Fatalf("Backends[0].SSH = %+v, unexpected", ssh)
	}
}

func TestParse_SSHMissingHost(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: stdio
    command: ["a"]
    ssh: {}
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for ssh with no host, got nil")
	}
}

func TestParse_SSHPortOutOfRange(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: stdio
    command: ["a"]
    ssh:
      host: user@example.com
      port: 70000
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for ssh port out of range, got nil")
	}
}

func TestParse_SSHRequiresStdioTransport(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: http
    url: "http://localhost:9090/mcp"
    ssh:
      host: user@example.com
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for ssh on a non-stdio transport, got nil")
	}
}

func TestParse_Docker(t *testing.T) {
	t.Setenv("TEST_DOCKER_HOST", "ssh://user@example.com")
	data := []byte(`
backends:
  - name: containerized
    transport: stdio
    command: ["mcp-server-filesystem"]
    docker:
      bin: podman
      image: your/mcp-image
      args: ["-v", "/data:/data"]
      env:
        DOCKER_HOST: "${TEST_DOCKER_HOST}"
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d := cfg.Backends[0].Docker
	if d == nil {
		t.Fatal("Backends[0].Docker = nil, want non-nil")
	}
	if d.Bin != "podman" || d.Image != "your/mcp-image" || len(d.Args) != 2 || d.Env["DOCKER_HOST"] != "ssh://user@example.com" {
		t.Fatalf("Backends[0].Docker = %+v, unexpected", d)
	}
}

func TestParse_DockerMissingImage(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: stdio
    command: ["a"]
    docker: {}
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for docker with no image, got nil")
	}
}

func TestParse_DockerRequiresStdioTransport(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: http
    url: "http://localhost:9090/mcp"
    docker:
      image: your/mcp-image
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for docker on a non-stdio transport, got nil")
	}
}

func TestParse_DockerAndSSHMutuallyExclusive(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: stdio
    command: ["a"]
    ssh:
      host: user@example.com
    docker:
      image: your/mcp-image
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for ssh and docker both set, got nil")
	}
}

func TestParse_DockerEnvInvalidKey(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: stdio
    command: ["a"]
    docker:
      image: your/mcp-image
      env:
        "not valid": x
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for invalid docker env key, got nil")
	}
}

func TestParse_Proxy(t *testing.T) {
	t.Setenv("TEST_PROXY_TOKEN", "secret")
	data := []byte(`
backends:
  - name: proxied
    transport: http
    url: "http://localhost:9090/mcp"
    proxy: "http://user:${TEST_PROXY_TOKEN}@proxy.example.com:8080"
  - name: direct
    transport: http
    url: "http://localhost:9091/mcp"
    proxy: "none"
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := cfg.Backends[0].Proxy, "http://user:secret@proxy.example.com:8080"; got != want {
		t.Fatalf("Backends[0].Proxy = %q, want %q (expanded)", got, want)
	}
	if got, want := cfg.Backends[1].Proxy, "none"; got != want {
		t.Fatalf("Backends[1].Proxy = %q, want %q", got, want)
	}
}

func TestParse_ProxyInvalidURL(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: http
    url: "http://localhost:9090/mcp"
    proxy: "://not-a-url"
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for invalid proxy url, got nil")
	}
}

func TestParse_OverrideReferencesUnknownBackend(t *testing.T) {
	data := []byte(`
backends:
  - name: known
    transport: stdio
    command: ["a"]

overrides:
  search: nonexistent
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for override referencing unknown backend, got nil")
	}
}

func TestParse_ResourceOverrides(t *testing.T) {
	data := []byte(`
backends:
  - name: filesystem-primary
    transport: stdio
    command: ["a"]
  - name: filesystem-secondary
    transport: stdio
    command: ["b"]

resource_overrides:
  "file:///data/README.md": filesystem-primary

resource_template_overrides:
  "file:///data/{path}": filesystem-primary
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ResourceOverrides["file:///data/README.md"] != "filesystem-primary" {
		t.Fatalf("ResourceOverrides[...] = %q, want %q", cfg.ResourceOverrides["file:///data/README.md"], "filesystem-primary")
	}
	if cfg.ResourceTemplateOverrides["file:///data/{path}"] != "filesystem-primary" {
		t.Fatalf("ResourceTemplateOverrides[...] = %q, want %q", cfg.ResourceTemplateOverrides["file:///data/{path}"], "filesystem-primary")
	}
}

func TestParse_ResourceOverrideReferencesUnknownBackend(t *testing.T) {
	data := []byte(`
backends:
  - name: known
    transport: stdio
    command: ["a"]

resource_overrides:
  "file:///data/README.md": nonexistent
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for resource_overrides referencing unknown backend, got nil")
	}
}

func TestParse_ResourceTemplateOverrideReferencesUnknownBackend(t *testing.T) {
	data := []byte(`
backends:
  - name: known
    transport: stdio
    command: ["a"]

resource_template_overrides:
  "file:///data/{path}": nonexistent
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for resource_template_overrides referencing unknown backend, got nil")
	}
}

func TestParse_PromptOverrides(t *testing.T) {
	data := []byte(`
backends:
  - name: linter
    transport: stdio
    command: ["a"]
  - name: linter-strict
    transport: stdio
    command: ["b"]

prompt_overrides:
  code-review: linter-strict
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.PromptOverrides["code-review"] != "linter-strict" {
		t.Fatalf("PromptOverrides[...] = %q, want %q", cfg.PromptOverrides["code-review"], "linter-strict")
	}
}

func TestParse_PromptOverrideReferencesUnknownBackend(t *testing.T) {
	data := []byte(`
backends:
  - name: known
    transport: stdio
    command: ["a"]

prompt_overrides:
  code-review: nonexistent
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for prompt_overrides referencing unknown backend, got nil")
	}
}

func TestParse_LoggingMaskKeys(t *testing.T) {
	data := []byte(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

logging:
  mask_keys: ["internal_id", "session_token"]
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Logging.MaskKeys) != 2 || cfg.Logging.MaskKeys[0] != "internal_id" || cfg.Logging.MaskKeys[1] != "session_token" {
		t.Fatalf("Logging.MaskKeys = %v, want [internal_id session_token]", cfg.Logging.MaskKeys)
	}
}

func TestParse_LoggingMaskKeysDefaultEmpty(t *testing.T) {
	data := []byte(`
backends:
  - name: a
    transport: stdio
    command: ["x"]
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Logging.MaskKeys) != 0 {
		t.Fatalf("Logging.MaskKeys = %v, want empty", cfg.Logging.MaskKeys)
	}
}
