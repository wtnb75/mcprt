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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/contrib/propagators/autoprop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

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

func TestConnect_Stdio_Docker(t *testing.T) {
	ctx := context.Background()
	wantDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	installFakeDocker(t, "docker")

	dumpPath := filepath.Join(t.TempDir(), "docker-env.txt")
	t.Setenv("MCPRT_TEST_DOCKER_ENV_DUMP", dumpPath)

	b, err := backend.Connect(ctx, config.BackendConfig{
		Name:      "echo",
		Transport: "stdio",
		Command:   []string{buildEchoserver(t)},
		Dir:       wantDir,
		Env:       map[string]string{"MCPRT_TEST_CONTAINER_VAR": "in-container"},
		Docker: &config.DockerConfig{
			Image: "irrelevant-image",
			Env:   map[string]string{"MCPRT_TEST_HOST_VAR": "host-only"},
		},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if got := callCwd(t, ctx, b); got != wantDir {
		t.Fatalf("cwd = %q, want %q", got, wantDir)
	}

	// backends[].env must reach the containerized command (passed as -e).
	if got := callEnv(t, ctx, b, "MCPRT_TEST_CONTAINER_VAR"); got != "in-container" {
		t.Fatalf("env(MCPRT_TEST_CONTAINER_VAR) = %q, want %q", got, "in-container")
	}

	// backends[].docker.env must NOT leak into the containerized command.
	if got := callEnv(t, ctx, b, "MCPRT_TEST_HOST_VAR"); got != "" {
		t.Fatalf("env(MCPRT_TEST_HOST_VAR) = %q, want empty (docker.env must not reach the container)", got)
	}

	// ...but it must reach the local docker CLI process itself.
	dump, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("ReadFile(dump): %v", err)
	}
	if !strings.Contains(string(dump), "MCPRT_TEST_HOST_VAR=host-only") {
		t.Fatalf("docker CLI env dump = %q, want it to contain MCPRT_TEST_HOST_VAR=host-only", dump)
	}
}

func TestConnect_Stdio_Docker_Bin(t *testing.T) {
	ctx := context.Background()
	installFakeDocker(t, "podman")

	b, err := backend.Connect(ctx, config.BackendConfig{
		Name:      "echo",
		Transport: "stdio",
		Command:   []string{buildEchoserver(t)},
		Docker: &config.DockerConfig{
			Bin:   "podman",
			Image: "irrelevant-image",
		},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
}

// installFakeDocker puts a fake docker-compatible CLI named binName on PATH.
// It mimics just enough of "docker run" to exercise backend.Connect's
// dockerCommand wiring without a real container runtime: it dumps its own
// process environment (to verify docker.Env reaches the CLI process, not the
// container), then re-execs the trailing command with only the explicit "-e"
// pairs as its environment (mimicking container isolation) after applying
// "-w" as its working directory.
func installFakeDocker(t *testing.T, binName string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake docker script requires a POSIX shell")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
env > "$MCPRT_TEST_DOCKER_ENV_DUMP"
[ "$1" = "run" ] && shift
envargs=""
workdir=""
while :; do
  case "$1" in
    -i|--rm) shift ;;
    -e) envargs="$envargs $2"; shift 2 ;;
    -w) workdir="$2"; shift 2 ;;
    *) break ;;
  esac
done
shift
if [ -n "$workdir" ]; then cd "$workdir" || exit 1; fi
exec env -i $envargs "$@"
`
	if err := os.WriteFile(filepath.Join(dir, binName), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(fake %s): %v", binName, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if os.Getenv("MCPRT_TEST_DOCKER_ENV_DUMP") == "" {
		t.Setenv("MCPRT_TEST_DOCKER_ENV_DUMP", filepath.Join(t.TempDir(), "docker-env.txt"))
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

func callEnv(t *testing.T, ctx context.Context, b *backend.Backend, name string) string {
	t.Helper()
	res, err := b.Session.CallTool(ctx, &mcp.CallToolParams{Name: "env", Arguments: map[string]any{"name": name}})
	if err != nil {
		t.Fatalf("CallTool(env, %q): %v", name, err)
	}
	var out struct {
		Value string `json:"value"`
	}
	unmarshalStructured(t, res, &out)
	return out.Value
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

func TestListResourcesAndResourceTemplates(t *testing.T) {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	handler := func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{Text: "stub"}}}, nil
	}
	fakeServer.AddResource(&mcp.Resource{URI: "file:///a", Name: "a"}, handler)
	fakeServer.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "file:///dir/{f}", Name: "dir"}, handler)

	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil))
	defer srv.Close()

	ctx := context.Background()
	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	resources, err := b.ListResources(ctx)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(resources) != 1 || resources[0].URI != "file:///a" {
		t.Fatalf("ListResources = %+v, want one resource with URI file:///a", resources)
	}

	templates, err := b.ListResourceTemplates(ctx)
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	if len(templates) != 1 || templates[0].URITemplate != "file:///dir/{f}" {
		t.Fatalf("ListResourceTemplates = %+v, want one template file:///dir/{f}", templates)
	}
}

func TestListPrompts(t *testing.T) {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	fakeServer.AddPrompt(&mcp.Prompt{Name: "greet", Description: "say hello"},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{}}, nil
		})

	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil))
	defer srv.Close()

	ctx := context.Background()
	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	prompts, err := b.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts) != 1 || prompts[0].Name != "greet" {
		t.Fatalf("ListPrompts = %+v, want one prompt named \"greet\"", prompts)
	}
}

func TestConnect_UnknownTransport(t *testing.T) {
	_, err := backend.Connect(context.Background(), config.BackendConfig{Name: "bad", Transport: "carrier-pigeon"})
	if err == nil {
		t.Fatal("Connect: expected error for unknown transport, got nil")
	}
}

func TestConnect_HTTP_InjectsTraceparentWhenSpanActive(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	var gotHeader string
	seen := false
	mcpHandler := newFakeMCPHandler()
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !seen {
			gotHeader = r.Header.Get("traceparent")
			seen = true
		}
		mcpHandler.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	tracer := otel.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "test-call")
	defer span.End()

	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if gotHeader == "" {
		t.Fatal("no traceparent header was injected on the outbound request despite an active span in ctx")
	}
}

func TestConnect_HTTP_DoesNotForwardBaggage(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(autoprop.NewTextMapPropagator()) // production's real propagator: tracecontext+baggage
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	var gotTraceparent, gotBaggage string
	seen := false
	mcpHandler := newFakeMCPHandler()
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !seen {
			gotTraceparent = r.Header.Get("traceparent")
			gotBaggage = r.Header.Get("baggage")
			seen = true
		}
		mcpHandler.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	tracer := otel.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "test-call")
	defer span.End()

	member, err := baggage.NewMember("api_key", "SECRET123")
	if err != nil {
		t.Fatalf("baggage.NewMember: %v", err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatalf("baggage.New: %v", err)
	}
	ctx = baggage.ContextWithBaggage(ctx, bag)

	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if gotTraceparent == "" {
		t.Fatal("traceparent was not forwarded even though a real span was active")
	}
	if gotBaggage != "" {
		t.Fatalf("baggage header = %q, want empty: client-controlled baggage must never reach backends", gotBaggage)
	}
}

func TestConnect_HTTP_NoTraceparentWithoutActiveSpan(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	var gotHeader string
	seen := false
	mcpHandler := newFakeMCPHandler()
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !seen {
			gotHeader = r.Header.Get("traceparent")
			seen = true
		}
		mcpHandler.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	ctx := context.Background()
	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if gotHeader != "" {
		t.Fatalf("traceparent = %q, want none for a ctx with no active span", gotHeader)
	}
}
