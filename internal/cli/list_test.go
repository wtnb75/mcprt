package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/cli"
)

func newFakeToolBackend(name string, toolNames ...string) *httptest.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, nil)
	for _, toolName := range toolNames {
		mcp.AddTool(srv, &mcp.Tool{Name: toolName, Description: "fake tool " + toolName},
			func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
				return nil, struct{}{}, nil
			})
	}
	return httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
}

func TestListCommand_Text(t *testing.T) {
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
	root.SetArgs([]string{"list", "--config", configPath})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "search") || !strings.Contains(got, "backend-a") {
		t.Fatalf("text output = %q, want it to mention tool %q and backend %q", got, "search", "backend-a")
	}
}

func TestListCommand_JSON(t *testing.T) {
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
	root.SetArgs([]string{"list", "--config", configPath, "--json"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	var parsed struct {
		Tools []struct {
			Name         string `json:"name"`
			Backend      string `json:"backend"`
			OriginalName string `json:"original_name"`
		} `json:"tools"`
		Conflicts []struct {
			Name   string   `json:"name"`
			Winner string   `json:"winner"`
			Hidden []string `json:"hidden"`
		} `json:"conflicts"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal JSON output: %v\noutput: %s", err, out.String())
	}
	if len(parsed.Tools) != 1 || parsed.Tools[0].Name != "search" || parsed.Tools[0].Backend != "backend-a" || parsed.Tools[0].OriginalName != "search" {
		t.Fatalf("Tools = %+v, want one entry {search, backend-a, search}", parsed.Tools)
	}
	if len(parsed.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none", parsed.Conflicts)
	}
}

func TestListCommand_ReportsConflict(t *testing.T) {
	backendA := newFakeToolBackend("backend-a", "search")
	defer backendA.Close()
	backendB := newFakeToolBackend("backend-b", "search")
	defer backendB.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
  - name: backend-b
    transport: http
    url: %q
`, backendA.URL, backendB.URL))

	root := cli.NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"list", "--config", configPath, "--json"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	var parsed struct {
		Conflicts []struct {
			Name   string   `json:"name"`
			Winner string   `json:"winner"`
			Hidden []string `json:"hidden"`
		} `json:"conflicts"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal JSON output: %v\noutput: %s", err, out.String())
	}
	if len(parsed.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one conflict", parsed.Conflicts)
	}
	c := parsed.Conflicts[0]
	if c.Name != "search" || c.Winner != "backend-a" || len(c.Hidden) != 1 || c.Hidden[0] != "backend-b" {
		t.Fatalf("Conflicts[0] = %+v, want {search, backend-a, [backend-b]}", c)
	}
}
