package cli_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/cli"
)

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing probe listener: %v", err)
	}
	return addr
}

func writeConfig(t *testing.T, yamlContent string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestServerCommand_ServesAggregatedTools(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: fake
    transport: http
    url: %q
`, gatewayAddr, backendHTTP.URL))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + gatewayAddr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}

	var names []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if len(names) != 1 || names[0] != "ping" {
		t.Fatalf("tools/list = %v, want [ping]", names)
	}
	_ = session.Close()

	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}

func TestServerCommand_ServesAggregatedResources(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	backendSrv.AddResource(&mcp.Resource{URI: "file:///a", Name: "a"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "hello"}}}, nil
		})
	backendSrv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "file:///dir/{f}", Name: "dir"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: req.Params.URI}}}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: fake
    transport: http
    url: %q
`, gatewayAddr, backendHTTP.URL))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + gatewayAddr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///a"})
	if err != nil {
		t.Fatalf("ReadResource(file:///a): %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].Text != "hello" {
		t.Fatalf("ReadResource(file:///a) = %+v, want text \"hello\"", res.Contents)
	}

	res, err = session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///dir/x"})
	if err != nil {
		t.Fatalf("ReadResource(file:///dir/x): %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].Text != "file:///dir/x" {
		t.Fatalf("ReadResource(file:///dir/x) = %+v, want text \"file:///dir/x\" (template match forwards actual URI)", res.Contents)
	}
	_ = session.Close()

	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}

// TestServerCommand_PrefixNotAppliedToResources checks the feature's central
// invariant: a backend's `prefix` renames its tools but must never be
// applied to resource URIs or resource template URI templates, since a URI
// already carries a backend-specific namespace and string-concatenating a
// prefix onto one would produce an invalid URI.
func TestServerCommand_PrefixNotAppliedToResources(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	backendSrv.AddResource(&mcp.Resource{URI: "file:///a", Name: "a"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "hello"}}}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: fake
    transport: http
    url: %q
    prefix: "gh__"
`, gatewayAddr, backendHTTP.URL))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + gatewayAddr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}

	var toolNames []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		toolNames = append(toolNames, tool.Name)
	}
	sort.Strings(toolNames)
	if len(toolNames) != 1 || toolNames[0] != "gh__ping" {
		t.Fatalf("tools/list = %v, want [gh__ping] (prefix must be applied to tool names)", toolNames)
	}

	var resourceURIs []string
	for resource, err := range session.Resources(ctx, nil) {
		if err != nil {
			t.Fatalf("listing resources: %v", err)
		}
		resourceURIs = append(resourceURIs, resource.URI)
	}
	sort.Strings(resourceURIs)
	if len(resourceURIs) != 1 || resourceURIs[0] != "file:///a" {
		t.Fatalf("resources/list = %v, want [file:///a] (prefix must never be applied to resource URIs)", resourceURIs)
	}

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///a"})
	if err != nil {
		t.Fatalf("ReadResource(file:///a): %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].Text != "hello" {
		t.Fatalf("ReadResource(file:///a) = %+v, want text \"hello\"", res.Contents)
	}
	_ = session.Close()

	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}

// TestServerCommand_StdioShutdownIsClean covers the stdio listener's
// shutdown path: gateway.ServeStdio returns ctx.Err() when the context is
// cancelled, and a cancelled context is a clean shutdown, not a failure the
// command should exit non-zero on.
func TestServerCommand_StdioShutdownIsClean(t *testing.T) {
	// mcp.StdioTransport reads os.Stdin at connect time, so point it at a
	// pipe nothing ever writes to: the session then stays open until the
	// context is cancelled. os.Stdout is left alone -- the server writes
	// nothing unless a client sends a request.
	//
	// This swaps the package-level os.Stdin for the test's duration, so this
	// test must not run with t.Parallel() (nor alongside another test that
	// touches os.Stdin).
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stdin pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = origStdin
		_ = w.Close()
		_ = r.Close()
	})

	configPath := writeConfig(t, "listen:\n  stdio: true\n\nbackends: []\n")

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	// Give the command a moment to load the config and start the listener.
	time.Sleep(200 * time.Millisecond)

	cancel()
	select {
	case err := <-execErr:
		if err != nil {
			t.Fatalf("server exited with error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit within 5s of context cancellation")
	}
}

// TestServerCommand_ListenerFailureCancelsOthers checks that when one
// listener fails immediately (here, the HTTP listener can't bind its port),
// runServer cancels the other listener instead of blocking on it forever.
func TestServerCommand_ListenerFailureCancelsOthers(t *testing.T) {
	// Swap stdin so ServeStdio blocks on a pipe nothing writes to, instead
	// of the test process's real stdin. Must not run with t.Parallel().
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stdin pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = origStdin
		_ = w.Close()
		_ = r.Close()
	})

	// Occupy a port so listen.http fails to bind immediately.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  stdio: true
  http: %q

backends: []
`, occupied.Addr().String()))

	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(context.Background(), []string{"server", "--config", configPath})
	}()

	select {
	case err := <-execErr:
		if err == nil {
			t.Fatal("Execute: expected an error from the failed HTTP listener, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return within 5s of the HTTP listener failing to bind; the stdio listener was not cancelled")
	}
}

func TestServerCommand_NoListenerConfigured(t *testing.T) {
	configPath := writeConfig(t, "backends: []\n")

	err := cli.Execute(context.Background(), []string{"server", "--config", configPath})
	if err == nil {
		t.Fatal("Execute: expected error when no listener is configured, got nil")
	}
}
