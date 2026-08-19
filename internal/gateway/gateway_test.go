// internal/gateway/gateway_test.go
package gateway_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
	"github.com/wtnb75/mcprt/internal/router"
)

type sourceOutput struct {
	Source string `json:"source"`
}

func newFakeBackendServer(name string, toolNames ...string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, nil)
	for _, toolName := range toolNames {
		mcp.AddTool(srv, &mcp.Tool{Name: toolName, Description: "fake tool " + toolName},
			func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, sourceOutput, error) {
				return nil, sourceOutput{Source: name}, nil
			})
	}
	return srv
}

// TestGateway_CallOnDeadBackendReturnsError checks that a call to a backend
// that is no longer reachable surfaces the error to the client (callHandler
// also logs it, which isn't asserted here).
func TestGateway_CallOnDeadBackendReturnsError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := newFakeBackendServer("backend-dead", "boom")
	httpBackend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpBackend.Close()

	ctx := context.Background()
	conn, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-dead", Transport: "http", URL: httpBackend.URL})
	if err != nil {
		t.Fatalf("connect backend-dead: %v", err)
	}
	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-dead tools: %v", err)
	}

	table := router.Resolve([]router.Entry{{BackendName: "backend-dead", Tools: tools}}, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-dead": conn}, table)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	// Kill the backend connection, then call the tool it used to serve.
	if err := conn.Close(); err != nil {
		t.Fatalf("close backend-dead: %v", err)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "boom", Arguments: map[string]any{}})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("call boom on dead backend: got no error (result %+v), want an error", res)
	}
}

func TestGateway_RoutesByPriorityAndExposesUniqueTools(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendAServer := newFakeBackendServer("backend-a", "search", "unique_a")
	backendBServer := newFakeBackendServer("backend-b", "search", "unique_b")

	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendAServer }, nil))
	defer httpA.Close()
	httpB := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendBServer }, nil))
	defer httpB.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()
	connB, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-b", Transport: "http", URL: httpB.URL})
	if err != nil {
		t.Fatalf("connect backend-b: %v", err)
	}
	defer func() { _ = connB.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	toolsB, err := connB.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-b tools: %v", err)
	}

	table := router.Resolve([]router.Entry{
		{BackendName: "backend-a", Tools: toolsA},
		{BackendName: "backend-b", Tools: toolsB},
	}, nil)

	if len(table.Conflicts) != 1 || table.Conflicts[0].ExposedName != "search" || table.Conflicts[0].Winner != "backend-a" {
		t.Fatalf("unexpected conflicts: %+v", table.Conflicts)
	}

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA, "backend-b": connB}, table)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var names []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list gateway tools: %v", err)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"search", "unique_a", "unique_b"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools/list = %v, want %v", names, want)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call search: %v", err)
	}
	structured, ok := res.StructuredContent.(map[string]any)
	if !ok || structured["source"] != "backend-a" {
		t.Fatalf("search result = %+v, want structured content with source=backend-a", res.StructuredContent)
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "unique_b", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call unique_b: %v", err)
	}
	structured, ok = res.StructuredContent.(map[string]any)
	if !ok || structured["source"] != "backend-b" {
		t.Fatalf("unique_b result = %+v, want structured content with source=backend-b", res.StructuredContent)
	}
}
