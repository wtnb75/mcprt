package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"

	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
	"github.com/wtnb75/mcprt/internal/router"
)

// TestMain forces OTEL_TRACES_EXPORTER=none for every test in this
// package's test binary: once runServer wires internal/telemetry.Setup,
// every test that calls runServer (directly, or via cli.Execute) would
// otherwise attempt the OTel SDK's default OTLP/HTTP export target
// (http://localhost:4318), which nothing here listens on -- Setup itself
// wouldn't fail (BatchSpanProcessor exports asynchronously), but its
// background goroutine would log periodic connection-refused noise into
// test output. Setting "none" makes autoexport.NewSpanExporter return a
// genuine no-op exporter, keeping every test in this package fast,
// deterministic, and independent of network access.
func TestMain(m *testing.M) {
	_ = os.Setenv("OTEL_TRACES_EXPORTER", "none")
	os.Exit(m.Run())
}

func TestParseLogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for s, want := range cases {
		got, err := parseLogLevel(s)
		if err != nil {
			t.Fatalf("parseLogLevel(%q): %v", s, err)
		}
		if got != want {
			t.Fatalf("parseLogLevel(%q) = %v, want %v", s, got, want)
		}
	}
	if _, err := parseLogLevel("bogus"); err == nil {
		t.Fatal("parseLogLevel(\"bogus\"): expected error, got nil")
	}
}

// TestConnectBackends_TimesOutHungBackend checks that a backend which never
// completes the MCP initialize handshake is excluded once
// backendConnectTimeout elapses, instead of blocking connectBackends
// forever.
//
// The wait tolerance here is generous: once ctx expires, the SDK's
// CommandTransport itself still needs to close stdin, wait out its own
// default 5s TerminateDuration for the process to notice and exit, then
// escalate to SIGTERM/SIGKILL if it doesn't -- that cleanup isn't bounded
// by backendConnectTimeout. The fix's guarantee is that the whole sequence
// is now *finite*, not that it completes within backendConnectTimeout.
func TestConnectBackends_TimesOutHungBackend(t *testing.T) {
	orig := backendConnectTimeout
	backendConnectTimeout = 100 * time.Millisecond
	t.Cleanup(func() { backendConnectTimeout = orig })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	configs := []config.BackendConfig{
		// "sleep 30" never speaks MCP over stdio, so the initialize
		// handshake hangs until backendConnectTimeout cancels it, and it
		// outlives the transport's own cleanup window, so reaching "no
		// backends" also proves the process was actually killed (SIGTERM),
		// not just abandoned to exit on its own.
		{Name: "hung", Transport: "stdio", Command: []string{"sleep", "30"}},
	}

	done := make(chan struct{})
	var conn connected
	go func() {
		conn = connectBackends(context.Background(), logger, configs, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("connectBackends did not return within 10s of backendConnectTimeout elapsing")
	}
	if len(conn.backends) != 0 {
		t.Fatalf("backends = %v, want none (the hung backend should be excluded)", conn.backends)
	}
}

// denyMethodHandler wraps an MCP StreamableHTTP handler so that any
// JSON-RPC request whose method is in denied gets a synthetic "Method not
// found" (-32601) JSON-RPC error response instead of ever reaching the real
// MCP server. This emulates how many non-Go-SDK MCP servers (TypeScript,
// Python) answer a capability they don't implement -- e.g. resources/list
// when they declare no resources -- with a JSON-RPC error, unlike
// go-sdk-based servers, which always answer with an empty list. There's no
// go-sdk server option to force that behavior, so a raw handler is the only
// way to reproduce it in a test.
func denyMethodHandler(inner http.Handler, denied ...string) http.HandlerFunc {
	deniedSet := make(map[string]bool, len(denied))
	for _, m := range denied {
		deniedSet[m] = true
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			inner.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		var probe struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if json.Unmarshal(body, &probe) == nil && deniedSet[probe.Method] {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      probe.ID,
				"error": map[string]any{
					"code":    -32601,
					"message": "Method not found",
				},
			})
			return
		}
		inner.ServeHTTP(w, r)
	}
}

// TestConnectBackends_ResourceListFailureKeepsBackendTools is the regression
// test for the fix: a backend whose resources/list (and
// resources/templates/list) errors out -- as a plain tools-only backend
// built with a non-Go SDK commonly does, by returning "method not found"
// for a capability it never declared -- must still be connected, with its
// tools intact, instead of being dropped entirely the way a ListTools
// failure is.
func TestConnectBackends_ResourceListFailureKeepsBackendTools(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})

	realHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil)
	backendHTTP := httptest.NewServer(denyMethodHandler(realHandler, "resources/list", "resources/templates/list"))
	defer backendHTTP.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	configs := []config.BackendConfig{
		{Name: "fake", Transport: "http", URL: backendHTTP.URL},
	}

	conn := connectBackends(context.Background(), logger, configs, nil)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()

	if _, ok := conn.backends["fake"]; !ok {
		t.Fatalf("backends = %v, want backend \"fake\" to still be connected despite its resources/list failing", conn.backends)
	}
	if len(conn.toolEntries) != 1 || len(conn.toolEntries[0].Items) != 1 || conn.toolEntries[0].Items[0].Name != "ping" {
		t.Fatalf("toolEntries = %+v, want one entry with tool \"ping\"", conn.toolEntries)
	}
	if len(conn.resourceEntries) != 1 || len(conn.resourceEntries[0].Items) != 0 {
		t.Fatalf("resourceEntries = %+v, want one entry with no items (a resources/list failure is treated as an empty list, not dropped)", conn.resourceEntries)
	}
	if len(conn.resourceTemplateEntries) != 1 || len(conn.resourceTemplateEntries[0].Items) != 0 {
		t.Fatalf("resourceTemplateEntries = %+v, want one entry with no items", conn.resourceTemplateEntries)
	}
}

// TestConnectBackends_PromptListFailureKeepsBackendTools is
// TestConnectBackends_ResourceListFailureKeepsBackendTools's counterpart for
// prompts/list: a backend whose prompts/list errors out must still be
// connected, with its tools intact, instead of being dropped entirely.
func TestConnectBackends_PromptListFailureKeepsBackendTools(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})

	realHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil)
	backendHTTP := httptest.NewServer(denyMethodHandler(realHandler, "prompts/list"))
	defer backendHTTP.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	configs := []config.BackendConfig{
		{Name: "fake", Transport: "http", URL: backendHTTP.URL},
	}

	conn := connectBackends(context.Background(), logger, configs, nil)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()

	if _, ok := conn.backends["fake"]; !ok {
		t.Fatalf("backends = %v, want backend \"fake\" to still be connected despite its prompts/list failing", conn.backends)
	}
	if len(conn.toolEntries) != 1 || len(conn.toolEntries[0].Items) != 1 || conn.toolEntries[0].Items[0].Name != "ping" {
		t.Fatalf("toolEntries = %+v, want one entry with tool \"ping\"", conn.toolEntries)
	}
	if len(conn.promptEntries) != 1 || len(conn.promptEntries[0].Items) != 0 {
		t.Fatalf("promptEntries = %+v, want one entry with no items (a prompts/list failure is treated as an empty list, not dropped)", conn.promptEntries)
	}
}

func TestConnectBackends_LogsSuccessfulConnect(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	configs := []config.BackendConfig{{Name: "fake", Transport: "http", URL: backendHTTP.URL}}

	conn := connectBackends(context.Background(), logger, configs, nil)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()

	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Msg       string `json:"msg"`
			Backend   string `json:"backend"`
			Transport string `json:"transport"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "backend connected" {
			found = true
			if rec.Backend != "fake" || rec.Transport != "http" {
				t.Fatalf("backend connected log = %+v, want backend=fake transport=http", rec)
			}
		}
	}
	if !found {
		t.Fatalf("log output = %q, want a \"backend connected\" entry", buf.String())
	}
}

// TestBuildGateway_NoListenerConfigured checks that buildGateway itself
// validates a listener is configured, independent of runServer's own
// earlier check -- watchSIGHUP (Task 5) relies on buildGateway to reject a
// reloaded config with no listener before swapping anything in.
func TestBuildGateway_NoListenerConfigured(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	if _, err := buildGateway(context.Background(), logger, cfg); err == nil {
		t.Fatal("buildGateway: expected error when no listener is configured, got nil")
	}
}

// TestBuildGateway_Success checks that buildGateway connects to every
// configured backend and returns a *gateway.Server exposing their tools --
// the same construction runServer used to do inline, now reusable for a
// SIGHUP-triggered reload.
func TestBuildGateway_Success(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	cfg := &config.Config{
		Listen: config.ListenConfig{HTTP: "127.0.0.1:0"},
		Backends: []config.BackendConfig{
			{Name: "fake", Transport: "http", URL: backendHTTP.URL},
		},
	}

	srv, err := buildGateway(context.Background(), logger, cfg)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}
	// Close the backend connection before the deferred backendHTTP.Close()
	// runs: httptest.Server.Close() blocks until all outstanding requests
	// complete, and a streamable-HTTP backend's standalone SSE stream stays
	// open indefinitely for server-initiated notifications, so leaving it
	// open here would hang backendHTTP.Close() (and the whole test binary)
	// instead of returning promptly.
	defer func() {
		for _, b := range srv.Backends() {
			_ = b.Close()
		}
	}()
	if len(srv.Backends()) != 1 {
		t.Fatalf("Backends() = %v, want 1 entry", srv.Backends())
	}
	if srv.Backend("fake") == nil {
		t.Fatal(`Backend("fake") = nil, want the connected backend`)
	}
}

func TestParseLogFormat(t *testing.T) {
	if _, err := parseLogFormat("text"); err != nil {
		t.Fatalf("parseLogFormat(\"text\"): %v", err)
	}
	if _, err := parseLogFormat("json"); err != nil {
		t.Fatalf("parseLogFormat(\"json\"): %v", err)
	}
	if _, err := parseLogFormat("bogus"); err == nil {
		t.Fatal("parseLogFormat(\"bogus\"): expected error, got nil")
	}

	newHandler, err := parseLogFormat("json")
	if err != nil {
		t.Fatalf("parseLogFormat(\"json\"): %v", err)
	}
	var buf bytes.Buffer
	logger := slog.New(newHandler(&buf, nil))
	logger.Info("hello")
	var rec struct {
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil || rec.Msg != "hello" {
		t.Fatalf("json handler output = %q, want a JSON line with msg=hello", buf.String())
	}
}

// TestRunServer_LogsListening checks runServer logs its listener
// configuration once at startup, before any listener goroutine starts. It
// swaps os.Stdin the same way TestServerCommand_StdioShutdownIsClean
// (server_test.go) does, so ServeStdio blocks on a pipe instead of the real
// stdin; this must not run with t.Parallel() (nor alongside another test
// touching os.Stdin).
func TestRunServer_LogsListening(t *testing.T) {
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

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("listen:\n  stdio: true\n\nbackends: []\n"), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, logger, configPath) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServer exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer did not exit within 5s of context cancellation")
	}

	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Msg   string `json:"msg"`
			Stdio bool   `json:"stdio"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "listening" {
			found = true
			if !rec.Stdio {
				t.Fatalf("listening log stdio = %v, want true", rec.Stdio)
			}
		}
	}
	if !found {
		t.Fatalf("log output = %q, want a \"listening\" entry", buf.String())
	}
}

// TestRunServer_ConfiguresGlobalTracerProvider checks that runServer wires
// internal/telemetry.Setup: after it starts, a span started via the
// package-level otel.Tracer (which delegates to whatever global provider
// is currently installed) is recording -- the default, pre-Setup global
// provider always returns a non-recording no-op span, so recording=true
// is only possible once Setup has installed a real SDK TracerProvider.
func TestRunServer_ConfiguresGlobalTracerProvider(t *testing.T) {
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

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("listen:\n  stdio: true\n\nbackends: []\n"), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, logger, configPath) }()

	deadline := time.Now().Add(2 * time.Second)
	var recording bool
	for time.Now().Before(deadline) {
		_, span := otel.Tracer("probe").Start(context.Background(), "probe")
		recording = span.IsRecording()
		span.End()
		if recording {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServer exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer did not exit within 5s of context cancellation")
	}

	if !recording {
		t.Fatal("global TracerProvider was not configured by runServer (span.IsRecording() = false)")
	}
}

// newFakeBackendHTTP starts an httptest server exposing a single tool named
// toolName, for buildGateway/watchSIGHUP tests that need a real backend to
// connect to. The caller must Close() the returned server.
func newFakeBackendHTTP(toolName string) *httptest.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: toolName, Description: toolName},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	return httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
}

// toolNamesOf lists resolved.Item.Name for every item in table.Items, for
// asserting which tools a *gateway.Server currently exposes without going
// through a real MCP session.
//
// Unused by any test in this file as of this task: gateway.Server exposes no
// accessor for its resolved *router.Table[*mcp.Tool] (only Backend/Backends/
// MCP), so nothing here can currently obtain one to pass in. Kept per this
// task's brief, which specifies it as reusable test infrastructure.
//
//nolint:unused
func toolNamesOf(table *router.Table[*mcp.Tool]) []string {
	var names []string
	for _, resolved := range table.Items {
		names = append(names, resolved.Item.Name)
	}
	sort.Strings(names)
	return names
}

// TestWatchSIGHUP_ReloadsConfig checks that a SIGHUP delivered to the test
// process makes watchSIGHUP rebuild the gateway from the (rewritten) config
// file and swap current to the new generation.
func TestWatchSIGHUP_ReloadsConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendA := newFakeBackendHTTP("from-a")
	defer backendA.Close()
	backendB := newFakeBackendHTTP("from-b")
	defer backendB.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeCfg := func(url string) {
		t.Helper()
		content := fmt.Sprintf("listen:\n  http: \"127.0.0.1:0\"\nbackends:\n  - name: b\n    transport: http\n    url: %q\n", url)
		if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
			t.Fatalf("writing config: %v", err)
		}
	}
	writeCfg(backendA.URL)

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	genCtx, genCancel := context.WithCancel(ctx)
	srv, err := buildGateway(genCtx, logger, cfg)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}
	// Generation 0's backend connection (backendA) is never closed by
	// scheduleDrain within this test's lifetime -- the production
	// reloadDrainTimeout default is 5 minutes, and this test doesn't shrink
	// it (TestScheduleDrain_ForceClosesBackendsAfterTimeout covers that
	// behavior directly). A streamable-HTTP backend's standalone SSE stream
	// stays open indefinitely for server-initiated notifications (see
	// TestBuildGateway_Success), so without an explicit Close() here, the
	// deferred backendA.Close()/backendB.Close() below would block forever
	// waiting for that connection to go idle. These defers run before
	// those (registered later, LIFO), closing both generations' backend
	// connections regardless of whether the SIGHUP swap below succeeds.
	defer func() {
		for _, b := range srv.Backends() {
			_ = b.Close()
		}
	}()
	current := new(atomic.Pointer[gateway.Server])
	current.Store(srv)
	defer func() {
		for _, b := range current.Load().Backends() {
			_ = b.Close()
		}
	}()

	done := make(chan struct{})
	go func() {
		watchSIGHUP(ctx, logger, configPath, current, genCancel)
		close(done)
	}()

	writeCfg(backendB.URL)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if current.Load() != srv {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	newSrv := current.Load()
	if newSrv == srv {
		t.Fatal("current was not swapped after SIGHUP")
	}
	if genCtx.Err() == nil {
		t.Fatal("generation 0's genCtx should be cancelled once superseded")
	}

	cancel()
	<-done
}

// syncBuffer is a bytes.Buffer guarded by a mutex, safe for a background
// goroutine (here, watchSIGHUP logging through it) and the test's own
// deadline poll loop to touch concurrently -- a plain bytes.Buffer would
// otherwise be a genuine data race under `go test -race`.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestWatchSIGHUP_IgnoresReloadWithoutHTTPListener checks that a SIGHUP
// whose newly-loaded config has no HTTP listener leaves current untouched
// and logs a warning, instead of swapping in a *gateway.Server nothing can
// reach (hot-reload is HTTP-only, see this plan's Global Constraints).
func TestWatchSIGHUP_IgnoresReloadWithoutHTTPListener(t *testing.T) {
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	backendA := newFakeBackendHTTP("from-a")
	defer backendA.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := fmt.Sprintf("listen:\n  http: \"127.0.0.1:0\"\nbackends:\n  - name: b\n    transport: http\n    url: %q\n", backendA.URL)
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	genCtx, genCancel := context.WithCancel(ctx)
	srv, err := buildGateway(genCtx, logger, cfg)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}
	current := new(atomic.Pointer[gateway.Server])
	current.Store(srv)
	// See the matching comment in TestWatchSIGHUP_ReloadsConfig: without an
	// explicit Close() here, the deferred backendA.Close() below would block
	// forever on backendA's still-open standalone SSE stream (current is
	// never swapped in this test, so nothing else closes it).
	defer func() {
		for _, b := range current.Load().Backends() {
			_ = b.Close()
		}
	}()

	done := make(chan struct{})
	go func() {
		watchSIGHUP(ctx, logger, configPath, current, genCancel)
		close(done)
	}()

	// Rewrite to a stdio-only config (no listen.http): buildGateway itself
	// would accept this (stdio is a valid listener), but watchSIGHUP must
	// refuse it as a hot-reload target.
	if err := os.WriteFile(configPath, []byte("listen:\n  stdio: true\nbackends: []\n"), 0o600); err != nil {
		t.Fatalf("rewriting config: %v", err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logBuf.String(), "hot-reload is only supported for HTTP listeners") {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logBuf.String(), "hot-reload is only supported for HTTP listeners") {
		t.Fatalf("log output = %q, want it to mention HTTP-only hot-reload", logBuf.String())
	}
	if current.Load() != srv {
		t.Fatal("current should not have been swapped")
	}

	cancel()
	<-done
}

// TestScheduleDrain_ForceClosesBackendsAfterTimeout checks that
// scheduleDrain cancels the superseded generation's context immediately,
// but only force-closes its backend connections once reloadDrainTimeout
// has elapsed.
func TestScheduleDrain_ForceClosesBackendsAfterTimeout(t *testing.T) {
	origTimeout := reloadDrainTimeout
	reloadDrainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { reloadDrainTimeout = origTimeout })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendA := newFakeBackendHTTP("from-a")
	defer backendA.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := fmt.Sprintf("listen:\n  http: \"127.0.0.1:0\"\nbackends:\n  - name: b\n    transport: http\n    url: %q\n", backendA.URL)
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	genCtx, genCancel := context.WithCancel(context.Background())
	srv, err := buildGateway(genCtx, logger, cfg)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}
	b := srv.Backend("b")
	if b == nil {
		t.Fatal(`Backend("b") = nil`)
	}

	scheduleDrain(logger, srv, genCancel)

	if genCtx.Err() == nil {
		t.Fatal("scheduleDrain should cancel genCtx immediately")
	}
	// Immediately after scheduleDrain returns, the backend must still be
	// usable (drain timeout hasn't elapsed yet).
	if _, err := b.ListTools(context.Background()); err != nil {
		t.Fatalf("backend should still be open immediately after scheduleDrain: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, lastErr = b.ListTools(context.Background()); lastErr != nil {
			return // force-closed as expected
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("backend was not force-closed within 2s of reloadDrainTimeout elapsing")
}
