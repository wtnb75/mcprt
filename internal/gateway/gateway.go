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

// Tables bundles the independent routing tables the gateway serves: tools,
// resources, and resource templates. They are built once at startup and
// never change while the gateway runs.
type Tables struct {
	Tools             *router.Table[*mcp.Tool]
	Resources         *router.Table[*mcp.Resource]
	ResourceTemplates *router.Table[*mcp.ResourceTemplate]
}

// New builds an mcp.Server that exposes tables' resolved tools/resources,
// forwarding each call to the backend that owns it. backends must contain
// an entry for every BackendName referenced in tables (the caller builds
// both from the same set of connected backends).
func New(logger *slog.Logger, backends map[string]*backend.Backend, tables Tables) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{Logger: logger})

	if tables.Tools != nil {
		for _, resolved := range tables.Tools.Items {
			registerTool(srv, logger, backends, resolved)
		}
	}
	if tables.Resources != nil {
		for _, resolved := range tables.Resources.Items {
			registerResource(srv, logger, backends, resolved)
		}
	}
	if tables.ResourceTemplates != nil {
		for _, resolved := range tables.ResourceTemplates.Items {
			registerResourceTemplate(srv, logger, backends, resolved)
		}
	}

	return srv
}

// registerTool registers resolved.Item, falling back to the next
// lower-priority backend's definition (if any) when one turns out to be
// unregisterable, so a conflict's winner having a malformed schema doesn't
// need to take a validly-defined loser down with it.
func registerTool(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Tool]) {
	candidates := append([]router.Candidate[*mcp.Tool]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addTool(srv, logger, c.Item, callHandler(logger, b, c.OriginalName)) {
			return
		}
	}
	logger.Error("tool unavailable: every candidate backend had an invalid definition", "tool", resolved.Item.Name)
}

// registerResource registers resolved.Item, falling back to the next
// lower-priority backend's definition (if any) when one turns out to have
// an invalid URI, so a conflict's winner having a malformed URI doesn't
// need to take a validly-defined loser down with it.
func registerResource(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Resource]) {
	candidates := append([]router.Candidate[*mcp.Resource]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addResource(srv, logger, c.Item, resourceReadHandler(logger, b, c.OriginalName)) {
			return
		}
	}
	logger.Error("resource unavailable: every candidate backend had an invalid URI", "uri", resolved.Item.URI)
}

// resourceReadHandler forwards resources/read to originalURI on backend b.
// originalURI is the fixed URI this exact resource was registered under:
// prefix is never applied to resource URIs, so it equals the exposed URI,
// and every call for this resource reads the same URI.
func resourceReadHandler(logger *slog.Logger, b *backend.Backend, originalURI string) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		result, err := b.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: originalURI})
		if err != nil {
			logger.Error("backend call failed", "backend", b.Name, "uri", originalURI, "error", err)
		}
		return result, err
	}
}

// addResource registers r on srv, recovering from AddResource's panic on an
// invalid or non-absolute URI so that one broken backend resource
// definition can't take down the whole gateway process at startup. It
// reports whether r was registered.
func addResource(srv *mcp.Server, logger *slog.Logger, r *mcp.Resource, h mcp.ResourceHandler) (ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Error("invalid resource definition", "uri", r.URI, "error", rec)
			ok = false
		}
	}()
	srv.AddResource(r, h)
	return true
}

// registerResourceTemplate is registerResource's counterpart for resource
// templates: same panic-recovery/fallback structure, but its read handler
// forwards the caller's actual matched URI instead of a fixed one.
func registerResourceTemplate(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.ResourceTemplate]) {
	candidates := append([]router.Candidate[*mcp.ResourceTemplate]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addResourceTemplate(srv, logger, c.Item, resourceTemplateReadHandler(logger, b)) {
			return
		}
	}
	logger.Error("resource template unavailable: every candidate backend had an invalid URI template", "uriTemplate", resolved.Item.URITemplate)
}

// resourceTemplateReadHandler forwards resources/read to the actual URI the
// client requested (req.Params.URI, the concrete URI that matched this
// template) on backend b -- unlike an exact resource, a template serves a
// different URI on every call, so the fixed-URI approach resourceReadHandler
// uses doesn't apply here.
func resourceTemplateReadHandler(logger *slog.Logger, b *backend.Backend) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		result, err := b.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
		if err != nil {
			logger.Error("backend call failed", "backend", b.Name, "uri", req.Params.URI, "error", err)
		}
		return result, err
	}
}

// addResourceTemplate registers t on srv, recovering from
// AddResourceTemplate's panic on an invalid URI template. It reports
// whether t was registered.
func addResourceTemplate(srv *mcp.Server, logger *slog.Logger, t *mcp.ResourceTemplate, h mcp.ResourceHandler) (ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Error("invalid resource template definition", "uriTemplate", t.URITemplate, "error", rec)
			ok = false
		}
	}()
	srv.AddResourceTemplate(t, h)
	return true
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
