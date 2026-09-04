package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/router"
)

// Tables bundles the independent routing tables the gateway serves: tools,
// resources, resource templates, and prompts. New uses them as the initial
// state; Server.UpdateTools/UpdateResources/UpdatePrompts replace them later
// in response to a backend's list_changed notification (see reconcile.go).
type Tables struct {
	Tools             *router.Table[*mcp.Tool]
	Resources         *router.Table[*mcp.Resource]
	ResourceTemplates *router.Table[*mcp.ResourceTemplate]
	Prompts           *router.Table[*mcp.Prompt]
}

// Entries bundles the raw, pre-resolution per-backend item lists Server
// needs in order to re-run router.Resolve when a backend's list changes
// (see Server.UpdateTools/UpdateResources/UpdatePrompts). It must be built
// from the same connected backend set as tables passed to New.
type Entries struct {
	Tools             []router.Entry[*mcp.Tool]
	Resources         []router.Entry[*mcp.Resource]
	ResourceTemplates []router.Entry[*mcp.ResourceTemplate]
	Prompts           []router.Entry[*mcp.Prompt]
}

// Overrides bundles the exposed-name -> winning-backend overrides for each
// category, as loaded from config.Config. router.Resolve takes overrides as
// an explicit argument rather than storing it, so Server retains these to
// re-supply on every reconcile.
type Overrides struct {
	Tools             map[string]string
	Resources         map[string]string
	ResourceTemplates map[string]string
	Prompts           map[string]string
}

// Server wraps an *mcp.Server with the reconcile state needed to react to a
// backend's list_changed notification: its per-backend raw item lists, the
// currently-registered routing table, and the exposed-name overrides -- all
// four independently for tools/resources/resource templates/prompts -- plus
// the connected backends themselves. mu protects the backends map and all
// eight of those reconcile fields together; the protected section is always
// in-memory work (router.Resolve plus the SDK's Add/Remove calls), never
// backend I/O, so one mutex is enough.
//
// backends is NOT fixed at construction: ConnectBackend (see reconcile.go)
// adds a backend that failed to connect at startup, and replaces a
// reconnecting backend's entry with its new connection, at any time while
// the Server is serving. Every read of it therefore goes through mu (see
// Backend/Backends).
type Server struct {
	mcp      *mcp.Server
	logger   *slog.Logger
	backends map[string]*backend.Backend
	maskKeys []string
	progress *ProgressRegistry

	mu sync.Mutex

	toolEntries   []router.Entry[*mcp.Tool]
	toolTable     *router.Table[*mcp.Tool]
	toolOverrides map[string]string

	resourceEntries           []router.Entry[*mcp.Resource]
	resourceTable             *router.Table[*mcp.Resource]
	resourceOverrides         map[string]string
	resourceTemplateEntries   []router.Entry[*mcp.ResourceTemplate]
	resourceTemplateTable     *router.Table[*mcp.ResourceTemplate]
	resourceTemplateOverrides map[string]string

	promptEntries   []router.Entry[*mcp.Prompt]
	promptTable     *router.Table[*mcp.Prompt]
	promptOverrides map[string]string
}

// MCP returns the underlying *mcp.Server, for ServeStdio/ServeHTTP.
func (s *Server) MCP() *mcp.Server { return s.mcp }

// Backend looks up a connected backend by name, for the cli layer's
// list_changed callbacks (see internal/cli/server.go) to re-list from
// without keeping their own separate reference. Locked: since
// ConnectBackend (backend reconnect/late-join), s.backends can be mutated
// concurrently by a supervisor goroutine at any time, not just during
// startup construction.
func (s *Server) Backend(name string) *backend.Backend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backends[name]
}

// Backends returns a snapshot of every currently connected backend, keyed by
// name, for scheduleDrain (see internal/cli/server.go) to force-close a
// superseded generation's connections after its drain timeout. Returns a
// copy rather than the live map: since ConnectBackend, s.backends can be
// mutated concurrently by a supervisor goroutine at any time, so handing out
// the live map would let a caller ranging over it race with that mutation
// after this call returns.
func (s *Server) Backends() map[string]*backend.Backend {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*backend.Backend, len(s.backends))
	for k, v := range s.backends {
		out[k] = v
	}
	return out
}

// emptyTable returns t, or a fresh empty table if t is nil -- New is called
// with tables.X == nil when a category has no items anywhere (see New's
// existing nil checks below), and Update* assumes toolTable etc are never
// nil so its diff loops don't need their own nil guards.
func emptyTable[T any](t *router.Table[T]) *router.Table[T] {
	if t != nil {
		return t
	}
	return &router.Table[T]{}
}

// New builds a Server that exposes tables' resolved tools/resources/prompts,
// forwarding each call to the backend that owns it, and retains entries and
// overrides so a later UpdateTools/UpdateResources/UpdatePrompts call can
// re-run router.Resolve when a backend reports its list has changed.
// backends must contain an entry for every BackendName referenced in
// tables (the caller builds both from the same set of connected backends).
func New(logger *slog.Logger, backends map[string]*backend.Backend, tables Tables, entries Entries, overrides Overrides, maskKeys []string, progress *ProgressRegistry) *Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{Logger: logger})

	s := &Server{
		mcp:      mcpSrv,
		logger:   logger,
		backends: backends,
		maskKeys: maskKeys,
		progress: progress,

		toolEntries:   entries.Tools,
		toolTable:     emptyTable(tables.Tools),
		toolOverrides: overrides.Tools,

		resourceEntries:           entries.Resources,
		resourceTable:             emptyTable(tables.Resources),
		resourceOverrides:         overrides.Resources,
		resourceTemplateEntries:   entries.ResourceTemplates,
		resourceTemplateTable:     emptyTable(tables.ResourceTemplates),
		resourceTemplateOverrides: overrides.ResourceTemplates,

		promptEntries:   entries.Prompts,
		promptTable:     emptyTable(tables.Prompts),
		promptOverrides: overrides.Prompts,
	}

	if tables.Tools != nil {
		for _, resolved := range tables.Tools.Items {
			registerTool(mcpSrv, logger, backends, resolved, maskKeys, progress)
		}
	}
	if tables.Resources != nil {
		for _, resolved := range tables.Resources.Items {
			registerResource(mcpSrv, logger, backends, resolved, maskKeys)
		}
	}
	if tables.ResourceTemplates != nil {
		for _, resolved := range tables.ResourceTemplates.Items {
			registerResourceTemplate(mcpSrv, logger, backends, resolved, maskKeys)
		}
	}
	if tables.Prompts != nil {
		for _, resolved := range tables.Prompts.Items {
			registerPrompt(mcpSrv, logger, backends, resolved, maskKeys)
		}
	}

	return s
}

// registerTool registers resolved.Item, falling back to the next
// lower-priority backend's definition (if any) when one turns out to be
// unregisterable, so a conflict's winner having a malformed schema doesn't
// need to take a validly-defined loser down with it. It reports whether
// anything was registered, so a reconcile caller (see reconcile.go) can
// clean up a stale prior registration when every candidate fails.
func registerTool(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Tool], maskKeys []string, progress *ProgressRegistry) (ok bool) {
	candidates := append([]router.Candidate[*mcp.Tool]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addTool(srv, logger, c.Item, callHandler(logger, maskKeys, b, c.OriginalName, progress)) {
			return true
		}
	}
	logger.Error("tool unavailable: every candidate backend had an invalid definition", "tool", resolved.Item.Name)
	return false
}

// registerResource registers resolved.Item, falling back to the next
// lower-priority backend's definition (if any) when one turns out to have
// an invalid URI, so a conflict's winner having a malformed URI doesn't
// need to take a validly-defined loser down with it.
func registerResource(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Resource], maskKeys []string) {
	candidates := append([]router.Candidate[*mcp.Resource]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addResource(srv, logger, c.Item, resourceReadHandler(logger, maskKeys, b, c.OriginalName)) {
			return
		}
	}
	logger.Error("resource unavailable: every candidate backend had an invalid URI", "uri", resolved.Item.URI)
}

// resourceReadHandler forwards resources/read to originalURI on backend b.
// originalURI is the fixed URI this exact resource was registered under:
// prefix is never applied to resource URIs, so it equals the exposed URI,
// and every call for this resource reads the same URI.
func resourceReadHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalURI string) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		start := time.Now()
		ctx, span := startCallSpan(ctx, req.Extra, "resources/read",
			attribute.String("mcp.backend", b.Name),
			attribute.String("mcp.resource.uri", originalURI))
		defer span.End()

		result, err := b.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: originalURI})
		recordOutcome(span, err)
		logCall(ctx, logger, "resource", "uri", originalURI, b.Name, req.Session, nil, maskKeys, start, err, nil)
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
func registerResourceTemplate(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.ResourceTemplate], maskKeys []string) {
	candidates := append([]router.Candidate[*mcp.ResourceTemplate]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addResourceTemplate(srv, logger, c.Item, resourceTemplateReadHandler(logger, maskKeys, b)) {
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
func resourceTemplateReadHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		start := time.Now()
		ctx, span := startCallSpan(ctx, req.Extra, "resources/templates/read",
			attribute.String("mcp.backend", b.Name),
			attribute.String("mcp.resource.uri", req.Params.URI))
		defer span.End()

		result, err := b.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
		recordOutcome(span, err)
		logCall(ctx, logger, "resource template", "uri", req.Params.URI, b.Name, req.Session, nil, maskKeys, start, err, nil)
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

// registerPrompt registers resolved.Item on srv. Unlike registerTool and
// registerResource, there is no panic-recovery/fallback loop here:
// mcp.Server.AddPrompt performs no schema validation and cannot panic (a
// Prompt has no JSON-Schema-bearing field for it to reject), so the winner
// is always registerable.
func registerPrompt(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Prompt], maskKeys []string) {
	b := backends[resolved.BackendName]
	srv.AddPrompt(resolved.Item, promptGetHandler(logger, maskKeys, b, resolved.OriginalName))
}

// promptGetHandler forwards prompts/get to originalName on backend b,
// passing the caller's arguments through unchanged.
func promptGetHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalName string) mcp.PromptHandler {
	return func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		start := time.Now()
		ctx, span := startCallSpan(ctx, req.Extra, "prompts/get",
			attribute.String("mcp.backend", b.Name),
			attribute.String("mcp.prompt.name", originalName))
		defer span.End()

		result, err := b.Session.GetPrompt(ctx, &mcp.GetPromptParams{
			Name:      originalName,
			Arguments: req.Params.Arguments,
		})
		recordOutcome(span, err)
		logCall(ctx, logger, "prompt", "prompt", originalName, b.Name, req.Session, req.Params.Arguments, maskKeys, start, err, nil)
		return result, err
	}
}

// callHandler forwards a tools/call to originalName on backend b, passing
// the raw arguments through unchanged. It wraps the call in a span
// (startCallSpan is a no-op for stdio-originated calls) and logs it via
// logCall, success or failure, so a dead or erroring backend — and normal
// usage — is visible to the operator. When progress is non-nil and the
// downstream request carries a progressToken, it registers a fresh
// correlation entry so a notifications/progress the backend sends mid-call
// (relayed via progress.Relay, wired through backend.ChangeCallbacks.
// OnProgress) reaches the downstream caller under its own token.
func callHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalName string, progress *ProgressRegistry) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		ctx, span := startCallSpan(ctx, req.Extra, "tools/call",
			attribute.String("mcp.backend", b.Name),
			attribute.String("mcp.tool.name", originalName))
		defer span.End()

		params := &mcp.CallToolParams{Name: originalName, Arguments: req.Params.Arguments}

		var entry *progressEntry
		if progress != nil {
			if token := req.Params.GetProgressToken(); token != nil {
				var internalToken uint64
				var cleanup func()
				internalToken, entry, cleanup = progress.Register(req.Session, token, b.Name)
				defer cleanup()
				// SetProgressToken only accepts int/int32/int64/string (see
				// go-sdk's setProgressToken); progress.Register hands out a
				// uint64, so it must be narrowed to int64 here.
				// normalizeProgressToken (progress.go) accepts int64 back,
				// so this round-trips correctly on the relay side.
				params.SetProgressToken(int64(internalToken))
			}
		}

		result, err := b.Session.CallTool(ctx, params)
		recordOutcome(span, err)
		logCall(ctx, logger, "tool", "tool", originalName, b.Name, req.Session, req.Params.Arguments, maskKeys, start, err, entry)
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

// ServeHTTP runs a Streamable HTTP server listening on addr, until ctx is
// cancelled. getServer is called once per brand-new session (the
// go-sdk's StreamableHTTPHandler looks up an existing session by its
// Mcp-Session-Id header instead of calling getServer again) -- not a fixed
// value -- so that a config hot-reload (see internal/cli/server.go's
// buildGateway/watchSIGHUP) can swap in a freshly-built *gateway.Server for
// new sessions without disturbing sessions already bound to the previous
// one.
func ServeHTTP(ctx context.Context, getServer func() *mcp.Server, addr string) error {
	handler := remoteAddrMiddleware(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return getServer() }, nil))
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
