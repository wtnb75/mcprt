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

func TestPingCommand_AllBackends_Text(t *testing.T) {
	backendA := newFakeToolBackend("backend-a", "search")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
  - name: backend-b
    transport: http
    url: "http://127.0.0.1:1/mcp"
`, backendA.URL))

	root := cli.NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ping", "--config", configPath})

	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("Execute: expected error because backend-b is unreachable, got nil")
	}

	got := out.String()
	if !strings.Contains(got, "backend-a") || !strings.Contains(got, "ok") {
		t.Fatalf("text output = %q, want it to report backend-a as ok", got)
	}
	if !strings.Contains(got, "backend-b") || !strings.Contains(got, "error") {
		t.Fatalf("text output = %q, want it to report backend-b as error", got)
	}
}

func TestPingCommand_AllBackends_JSON(t *testing.T) {
	backendA := newFakeToolBackend("backend-a", "search")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
  - name: backend-b
    transport: http
    url: "http://127.0.0.1:1/mcp"
`, backendA.URL))

	root := cli.NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ping", "--config", configPath, "--json"})

	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("Execute: expected error because backend-b is unreachable, got nil")
	}

	var parsed struct {
		Backends []struct {
			Backend string `json:"backend"`
			OK      bool   `json:"ok"`
			Tools   []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Error string `json:"error"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal JSON output: %v\noutput: %s", err, out.String())
	}
	if len(parsed.Backends) != 2 {
		t.Fatalf("Backends = %+v, want 2 entries", parsed.Backends)
	}

	byName := map[string]bool{}
	for _, b := range parsed.Backends {
		byName[b.Backend] = b.OK
	}
	if ok, found := byName["backend-a"]; !found || !ok {
		t.Fatalf("backend-a entry = %v, want ok=true", byName)
	}
	if ok, found := byName["backend-b"]; !found || ok {
		t.Fatalf("backend-b entry = %v, want ok=false", byName)
	}
}

func TestPingCommand_AllBackends_NoneConfigured(t *testing.T) {
	configPath := writeConfig(t, `backends: []`)

	err := cli.Execute(context.Background(), []string{"ping", "--config", configPath})
	if err == nil {
		t.Fatal("Execute: expected error when no backends are configured, got nil")
	}
}
