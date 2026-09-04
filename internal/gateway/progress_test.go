package gateway_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/gateway"
)

// newCapturedDownstreamSession spins up a tiny MCP server with one tool,
// connects a client to it (with progressCh wired as the client's
// ProgressNotificationHandler), calls the tool once to capture the
// server-side *mcp.ServerSession for that connection, and returns it plus
// progressCh and a cleanup func. This mirrors what callHandler sees as
// req.Session for a real downstream call.
func newCapturedDownstreamSession(t *testing.T) (session *mcp.ServerSession, progressCh <-chan *mcp.ProgressNotificationParams, cleanup func()) {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "probe", Version: "v1"}, nil)
	var captured *mcp.ServerSession
	srv.AddTool(&mcp.Tool{Name: "probe", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			captured = req.Session
			return &mcp.CallToolResult{}, nil
		})
	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))

	ch := make(chan *mcp.ProgressNotificationParams, 128)
	client := mcp.NewClient(&mcp.Implementation{Name: "probe-client", Version: "v1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			ch <- req.Params
		},
	})
	clientSession, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: httpSrv.URL}, nil)
	if err != nil {
		t.Fatalf("connect probe client: %v", err)
	}

	if _, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "probe", Arguments: map[string]any{}}); err != nil {
		t.Fatalf("call probe: %v", err)
	}
	if captured == nil {
		t.Fatal("probe tool handler never ran; no server-side session captured")
	}

	return captured, ch, func() {
		_ = clientSession.Close()
		httpSrv.Close()
	}
}

func TestProgressRegistry_RegisterRelaySummary(t *testing.T) {
	session, progressCh, cleanupSession := newCapturedDownstreamSession(t)
	defer cleanupSession()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := gateway.NewProgressRegistry()

	internalToken, entry, cleanup := reg.Register(session, "original-token")
	defer cleanup()

	reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: internalToken, Progress: 1, Total: 2, Message: "step1"})
	// A relay carrying the token as float64 (as it would arrive after a real
	// JSON round-trip) must resolve to the same entry.
	reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: float64(internalToken), Progress: 2, Total: 2, Message: "step2"})

	var got []*mcp.ProgressNotificationParams
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		select {
		case p := <-progressCh:
			got = append(got, p)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(got) != 2 {
		t.Fatalf("received %d progress notifications, want 2", len(got))
	}
	if got[0].ProgressToken != "original-token" || got[0].Message != "step1" {
		t.Fatalf("first notification = %+v, want ProgressToken=original-token Message=step1", got[0])
	}
	if got[1].ProgressToken != "original-token" || got[1].Message != "step2" {
		t.Fatalf("second notification = %+v, want ProgressToken=original-token Message=step2", got[1])
	}

	count, lastMessage := entry.Summary()
	if count != 2 || lastMessage != "step2" {
		t.Fatalf("Summary() = (%d, %q), want (2, \"step2\")", count, lastMessage)
	}
}

func TestProgressRegistry_RelayIgnoredAfterCleanup(t *testing.T) {
	session, progressCh, cleanupSession := newCapturedDownstreamSession(t)
	defer cleanupSession()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := gateway.NewProgressRegistry()

	internalToken, _, cleanup := reg.Register(session, "original-token")
	cleanup() // call already completed before any progress notification arrived

	reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: internalToken, Progress: 1, Message: "too-late"})

	select {
	case p := <-progressCh:
		t.Fatalf("received progress notification %+v after cleanup, want none relayed", p)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestProgressRegistry_RelayIgnoresUnknownToken(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := gateway.NewProgressRegistry()

	// No Register call at all: Relay must not panic on a nil session lookup.
	reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: uint64(999), Progress: 1})
	reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: "not-a-number", Progress: 1})
}

// TestProgressRegistry_ConcurrentRegisterRelayCleanup exercises Register,
// Relay, and cleanup from many goroutines at once against one shared
// session -- go test -race must find nothing.
func TestProgressRegistry_ConcurrentRegisterRelayCleanup(t *testing.T) {
	session, _, cleanupSession := newCapturedDownstreamSession(t)
	defer cleanupSession()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := gateway.NewProgressRegistry()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			internalToken, _, cleanup := reg.Register(session, i)
			defer cleanup()
			for j := 0; j < 3; j++ {
				reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: internalToken, Progress: float64(j)})
			}
		}(i)
	}
	wg.Wait()

	// The registry must still be usable afterward.
	internalToken, entry, cleanup := reg.Register(session, "final")
	defer cleanup()
	reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: internalToken, Progress: 1, Message: "final-msg"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if count, _ := entry.Summary(); count == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if count, lastMessage := entry.Summary(); count != 1 || lastMessage != "final-msg" {
		t.Fatalf("Summary() after concurrent stress = (%d, %q), want (1, \"final-msg\")", count, lastMessage)
	}
}
