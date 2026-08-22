// internal/gateway/gateway_test.go
package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
	"github.com/wtnb75/mcprt/internal/router"
)

// testSpanExporter is the single, shared in-memory span exporter for this
// whole test binary -- see TestMain and the Task 3 plan note above for why
// each test that checks spans must Reset() it rather than installing its
// own fresh TracerProvider.
var testSpanExporter = tracetest.NewInMemoryExporter()

// TestMain installs testSpanExporter's TracerProvider exactly once, before
// any test runs, and sets a TraceContext propagator. This is the only
// otel.SetTracerProvider call in this test binary.
func TestMain(m *testing.M) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(testSpanExporter))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	os.Exit(m.Run())
}

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

	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-dead", Items: tools}}, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-dead": conn}, gateway.Tables{Tools: table}, nil)

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

// TestGateway_FallsBackWhenWinnerSchemaInvalid checks that when the
// conflict winner's tool definition is malformed (AddTool panics on it),
// the gateway falls back to the next candidate rather than dropping the
// tool entirely.
func TestGateway_FallsBackWhenWinnerSchemaInvalid(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendBServer := newFakeBackendServer("backend-b", "search")
	httpB := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendBServer }, nil))
	defer httpB.Close()

	ctx := context.Background()
	connB, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-b", Transport: "http", URL: httpB.URL})
	if err != nil {
		t.Fatalf("connect backend-b: %v", err)
	}
	defer func() { _ = connB.Close() }()

	toolsB, err := connB.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-b tools: %v", err)
	}

	// Hand-build a table: the winner ("backend-a") has no InputSchema,
	// which mcp.Server.AddTool panics on; the loser ("backend-b") is a
	// valid fallback candidate for the same exposed name.
	table := &router.Table[*mcp.Tool]{
		Items: map[string]*router.Resolved[*mcp.Tool]{
			"search": {
				Item:         &mcp.Tool{Name: "search"},
				BackendName:  "backend-a",
				OriginalName: "search",
				Fallbacks: []router.Candidate[*mcp.Tool]{{
					Item:         toolsB[0],
					BackendName:  "backend-b",
					OriginalName: "search",
				}},
			},
		},
	}

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-b": connB}, gateway.Tables{Tools: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call search: %v", err)
	}
	structured, ok := res.StructuredContent.(map[string]any)
	if !ok || structured["source"] != "backend-b" {
		t.Fatalf("search result = %+v, want structured content with source=backend-b (the fallback)", res.StructuredContent)
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

	table := router.Resolve([]router.Entry[*mcp.Tool]{
		{BackendName: "backend-a", Items: toolsA},
		{BackendName: "backend-b", Items: toolsB},
	}, toolNameOf, toolRename, nil)

	if len(table.Conflicts) != 1 || table.Conflicts[0].ExposedName != "search" || table.Conflicts[0].Winner != "backend-a" {
		t.Fatalf("unexpected conflicts: %+v", table.Conflicts)
	}

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA, "backend-b": connB}, gateway.Tables{Tools: table}, nil)

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

func toolNameOf(t *mcp.Tool) string { return t.Name }

func toolRename(t *mcp.Tool, name string) *mcp.Tool {
	c := *t
	c.Name = name
	return &c
}

func newFakeResourceBackendServer(name string, uris ...string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, nil)
	for _, uri := range uris {
		srv.AddResource(&mcp.Resource{URI: uri, Name: uri},
			func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: name}}}, nil
			})
	}
	return srv
}

func newFakeResourceTemplateBackendServer(name, uriTemplate string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, nil)
	srv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: uriTemplate, Name: uriTemplate},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: req.Params.URI}}}, nil
		})
	return srv
}

func newFakePromptBackendServer(name string, promptNames ...string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, nil)
	for _, promptName := range promptNames {
		srv.AddPrompt(&mcp.Prompt{Name: promptName, Description: "fake prompt " + promptName},
			func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{
					Role:    "user",
					Content: &mcp.TextContent{Text: name + ":" + req.Params.Name},
				}}}, nil
			})
	}
	return srv
}

func promptNameOf(p *mcp.Prompt) string { return p.Name }

func promptRename(p *mcp.Prompt, name string) *mcp.Prompt {
	c := *p
	c.Name = name
	return &c
}

// TestGateway_ResourceReadExact checks that resources/read for an exact
// registered URI is forwarded to the owning backend and its result
// returned unchanged.
func TestGateway_ResourceReadExact(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := newFakeResourceBackendServer("backend-a", "file:///a")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	resources, err := connA.ListResources(ctx)
	if err != nil {
		t.Fatalf("list backend-a resources: %v", err)
	}

	resourceNameOf := func(r *mcp.Resource) string { return r.URI }
	resourceRename := func(r *mcp.Resource, name string) *mcp.Resource { c := *r; c.URI = name; return &c }
	table := router.Resolve([]router.Entry[*mcp.Resource]{
		{BackendName: "backend-a", Items: resources},
	}, resourceNameOf, resourceRename, nil)

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Resources: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///a"})
	if err != nil {
		t.Fatalf("ReadResource(file:///a): %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].Text != "backend-a" {
		t.Fatalf("ReadResource result = %+v, want text \"backend-a\"", res.Contents)
	}
}

// TestGateway_ResourceTemplateReadForwardsActualURI checks that
// resources/read for a URI matching a registered template forwards the
// client's actual requested URI to the backend (not the template string).
func TestGateway_ResourceTemplateReadForwardsActualURI(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := newFakeResourceTemplateBackendServer("backend-a", "file:///dir/{f}")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	templates, err := connA.ListResourceTemplates(ctx)
	if err != nil {
		t.Fatalf("list backend-a resource templates: %v", err)
	}

	templateNameOf := func(rt *mcp.ResourceTemplate) string { return rt.URITemplate }
	templateRename := func(rt *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate {
		c := *rt
		c.URITemplate = name
		return &c
	}
	table := router.Resolve([]router.Entry[*mcp.ResourceTemplate]{
		{BackendName: "backend-a", Items: templates},
	}, templateNameOf, templateRename, nil)

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{ResourceTemplates: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///dir/x"})
	if err != nil {
		t.Fatalf("ReadResource(file:///dir/x): %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].Text != "file:///dir/x" {
		t.Fatalf("ReadResource result = %+v, want text \"file:///dir/x\" (the actual requested URI forwarded to the backend)", res.Contents)
	}
}

// TestGateway_ResourceFallsBackWhenWinnerURIInvalid checks that when the
// conflict winner's resource URI is malformed (AddResource panics on it),
// the gateway falls back to the next candidate rather than dropping the
// resource entirely.
func TestGateway_ResourceFallsBackWhenWinnerURIInvalid(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendBServer := newFakeResourceBackendServer("backend-b", "file:///a")
	httpB := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendBServer }, nil))
	defer httpB.Close()

	ctx := context.Background()
	connB, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-b", Transport: "http", URL: httpB.URL})
	if err != nil {
		t.Fatalf("connect backend-b: %v", err)
	}
	defer func() { _ = connB.Close() }()

	resourcesB, err := connB.ListResources(ctx)
	if err != nil {
		t.Fatalf("list backend-b resources: %v", err)
	}

	// Hand-build a table: the winner ("backend-a") has an invalid URI, which
	// mcp.Server.AddResource panics on; the loser ("backend-b") is a valid
	// fallback candidate.
	table := &router.Table[*mcp.Resource]{
		Items: map[string]*router.Resolved[*mcp.Resource]{
			"resource-key": {
				Item:         &mcp.Resource{URI: "http://%gg", Name: "bad"},
				BackendName:  "backend-a",
				OriginalName: "http://%gg",
				Fallbacks: []router.Candidate[*mcp.Resource]{{
					Item:         resourcesB[0],
					BackendName:  "backend-b",
					OriginalName: "file:///a",
				}},
			},
		},
	}

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-b": connB}, gateway.Tables{Resources: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///a"})
	if err != nil {
		t.Fatalf("ReadResource(file:///a): %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].Text != "backend-b" {
		t.Fatalf("ReadResource result = %+v, want text \"backend-b\" (the fallback)", res.Contents)
	}
}

// TestGateway_PromptGetForwardsArgumentsAndResult checks that prompts/get
// for a registered prompt is forwarded to the owning backend with the
// caller's arguments, and the backend's result is returned unchanged.
func TestGateway_PromptGetForwardsArgumentsAndResult(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	backendServer.AddPrompt(&mcp.Prompt{Name: "greet"},
		func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: "hello " + req.Params.Arguments["name"]},
			}}}, nil
		})
	httpBackend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpBackend.Close()

	ctx := context.Background()
	conn, err := backend.Connect(ctx, config.BackendConfig{Name: "backend", Transport: "http", URL: httpBackend.URL})
	if err != nil {
		t.Fatalf("connect backend: %v", err)
	}
	defer func() { _ = conn.Close() }()
	prompts, err := conn.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("list backend prompts: %v", err)
	}

	table := router.Resolve([]router.Entry[*mcp.Prompt]{{BackendName: "backend", Items: prompts}}, promptNameOf, promptRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"backend": conn}, gateway.Tables{Prompts: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet", Arguments: map[string]string{"name": "world"}})
	if err != nil {
		t.Fatalf("GetPrompt(greet): %v", err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("GetPrompt(greet) messages = %+v, want 1", res.Messages)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "hello world" {
		t.Fatalf("GetPrompt(greet) content = %+v, want text \"hello world\"", res.Messages[0].Content)
	}
}

// TestGateway_PromptRoutesByPriorityAndExposesUniquePrompts checks
// conflict resolution and prompts/list for two backends that share one
// prompt name and each also expose a unique one.
func TestGateway_PromptRoutesByPriorityAndExposesUniquePrompts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendAServer := newFakePromptBackendServer("backend-a", "review", "unique_a")
	backendBServer := newFakePromptBackendServer("backend-b", "review", "unique_b")

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

	promptsA, err := connA.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("list backend-a prompts: %v", err)
	}
	promptsB, err := connB.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("list backend-b prompts: %v", err)
	}

	table := router.Resolve([]router.Entry[*mcp.Prompt]{
		{BackendName: "backend-a", Items: promptsA},
		{BackendName: "backend-b", Items: promptsB},
	}, promptNameOf, promptRename, nil)

	if len(table.Conflicts) != 1 || table.Conflicts[0].ExposedName != "review" || table.Conflicts[0].Winner != "backend-a" {
		t.Fatalf("unexpected conflicts: %+v", table.Conflicts)
	}

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA, "backend-b": connB}, gateway.Tables{Prompts: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var names []string
	for prompt, err := range session.Prompts(ctx, nil) {
		if err != nil {
			t.Fatalf("list gateway prompts: %v", err)
		}
		names = append(names, prompt.Name)
	}
	sort.Strings(names)
	want := []string{"review", "unique_a", "unique_b"}
	if !slices.Equal(names, want) {
		t.Fatalf("prompts/list = %v, want %v", names, want)
	}

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "review"})
	if err != nil {
		t.Fatalf("GetPrompt(review): %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "backend-a:review" {
		t.Fatalf("GetPrompt(review) content = %+v, want text \"backend-a:review\" (the conflict winner)", res.Messages[0].Content)
	}

	res, err = session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "unique_b"})
	if err != nil {
		t.Fatalf("GetPrompt(unique_b): %v", err)
	}
	text, ok = res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "backend-b:unique_b" {
		t.Fatalf("GetPrompt(unique_b) content = %+v, want text \"backend-b:unique_b\"", res.Messages[0].Content)
	}
}

// TestGateway_PromptGetOnDeadBackendReturnsError checks that a prompts/get
// call to a backend that is no longer reachable surfaces the error to the
// client, the same way TestGateway_CallOnDeadBackendReturnsError already
// checks for tools/call.
func TestGateway_PromptGetOnDeadBackendReturnsError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := newFakePromptBackendServer("backend-dead", "boom")
	httpBackend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpBackend.Close()

	ctx := context.Background()
	conn, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-dead", Transport: "http", URL: httpBackend.URL})
	if err != nil {
		t.Fatalf("connect backend-dead: %v", err)
	}
	prompts, err := conn.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("list backend-dead prompts: %v", err)
	}

	table := router.Resolve([]router.Entry[*mcp.Prompt]{{BackendName: "backend-dead", Items: prompts}}, promptNameOf, promptRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-dead": conn}, gateway.Tables{Prompts: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	if err := conn.Close(); err != nil {
		t.Fatalf("close backend-dead: %v", err)
	}

	_, err = session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "boom"})
	if err == nil {
		t.Fatal("GetPrompt(boom) on dead backend: got no error, want one")
	}
}

// TestGateway_CallLogsSuccessWithMaskedArguments checks the core behavior
// change: a successful tool call now logs one Info line (previously none),
// carrying caller identity and masked arguments.
func TestGateway_CallLogsSuccessWithMaskedArguments(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	backendServer := newFakeBackendServer("backend-a", "secure")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-a", Items: toolsA}}, toolNameOf, toolRename, nil)

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, []string{"secret_value"})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "secure", Arguments: map[string]any{"user": "alice", "secret_value": "hunter2"}})
	if err != nil {
		t.Fatalf("call secure: %v", err)
	}

	rec := findLogLine(t, buf.String(), "tool call")
	if rec["backend"] != "backend-a" || rec["tool"] != "secure" {
		t.Fatalf("backend/tool = %v/%v, want backend-a/secure", rec["backend"], rec["tool"])
	}
	if rec["session_id"] == "" || rec["session_id"] == nil {
		t.Fatalf("session_id = %v, want non-empty", rec["session_id"])
	}
	if rec["client_name"] != "test-client" || rec["client_version"] != "v1" {
		t.Fatalf("client_name/client_version = %v/%v, want test-client/v1", rec["client_name"], rec["client_version"])
	}
	args, ok := rec["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("arguments = %v, want a map", rec["arguments"])
	}
	if args["user"] != "alice" {
		t.Fatalf("arguments.user = %v, want alice (unmasked)", args["user"])
	}
	if args["secret_value"] != "***" {
		t.Fatalf("arguments.secret_value = %v, want *** (masked via config-supplied maskKeys)", args["secret_value"])
	}
}

// TestGateway_CallLogsNoArguments checks that a tool call with no arguments
// passed omits the "arguments" field entirely, rather than logging an empty
// or typed-nil value.
func TestGateway_CallLogsNoArguments(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	backendServer := newFakeBackendServer("backend-a", "noargs")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-a", Items: toolsA}}, toolNameOf, toolRename, nil)

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	// Call with no Arguments supplied (nil on wire)
	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "noargs"})
	if err != nil {
		t.Fatalf("call noargs: %v", err)
	}

	rec := findLogLine(t, buf.String(), "tool call")
	if rec["backend"] != "backend-a" || rec["tool"] != "noargs" {
		t.Fatalf("backend/tool = %v/%v, want backend-a/noargs", rec["backend"], rec["tool"])
	}
	if _, ok := rec["arguments"]; ok {
		t.Fatalf("log line has arguments field, want it omitted for tool call with no Arguments")
	}
}

// TestGateway_CallLogsFailure checks that a failed tool call logs one Error
// line with the failure reason, alongside the same caller-identity fields
// the success path carries.
func TestGateway_CallLogsFailure(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

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
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-dead", Items: tools}}, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-dead": conn}, gateway.Tables{Tools: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	if err := conn.Close(); err != nil {
		t.Fatalf("close backend-dead: %v", err)
	}

	_, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "boom", Arguments: map[string]any{}})

	rec := findLogLine(t, buf.String(), "tool call failed")
	if rec["level"] != "ERROR" {
		t.Fatalf("level = %v, want ERROR", rec["level"])
	}
	if rec["error"] == "" || rec["error"] == nil {
		t.Fatalf("error field = %v, want non-empty", rec["error"])
	}
}

// findLogLine returns the first JSON log line in out whose "msg" equals
// wantMsg, failing the test if none matches.
func findLogLine(t *testing.T, out, wantMsg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err == nil && rec["msg"] == wantMsg {
			return rec
		}
	}
	t.Fatalf("log output = %q, want a line with msg=%q", out, wantMsg)
	return nil
}

// TestGateway_ServeHTTP_LogsRemoteAddr checks that a call served through the
// real ServeHTTP (unlike the other tests in this file, which wrap srv in a
// bare mcp.NewStreamableHTTPHandler) carries a remote_addr field in its
// audit log line, via ServeHTTP's remoteAddrMiddleware.
func TestGateway_ServeHTTP_LogsRemoteAddr(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	backendServer := newFakeBackendServer("backend-a", "ping")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-a", Items: toolsA}}, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing probe listener: %v", err)
	}

	gwCtx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- gateway.ServeHTTP(gwCtx, srv, addr) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + addr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}

	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "ping", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call ping: %v", err)
	}
	_ = session.Close()

	cancel()
	if err := <-serveErr; err != nil {
		t.Fatalf("ServeHTTP exited with error: %v", err)
	}

	rec := findLogLine(t, buf.String(), "tool call")
	addrField, ok := rec["remote_addr"].(string)
	if !ok || !strings.HasPrefix(addrField, "127.0.0.1:") {
		t.Fatalf("remote_addr = %v, want a 127.0.0.1:<port> string", rec["remote_addr"])
	}
}

// TestGateway_CallCreatesSpanWithParentFromTraceparent checks the core
// behavior change: a tool call arriving over HTTP with a traceparent
// header now produces one span, continuing that trace, with the expected
// name and attributes.
func TestGateway_CallCreatesSpanWithParentFromTraceparent(t *testing.T) {
	testSpanExporter.Reset()
	exp := testSpanExporter

	backendServer := newFakeBackendServer("backend-a", "ping")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-a", Items: toolsA}}, toolNameOf, toolRename, nil)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	// Every outbound request from this client (including the tools/call
	// POST) carries a fixed traceparent, simulating an already-traced
	// upstream caller.
	traceHeaders := http.Header{}
	traceHeaders.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   gw.URL,
		HTTPClient: &http.Client{Transport: headerSetRoundTripper{headers: traceHeaders, base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ping", Arguments: map[string]any{}}); err != nil {
		t.Fatalf("call ping: %v", err)
	}

	var callSpan *tracetest.SpanStub
	for i, s := range exp.GetSpans() {
		if s.Name == "tools/call" {
			callSpan = &exp.GetSpans()[i]
			break
		}
	}
	if callSpan == nil {
		t.Fatalf("no \"tools/call\" span recorded; spans = %+v", exp.GetSpans())
	}
	if callSpan.Parent.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("parent trace id = %s, want 4bf92f3577b34da6a3ce929d0e0e4736", callSpan.Parent.TraceID().String())
	}
	attrs := attribute.NewSet(callSpan.Attributes...)
	if v, ok := attrs.Value("mcp.backend"); !ok || v.AsString() != "backend-a" {
		t.Fatalf("mcp.backend attribute = %v (ok=%v), want backend-a", v, ok)
	}
	if v, ok := attrs.Value("mcp.tool.name"); !ok || v.AsString() != "ping" {
		t.Fatalf("mcp.tool.name attribute = %v (ok=%v), want ping", v, ok)
	}
}

// headerSetRoundTripper sets fixed headers on every outbound request,
// simulating a client that already carries trace context (or any other
// fixed header) on every request it sends — distinct from
// internal/backend's headerRoundTripper, which is production code for
// injecting configured backend-auth headers; this is test-only and lives
// in the gateway_test package, testing the gateway's inbound side.
type headerSetRoundTripper struct {
	headers http.Header
	base    http.RoundTripper
}

func (h headerSetRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, vs := range h.headers {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	return h.base.RoundTrip(req)
}

// TestGateway_ResourceReadCreatesSpan checks that the resource-read
// handler (a different attribute set than the tool handler) also produces
// a span when called over HTTP, without a traceparent header this time
// (exercising the "no inbound parent, so a new root span starts" path).
func TestGateway_ResourceReadCreatesSpan(t *testing.T) {
	testSpanExporter.Reset()
	exp := testSpanExporter

	backendServer := newFakeResourceBackendServer("backend-a", "file:///a.txt")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	resourcesA, err := connA.ListResources(ctx)
	if err != nil {
		t.Fatalf("list backend-a resources: %v", err)
	}
	resourceNameOf := func(r *mcp.Resource) string { return r.URI }
	resourceRename := func(r *mcp.Resource, name string) *mcp.Resource { c := *r; c.URI = name; return &c }
	table := router.Resolve([]router.Entry[*mcp.Resource]{{BackendName: "backend-a", Items: resourcesA}}, resourceNameOf, resourceRename, nil)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Resources: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///a.txt"}); err != nil {
		t.Fatalf("read resource: %v", err)
	}

	var readSpan *tracetest.SpanStub
	for i, s := range exp.GetSpans() {
		if s.Name == "resources/read" {
			readSpan = &exp.GetSpans()[i]
			break
		}
	}
	if readSpan == nil {
		t.Fatalf("no \"resources/read\" span recorded; spans = %+v", exp.GetSpans())
	}
	attrs := attribute.NewSet(readSpan.Attributes...)
	if v, ok := attrs.Value("mcp.resource.uri"); !ok || v.AsString() != "file:///a.txt" {
		t.Fatalf("mcp.resource.uri attribute = %v (ok=%v), want file:///a.txt", v, ok)
	}
}
