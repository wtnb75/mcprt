package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wtnb75/mcprt/internal/cli"
)

func TestPingCommand_Text(t *testing.T) {
	backendA := newFakeToolBackend("backend-a", "search")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	root := cli.NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ping", "--config", configPath, "backend-a"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "backend-a") || !strings.Contains(got, "search") {
		t.Fatalf("text output = %q, want it to mention backend %q and tool %q", got, "backend-a", "search")
	}
}

func TestPingCommand_JSON(t *testing.T) {
	backendA := newFakeToolBackend("backend-a", "search")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	root := cli.NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ping", "--config", configPath, "--json", "backend-a"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	var parsed struct {
		Backend string `json:"backend"`
		Tools   []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal JSON output: %v\noutput: %s", err, out.String())
	}
	if parsed.Backend != "backend-a" {
		t.Fatalf("Backend = %q, want %q", parsed.Backend, "backend-a")
	}
	if len(parsed.Tools) != 1 || parsed.Tools[0].Name != "search" {
		t.Fatalf("Tools = %+v, want one entry named %q", parsed.Tools, "search")
	}
}

func TestPingCommand_UnknownBackend(t *testing.T) {
	configPath := writeConfig(t, `
backends:
  - name: backend-a
    transport: stdio
    command: ["true"]
`)

	err := cli.Execute(context.Background(), []string{"ping", "--config", configPath, "does-not-exist"})
	if err == nil {
		t.Fatal("Execute: expected error for unknown backend name, got nil")
	}
}

func TestPingCommand_ConnectFailure(t *testing.T) {
	configPath := writeConfig(t, `
backends:
  - name: backend-a
    transport: http
    url: "http://127.0.0.1:1/mcp"
`)

	err := cli.Execute(context.Background(), []string{"ping", "--config", configPath, "backend-a"})
	if err == nil {
		t.Fatal("Execute: expected error when the backend is unreachable, got nil")
	}
}
