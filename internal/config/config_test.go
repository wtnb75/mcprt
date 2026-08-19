package config_test

import (
	"testing"

	"github.com/wtnb75/mcprt/internal/config"
)

func TestParse_ValidConfig(t *testing.T) {
	data := []byte(`
listen:
  stdio: true
  http: ":8080"

backends:
  - name: filesystem
    transport: stdio
    command: ["mcp-server-filesystem", "--root", "/data"]
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
	if fs.Name != "filesystem" || fs.Transport != "stdio" || len(fs.Command) != 3 || fs.Env["FOO"] != "bar" {
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
