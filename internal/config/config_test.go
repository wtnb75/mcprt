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

func TestParse_EnvFile(t *testing.T) {
	// BAR references FOO within the file itself: godotenv expands ${VAR}
	// refs against vars already parsed from the same file, not the host
	// process environment.
	envFile := filepath.Join(t.TempDir(), ".env")
	writeFile(t, envFile, "FOO=from-file\nBAR=${FOO}\n")

	data := []byte(fmt.Sprintf(`
backends:
  - name: filesystem
    transport: stdio
    command: ["a"]
    env_file: %q
    env:
      FOO: from-config
`, envFile))

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
