package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wtnb75/mcprt/internal/config"

	"github.com/wtnb75/mcprt/internal/cli"
)

// TestInitAndImport_DefaultHTTPListenMatch checks that `mcprt init` and
// `mcprt import` generate the exact same default HTTP listen address --
// both are independent template generators for the same config.yaml shape,
// and a default hardcoded separately in each risks drifting apart if one is
// ever edited without the other.
func TestInitAndImport_DefaultHTTPListenMatch(t *testing.T) {
	initPath := filepath.Join(t.TempDir(), "init-config.yaml")
	if err := cli.Execute(context.Background(), []string{"init", initPath}); err != nil {
		t.Fatalf("Execute init: unexpected error: %v", err)
	}
	initData, err := os.ReadFile(initPath)
	if err != nil {
		t.Fatalf("reading init config: %v", err)
	}
	initCfg, err := config.Parse(initData)
	if err != nil {
		t.Fatalf("init config did not parse: %v\ncontent:\n%s", err, initData)
	}

	jsonPath := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(jsonPath, []byte(`{"mcpServers":{"fs":{"command":"npx","args":["-y","mcp-server-filesystem"]}}}`), 0o600); err != nil {
		t.Fatalf("writing mcp.json: %v", err)
	}
	importPath := filepath.Join(t.TempDir(), "import-config.yaml")
	root := cli.NewRootCmd()
	root.SetArgs([]string{"import", jsonPath, importPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute import: unexpected error: %v", err)
	}
	importData, err := os.ReadFile(importPath)
	if err != nil {
		t.Fatalf("reading import config: %v", err)
	}
	importCfg, err := config.Parse(importData)
	if err != nil {
		t.Fatalf("import config did not parse: %v\ncontent:\n%s", err, importData)
	}

	if initCfg.Listen.HTTP != importCfg.Listen.HTTP {
		t.Fatalf("mcprt init default HTTP listen = %q, mcprt import default HTTP listen = %q, want them to match (both should come from the same shared default)",
			initCfg.Listen.HTTP, importCfg.Listen.HTTP)
	}
}

func TestInitCommand_WritesParsableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	if err := cli.Execute(context.Background(), []string{"init", path}); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated config: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("generated config did not parse: %v\ncontent:\n%s", err, data)
	}
	if len(cfg.ResourceOverrides) == 0 {
		t.Fatalf("generated config has no resource_overrides example:\n%s", data)
	}
	if len(cfg.ResourceTemplateOverrides) == 0 {
		t.Fatalf("generated config has no resource_template_overrides example:\n%s", data)
	}
	if len(cfg.PromptOverrides) == 0 {
		t.Fatalf("generated config has no prompt_overrides example:\n%s", data)
	}
}

func TestInitCommand_DefaultPath(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	if err := cli.Execute(context.Background(), []string{"init"}); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatalf("expected config.yaml to be created: %v", err)
	}
}

func TestInitCommand_RefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("existing: true\n"), 0o600); err != nil {
		t.Fatalf("writing existing file: %v", err)
	}

	err := cli.Execute(context.Background(), []string{"init", path})
	if err == nil {
		t.Fatal("Execute: expected error when the output file already exists, got nil")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if string(data) != "existing: true\n" {
		t.Fatalf("existing file was overwritten: %s", data)
	}
}

func TestInitCommand_ForceOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("existing: true\n"), 0o600); err != nil {
		t.Fatalf("writing existing file: %v", err)
	}

	if err := cli.Execute(context.Background(), []string{"init", "--force", path}); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if _, err := config.Parse(data); err != nil {
		t.Fatalf("generated config did not parse: %v\ncontent:\n%s", err, data)
	}
}
