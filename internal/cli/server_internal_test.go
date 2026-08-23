package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"

	"github.com/wtnb75/mcprt/internal/config"
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
		conn = connectBackends(context.Background(), logger, configs)
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

	conn := connectBackends(context.Background(), logger, configs)
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

	conn := connectBackends(context.Background(), logger, configs)
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

	conn := connectBackends(context.Background(), logger, configs)
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
