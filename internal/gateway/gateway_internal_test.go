package gateway

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
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
	go func() { serveErr <- ServeHTTP(ctx, srv, addr) }()

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
