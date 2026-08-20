package cli

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
)

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
	var backends map[string]*backend.Backend
	go func() {
		backends, _ = connectBackends(context.Background(), logger, configs)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("connectBackends did not return within 10s of backendConnectTimeout elapsing")
	}
	if len(backends) != 0 {
		t.Fatalf("backends = %v, want none (the hung backend should be excluded)", backends)
	}
}
