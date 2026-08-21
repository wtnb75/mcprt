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

	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-dead", Items: tools}}, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-dead": conn}, gateway.Tables{Tools: table})

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

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-b": connB}, gateway.Tables{Tools: table})

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

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA, "backend-b": connB}, gateway.Tables{Tools: table})

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

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Resources: table})

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

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{ResourceTemplates: table})

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

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-b": connB}, gateway.Tables{Resources: table})

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
	srv := gateway.New(logger, map[string]*backend.Backend{"backend": conn}, gateway.Tables{Prompts: table})

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

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA, "backend-b": connB}, gateway.Tables{Prompts: table})

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
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-dead": conn}, gateway.Tables{Prompts: table})

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
