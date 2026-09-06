package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wtnb75/mcprt/internal/cli"
	"github.com/wtnb75/mcprt/internal/config"
)

func writeJSON(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing mcp.json: %v", err)
	}
	return path
}

func TestImportCommand_MCPServersStdioAndHTTP(t *testing.T) {
	inPath := writeJSON(t, `{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "mcp-server-filesystem", "/data"],
      "env": {"FOO": "bar"}
    },
    "remote": {
      "type": "streamable-http",
      "url": "https://example.com/mcp",
      "headers": {"Authorization": "Bearer tok"}
    }
  }
}`)
	outPath := filepath.Join(t.TempDir(), "config.yaml")

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"import", inPath, outPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated config: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("generated config did not parse: %v\ncontent:\n%s", err, data)
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("Backends = %+v, want 2 entries", cfg.Backends)
	}

	byName := map[string]config.BackendConfig{}
	for _, b := range cfg.Backends {
		byName[b.Name] = b
	}

	fs := byName["filesystem"]
	if fs.Transport != "stdio" {
		t.Fatalf("filesystem.Transport = %q, want stdio", fs.Transport)
	}
	wantCommand := []string{"npx", "-y", "mcp-server-filesystem", "/data"}
	if len(fs.Command) != len(wantCommand) {
		t.Fatalf("filesystem.Command = %v, want %v", fs.Command, wantCommand)
	}
	for i, c := range wantCommand {
		if fs.Command[i] != c {
			t.Fatalf("filesystem.Command = %v, want %v", fs.Command, wantCommand)
		}
	}
	if fs.Env["FOO"] != "bar" {
		t.Fatalf("filesystem.Env = %v, want FOO=bar", fs.Env)
	}

	remote := byName["remote"]
	if remote.Transport != "http" {
		t.Fatalf("remote.Transport = %q, want http", remote.Transport)
	}
	if remote.URL != "https://example.com/mcp" {
		t.Fatalf("remote.URL = %q, want https://example.com/mcp", remote.URL)
	}
	if remote.Headers["Authorization"] != "Bearer tok" {
		t.Fatalf("remote.Headers = %v, want Authorization=Bearer tok", remote.Headers)
	}
}

func TestImportCommand_VSCodeServersKeyAndTypeInference(t *testing.T) {
	inPath := writeJSON(t, `{
  "servers": {
    "inferred-stdio": {"command": "uvx", "args": ["some-server"]},
    "inferred-http": {"url": "http://localhost:9000/mcp"}
  }
}`)
	outPath := filepath.Join(t.TempDir(), "config.yaml")

	if err := cli.Execute(context.Background(), []string{"import", inPath, outPath}); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated config: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("generated config did not parse: %v\ncontent:\n%s", err, data)
	}
	byName := map[string]config.BackendConfig{}
	for _, b := range cfg.Backends {
		byName[b.Name] = b
	}
	if byName["inferred-stdio"].Transport != "stdio" {
		t.Fatalf("inferred-stdio.Transport = %q, want stdio", byName["inferred-stdio"].Transport)
	}
	if byName["inferred-http"].Transport != "http" {
		t.Fatalf("inferred-http.Transport = %q, want http", byName["inferred-http"].Transport)
	}
}

func TestImportCommand_SSEWarning(t *testing.T) {
	inPath := writeJSON(t, `{
  "mcpServers": {
    "legacy": {"type": "sse", "url": "http://localhost:9000/sse"}
  }
}`)
	outPath := filepath.Join(t.TempDir(), "config.yaml")

	var errOut bytes.Buffer
	root := cli.NewRootCmd()
	root.SetErr(&errOut)
	root.SetArgs([]string{"import", inPath, outPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if !strings.Contains(errOut.String(), "sse") {
		t.Fatalf("stderr = %q, want an sse-transport warning", errOut.String())
	}
}

func TestImportCommand_DuplicateNameSkipped(t *testing.T) {
	inPath := writeJSON(t, `{
  "mcpServers": {"dup": {"command": "a"}},
  "servers": {"dup": {"command": "b"}}
}`)
	outPath := filepath.Join(t.TempDir(), "config.yaml")

	var errOut bytes.Buffer
	root := cli.NewRootCmd()
	root.SetErr(&errOut)
	root.SetArgs([]string{"import", inPath, outPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if !strings.Contains(errOut.String(), "duplicate") {
		t.Fatalf("stderr = %q, want a duplicate-name warning", errOut.String())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated config: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("generated config did not parse: %v", err)
	}
	if len(cfg.Backends) != 1 {
		t.Fatalf("Backends = %+v, want exactly 1 (the duplicate must be skipped)", cfg.Backends)
	}
}

func TestImportCommand_UnconvertibleEntrySkipped(t *testing.T) {
	inPath := writeJSON(t, `{
  "mcpServers": {
    "useless": {}
  }
}`)
	outPath := filepath.Join(t.TempDir(), "config.yaml")

	err := cli.Execute(context.Background(), []string{"import", inPath, outPath})
	if err == nil {
		t.Fatal("Execute: expected error when no backend could be imported, got nil")
	}
	if _, statErr := os.Stat(outPath); statErr == nil {
		t.Fatal("Execute: output file should not be written when nothing was imported")
	}
}

func TestImportCommand_UnresolvablePlaceholderWarning(t *testing.T) {
	inPath := writeJSON(t, `{
  "mcpServers": {
    "vscode-style": {
      "command": "node",
      "args": ["server.js"],
      "env": {"TOKEN": "${env:MY_TOKEN}", "ROOT": "${workspaceFolder}"}
    }
  }
}`)
	outPath := filepath.Join(t.TempDir(), "config.yaml")

	var errOut bytes.Buffer
	root := cli.NewRootCmd()
	root.SetErr(&errOut)
	root.SetArgs([]string{"import", inPath, outPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if !strings.Contains(errOut.String(), "workspaceFolder") {
		t.Fatalf("stderr = %q, want a warning naming the unresolvable placeholder", errOut.String())
	}
	if strings.Contains(errOut.String(), "MY_TOKEN") {
		t.Fatalf("stderr = %q, want no warning about TOKEN: ${env:...} translates to mcprt's own resolvable ${VAR} syntax", errOut.String())
	}

	// config.Parse expands "${VAR}" against the process environment at load
	// time (see config.expandEnvRefs), so asserting through it here would
	// just observe that expansion instead of what import actually wrote.
	// Check the raw YAML bytes instead.
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated config: %v", err)
	}
	if !strings.Contains(string(data), "${MY_TOKEN}") {
		t.Fatalf("generated config = %s, want it to contain ${MY_TOKEN} (translated from ${env:MY_TOKEN})", data)
	}
	if !strings.Contains(string(data), "${workspaceFolder}") {
		t.Fatalf("generated config = %s, want it to contain ${workspaceFolder} left as-is", data)
	}
}

func TestImportCommand_NoServersKey(t *testing.T) {
	inPath := writeJSON(t, `{"foo": "bar"}`)
	outPath := filepath.Join(t.TempDir(), "config.yaml")

	err := cli.Execute(context.Background(), []string{"import", inPath, outPath})
	if err == nil {
		t.Fatal("Execute: expected error when neither mcpServers nor servers is present, got nil")
	}
}

func TestImportCommand_RefusesToOverwrite(t *testing.T) {
	inPath := writeJSON(t, `{"mcpServers": {"a": {"command": "true"}}}`)
	outPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(outPath, []byte("existing: true\n"), 0o600); err != nil {
		t.Fatalf("writing existing file: %v", err)
	}

	err := cli.Execute(context.Background(), []string{"import", inPath, outPath})
	if err == nil {
		t.Fatal("Execute: expected error when the output file already exists, got nil")
	}

	if err := cli.Execute(context.Background(), []string{"import", "--force", inPath, outPath}); err != nil {
		t.Fatalf("Execute with --force: unexpected error: %v", err)
	}
}

func TestImportCommand_NoOutputPathWritesToStdout(t *testing.T) {
	inPath := writeJSON(t, `{"mcpServers": {"a": {"command": "true"}}}`)
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	var out, errOut bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"import", inPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	cfg, err := config.Parse(out.Bytes())
	if err != nil {
		t.Fatalf("stdout did not parse as a config: %v\nstdout:\n%s", err, out.String())
	}
	if len(cfg.Backends) != 1 || cfg.Backends[0].Name != "a" {
		t.Fatalf("stdout Backends = %+v, want a single backend named \"a\"", cfg.Backends)
	}
	if !strings.Contains(errOut.String(), "imported") {
		t.Fatalf("stderr = %q, want the imported-count status message", errOut.String())
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		t.Fatal("Execute: config.yaml should not be written to the working directory when no output path is given")
	}
}
