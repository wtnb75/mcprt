package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wtnb75/mcprt/internal/cli"
)

func writeExportConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing config.yaml: %v", err)
	}
	return path
}

type exportedServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type exportedFile struct {
	MCPServers map[string]exportedServer `json:"mcpServers"`
}

func TestExportCommand_StdioAndHTTP(t *testing.T) {
	configPath := writeExportConfig(t, `
listen:
  stdio: true
backends:
  - name: filesystem
    transport: stdio
    command: ["npx", "-y", "mcp-server-filesystem", "/data"]
    dir: /work
    env:
      FOO: bar
  - name: remote
    transport: http
    url: https://example.com/mcp
    headers:
      Authorization: Bearer tok
`)
	outPath := filepath.Join(t.TempDir(), "mcp.json")

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"export", "--config", configPath, outPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated mcp.json: %v", err)
	}
	var got exportedFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("generated mcp.json did not parse: %v\ncontent:\n%s", err, data)
	}
	if len(got.MCPServers) != 2 {
		t.Fatalf("MCPServers = %+v, want 2 entries", got.MCPServers)
	}

	fs := got.MCPServers["filesystem"]
	if fs.Command != "npx" {
		t.Fatalf("filesystem.Command = %q, want npx", fs.Command)
	}
	wantArgs := []string{"-y", "mcp-server-filesystem", "/data"}
	if len(fs.Args) != len(wantArgs) {
		t.Fatalf("filesystem.Args = %v, want %v", fs.Args, wantArgs)
	}
	for i, a := range wantArgs {
		if fs.Args[i] != a {
			t.Fatalf("filesystem.Args = %v, want %v", fs.Args, wantArgs)
		}
	}
	if fs.Cwd != "/work" {
		t.Fatalf("filesystem.Cwd = %q, want /work", fs.Cwd)
	}
	if fs.Env["FOO"] != "bar" {
		t.Fatalf("filesystem.Env = %v, want FOO=bar", fs.Env)
	}

	remote := got.MCPServers["remote"]
	if remote.Type != "streamable-http" {
		t.Fatalf("remote.Type = %q, want streamable-http", remote.Type)
	}
	if remote.URL != "https://example.com/mcp" {
		t.Fatalf("remote.URL = %q, want https://example.com/mcp", remote.URL)
	}
	if remote.Headers["Authorization"] != "Bearer tok" {
		t.Fatalf("remote.Headers = %v, want Authorization=Bearer tok", remote.Headers)
	}
}

func TestExportCommand_VarPlaceholderTranslatedNotExpanded(t *testing.T) {
	t.Setenv("MY_TOKEN", "super-secret-value")
	configPath := writeExportConfig(t, `
backends:
  - name: srv
    transport: stdio
    command: ["node", "server.js"]
    env:
      TOKEN: "${MY_TOKEN}"
`)
	outPath := filepath.Join(t.TempDir(), "mcp.json")

	if err := cli.Execute(context.Background(), []string{"export", "--config", configPath, outPath}); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated mcp.json: %v", err)
	}
	if strings.Contains(string(data), "super-secret-value") {
		t.Fatalf("generated mcp.json leaked the expanded secret value: %s", data)
	}
	if !strings.Contains(string(data), "${env:MY_TOKEN}") {
		t.Fatalf("generated mcp.json = %s, want it to contain ${env:MY_TOKEN} (translated from ${MY_TOKEN})", data)
	}
}

func TestExportCommand_SSHBackendWarnedAndSkipped(t *testing.T) {
	configPath := writeExportConfig(t, `
backends:
  - name: remote-ssh
    transport: stdio
    command: ["mcp-server"]
    ssh:
      host: example.com
  - name: local
    transport: stdio
    command: ["mcp-server"]
`)
	outPath := filepath.Join(t.TempDir(), "mcp.json")

	var errOut bytes.Buffer
	root := cli.NewRootCmd()
	root.SetErr(&errOut)
	root.SetArgs([]string{"export", "--config", configPath, outPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if !strings.Contains(errOut.String(), "remote-ssh") || !strings.Contains(errOut.String(), "ssh") {
		t.Fatalf("stderr = %q, want a warning naming the skipped ssh backend", errOut.String())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated mcp.json: %v", err)
	}
	var got exportedFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("generated mcp.json did not parse: %v", err)
	}
	if len(got.MCPServers) != 1 {
		t.Fatalf("MCPServers = %+v, want exactly 1 (ssh backend must be skipped)", got.MCPServers)
	}
	if _, ok := got.MCPServers["remote-ssh"]; ok {
		t.Fatalf("MCPServers = %+v, remote-ssh must not be present", got.MCPServers)
	}
}

func TestExportCommand_EnvFileWarnedNotInlined(t *testing.T) {
	configPath := writeExportConfig(t, `
backends:
  - name: srv
    transport: stdio
    command: ["node", "server.js"]
    env_file: /does/not/need/to/exist.env
    env:
      FOO: bar
`)
	outPath := filepath.Join(t.TempDir(), "mcp.json")

	var errOut bytes.Buffer
	root := cli.NewRootCmd()
	root.SetErr(&errOut)
	root.SetArgs([]string{"export", "--config", configPath, outPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if !strings.Contains(errOut.String(), "env_file") {
		t.Fatalf("stderr = %q, want a warning about env_file not being included", errOut.String())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated mcp.json: %v", err)
	}
	var got exportedFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("generated mcp.json did not parse: %v", err)
	}
	if got.MCPServers["srv"].Env["FOO"] != "bar" {
		t.Fatalf("MCPServers[srv].Env = %v, want FOO=bar to still be present", got.MCPServers["srv"].Env)
	}
}

func TestExportCommand_ProxyPrefixOverridesWarnedAndDropped(t *testing.T) {
	configPath := writeExportConfig(t, `
backends:
  - name: srv
    transport: http
    url: https://example.com/mcp
    proxy: http://proxy.example.com:8080
    prefix: srv_
overrides:
  some_tool: srv
`)
	outPath := filepath.Join(t.TempDir(), "mcp.json")

	var errOut bytes.Buffer
	root := cli.NewRootCmd()
	root.SetErr(&errOut)
	root.SetArgs([]string{"export", "--config", configPath, outPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if !strings.Contains(errOut.String(), "proxy") {
		t.Fatalf("stderr = %q, want a warning about proxy being dropped", errOut.String())
	}
	if !strings.Contains(errOut.String(), "prefix") {
		t.Fatalf("stderr = %q, want a warning about prefix being dropped", errOut.String())
	}
	if !strings.Contains(errOut.String(), "override") {
		t.Fatalf("stderr = %q, want a warning about overrides being dropped", errOut.String())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated mcp.json: %v", err)
	}
	if strings.Contains(string(data), "proxy") || strings.Contains(string(data), "prefix") {
		t.Fatalf("generated mcp.json = %s, must not contain proxy/prefix fields", data)
	}
}

func TestExportCommand_ResourceOverridesWarnedAndDropped(t *testing.T) {
	configPath := writeExportConfig(t, `
backends:
  - name: srv
    transport: http
    url: https://example.com/mcp
resource_overrides:
  "file:///data.txt": srv
resource_template_overrides:
  "file:///{path}": srv
`)
	outPath := filepath.Join(t.TempDir(), "mcp.json")

	var errOut bytes.Buffer
	root := cli.NewRootCmd()
	root.SetErr(&errOut)
	root.SetArgs([]string{"export", "--config", configPath, outPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if !strings.Contains(errOut.String(), "resource_overrides") {
		t.Fatalf("stderr = %q, want a warning about resource_overrides being dropped", errOut.String())
	}
	if !strings.Contains(errOut.String(), "resource_template_overrides") {
		t.Fatalf("stderr = %q, want a warning about resource_template_overrides being dropped", errOut.String())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated mcp.json: %v", err)
	}
	if strings.Contains(string(data), "resource_overrides") || strings.Contains(string(data), "resource_template_overrides") {
		t.Fatalf("generated mcp.json = %s, must not contain resource_overrides/resource_template_overrides fields", data)
	}
}

func TestExportCommand_PromptOverridesWarnedAndDropped(t *testing.T) {
	configPath := writeExportConfig(t, `
backends:
  - name: srv
    transport: http
    url: https://example.com/mcp
prompt_overrides:
  code-review: srv
`)
	outPath := filepath.Join(t.TempDir(), "mcp.json")

	var errOut bytes.Buffer
	root := cli.NewRootCmd()
	root.SetErr(&errOut)
	root.SetArgs([]string{"export", "--config", configPath, outPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if !strings.Contains(errOut.String(), "prompt_overrides") {
		t.Fatalf("stderr = %q, want a warning about prompt_overrides being dropped", errOut.String())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated mcp.json: %v", err)
	}
	if strings.Contains(string(data), "prompt_overrides") {
		t.Fatalf("generated mcp.json = %s, must not contain a prompt_overrides field", data)
	}
}

func TestExportCommand_NoBackendsExported(t *testing.T) {
	configPath := writeExportConfig(t, `
backends:
  - name: remote-ssh
    transport: stdio
    command: ["mcp-server"]
    ssh:
      host: example.com
`)
	outPath := filepath.Join(t.TempDir(), "mcp.json")

	err := cli.Execute(context.Background(), []string{"export", "--config", configPath, outPath})
	if err == nil {
		t.Fatal("Execute: expected error when no backend could be exported, got nil")
	}
	if _, statErr := os.Stat(outPath); statErr == nil {
		t.Fatal("Execute: output file should not be written when nothing was exported")
	}
}

func TestExportCommand_RefusesToOverwrite(t *testing.T) {
	configPath := writeExportConfig(t, `
backends:
  - name: srv
    transport: stdio
    command: ["true"]
`)
	outPath := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(outPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("writing existing file: %v", err)
	}

	err := cli.Execute(context.Background(), []string{"export", "--config", configPath, outPath})
	if err == nil {
		t.Fatal("Execute: expected error when the output file already exists, got nil")
	}

	if err := cli.Execute(context.Background(), []string{"export", "--config", configPath, "--force", outPath}); err != nil {
		t.Fatalf("Execute with --force: unexpected error: %v", err)
	}
}

func TestExportCommand_DefaultOutputPath(t *testing.T) {
	configPath := writeExportConfig(t, `
backends:
  - name: srv
    transport: stdio
    command: ["true"]
`)
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	if err := cli.Execute(context.Background(), []string{"export", "--config", configPath}); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mcp.json")); err != nil {
		t.Fatalf("expected mcp.json to be created: %v", err)
	}
}
