package gateway

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServeHTTP_ShutdownTimeoutReturnsError checks that when a client holds
// a connection open past shutdownTimeout, ServeHTTP surfaces the timeout as
// an error instead of silently reporting a clean shutdown.
func TestServeHTTP_ShutdownTimeoutReturnsError(t *testing.T) {
	orig := shutdownTimeout
	shutdownTimeout = 100 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = orig })

	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v1"},
		&mcp.ServerOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing probe listener: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- ServeHTTP(ctx, func() *mcp.Server { return srv }, addr) }()

	// Connect and hold the session open (never call Close): the standalone
	// SSE stream this opens keeps its request handler running, which is
	// exactly the "active connection" Shutdown waits on.
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
		t.Fatalf("connecting to test server: %v", connectErr)
	}
	defer func() { _ = session.Close() }()

	cancel()
	select {
	case err := <-serveErr:
		if err == nil {
			t.Fatal("ServeHTTP: expected an error when graceful shutdown times out, got nil")
		}
		if !strings.Contains(err.Error(), "graceful shutdown timed out") {
			t.Fatalf("ServeHTTP error = %q, want it to mention a graceful shutdown timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTP did not return within 5s of context cancellation")
	}
}

// TestServeHTTP_GetServerCalledPerNewSession checks that ServeHTTP consults
// getServer again for each brand-new session (not just once at startup),
// and that swapping what getServer returns mid-run steers subsequent new
// sessions to the new value -- the mechanism config hot-reload depends on
// (see internal/cli/server.go's buildGateway/watchSIGHUP).
func TestServeHTTP_GetServerCalledPerNewSession(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srvA := mcp.NewServer(&mcp.Implementation{Name: "gen-a", Version: "v1"}, &mcp.ServerOptions{Logger: logger})
	mcp.AddTool(srvA, &mcp.Tool{Name: "from-a", Description: "from a"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	srvB := mcp.NewServer(&mcp.Implementation{Name: "gen-b", Version: "v1"}, &mcp.ServerOptions{Logger: logger})
	mcp.AddTool(srvB, &mcp.Tool{Name: "from-b", Description: "from b"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})

	var current atomic.Pointer[mcp.Server]
	current.Store(srvA)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing probe listener: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- ServeHTTP(ctx, func() *mcp.Server { return current.Load() }, addr) }()
	t.Cleanup(func() {
		cancel()
		<-serveErr
	})

	dial := func() *mcp.ClientSession {
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
		var session *mcp.ClientSession
		var connectErr error
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			session, connectErr = client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: "http://" + addr}, nil)
			if connectErr == nil {
				return session
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("connecting to test server: %v", connectErr)
		return nil
	}
	toolNames := func(t *testing.T, s *mcp.ClientSession) []string {
		t.Helper()
		var names []string
		for tool, err := range s.Tools(context.Background(), nil) {
			if err != nil {
				t.Fatalf("listing tools: %v", err)
			}
			names = append(names, tool.Name)
		}
		return names
	}

	sessionA := dial()
	defer func() { _ = sessionA.Close() }()
	if got := toolNames(t, sessionA); len(got) != 1 || got[0] != "from-a" {
		t.Fatalf("sessionA tools = %v, want [from-a]", got)
	}

	current.Store(srvB)

	sessionB := dial()
	defer func() { _ = sessionB.Close() }()
	if got := toolNames(t, sessionB); len(got) != 1 || got[0] != "from-b" {
		t.Fatalf("sessionB tools = %v, want [from-b]", got)
	}

	// sessionA, established before the swap, must keep working against
	// srvA -- this is the "existing sessions aren't disturbed" half of the
	// hot-reload guarantee.
	if got := toolNames(t, sessionA); len(got) != 1 || got[0] != "from-a" {
		t.Fatalf("sessionA tools after swap = %v, want [from-a] (must stay on its original server)", got)
	}
}
