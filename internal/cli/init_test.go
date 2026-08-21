package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wtnb75/mcprt/internal/config"

	"github.com/wtnb75/mcprt/internal/cli"
)

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
