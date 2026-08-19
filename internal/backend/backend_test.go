package backend_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
)

func TestConnect_Stdio(t *testing.T) {
	ctx := context.Background()
	b, err := backend.Connect(ctx, config.BackendConfig{
		Name:      "echo",
		Transport: "stdio",
		Command:   []string{"go", "run", "./testdata/echoserver"},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	tools, err := b.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools = %v, want a single tool named \"echo\"", tools)
	}
}

func TestConnect_HTTPWithHeaders(t *testing.T) {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	mcp.AddTool(fakeServer, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil)

	var gotAuth string
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		mcpHandler.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	ctx := context.Background()
	b, err := backend.Connect(ctx, config.BackendConfig{
		Name:      "fake",
		Transport: "http",
		URL:       srv.URL,
		Headers:   map[string]string{"Authorization": "Bearer test-token"},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer test-token")
	}
}

func TestConnect_UnknownTransport(t *testing.T) {
	_, err := backend.Connect(context.Background(), config.BackendConfig{Name: "bad", Transport: "carrier-pigeon"})
	if err == nil {
		t.Fatal("Connect: expected error for unknown transport, got nil")
	}
}
