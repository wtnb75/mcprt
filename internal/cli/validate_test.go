package cli_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/wtnb75/mcprt/internal/cli"
)

func TestValidateCommand_ValidConfig(t *testing.T) {
	configPath := writeConfig(t, `
listen:
  stdio: true

backends:
  - name: fake
    transport: stdio
    command: ["true"]
`)

	root := cli.NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"validate", "--config", configPath})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("Execute: expected a success message on stdout, got none")
	}
}

func TestValidateCommand_DuplicateBackendName(t *testing.T) {
	configPath := writeConfig(t, `
backends:
  - name: fake
    transport: stdio
    command: ["true"]
  - name: fake
    transport: stdio
    command: ["true"]
`)

	err := cli.Execute(context.Background(), []string{"validate", "--config", configPath})
	if err == nil {
		t.Fatal("Execute: expected error for duplicate backend name, got nil")
	}
}

func TestValidateCommand_OverrideReferencesUnknownBackend(t *testing.T) {
	configPath := writeConfig(t, `
backends:
  - name: fake
    transport: stdio
    command: ["true"]

overrides:
  search: nonexistent
`)

	err := cli.Execute(context.Background(), []string{"validate", "--config", configPath})
	if err == nil {
		t.Fatal("Execute: expected error for override referencing unknown backend, got nil")
	}
}

func TestValidateCommand_MissingConfigFile(t *testing.T) {
	err := cli.Execute(context.Background(), []string{"validate", "--config", "/nonexistent/config.yaml"})
	if err == nil {
		t.Fatal("Execute: expected error for missing config file, got nil")
	}
}
