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
	"os/signal"
	"path/filepath"
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
	live := new(generations)
	live.add(srv)
	defer func() {
		for _, b := range current.Load().Backends() {
			_ = b.Close()
		}
	}()

	// Register a throwaway SIGHUP handler synchronously, before spawning
	// watchSIGHUP (whose own signal.Notify runs inside its goroutine, racing
	// against the syscall.Kill below). Without this, if the kill lands
	// before that goroutine gets scheduled, the process has no registered
	// handler for SIGHUP yet and the Go runtime applies the default action
	// (terminate) -- killing the whole test binary instead of just this
	// test. Once ANY handler is registered for a signal, the runtime never
	// reverts to default disposition for it, and every registered channel
	// (this one and watchSIGHUP's own) receives every subsequent delivery,
	// so this doesn't interfere with watchSIGHUP actually seeing the signal.
	ready := make(chan os.Signal, 1)
	signal.Notify(ready, syscall.SIGHUP)
	defer signal.Stop(ready)

	done := make(chan struct{})
	go func() {
		watchSIGHUP(ctx, logger, configPath, startupListen{http: cfg.Listen.HTTP}, current, live, genCancel)
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

// closeWithin closes an httptest backend server and fails the test if that
// takes longer than d. httptest.Server.Close() blocks until every connection
// to it goes idle, and a streamable-HTTP MCP backend's standalone SSE stream
// only goes idle once the gateway side closes its session -- so a Close()
// that hangs here means some gateway generation's backend connection was
// never closed at all.
func closeWithin(t *testing.T, srv *httptest.Server, d time.Duration, what string) {
	t.Helper()
	closed := make(chan struct{})
	go func() {
		srv.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(d):
		t.Fatalf("%s: backend server did not close within %v; its gateway-side connection is still open", what, d)
	}
}

// TestRunServer_ShutdownClosesDrainingGenerations checks that process
// shutdown closes the backend connections of EVERY live generation, not just
// the one current points at: a generation superseded by a SIGHUP reload is
// still inside its reloadDrainTimeout window (5 minutes by default, not
// shrunk here on purpose -- so anything that gets closed during this test was
// closed by the shutdown path, never by scheduleDrain's own timer), and its
// backends are real subprocesses/containers in production, which must not be
// left behind when mcprt exits.
func TestRunServer_ShutdownClosesDrainingGenerations(t *testing.T) {
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	backendA := newFakeBackendHTTP("from-a")
	backendB := newFakeBackendHTTP("from-b")

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeCfg := func(url string) {
		t.Helper()
		content := fmt.Sprintf("listen:\n  http: \"127.0.0.1:0\"\nbackends:\n  - name: b\n    transport: http\n    url: %q\n", url)
		if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
			t.Fatalf("writing config: %v", err)
		}
	}
	writeCfg(backendA.URL)

	// See the matching comment in TestWatchSIGHUP_ReloadsConfig: register a
	// throwaway SIGHUP handler synchronously, before the process under test
	// can receive one, so a delivery that lands before runServer's watchSIGHUP
	// goroutine registers its own can never terminate the test binary.
	ready := make(chan os.Signal, 1)
	signal.Notify(ready, syscall.SIGHUP)
	defer signal.Stop(ready)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, logger, configPath) }()

	// Wait for runServer to get as far as spawning watchSIGHUP (which
	// registers its own signal.Notify) before signalling, so exactly one
	// reload happens: a SIGHUP that lands earlier would be dropped, and one
	// sent again later would build a third generation this test doesn't need.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logBuf.String(), "listening") {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	// Point the config at backend B and reload.
	writeCfg(backendB.URL)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logBuf.String(), "config reloaded") {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logBuf.String(), "config reloaded") {
		t.Fatalf("log output = %q, want a \"config reloaded\" entry", logBuf.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServer exited with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runServer did not exit within 10s of context cancellation")
	}

	// Generation 0 (backend A) was superseded but is still mid-drain; the
	// current generation (backend B) is the only one the old shutdown path
	// closed. Both must be closed by now.
	closeWithin(t, backendA, 5*time.Second, "superseded generation 0")
	closeWithin(t, backendB, 5*time.Second, "current generation")
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
	live := new(generations)
	live.add(srv)
	// See the matching comment in TestWatchSIGHUP_ReloadsConfig: without an
	// explicit Close() here, the deferred backendA.Close() below would block
	// forever on backendA's still-open standalone SSE stream (current is
	// never swapped in this test, so nothing else closes it).
	defer func() {
		for _, b := range current.Load().Backends() {
			_ = b.Close()
		}
	}()

	// See the matching comment in TestWatchSIGHUP_ReloadsConfig: register a
	// throwaway SIGHUP handler synchronously before spawning watchSIGHUP, so
	// the syscall.Kill below can never land while SIGHUP's disposition is
	// still "default" (which would terminate the whole test binary instead
	// of just this test).
	ready := make(chan os.Signal, 1)
	signal.Notify(ready, syscall.SIGHUP)
	defer signal.Stop(ready)

	done := make(chan struct{})
	go func() {
		watchSIGHUP(ctx, logger, configPath, startupListen{http: cfg.Listen.HTTP}, current, live, genCancel)
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

// TestWatchSIGHUP_ReportsListenChangesAgainstStartupListeners checks that
// both listener-related messages describe the RUNNING process, not the
// reloaded config: a changed listen.http is reported as not applied (listener
// re-binding needs a restart, so an operator who edited it must not be left
// thinking the new address is live), and the stdio warning stays silent for a
// process that never started a stdio listener, however the new config reads.
func TestWatchSIGHUP_ReportsListenChangesAgainstStartupListeners(t *testing.T) {
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	backendA := newFakeBackendHTTP("from-a")
	defer backendA.Close()
	backendB := newFakeBackendHTTP("from-b")
	defer backendB.Close()

	// The process is (pretend-)bound to boundAddr; the reloaded config below
	// asks for requestedAddr and additionally turns stdio on. Neither can take
	// effect. No listener is actually started here -- watchSIGHUP only ever
	// compares these strings.
	const boundAddr = "127.0.0.1:18888"
	const requestedAddr = "127.0.0.1:19999"

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeCfg := func(listen, url string) {
		t.Helper()
		content := fmt.Sprintf("listen:\n  http: %q\nbackends:\n  - name: b\n    transport: http\n    url: %q\n", listen, url)
		if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
			t.Fatalf("writing config: %v", err)
		}
	}
	writeCfg(boundAddr, backendA.URL)

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
	closeAll := func(s *gateway.Server) {
		for _, b := range s.Backends() {
			_ = b.Close()
		}
	}
	// See TestWatchSIGHUP_ReloadsConfig: both generations' backend connections
	// must be closed before the deferred backendA/backendB Close() calls, or
	// those block on a still-open standalone SSE stream.
	defer closeAll(srv)
	current := new(atomic.Pointer[gateway.Server])
	current.Store(srv)
	live := new(generations)
	live.add(srv)
	defer func() { closeAll(current.Load()) }()

	ready := make(chan os.Signal, 1)
	signal.Notify(ready, syscall.SIGHUP)
	defer signal.Stop(ready)

	done := make(chan struct{})
	go func() {
		watchSIGHUP(ctx, logger, configPath, startupListen{http: boundAddr, stdio: false}, current, live, genCancel)
		close(done)
	}()

	// The reloaded config asks for a different HTTP address AND a stdio
	// listener the process doesn't have.
	content := fmt.Sprintf("listen:\n  stdio: true\n  http: %q\nbackends:\n  - name: b\n    transport: http\n    url: %q\n", requestedAddr, backendB.URL)
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("rewriting config: %v", err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && current.Load() == srv {
		time.Sleep(20 * time.Millisecond)
	}
	if current.Load() == srv {
		t.Fatalf("current was not swapped after SIGHUP; log = %q", logBuf.String())
	}

	got := logBuf.String()
	if !strings.Contains(got, "listen.http change requires a process restart") {
		t.Fatalf("log output = %q, want a warning that the listen.http change was not applied", got)
	}
	if !strings.Contains(got, boundAddr) || !strings.Contains(got, requestedAddr) {
		t.Fatalf("log output = %q, want it to name both the running address %q and the requested one %q", got, boundAddr, requestedAddr)
	}
	if strings.Contains(got, "stdio session") {
		t.Fatalf("log output = %q, want no stdio-session warning: this process never started a stdio listener, so the reloaded config's listen.stdio is irrelevant", got)
	}

	cancel()
	<-done
}

// TestWatchSIGHUP_StdioWarningMentionsDrainForceClose checks the warning a
// stdio+http process gets on reload: the surviving stdio session is pinned to
// the superseded generation, whose backend connections scheduleDrain
// force-closes once reloadDrainTimeout elapses -- so saying only that it
// "keeps running under the old generation" would be true for at most that
// long. The message must carry the actual timeout value, not a hardcoded one.
func TestWatchSIGHUP_StdioWarningMentionsDrainForceClose(t *testing.T) {
	// Long enough that the drain timer never fires during this test, and
	// distinctive enough that finding it in the log proves the message
	// interpolates the real value.
	origTimeout := reloadDrainTimeout
	reloadDrainTimeout = 90 * time.Second
	t.Cleanup(func() { reloadDrainTimeout = origTimeout })

	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	backendA := newFakeBackendHTTP("from-a")
	defer backendA.Close()
	backendB := newFakeBackendHTTP("from-b")
	defer backendB.Close()

	const boundAddr = "127.0.0.1:18888"
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeCfg := func(url string) {
		t.Helper()
		content := fmt.Sprintf("listen:\n  stdio: true\n  http: %q\nbackends:\n  - name: b\n    transport: http\n    url: %q\n", boundAddr, url)
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
	closeAll := func(s *gateway.Server) {
		for _, b := range s.Backends() {
			_ = b.Close()
		}
	}
	defer closeAll(srv)
	current := new(atomic.Pointer[gateway.Server])
	current.Store(srv)
	live := new(generations)
	live.add(srv)
	defer func() { closeAll(current.Load()) }()

	ready := make(chan os.Signal, 1)
	signal.Notify(ready, syscall.SIGHUP)
	defer signal.Stop(ready)

	done := make(chan struct{})
	go func() {
		// The process really did start a stdio listener (stdio: true above).
		watchSIGHUP(ctx, logger, configPath, startupListen{http: boundAddr, stdio: true}, current, live, genCancel)
		close(done)
	}()

	writeCfg(backendB.URL)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && current.Load() == srv {
		time.Sleep(20 * time.Millisecond)
	}
	if current.Load() == srv {
		t.Fatalf("current was not swapped after SIGHUP; log = %q", logBuf.String())
	}

	got := logBuf.String()
	if !strings.Contains(got, "stdio session") {
		t.Fatalf("log output = %q, want a warning about the surviving stdio session", got)
	}
	if !strings.Contains(got, "force-closed") || !strings.Contains(got, "restart") {
		t.Fatalf("log output = %q, want the stdio warning to say the old generation's backends are force-closed and a process restart is required", got)
	}
	if !strings.Contains(got, reloadDrainTimeout.String()) {
		t.Fatalf("log output = %q, want it to name the actual drain timeout (%v)", got, reloadDrainTimeout)
	}

	cancel()
	<-done
}

// TestWatchSIGHUP_MalformedConfigKeepsCurrentGeneration covers the
// config.Load failure branch -- the most likely real-world SIGHUP outcome,
// since an operator can signal mid-edit: the reload is abandoned, the old
// generation stays live, and the failure is logged at error level.
func TestWatchSIGHUP_MalformedConfigKeepsCurrentGeneration(t *testing.T) {
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	backendA := newFakeBackendHTTP("from-a")
	defer backendA.Close()

	const boundAddr = "127.0.0.1:18888"
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := fmt.Sprintf("listen:\n  http: %q\nbackends:\n  - name: b\n    transport: http\n    url: %q\n", boundAddr, backendA.URL)
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
	live := new(generations)
	live.add(srv)
	// current is never swapped in this test, so nothing else closes
	// generation 0's connection before the deferred backendA.Close() above.
	defer func() {
		for _, b := range current.Load().Backends() {
			_ = b.Close()
		}
	}()

	ready := make(chan os.Signal, 1)
	signal.Notify(ready, syscall.SIGHUP)
	defer signal.Stop(ready)

	done := make(chan struct{})
	go func() {
		watchSIGHUP(ctx, logger, configPath, startupListen{http: boundAddr}, current, live, genCancel)
		close(done)
	}()

	// A tab-indented mapping is invalid YAML anywhere in the document, so
	// config.Load fails at the parse step -- the half-saved-file case.
	if err := os.WriteFile(configPath, []byte("listen:\n\thttp: \"127.0.0.1:18888\"\n"), 0o600); err != nil {
		t.Fatalf("rewriting config: %v", err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logBuf.String(), "config reload failed") {
		time.Sleep(20 * time.Millisecond)
	}
	got := logBuf.String()
	if !strings.Contains(got, "config reload failed, keeping current config") {
		t.Fatalf("log output = %q, want a \"config reload failed, keeping current config\" entry", got)
	}
	if !strings.Contains(got, "level=ERROR") {
		t.Fatalf("log output = %q, want the reload failure logged at error level", got)
	}
	if current.Load() != srv {
		t.Fatal("current must not be swapped when the reloaded config doesn't parse")
	}

	cancel()
	<-done
}

// TestWatchSIGHUP_ConsecutiveReloadsSupersedeEachGeneration checks that a
// SECOND SIGHUP supersedes generation 1 -- not generation 0 all over again.
// watchSIGHUP keeps the live generation's cancel func in genCancel and
// reassigns it after every swap; without that reassignment generation 0 would
// be cancelled twice while generation 1's context stayed live for the
// process's whole lifetime.
//
// Generation 1's cancellation is observed through the one thing its context
// still drives after buildGateway returned: its backends' list_changed
// callbacks (toolsChangedCallback derives each re-list context from the
// generation context). Once generation 1 is superseded, a list_changed
// notification from ITS backend must fail the re-list rather than quietly
// succeed -- while generation 2, built from the same code path, still lists
// fine.
func TestWatchSIGHUP_ConsecutiveReloadsSupersedeEachGeneration(t *testing.T) {
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	backendA := newFakeBackendHTTP("from-a")
	defer backendA.Close()
	// Generation 1's backend is built inline rather than via
	// newFakeBackendHTTP, because this test needs the *mcp.Server itself to
	// fire a tools/list_changed notification at it later.
	mcpB := mcp.NewServer(&mcp.Implementation{Name: "backend-b", Version: "v1"}, nil)
	noopTool := func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
		return nil, struct{}{}, nil
	}
	mcp.AddTool(mcpB, &mcp.Tool{Name: "from-b", Description: "from-b"}, noopTool)
	backendB := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpB }, nil))
	defer backendB.Close()
	backendC := newFakeBackendHTTP("from-c")
	defer backendC.Close()

	const boundAddr = "127.0.0.1:18888"
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeCfg := func(url string) {
		t.Helper()
		content := fmt.Sprintf("listen:\n  http: %q\nbackends:\n  - name: b\n    transport: http\n    url: %q\n", boundAddr, url)
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
	gen0, err := buildGateway(genCtx, logger, cfg)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}
	closeAll := func(s *gateway.Server) {
		for _, b := range s.Backends() {
			_ = b.Close()
		}
	}
	// Every generation this test creates is closed explicitly (see
	// TestWatchSIGHUP_ReloadsConfig): reloadDrainTimeout keeps its production
	// 5-minute default here, so scheduleDrain's timer never fires and the
	// deferred backendA/B/C Close() calls below would otherwise block on
	// still-open standalone SSE streams.
	defer closeAll(gen0)
	current := new(atomic.Pointer[gateway.Server])
	current.Store(gen0)
	live := new(generations)
	live.add(gen0)
	defer func() { closeAll(current.Load()) }()

	ready := make(chan os.Signal, 1)
	signal.Notify(ready, syscall.SIGHUP)
	defer signal.Stop(ready)

	done := make(chan struct{})
	go func() {
		watchSIGHUP(ctx, logger, configPath, startupListen{http: boundAddr}, current, live, genCancel)
		close(done)
	}()

	reload := func(prev *gateway.Server, url string) *gateway.Server {
		t.Helper()
		writeCfg(url)
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatalf("sending SIGHUP: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && current.Load() == prev {
			time.Sleep(20 * time.Millisecond)
		}
		next := current.Load()
		if next == prev {
			t.Fatalf("current was not swapped after SIGHUP; log = %q", logBuf.String())
		}
		return next
	}

	gen1 := reload(gen0, backendB.URL)
	defer closeAll(gen1)
	gen2 := reload(gen1, backendC.URL)
	if gen2 == gen0 {
		t.Fatal("the second reload must build a third generation, not restore generation 0")
	}

	// Generation 2 is live and healthy: its own backend still lists fine.
	if _, err := gen2.Backend("b").ListTools(context.Background()); err != nil {
		t.Fatalf("generation 2's backend should be usable: %v", err)
	}

	// Generation 1 is superseded, so its context must be cancelled: a
	// list_changed notification from its backend now fails the re-list. Each
	// AddTool fires one notification, so keep firing until the failure shows
	// up -- the very first one could race scheduleDrain's cancel, which
	// watchSIGHUP calls just after the swap this test polls for.
	deadline := time.Now().Add(5 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		if strings.Contains(logBuf.String(), "list_changed: re-list failed") {
			break
		}
		mcp.AddTool(mcpB, &mcp.Tool{Name: fmt.Sprintf("extra-%d", i), Description: "extra"}, noopTool)
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(logBuf.String(), "list_changed: re-list failed") {
		t.Fatalf("log output = %q, want generation 1's re-list to fail: its generation context should have been cancelled when generation 2 superseded it", logBuf.String())
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

	// scheduleDrain's timer only closes a generation it can still take out of
	// the live set (that's what keeps it from double-closing one process
	// shutdown already closed), so register srv there first, exactly as
	// runServer/watchSIGHUP do.
	live := new(generations)
	live.add(srv)
	scheduleDrain(logger, srv, genCancel, live)

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
