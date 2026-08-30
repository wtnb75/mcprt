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

// newFakeCallBackend serves two tools: "echo", which echoes its arguments
// back as text content, and "boom", which always returns an error result.
func newFakeCallBackend(name string) *httptest.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "echo", Description: "echoes arguments back", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args, err := json.Marshal(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(args)}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "boom", Description: "always fails", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "boom failed"}}}, nil
		})
	return httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
}

func TestCallCommand_Text(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
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
	root.SetArgs([]string{"call", "--config", configPath, "--args", `{"message":"hi"}`, "echo"})

	// A cancellable context, not context.Background(): runCall's
	// connectBackends (gwH == nil, a production-correct choice for a
	// one-shot CLI command whose process exits right after) spawns a
	// persistent superviseBackend goroutine per backend that keeps retrying
	// for as long as ctx stays alive. In a real `mcprt call` invocation the
	// process exits right after and takes that goroutine with it, but in a
	// test the process doesn't exit, so context.Background() would leak it
	// into later tests, racing their writes to package-level vars like
	// backendConnectTimeout under `go test -race` (see this plan's Task 2
	// review, finding 1). cancel() runs (via defer, LIFO) before
	// backendA.Close() above, so any straggling supervisor stops retrying
	// before the backend it would retry against goes away.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, `"message":"hi"`) {
		t.Fatalf("text output = %q, want it to contain the echoed arguments", got)
	}
}

func TestCallCommand_JSON(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
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
	root.SetArgs([]string{"call", "--config", configPath, "--args", `{"message":"hi"}`, "--json", "echo"})

	// See TestCallCommand_Text's matching comment: a cancellable context so
	// runCall's backend supervisor goroutine doesn't outlive this test.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal JSON output: %v\noutput: %s", err, out.String())
	}
	if len(parsed.Content) != 1 || !strings.Contains(parsed.Content[0].Text, `"message":"hi"`) {
		t.Fatalf("Content = %+v, want one text block containing the echoed arguments", parsed.Content)
	}
	if parsed.IsError {
		t.Fatalf("IsError = true, want false")
	}
}

func TestCallCommand_NoArgs(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	// See TestCallCommand_Text's matching comment: a cancellable context so
	// runCall's backend supervisor goroutine doesn't outlive this test.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cli.Execute(ctx, []string{"call", "--config", configPath, "echo"}); err != nil {
		t.Fatalf("Execute: unexpected error calling with no --args: %v", err)
	}
}

func TestCallCommand_UnknownTool(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	// See TestCallCommand_Text's matching comment: a cancellable context so
	// runCall's backend supervisor goroutine doesn't outlive this test.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cli.Execute(ctx, []string{"call", "--config", configPath, "does-not-exist"}); err == nil {
		t.Fatal("Execute: expected error for unknown tool name, got nil")
	}
}

func TestCallCommand_InvalidArgsJSON(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	// See TestCallCommand_Text's matching comment: a cancellable context so
	// runCall's backend supervisor goroutine doesn't outlive this test. (This
	// particular call fails before connectBackends even runs -- --args is
	// validated first -- but the context is fixed uniformly across this file
	// rather than special-cased per test, since it costs nothing here and
	// keeps the pattern obviously safe against a future reordering inside
	// runCall.)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cli.Execute(ctx, []string{"call", "--config", configPath, "--args", "not json", "echo"}); err == nil {
		t.Fatal("Execute: expected error for invalid --args JSON, got nil")
	}
}

func TestCallCommand_ToolReturnsError(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	// See TestCallCommand_Text's matching comment: a cancellable context so
	// runCall's backend supervisor goroutine doesn't outlive this test.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cli.Execute(ctx, []string{"call", "--config", configPath, "boom"}); err == nil {
		t.Fatal("Execute: expected a non-nil error when the tool result has IsError=true, got nil")
	}
}
