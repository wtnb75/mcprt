package backend_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync/atomic"
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
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	if len(tools) != 3 || !slices.Contains(names, "echo") || !slices.Contains(names, "cwd") || !slices.Contains(names, "env") {
		t.Fatalf("ListTools = %v, want tools named \"echo\", \"cwd\" and \"env\"", names)
	}
}

func TestConnect_Stdio_Dir(t *testing.T) {
	ctx := context.Background()
	wantDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	b, err := backend.Connect(ctx, config.BackendConfig{
		Name:      "echo",
		Transport: "stdio",
		Command:   []string{buildEchoserver(t)},
		Dir:       wantDir,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if got := callCwd(t, ctx, b); got != wantDir {
		t.Fatalf("cwd = %q, want %q", got, wantDir)
	}
}

func TestConnect_Stdio_SSH(t *testing.T) {
	ctx := context.Background()
	wantDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	installFakeSSH(t)

	b, err := backend.Connect(ctx, config.BackendConfig{
		Name:      "echo",
		Transport: "stdio",
		Command:   []string{buildEchoserver(t)},
		Dir:       wantDir,
		Env:       map[string]string{"MCPRT_TEST_SSH_VAR": "via-ssh"},
		SSH:       &config.SSHConfig{Host: "irrelevant-host"},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if got := callCwd(t, ctx, b); got != wantDir {
		t.Fatalf("cwd = %q, want %q", got, wantDir)
	}

	res, err := b.Session.CallTool(ctx, &mcp.CallToolParams{Name: "env", Arguments: map[string]any{"name": "MCPRT_TEST_SSH_VAR"}})
	if err != nil {
		t.Fatalf("CallTool(env): %v", err)
	}
	var envOut struct {
		Value string `json:"value"`
	}
	unmarshalStructured(t, res, &envOut)
	if envOut.Value != "via-ssh" {
		t.Fatalf("env(MCPRT_TEST_SSH_VAR) = %q, want %q", envOut.Value, "via-ssh")
	}
}

// buildEchoserver builds the echoserver test fixture into a plain binary and
// returns its path. Tests that set Dir (or ssh, which behaves the same way)
// can't use "go run": with cmd.Dir pointing outside the module, "go run"
// can't resolve go.mod for the package.
func buildEchoserver(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "echoserver")
	build := exec.Command("go", "build", "-o", binPath, "./testdata/echoserver")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build echoserver: %v\n%s", err, out)
	}
	return binPath
}

// installFakeSSH puts a fake "ssh" on PATH that ignores all its connection
// arguments and just runs the last argument (the remote script backend.go
// builds) locally via sh -c. This lets tests exercise the real
// backend.Connect -> ssh -> remote-script -> MCP-handshake path without a
// real ssh server.
func installFakeSSH(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh script requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nfor last; do :; done\nexec sh -c \"$last\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(fake ssh): %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func callCwd(t *testing.T, ctx context.Context, b *backend.Backend) string {
	t.Helper()
	res, err := b.Session.CallTool(ctx, &mcp.CallToolParams{Name: "cwd"})
	if err != nil {
		t.Fatalf("CallTool(cwd): %v", err)
	}
	var out struct {
		Dir string `json:"dir"`
	}
	unmarshalStructured(t, res, &out)
	return out.Dir
}

func unmarshalStructured(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
}

// newFakeMCPHandler returns an HTTP handler serving a minimal MCP server
// with a single no-op "ping" tool, for backend.Connect(transport: "http")
// tests to dial.
func newFakeMCPHandler() http.Handler {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	mcp.AddTool(fakeServer, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil)
}

func TestConnect_HTTPWithHeaders(t *testing.T) {
	mcpHandler := newFakeMCPHandler()

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

func TestConnect_HTTP_Proxy(t *testing.T) {
	ctx := context.Background()
	backendSrv := httptest.NewServer(newFakeMCPHandler())
	defer backendSrv.Close()

	// A forward-proxy request already arrives with an absolute-form URL, so
	// a no-op Director is enough to turn ReverseProxy into a forward proxy;
	// FlushInterval: -1 streams the response immediately instead of
	// buffering, which the MCP client's long-lived SSE connection needs.
	var proxied atomic.Bool
	proxySrv := httptest.NewServer(&httputil.ReverseProxy{
		Director:      func(r *http.Request) { proxied.Store(true) },
		FlushInterval: -1,
	})
	defer proxySrv.Close()

	b, err := backend.Connect(ctx, config.BackendConfig{
		Name:      "proxied",
		Transport: "http",
		URL:       backendSrv.URL,
		Proxy:     proxySrv.URL,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if !proxied.Load() {
		t.Fatal("request did not go through the configured proxy")
	}
}

func TestConnect_HTTP_ProxyNone(t *testing.T) {
	ctx := context.Background()
	backendSrv := httptest.NewServer(newFakeMCPHandler())
	defer backendSrv.Close()

	// Nothing listens on this port. If proxy: "none" failed to override
	// HTTP_PROXY, ListTools below would fail to connect.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")

	b, err := backend.Connect(ctx, config.BackendConfig{
		Name:      "direct",
		Transport: "http",
		URL:       backendSrv.URL,
		Proxy:     "none",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
}

func TestConnect_UnknownTransport(t *testing.T) {
	_, err := backend.Connect(context.Background(), config.BackendConfig{Name: "bad", Transport: "carrier-pigeon"})
	if err == nil {
		t.Fatal("Connect: expected error for unknown transport, got nil")
	}
}
