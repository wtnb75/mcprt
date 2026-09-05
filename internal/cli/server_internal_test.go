package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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

	"github.com/wtnb75/mcprt/internal/backend"
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

	ctx := t.Context() // stop the "hung" backend's supervisor from retrying forever after this test returns

	done := make(chan struct{})
	var conn connected
	go func() {
		conn = connectBackends(ctx, logger, configs, nil)
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

	ctx, cancel := context.WithCancel(context.Background())
	conn := connectBackends(ctx, logger, configs, nil)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()
	defer cancel() // registered last -> runs first, before the backend-close loop above

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

	ctx, cancel := context.WithCancel(context.Background())
	conn := connectBackends(ctx, logger, configs, nil)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()
	defer cancel()

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

	ctx, cancel := context.WithCancel(context.Background())
	conn := connectBackends(ctx, logger, configs, nil)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()
	defer cancel()

	var found bool
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
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

// freeAddr reserves an OS-assigned TCP port, releases it immediately, and
// returns its address -- for tests that need to know an address in advance
// (before anything is listening there) and bind to it later.
func freeAddr(t *testing.T) string {
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

// connectBackendsWaitable is connectBackends plus the one thing tests need
// that production doesn't: a channel that closes once every spawned
// superviseBackend goroutine has actually returned. connectBackends itself
// returns as soon as its collection window closes, without ever waiting for
// the (deliberately long-lived) supervisor goroutines it spawns to exit --
// by design, since in production they run for the life of the generation. A
// test that cancels ctx and wants to assert nothing of its own survives past
// that point (so it can't leak a goroutine that goes on reading
// package-level test vars like backendConnectTimeout into a LATER test -- a
// real `go test -race` failure, confirmed by reproducing it) waits on this
// before returning.
//
// It is a straight alias for the production superviseBackends rather than a
// second implementation: an earlier copy of connectBackends' collection loop
// lived here and had already gone stale, missing the config-order-preserving
// and distinct-index-counting fixes production had since grown.
func connectBackendsWaitable(ctx context.Context, logger *slog.Logger, configs []config.BackendConfig, gwH *gwHolder) (connected, <-chan struct{}) {
	return superviseBackends(ctx, logger, configs, gwH)
}

// waitSupervisorsDone waits for done (from connectBackendsWaitable) to
// close, failing the test if that takes too long -- ctx must already be
// cancelled and every backend connection superviseBackend could be waiting
// on already closed by the time this is called, so a healthy supervisor
// returns almost immediately (its next connectAndList attempt fails fast
// against the cancelled ctx); taking longer than this means something is
// stuck, not just slow.
func waitSupervisorsDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor goroutine(s) did not return within 5s of ctx cancellation")
	}
}

// TestSuperviseBackend_ReconnectsAfterDisconnect checks the full
// disconnect -> item removal -> automatic reconnect -> item restoration
// cycle, driven through connectBackends + a real *gateway.Server, exactly
// as buildGateway wires them together.
func TestSuperviseBackend_ReconnectsAfterDisconnect(t *testing.T) {
	origMin, origMax := backendBackoffMin, backendBackoffMax
	backendBackoffMin, backendBackoffMax = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { backendBackoffMin, backendBackoffMax = origMin, origMax })

	backendHTTP := newFakeBackendHTTP("ping")
	defer backendHTTP.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())

	gwH := &gwHolder{}
	conn, supervisorsDone := connectBackendsWaitable(ctx, logger, []config.BackendConfig{{Name: "fake", Transport: "http", URL: backendHTTP.URL}}, gwH)
	firstConn, ok := conn.backends["fake"]
	if !ok {
		t.Fatalf("backends = %v, want backend \"fake\" connected", conn.backends)
	}

	toolTable := router.Resolve(conn.toolEntries, gateway.ToolNameOf, gateway.ToolRename, nil)
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: conn.backends,
		Tables:   gateway.Tables{Tools: toolTable},
		Entries:  gateway.Entries{Tools: conn.toolEntries},
	})
	gwH.ptr.Store(srv)
	// These three defers run in the OPPOSITE order they're registered in
	// (LIFO), so registering them bottom-to-top of the intended sequence
	// gives, at test end: (1) cancel ctx first, so superviseBackend's retry
	// loop can't win a race and reconnect one more time between the Close()
	// below unblocking its Session.Wait() and the goroutine actually exiting
	// -- exactly the ordering Step 1 established for the four pre-existing
	// connectBackends tests; (2) close whatever backend connection is live
	// for "fake" (the reconnected one by now) plus firstConn (a no-op if
	// reconnect already replaced it in srv.Backends(), Close being
	// idempotent, kept in case an earlier Fatalf returned before any
	// reconnect happened) -- both before the deferred backendHTTP.Close()
	// further below runs, since a streamable-HTTP backend's standalone SSE
	// stream stays open indefinitely for server-initiated notifications (see
	// TestBuildGateway_Success) and would otherwise hang it forever; then
	// (3) wait for the supervisor goroutine spawned by
	// connectBackendsWaitable above to have actually returned. cancel() (1)
	// alone only starts that goroutine's unwind asynchronously -- without
	// this wait, it can survive past this test's return and go on reading
	// package-level test vars like backendConnectTimeout, racing with a
	// LATER test's own writes to them (reproduced as a genuine `go test
	// -race` failure before this wait was added).
	defer waitSupervisorsDone(t, supervisorsDone)
	defer func() {
		for _, b := range srv.Backends() {
			_ = b.Close()
		}
		_ = firstConn.Close()
	}()
	defer cancel()

	toolNames := func() []string {
		gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
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
				t.Fatalf("listing tools: %v", err)
			}
			names = append(names, tool.Name)
		}
		return names
	}

	if got := toolNames(); len(got) != 1 || got[0] != "ping" {
		t.Fatalf("initial tools = %v, want [ping]", got)
	}

	// Client-initiated Close() is a terminal disconnect (unlike a server-side
	// connection reset, which go-sdk's streamable transport would silently
	// auto-heal on its own -- see this plan's Global Constraints), so this
	// reliably fires Session.Wait() inside superviseBackend.
	if err := firstConn.Close(); err != nil {
		t.Fatalf("closing backend connection: %v", err)
	}

	// superviseBackend's disconnect -> clear -> reconnect -> re-register
	// cycle can complete in well under a millisecond against a local
	// httptest backend (backoff is shrunk above, and there is no backoff at
	// all on the graceful-disconnect path -- see superviseBackend), so a
	// second, freshly-issued toolNames() call after this loop breaks could
	// race past the empty window and observe the *next* reconnect's tools
	// instead. Deciding on the sampled value from inside the loop (like the
	// reconnect-wait loop below already does) instead of re-querying
	// afterward avoids that race while still proving superviseBackend
	// actually cleared the tools at some point within the deadline.
	deadline := time.Now().Add(2 * time.Second)
	var sawEmpty bool
	for time.Now().Before(deadline) {
		if len(toolNames()) == 0 {
			sawEmpty = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawEmpty {
		t.Fatal("tools never went empty within 2s of disconnect (superviseBackend must clear them)")
	}

	// The fake backend server is still up -- superviseBackend's retry loop
	// (backoff shrunk above) should reconnect automatically.
	deadline = time.Now().Add(2 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = toolNames()
		if len(got) == 1 && got[0] == "ping" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != 1 || got[0] != "ping" {
		t.Fatalf("tools after automatic reconnect = %v, want [ping]", got)
	}
	if srv.Backends()["fake"] == firstConn {
		t.Fatalf("Backends()[\"fake\"] still points at the closed connection, want a fresh one from the reconnect")
	}
}

// TestSuperviseBackend_LateConnectJoinsViaConnectBackend checks that a
// backend which failed to connect within connectBackends' collection
// window (backendConnectTimeout) joins the gateway automatically via
// ConnectBackend once it eventually succeeds -- the "backend that failed
// to connect at mcprt startup" case from this plan's spec.
func TestSuperviseBackend_LateConnectJoinsViaConnectBackend(t *testing.T) {
	origTimeout := backendConnectTimeout
	backendConnectTimeout = 100 * time.Millisecond
	t.Cleanup(func() { backendConnectTimeout = origTimeout })
	origMin, origMax := backendBackoffMin, backendBackoffMax
	backendBackoffMin, backendBackoffMax = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { backendBackoffMin, backendBackoffMax = origMin, origMax })

	addr := freeAddr(t) // nothing listens here yet

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	var supervisorsDone <-chan struct{}
	// Registered first, so LIFO runs it dead last, after every other cleanup
	// step below (cancel, then server-side close, then client-side close)
	// has already run: without waiting for the supervisor goroutine spawned
	// by connectBackendsWaitable to actually return, it can survive past
	// this test's return and go on reading package-level test vars like
	// backendConnectTimeout, racing with a LATER test's own writes to them
	// (see connectBackendsWaitable's doc comment; reproduced as a genuine
	// `go test -race` failure before this wait was added).
	defer func() { waitSupervisorsDone(t, supervisorsDone) }()

	gwH := &gwHolder{}
	conn, supervisorsDone := connectBackendsWaitable(ctx, logger, []config.BackendConfig{{Name: "late", Transport: "http", URL: "http://" + addr}}, gwH)
	if _, ok := conn.backends["late"]; ok {
		t.Fatalf("backends = %v, want \"late\" excluded (nothing was listening within backendConnectTimeout)", conn.backends)
	}

	srv := gateway.New(gateway.NewConfig{Logger: logger, Backends: conn.backends})
	gwH.ptr.Store(srv) // matches buildGateway: gwH.ptr is populated right after gateway.New

	// Now start listening on the SAME address the config already points at.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listening on %s: %v", addr, err)
	}
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "late-tool", Description: "late-tool"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	lateHTTP := &http.Server{Handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil)}
	go func() { _ = lateHTTP.Serve(ln) }()
	// The next three defers run (LIFO) in this order: close the client-side
	// backend connection FIRST -- a server-side-only close (lateHTTP.Close()
	// below) is exactly the kind of disconnect go-sdk's streamable transport
	// can silently auto-heal on its own (see
	// TestSuperviseBackend_ReconnectsAfterDisconnect's matching comment on
	// firstConn.Close()), so without an explicit client-side Close() here,
	// superviseBackend's Session.Wait() might never return at all, and
	// waitSupervisorsDone above would time out waiting for it forever; then
	// close the now-redundant lateHTTP listener; then cancel ctx last, so
	// superviseBackend's post-disconnect reconnect attempt (triggered by the
	// client-side close above) fails against an already-cancelled ctx
	// instead of racing to actually succeed against the now-closed lateHTTP.
	defer cancel()
	defer func() { _ = lateHTTP.Close() }()
	defer func() {
		if b := srv.Backend("late"); b != nil {
			_ = b.Close()
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && srv.Backend("late") == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.Backend("late") == nil {
		t.Fatal("srv.Backend(\"late\") = nil after starting the backend, want it registered via superviseBackend's retry")
	}
}

// TestSuperviseBackend_StopsRetryingWhenContextCancelled checks that
// cancelling ctx makes superviseBackend's retry loop return promptly,
// instead of continuing to retry forever -- this is what lets a superseded
// hot-reload generation's supervisors wind down (see this plan's header
// note on genCtx scoping).
func TestSuperviseBackend_StopsRetryingWhenContextCancelled(t *testing.T) {
	origMin, origMax := backendBackoffMin, backendBackoffMax
	backendBackoffMin, backendBackoffMax = 50*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { backendBackoffMin, backendBackoffMax = origMin, origMax })

	addr := freeAddr(t) // nothing ever listens here -- every attempt fails
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		superviseBackend(ctx, logger, config.BackendConfig{Name: "unreachable", Transport: "http", URL: "http://" + addr}, nil, nil, nil)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond) // let it start its first (failing) attempt
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("superviseBackend did not return within 2s of ctx cancellation")
	}
}

// TestSuperviseBackend_LogsErrorOnceThenWarnForRepeatedFailures checks that
// a backend stuck failing to connect logs its FIRST failure at Error (so an
// operator notices immediately), but every failure after that at Warn --
// otherwise a backend down for hours logs an Error line at least once a
// minute even at max backoff, which either pages on-call for a condition
// that's already known and retrying on its own, or trains them to ignore
// Error-level alerts altogether.
func TestSuperviseBackend_LogsErrorOnceThenWarnForRepeatedFailures(t *testing.T) {
	origMin, origMax := backendBackoffMin, backendBackoffMax
	backendBackoffMin, backendBackoffMax = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { backendBackoffMin, backendBackoffMax = origMin, origMax })

	addr := freeAddr(t) // nothing ever listens here -- every attempt fails
	var buf syncBuffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		superviseBackend(ctx, logger, config.BackendConfig{Name: "unreachable", Transport: "http", URL: "http://" + addr}, nil, nil, nil)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && strings.Count(buf.String(), "backend connect failed, retrying") < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("superviseBackend did not return within 2s of ctx cancellation")
	}

	var levels []string
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "backend connect failed, retrying" {
			levels = append(levels, rec.Level)
		}
	}
	if len(levels) < 3 {
		t.Fatalf("got %d retry log lines, want at least 3 to check level downgrade: %v", len(levels), levels)
	}
	if levels[0] != "ERROR" {
		t.Fatalf("first retry log level = %q, want ERROR", levels[0])
	}
	for i, l := range levels[1:] {
		if l != "WARN" {
			t.Fatalf("retry log line %d level = %q, want WARN", i+1, l)
		}
	}
}

// TestCollectConnectResults_FlappingBackendDoesNotDropHealthyBackend checks
// that a backend index which reports MORE THAN ONCE inside the collection
// window (a flapping backend: connect, disconnect, reconnect, all before
// connectBackends' deadline fires -- superviseBackend's onFirstConnect fires
// on every successful connect for as long as gwH.ptr is still nil, see its
// doc comment) does not consume a slot meant for a DIFFERENT, healthy
// backend's result -- this plan's Task 2 review, finding 2.
//
// The OLD collection loop ("for range configs { select { case r :=
// <-resultCh: results[r.i] = r.c } }") counted raw channel receives against
// len(configs), not distinct backend indices. With 2 configs and this exact
// arrival order -- flappy's (index 0) two results both already buffered
// ahead of healthy's (index 1) single result -- that loop would spend both
// of its 2 permitted iterations reading index 0's two sends and never reach
// healthy's already-buffered result: a backend that connected fine gets
// silently excluded from the founding set, and since its supervisor is now
// parked in Session.Wait() believing it already reported, it never joins
// later via ConnectBackend either. collectConnectResults fixes this by
// counting DISTINCT indices filled, not raw receives, and closing whichever
// result an in-window reconnect supersedes.
func TestCollectConnectResults_FlappingBackendDoesNotDropHealthyBackend(t *testing.T) {
	backendHTTP := newFakeBackendHTTP("ping")
	defer backendHTTP.Close()

	connectOne := func(name string) *backend.Backend {
		t.Helper()
		b, err := backend.Connect(context.Background(), config.BackendConfig{Name: name, Transport: "http", URL: backendHTTP.URL}, backend.ChangeCallbacks{})
		if err != nil {
			t.Fatalf("connecting fake backend %q: %v", name, err)
		}
		return b
	}

	// flappyFirst simulates the connection that a flapping backend's
	// supervisor reports first, before disconnecting and reconnecting --
	// flappySecond is the reconnect's result. healthy is a different,
	// perfectly healthy backend that connects exactly once.
	flappyFirst := &connectResult{backend: connectOne("flappy")}
	flappySecond := &connectResult{backend: connectOne("flappy")}
	healthy := &connectResult{backend: connectOne("healthy")}
	defer func() {
		// flappyFirst is expected to already be closed BY
		// collectConnectResults itself (it's the result an in-window
		// reconnect supersedes); the other two are still live here and
		// owned by this test.
		_ = flappySecond.backend.Close()
		_ = healthy.backend.Close()
	}()

	resultCh := make(chan indexedConnectResult, 4)
	// Exactly the arrival order that broke the OLD collection loop: both of
	// flappy's (index 0) results land before healthy's (index 1) result.
	resultCh <- indexedConnectResult{0, flappyFirst}
	resultCh <- indexedConnectResult{0, flappySecond}
	resultCh <- indexedConnectResult{1, healthy}

	results := collectConnectResults(resultCh, 2, make(chan time.Time), nil) // a deadline that never fires

	if results[0] != flappySecond {
		t.Fatalf("results[0] = %+v, want flappySecond (the latest connect for a flapping backend wins)", results[0])
	}
	if results[1] != healthy {
		t.Fatalf("results[1] = %v, want healthy's result -- it must not be dropped just because a DIFFERENT backend (flappy) reported twice", results[1])
	}

	// flappyFirst must have been closed as part of being superseded --
	// verify by confirming it can no longer serve a request, bounded so a
	// bug here fails the test instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := flappyFirst.backend.ListTools(ctx); err == nil {
		t.Fatal("flappyFirst.ListTools succeeded after being superseded, want collectConnectResults to have closed it")
	}
}

// TestConnectBackends_ReturnsWithoutWaitingOutTimeoutForADeadBackend checks
// that a backend which is down (and stays down) does not make connectBackends
// burn the whole backendConnectTimeout before returning: every supervisor
// signals once its OWN first connectAndList attempt has resolved -- success or
// failure -- and once all of them have, the collection window closes
// immediately instead of waiting for a slot that a permanently-failing backend
// will never fill.
//
// Before this, the collection loop's only exit for a failing backend was the
// deadline itself, so `mcprt list`/`mcprt call` against a dead backend took
// the full 30s default, and runServer's HTTP listener bind (which happens
// AFTER buildGateway) was delayed by the same 30s -- a readiness-probe hazard
// in container deployments.
func TestConnectBackends_ReturnsWithoutWaitingOutTimeoutForADeadBackend(t *testing.T) {
	origTimeout := backendConnectTimeout
	backendConnectTimeout = 10 * time.Second
	t.Cleanup(func() { backendConnectTimeout = origTimeout })
	origMin, origMax := backendBackoffMin, backendBackoffMax
	backendBackoffMin, backendBackoffMax = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { backendBackoffMin, backendBackoffMax = origMin, origMax })

	addr := freeAddr(t) // nothing ever listens here -- every attempt fails at once
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	var supervisorsDone <-chan struct{}
	// Registered first -> runs last (LIFO), after cancel() below: the
	// supervisor keeps retrying this dead backend forever otherwise, reading
	// the package-level vars this test writes above while a LATER test writes
	// them too (see connectBackendsWaitable's doc comment).
	defer func() { waitSupervisorsDone(t, supervisorsDone) }()
	defer cancel()

	start := time.Now()
	conn, supervisorsDone := connectBackendsWaitable(ctx, logger,
		[]config.BackendConfig{{Name: "down", Transport: "http", URL: "http://" + addr}}, nil)
	elapsed := time.Since(start)

	if len(conn.backends) != 0 {
		t.Fatalf("backends = %v, want none (nothing is listening on %s)", conn.backends, addr)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("connectBackends took %v with backendConnectTimeout=%v; it must return as soon as every backend's FIRST attempt has resolved, not wait out the timeout for a backend that is merely retrying", elapsed, backendConnectTimeout)
	}
}

// TestCollectConnectResults_KeepsResultAlreadyBufferedWhenDeadlineFires checks
// that a result which has ALREADY landed in resultCh when the collection
// window closes is still picked up, instead of being discarded by whichever
// select branch Go happens to pick.
//
// Go's select chooses uniformly at random among ready cases, so with both a
// fired deadline and a buffered result ready, the old loop dropped the result
// about half the time -- and the backend it belonged to was then excluded from
// the founding set for good: its supervisor is parked in Session.Wait()
// believing it already reported, so it never joins later via ConnectBackend
// either. The iteration count below turns that coin flip into a
// deterministic failure.
func TestCollectConnectResults_KeepsResultAlreadyBufferedWhenDeadlineFires(t *testing.T) {
	for i := range 200 {
		healthy := &connectResult{backend: &backend.Backend{Name: "healthy"}}
		resultCh := make(chan indexedConnectResult, 2)
		resultCh <- indexedConnectResult{1, healthy}

		deadline := make(chan time.Time)
		close(deadline) // an already-fired deadline: always ready

		results := collectConnectResults(resultCh, 2, deadline, nil)
		if results[1] != healthy {
			t.Fatalf("iteration %d: results[1] = %v, want healthy's already-buffered result -- collectConnectResults must drain what already arrived before returning", i, results[1])
		}
	}
}

// serveIdenticalToolBackend starts an MCP backend on ln exposing a single
// "ping" tool whose name, description and input schema are identical no
// matter which payload it serves -- only the text its handler returns
// differs. Two such backends are therefore indistinguishable to the
// gateway's value-equality reconcile diff (reflect.DeepEqual over the
// resolved *mcp.Tool), while still being distinguishable to the test that
// calls them.
func serveIdenticalToolBackend(ln net.Listener, payload string) *http.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "ping", Description: "ping", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: payload}}}, nil
		})
	h := &http.Server{Handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)}
	go func() { _ = h.Serve(ln) }()
	return h
}

// TestConnectBackend_ReconnectRebindsHandlerAfterDisconnectMissedWhileHolderNil
// reproduces the narrow publication window where gwH.ptr is still nil (see
// superviseBackend): a backend that made the founding set disconnects before
// gateway.New has run, so superviseBackend's disconnect-triggered clear
// (gw.UpdateTools(name, nil)) is skipped -- there is no gw to call it on yet.
// By the time the supervisor reconnects, gwH.ptr IS populated, so the
// reconnect goes through ConnectBackend -- whose UpdateTools diff compares
// the newly-listed tools against a table that was never cleared. When the
// reconnected backend's tool set is value-identical to the one still
// registered from the FIRST, now-dead connection, that diff sees no change
// at all and skips re-registration, leaving the gateway's tool handler bound
// to the closed *backend.Backend forever: every tools/call fails permanently.
//
// What actually changed here is the backend OBJECT'S IDENTITY, which a
// value-equality diff cannot observe -- so ConnectBackend must force
// re-registration of everything the (re)connecting backend owns.
func TestConnectBackend_ReconnectRebindsHandlerAfterDisconnectMissedWhileHolderNil(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := t.Context()

	addr := freeAddr(t)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listening on %s: %v", addr, err)
	}
	first := serveIdenticalToolBackend(ln, "from-first-connection")

	bc := config.BackendConfig{Name: "fake", Transport: "http", URL: "http://" + addr}

	// 1. The founding connect plus gateway.New, exactly as
	//    connectBackends/buildGateway wire them.
	c1, err := connectAndList(ctx, logger, bc, backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("first connect: %v", err)
	}
	entries := []router.Entry[*mcp.Tool]{{BackendName: bc.Name, Items: c1.tools}}
	table := router.Resolve(entries, gateway.ToolNameOf, gateway.ToolRename, nil)
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{bc.Name: c1.backend},
		Tables:   gateway.Tables{Tools: table},
		Entries:  gateway.Entries{Tools: entries},
	})

	// 2. The disconnect lands while gwH.ptr is still nil, so no clear runs:
	//    the gateway keeps serving "ping" through c1's dead connection.
	if err := c1.backend.Close(); err != nil {
		t.Fatalf("closing the first backend connection: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("stopping the first backend server: %v", err)
	}

	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("re-listening on %s: %v", addr, err)
	}
	second := serveIdenticalToolBackend(ln2, "from-second-connection")
	defer func() { _ = second.Close() }()

	// 3. gwH.ptr is populated by now, so the supervisor's reconnect goes
	//    through ConnectBackend -- with a tool set identical to what is
	//    still registered.
	c2, err := connectAndList(ctx, logger, bc, backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer func() { _ = c2.backend.Close() }()
	srv.ConnectBackend(bc.Name, c2.backend, bc.Prefix, c2.tools, c2.resources, c2.resourceTemplates, c2.prompts)

	// 4. A downstream tools/call must reach the NEW connection.
	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ping"})
	if err != nil {
		t.Fatalf("tools/call after reconnect: %v -- the gateway's handler is still bound to the first, closed connection", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("tools/call content = %+v, want exactly one text content", res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("tools/call content[0] = %T, want *mcp.TextContent", res.Content[0])
	}
	if text.Text != "from-second-connection" {
		t.Fatalf("tools/call reached %q, want %q (the reconnected backend)", text.Text, "from-second-connection")
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

	// A cancellable context, not context.Background(): buildGateway's
	// connectBackends spawns a persistent superviseBackend goroutine for
	// "fake" that retries forever for as long as its ctx stays alive.
	// context.Background() is never cancelled, so that goroutine would
	// outlive this test and go on reading package-level vars like
	// backendConnectTimeout in the background, racing a LATER test's writes
	// to them -- reproduced as a genuine `go test -race` failure (this
	// plan's Task 2 review, finding 1) before this fix.
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := buildGateway(ctx, logger, cfg)
	if err != nil {
		cancel()
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
	// Registered last (LIFO) -> runs first, before the backend-close loop
	// above: cancel the supervisor's ctx before closing its connection, so
	// it can't win a race and reconnect one more time between the Close()
	// above unblocking its Session.Wait() and the goroutine actually seeing
	// ctx.Done() -- matching the pattern already established for
	// TestConnectBackends_* (see e.g.
	// TestConnectBackends_ResourceListFailureKeepsBackendTools).
	defer cancel()
	if len(srv.Backends()) != 1 {
		t.Fatalf("Backends() = %v, want 1 entry", srv.Backends())
	}
	if srv.Backend("fake") == nil {
		t.Fatal(`Backend("fake") = nil, want the connected backend`)
	}
}

// TestBuildGateway_LogsNameConflictWithSingularKind checks that buildGateway's
// own name_conflict logging (the four LogEvent calls right after each
// router.Resolve call in buildGateway) reports kind="tool" -- the SAME
// singular spelling internal/gateway/reconcile.go's logNewConflicts uses --
// for a real tool-name conflict between two backends, driven entirely
// through buildGateway's actual code path (two backends each exposing a
// tool named "dup", not a hand-constructed router.Conflict). This locks the
// cross-file agreement Finding 1 fixed: a future edit that reintroduces a
// "tool"/"tools" (or any other) mismatch between buildGateway and
// logNewConflicts would fail this test.
func TestBuildGateway_LogsNameConflictWithSingularKind(t *testing.T) {
	backendA := newFakeBackendHTTP("dup")
	defer backendA.Close()
	backendB := newFakeBackendHTTP("dup")
	defer backendB.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	cfg := &config.Config{
		Listen: config.ListenConfig{HTTP: "127.0.0.1:0"},
		Backends: []config.BackendConfig{
			{Name: "a", Transport: "http", URL: backendA.URL},
			{Name: "b", Transport: "http", URL: backendB.URL},
		},
	}

	// A cancellable context, not context.Background() -- see
	// TestBuildGateway_Success's matching comment on why this matters for
	// `go test -race`.
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := buildGateway(ctx, logger, cfg)
	if err != nil {
		cancel()
		t.Fatalf("buildGateway: %v", err)
	}
	defer func() {
		for _, b := range srv.Backends() {
			_ = b.Close()
		}
	}()
	defer cancel()

	var rec map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err == nil && r["event"] == gateway.EventNameConflict {
			rec = r
			break
		}
	}
	if rec == nil {
		t.Fatalf("log output = %q, want a line with event=%s for the \"dup\" tool-name conflict between backends \"a\" and \"b\"", buf.String(), gateway.EventNameConflict)
	}
	if rec["kind"] != "tool" {
		t.Fatalf("kind = %v, want \"tool\" (not \"tools\") -- must match logNewConflicts' vocabulary", rec["kind"])
	}
	if rec["name"] != "dup" {
		t.Fatalf("name = %v, want \"dup\"", rec["name"])
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
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
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

// closeErrConnection wraps a real mcp.Connection, forwarding everything to
// it except Close, which closes the real connection but then returns a
// fixed error instead of that connection's own result -- used to
// deterministically force a *backend.Backend's Close() to return an error,
// so tests can check that error is actually surfaced instead of discarded.
type closeErrConnection struct {
	mcp.Connection
	err error
}

func (c closeErrConnection) Close() error {
	_ = c.Connection.Close()
	return c.err
}

// closeErrTransport wraps a real mcp.Transport, making the Connection its
// Connect returns a closeErrConnection.
type closeErrTransport struct {
	inner mcp.Transport
	err   error
}

func (t closeErrTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return closeErrConnection{conn, t.err}, nil
}

// backendWithCloseError returns a live *backend.Backend (backed by a real,
// working in-memory MCP session) whose Close() is guaranteed to return
// closeErr instead of nil, regardless of whether the underlying session
// itself closes cleanly.
func backendWithCloseError(t *testing.T, name string, closeErr error) *backend.Backend {
	t.Helper()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	ctx := context.Background()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, closeErrTransport{inner: clientTransport, err: closeErr}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	return &backend.Backend{Name: name, Session: session}
}

// TestCloseBackends_LogsErrorInsteadOfDiscarding checks that closeBackends
// (shared by runServer's final shutdown and scheduleDrain's forced close)
// logs a backend's Close error at Warn instead of silently discarding it.
func TestCloseBackends_LogsErrorInsteadOfDiscarding(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	b := backendWithCloseError(t, "flaky", errors.New("boom"))

	closeBackends(logger, map[string]*backend.Backend{"flaky": b})

	var found bool
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Level   string `json:"level"`
			Msg     string `json:"msg"`
			Backend string `json:"backend"`
			Error   string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "closing backend connection" {
			found = true
			if rec.Level != "WARN" || rec.Backend != "flaky" || rec.Error != "boom" {
				t.Fatalf("log line = %+v, want level=WARN backend=flaky error=boom", rec)
			}
		}
	}
	if !found {
		t.Fatalf("log output = %q, want a \"closing backend connection\" entry", buf.String())
	}
}

// TestCloseBackends_NoErrorLogsNothing checks that closeBackends stays
// silent for a backend that closes cleanly, so a normal shutdown isn't
// polluted with a log line for every backend.
func TestCloseBackends_NoErrorLogsNothing(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	b := backendWithCloseError(t, "clean", nil)

	closeBackends(logger, map[string]*backend.Backend{"clean": b})

	if buf.Len() != 0 {
		t.Fatalf("log output = %q, want none for a clean Close", buf.String())
	}
}

// TestSuperviseBackend_OnElicit_BoundedByElicitTimeout checks that
// elicitTimeout genuinely bounds how long superviseBackend's OnElicit
// closure waits on a downstream session that never answers -- the same
// "package var a test can shrink" convention already proven for
// backendConnectTimeout (TestConnectBackends_TimesOutHungBackend) and
// reloadDrainTimeout (above), extended to elicitTimeout, which had no such
// test despite its doc comment claiming the same property.
//
// It wires the real production path (connectBackendsWaitable, i.e.
// superviseBackends/superviseBackend, with a real *gateway.ElicitationRouter
// in gwH.relays.Elicit) against a real backend whose "ask" tool calls
// req.Session.Elicit. Rather than routing that elicitation through a full
// gateway.Server and a downstream client connected to it (already covered
// end-to-end by TestServerCommand_RoutesElicitationToDownstreamClient in
// server_test.go), it Enters a stand-in downstream session directly into
// gwH.relays.Elicit -- exactly what gateway.callHandler would do around a
// real tools/call, just done here by hand so the test can control precisely
// how that session behaves. That stand-in session's ElicitationHandler
// blocks on a channel the test only closes at cleanup, so it never itself
// answers; the only thing that can end the wait is cb.OnElicit's own
// context.WithTimeout(ctx, elicitTimeout).
func TestSuperviseBackend_OnElicit_BoundedByElicitTimeout(t *testing.T) {
	origElicitTimeout := elicitTimeout
	elicitTimeout = 100 * time.Millisecond
	t.Cleanup(func() { elicitTimeout = origElicitTimeout })

	// blockElicit is never closed until cleanup, so the stub downstream
	// session's ElicitationHandler below blocks for as long as the test
	// runs -- simulating a downstream client that received the elicitation
	// but never answers it, the case elicitTimeout exists to bound.
	blockElicit := make(chan struct{})
	t.Cleanup(func() { close(blockElicit) })

	// stubSrv/stubClient exist solely to hand this test a real
	// *mcp.ServerSession to Enter into gwH.relays.Elicit -- ElicitationRouter's
	// live map is typed *mcp.ServerSession, which cannot be faked with an
	// interface, so a genuine (if otherwise unused) MCP connection is the
	// only way to get one. Capturing req.Session from a tool call is the
	// same technique TestConnect_ElicitationCallback (internal/backend)
	// uses to observe a session from inside a handler.
	sessionCh := make(chan *mcp.ServerSession, 1)
	stubSrv := mcp.NewServer(&mcp.Implementation{Name: "stub-downstream", Version: "v1"}, nil)
	stubSrv.AddTool(&mcp.Tool{Name: "capture", Description: "capture", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sessionCh <- req.Session
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	stubHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return stubSrv }, nil))
	defer stubHTTP.Close()

	stubClient := mcp.NewClient(&mcp.Implementation{Name: "stub-client", Version: "v1"}, &mcp.ClientOptions{
		ElicitationHandler: func(ctx context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			select {
			case <-blockElicit:
			case <-ctx.Done():
			}
			return nil, ctx.Err()
		},
	})
	stubSession, err := stubClient.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: stubHTTP.URL}, nil)
	if err != nil {
		t.Fatalf("connect stub downstream: %v", err)
	}
	defer func() { _ = stubSession.Close() }()
	if _, err := stubSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "capture", Arguments: map[string]any{}}); err != nil {
		t.Fatalf("capture call: %v", err)
	}
	var capturedSession *mcp.ServerSession
	select {
	case capturedSession = <-sessionCh:
	case <-time.After(5 * time.Second):
		t.Fatal("stub server never captured a session")
	}

	// backendSrv is the real backend whose tool triggers cb.OnElicit under
	// test, exactly like TestServerCommand_RoutesElicitationToDownstreamClient's
	// "ask" tool.
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	backendSrv.AddTool(&mcp.Tool{Name: "ask", Description: "ask", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, err := req.Session.Elicit(ctx, &mcp.ElicitParams{
				Message:         "confirm?",
				RequestedSchema: map[string]any{"type": "object"},
			})
			if err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Action}}}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	// A JSON handler into a buffer (rather than the text-into-io.Discard
	// handler most tests in this file use) so this test can also assert,
	// below, exactly which gateway event cb.OnElicit logs for a timeout --
	// proving "elicitation_timeout" and "elicitation_failed" aren't
	// transposed (see gateway.EventElicitationTimeout's call site in
	// server.go's cb.OnElicit).
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	ctx, cancel := context.WithCancel(context.Background())
	var supervisorsDone <-chan struct{}
	// Registered first, so LIFO runs it dead last -- see
	// connectBackendsWaitable's doc comment on why a test must wait for the
	// supervisor goroutine to actually exit before returning, instead of
	// just cancelling ctx and trusting it happens eventually.
	defer func() { waitSupervisorsDone(t, supervisorsDone) }()

	gwH := &gwHolder{relays: gateway.Relays{Elicit: gateway.NewElicitationRouter()}}
	conn, supervisorsDone := connectBackendsWaitable(ctx, logger,
		[]config.BackendConfig{{Name: "fake", Transport: "http", URL: backendHTTP.URL}}, gwH)
	b, ok := conn.backends["fake"]
	if !ok {
		t.Fatalf("backends = %v, want backend \"fake\" connected", conn.backends)
	}
	defer func() { _ = b.Close() }()
	defer cancel()

	// Register the stub session as the one in-flight downstream call for
	// "fake", exactly as gateway.callHandler's Enter/leave pair would do
	// around a real tools/call -- done directly here since this test only
	// needs cb.OnElicit's timeout behavior, not the routing machinery.
	leave := gwH.relays.Elicit.Enter("fake", capturedSession)
	defer leave()

	callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer callCancel()

	start := time.Now()
	res, callErr := b.Session.CallTool(callCtx, &mcp.CallToolParams{Name: "ask", Arguments: map[string]any{}})
	elapsed := time.Since(start)

	// 2s gives generous headroom over the 100ms shrunk elicitTimeout for
	// local httptest round trips, while still being nowhere near the
	// default 5-minute elicitTimeout -- proving the SHRUNK value is what
	// actually bounded this call.
	if elapsed > 2*time.Second {
		t.Fatalf("tools/call \"ask\" took %v; want well under the default 5-minute elicitTimeout, bounded instead by the shrunk 100ms value", elapsed)
	}
	if callErr == nil && (res == nil || !res.IsError) {
		t.Fatalf("call \"ask\" result = %+v, err = %v; want an error surfaced once the relayed elicitation timed out", res, callErr)
	}

	// Confirm cb.OnElicit logged the TIMEOUT event specifically, not
	// elicitation_failed -- the two are logged from different branches of
	// the same errors.Is(err, context.DeadlineExceeded) check in server.go,
	// and nothing before this test asserted which one actually fires here.
	var sawTimeout, sawFailed bool
	for line := range strings.SplitSeq(strings.TrimSpace(logBuf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		switch rec["event"] {
		case gateway.EventElicitationTimeout:
			sawTimeout = true
			if rec["backend"] != "fake" {
				t.Fatalf("elicitation_timeout backend = %v, want fake", rec["backend"])
			}
		case gateway.EventElicitationFailed:
			sawFailed = true
		}
	}
	if !sawTimeout {
		t.Fatalf("log output = %q, want a line with event=%s", logBuf.String(), gateway.EventElicitationTimeout)
	}
	if sawFailed {
		t.Fatalf("log output = %q, want no event=%s line (this is the timeout case, not a generic failure)", logBuf.String(), gateway.EventElicitationFailed)
	}
}

// TestToolsChangedCallback_LogsReconciledEventOnSuccess checks that a
// successful list_changed reconcile now emits a "list_changed_reconciled"
// gateway event (via gateway.LogEvent) -- previously nothing was logged on
// this path at all, only on failure.
func TestToolsChangedCallback_LogsReconciledEventOnSuccess(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	httpBackend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer httpBackend.Close()

	ctx := context.Background()
	conn, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: httpBackend.URL}, backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close() }()
	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	entries := []router.Entry[*mcp.Tool]{{BackendName: "fake", Items: tools}}
	table := router.Resolve(entries, gateway.ToolNameOf, gateway.ToolRename, nil)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"fake": conn},
		Tables:   gateway.Tables{Tools: table},
		Entries:  gateway.Entries{Tools: entries},
	})

	gwH := &gwHolder{}
	gwH.ptr.Store(srv)

	toolsChangedCallback(ctx, logger, "fake", gwH)()

	var rec map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err == nil && r["event"] == gateway.EventListChangedReconciled {
			rec = r
			break
		}
	}
	if rec == nil {
		t.Fatalf("log output = %q, want a line with event=list_changed_reconciled", buf.String())
	}
	if rec["backend"] != "fake" || rec["kind"] != "tool" {
		t.Fatalf("backend/kind = %v/%v, want fake/tool", rec["backend"], rec["kind"])
	}
	if rec["count"] != float64(1) { // JSON numbers decode as float64
		t.Fatalf("count = %v, want 1", rec["count"])
	}
}
