package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

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
		b := backends[resolved.BackendName]
		addTool(srv, logger, resolved.Tool, callHandler(b, resolved.OriginalName))
	}

	return srv
}

// callHandler forwards a tools/call to originalName on backend b, passing
// the raw arguments through unchanged.
func callHandler(b *backend.Backend, originalName string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return b.Session.CallTool(ctx, &mcp.CallToolParams{
			Name:      originalName,
			Arguments: req.Params.Arguments,
		})
	}
}

// addTool registers t on srv, recovering from AddTool's panic on a
// malformed schema so that one broken backend tool definition can't take
// down the whole gateway process at startup.
func addTool(srv *mcp.Server, logger *slog.Logger, t *mcp.Tool, h mcp.ToolHandler) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("skipping tool: invalid definition", "tool", t.Name, "error", r)
		}
	}()
	srv.AddTool(t, h)
}

// ServeStdio runs srv over stdin/stdout until ctx is cancelled or the
// client disconnects.
func ServeStdio(ctx context.Context, srv *mcp.Server) error {
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// ServeHTTP runs srv as a Streamable HTTP server listening on addr, until
// ctx is cancelled.
func ServeHTTP(ctx context.Context, srv *mcp.Server, addr string) error {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	httpServer := &http.Server{Addr: addr, Handler: handler}

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	select {
	case <-ctx.Done():
		return httpServer.Shutdown(context.Background())
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
