package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/router"
)

// New builds an mcp.Server that exposes table's resolved tools, forwarding
// each tools/call to the backend that owns it. backends must contain an
// entry for every BackendName referenced in table (the caller builds both
// from the same set of connected backends).
func New(logger *slog.Logger, backends map[string]*backend.Backend, table *router.Table) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{Logger: logger})

	for _, resolved := range table.Tools {
		registerTool(srv, logger, backends, resolved)
	}

	return srv
}

// registerTool registers resolved.Tool, falling back to the next
// lower-priority backend's definition (if any) when one turns out to be
// unregisterable, so a conflict's winner having a malformed schema doesn't
// need to take a validly-defined loser down with it.
func registerTool(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved) {
	candidates := append([]router.Candidate{{
		Tool:         resolved.Tool,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addTool(srv, logger, c.Tool, callHandler(logger, b, c.OriginalName)) {
			return
		}
	}
	logger.Error("tool unavailable: every candidate backend had an invalid definition", "tool", resolved.Tool.Name)
}

// callHandler forwards a tools/call to originalName on backend b, passing
// the raw arguments through unchanged. A failure is returned to the client
// and logged, so a dead or erroring backend is visible to the operator.
func callHandler(logger *slog.Logger, b *backend.Backend, originalName string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result, err := b.Session.CallTool(ctx, &mcp.CallToolParams{
			Name:      originalName,
			Arguments: req.Params.Arguments,
		})
		if err != nil {
			logger.Error("backend call failed", "backend", b.Name, "tool", originalName, "error", err)
		}
		return result, err
	}
}

// addTool registers t on srv, recovering from AddTool's panic on a
// malformed schema so that one broken backend tool definition can't take
// down the whole gateway process at startup. It reports whether t was
// registered.
func addTool(srv *mcp.Server, logger *slog.Logger, t *mcp.Tool, h mcp.ToolHandler) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("invalid tool definition", "tool", t.Name, "error", r)
			ok = false
		}
	}()
	srv.AddTool(t, h)
	return true
}

// ServeStdio runs srv over stdin/stdout until ctx is cancelled or the
// client disconnects.
func ServeStdio(ctx context.Context, srv *mcp.Server) error {
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// shutdownTimeout bounds ServeHTTP's graceful shutdown: MCP Streamable HTTP
// clients hold a long-lived SSE stream open, so Shutdown would otherwise
// wait forever for the connection to go idle. A var so tests can shrink it.
var shutdownTimeout = 5 * time.Second

// ServeHTTP runs srv as a Streamable HTTP server listening on addr, until
// ctx is cancelled.
func ServeHTTP(ctx context.Context, srv *mcp.Server, addr string) error {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	httpServer := &http.Server{Addr: addr, Handler: handler}

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(sctx); err != nil {
			_ = httpServer.Close() // force-close whatever Shutdown couldn't drain in time
			return fmt.Errorf("graceful shutdown timed out: %w", err)
		}
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
