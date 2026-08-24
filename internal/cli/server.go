package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
	"github.com/wtnb75/mcprt/internal/router"
	"github.com/wtnb75/mcprt/internal/telemetry"
)

// backendConnectTimeout bounds how long connectBackends waits on any single
// backend's Connect plus its
// ListTools/ListResources/ListResourceTemplates/ListPrompts calls, so one
// hung backend can't stall the whole gateway's startup (see
// connectBackends). A var so tests can shrink it.
var backendConnectTimeout = 30 * time.Second

// telemetryShutdownTimeout bounds internal/telemetry.Setup's returned
// shutdown func, which flushes buffered spans to the configured OTLP
// exporter. A var so tests can shrink it.
var telemetryShutdownTimeout = 5 * time.Second

func newServerCmd() *cobra.Command {
	var configPath string
	var logLevel string
	var logFormat string

	// SilenceUsage/SilenceErrors: don't dump flag usage on a runtime error
	// (bad config, backend failure, etc.), and let cli.Execute's caller
	// (main.go) be the one place that prints the error.
	cmd := &cobra.Command{
		Use:           "server",
		Short:         "run the mcprt gateway server",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			level, err := parseLogLevel(logLevel)
			if err != nil {
				return err
			}
			newHandler, err := parseLogFormat(logFormat)
			if err != nil {
				return err
			}
			logger := slog.New(newHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))
			return runServer(cmd.Context(), logger, configPath)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to the gateway config file (required)")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, error")
	cmd.Flags().StringVar(&logFormat, "log-format", "text", "log format: text or json")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err) // programmer error: "config" flag name must match Flags().StringVar above
	}

	return cmd
}

func parseLogLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (want debug, info, warn, or error)", s)
	}
}

func parseLogFormat(s string) (func(io.Writer, *slog.HandlerOptions) slog.Handler, error) {
	switch s {
	case "text":
		return func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewTextHandler(w, o) }, nil
	case "json":
		return func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewJSONHandler(w, o) }, nil
	default:
		return nil, fmt.Errorf("unknown log format %q (want text or json)", s)
	}
}

func runServer(ctx context.Context, logger *slog.Logger, configPath string) error {
	shutdownTelemetry, err := telemetry.Setup(ctx)
	if err != nil {
		return fmt.Errorf("configuring tracing: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
		defer cancel()
		if err := shutdownTelemetry(sctx); err != nil {
			logger.Error("tracer shutdown failed", "error", err)
		}
	}()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if !cfg.Listen.Stdio && cfg.Listen.HTTP == "" {
		return errors.New("no listener configured: enable listen.stdio or set listen.http")
	}

	// A child context we can cancel ourselves: if one listener fails while
	// another is still healthy, cancelling here tells the healthy one to
	// shut down too instead of leaving runServer blocked waiting on it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var gwH gwHolder
	conn := connectBackends(ctx, logger, cfg.Backends, &gwH)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()

	toolTable := router.Resolve(conn.toolEntries, toolNameOf, toolRename, cfg.Overrides)
	for _, c := range toolTable.Conflicts {
		logger.Warn("tool name conflict", "tool", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	resourceTable := router.Resolve(conn.resourceEntries, resourceNameOf, resourceRename, cfg.ResourceOverrides)
	for _, c := range resourceTable.Conflicts {
		logger.Warn("resource URI conflict", "uri", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	resourceTemplateTable := router.Resolve(conn.resourceTemplateEntries, resourceTemplateNameOf, resourceTemplateRename, cfg.ResourceTemplateOverrides)
	for _, c := range resourceTemplateTable.Conflicts {
		logger.Warn("resource template URI conflict", "uriTemplate", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	promptTable := router.Resolve(conn.promptEntries, promptNameOf, promptRename, cfg.PromptOverrides)
	for _, c := range promptTable.Conflicts {
		logger.Warn("prompt name conflict", "prompt", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	srv := gateway.New(logger, conn.backends, gateway.Tables{
		Tools:             toolTable,
		Resources:         resourceTable,
		ResourceTemplates: resourceTemplateTable,
		Prompts:           promptTable,
	}, gateway.Entries{
		Tools:             conn.toolEntries,
		Resources:         conn.resourceEntries,
		ResourceTemplates: conn.resourceTemplateEntries,
		Prompts:           conn.promptEntries,
	}, gateway.Overrides{
		Tools:             cfg.Overrides,
		Resources:         cfg.ResourceOverrides,
		ResourceTemplates: cfg.ResourceTemplateOverrides,
		Prompts:           cfg.PromptOverrides,
	}, cfg.Logging.MaskKeys)
	gwH.ptr.Store(srv)

	logger.Info("listening", "stdio", cfg.Listen.Stdio, "http", cfg.Listen.HTTP)

	running := 0
	errCh := make(chan error, 2)
	if cfg.Listen.Stdio {
		running++
		go func() { errCh <- gateway.ServeStdio(ctx, srv.MCP()) }()
	}
	if cfg.Listen.HTTP != "" {
		running++
		go func() { errCh <- gateway.ServeHTTP(ctx, srv.MCP(), cfg.Listen.HTTP) }()
	}

	// Log each listener's outcome as it arrives, so a listener that fails
	// while another is still healthy is reported immediately. A cancelled
	// context is how a clean shutdown reaches ServeStdio, so it isn't a
	// failure. cancel() on a real failure stops the other listener too,
	// instead of waiting on it indefinitely.
	var firstErr error
	for i := 0; i < running; i++ {
		err := <-errCh
		if err == nil {
			continue
		}
		if errors.Is(err, context.Canceled) {
			logger.Debug("listener stopped due to shutdown", "error", err)
			continue
		}
		logger.Error("listener stopped with error", "error", err)
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	return firstErr
}

// gwHolder lets a backend's ChangeCallbacks closures (built inside
// connectBackends, before the *gateway.Server exists) reference it once
// runServer finishes building it. A nil Load() means the initial
// connect-and-list sequence gateway.New's caller runs hasn't completed yet;
// a notification that fires in that window is dropped -- the pending
// initial ListTools/ListResources/ListResourceTemplates/ListPrompts that
// runServer is about to do anyway will reflect the same change, so nothing
// is permanently lost.
type gwHolder struct {
	ptr atomic.Pointer[gateway.Server]
}

// toolsChangedCallback returns a func to use as backend.ChangeCallbacks.
// OnToolsChanged for backendName: on fire, it re-lists that backend's tools
// (bounded by backendConnectTimeout) and reconciles gwH's Server, or logs a
// warning and keeps the previous list if either isn't ready yet.
func toolsChangedCallback(ctx context.Context, logger *slog.Logger, backendName string, gwH *gwHolder) func() {
	return func() {
		gw := gwH.ptr.Load()
		if gw == nil {
			return
		}
		b := gw.Backend(backendName)
		if b == nil {
			return
		}
		lctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
		defer cancel()
		tools, err := b.ListTools(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "tools", "error", err)
			return
		}
		gw.UpdateTools(backendName, tools)
	}
}

// resourcesChangedCallback is toolsChangedCallback's counterpart for
// notifications/resources/list_changed, which the MCP spec fires for BOTH
// resources and resource templates -- it re-lists both and reconciles them
// together via a single UpdateResources call.
func resourcesChangedCallback(ctx context.Context, logger *slog.Logger, backendName string, gwH *gwHolder) func() {
	return func() {
		gw := gwH.ptr.Load()
		if gw == nil {
			return
		}
		b := gw.Backend(backendName)
		if b == nil {
			return
		}
		lctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
		defer cancel()
		resources, err := b.ListResources(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "resources", "error", err)
			return
		}
		templates, err := b.ListResourceTemplates(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "resource templates", "error", err)
			return
		}
		gw.UpdateResources(backendName, resources, templates)
	}
}

// promptsChangedCallback is toolsChangedCallback's counterpart for
// notifications/prompts/list_changed.
func promptsChangedCallback(ctx context.Context, logger *slog.Logger, backendName string, gwH *gwHolder) func() {
	return func() {
		gw := gwH.ptr.Load()
		if gw == nil {
			return
		}
		b := gw.Backend(backendName)
		if b == nil {
			return
		}
		lctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
		defer cancel()
		prompts, err := b.ListPrompts(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "prompts", "error", err)
			return
		}
		gw.UpdatePrompts(backendName, prompts)
	}
}

// connected is the outcome of connectBackends: the live backend
// connections, plus each kind of item list gathered from them, ready to
// pass to router.Resolve. Entries preserve configs' order, since
// router.Resolve treats that order as priority (index 0 = highest).
type connected struct {
	backends                map[string]*backend.Backend
	toolEntries             []router.Entry[*mcp.Tool]
	resourceEntries         []router.Entry[*mcp.Resource]
	resourceTemplateEntries []router.Entry[*mcp.ResourceTemplate]
	promptEntries           []router.Entry[*mcp.Prompt]
}

// connectBackends connects to every configured backend concurrently and
// lists its tools, resources, resource templates, and prompts. A backend
// that fails to connect, fails to list tools, or exceeds
// backendConnectTimeout is logged and excluded entirely (best-effort); it
// does not fail or stall the whole startup. A backend that fails to list
// resources, resource templates, or prompts is kept with its tools intact
// and treated as having none of that kind: many non-Go-SDK MCP servers
// answer resources/list, resources/templates/list, or prompts/list with a
// JSON-RPC "method not found" error when they don't implement that
// capability at all, rather than an empty list, and that must not take down
// an otherwise-working tools-only backend.
func connectBackends(ctx context.Context, logger *slog.Logger, configs []config.BackendConfig, gwH *gwHolder) connected {
	type outcome struct {
		backend               *backend.Backend
		toolEntry             router.Entry[*mcp.Tool]
		resourceEntry         router.Entry[*mcp.Resource]
		resourceTemplateEntry router.Entry[*mcp.ResourceTemplate]
		promptEntry           router.Entry[*mcp.Prompt]
	}
	outcomes := make([]*outcome, len(configs))

	var wg sync.WaitGroup
	for i, bc := range configs {
		wg.Add(1)
		// Callbacks are built here, using ctx (this func's own long-lived
		// context), NOT the timeout-bounded ctx the goroutine below derives
		// for the initial Connect+List sequence -- a callback captured from
		// that derived context would already be expired by the time a real
		// list_changed notification could plausibly arrive.
		cb := backend.ChangeCallbacks{}
		if gwH != nil {
			cb = backend.ChangeCallbacks{
				OnToolsChanged:     toolsChangedCallback(ctx, logger, bc.Name, gwH),
				OnResourcesChanged: resourcesChangedCallback(ctx, logger, bc.Name, gwH),
				OnPromptsChanged:   promptsChangedCallback(ctx, logger, bc.Name, gwH),
			}
		}
		go func(i int, bc config.BackendConfig, cb backend.ChangeCallbacks) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
			defer cancel()

			b, err := backend.Connect(ctx, bc, cb)
			if err != nil {
				logger.Error("skipping backend: connect failed", "backend", bc.Name, "error", err)
				return
			}
			logger.Info("backend connected", "backend", bc.Name, "transport", bc.Transport)
			tools, err := b.ListTools(ctx)
			if err != nil {
				logger.Error("skipping backend: list tools failed", "backend", bc.Name, "error", err)
				_ = b.Close()
				return
			}
			resources, err := b.ListResources(ctx)
			if err != nil {
				logger.Warn("backend lists no resources", "backend", bc.Name, "error", err)
				resources = nil
			}
			resourceTemplates, err := b.ListResourceTemplates(ctx)
			if err != nil {
				logger.Warn("backend lists no resource templates", "backend", bc.Name, "error", err)
				resourceTemplates = nil
			}
			prompts, err := b.ListPrompts(ctx)
			if err != nil {
				logger.Warn("backend lists no prompts", "backend", bc.Name, "error", err)
				prompts = nil
			}
			outcomes[i] = &outcome{
				backend: b,
				toolEntry: router.Entry[*mcp.Tool]{
					BackendName: bc.Name, Prefix: bc.Prefix, Items: tools,
				},
				// resource/resource template entries never carry a prefix:
				// URIs already encode a backend-specific namespace, and
				// string-concatenating a prefix onto one would produce an
				// invalid URI.
				resourceEntry: router.Entry[*mcp.Resource]{
					BackendName: bc.Name, Items: resources,
				},
				resourceTemplateEntry: router.Entry[*mcp.ResourceTemplate]{
					BackendName: bc.Name, Items: resourceTemplates,
				},
				// prompt entries DO carry a prefix, like tools: prompt
				// names are a flat namespace, not a URI.
				promptEntry: router.Entry[*mcp.Prompt]{
					BackendName: bc.Name, Prefix: bc.Prefix, Items: prompts,
				},
			}
		}(i, bc, cb)
	}
	wg.Wait()

	result := connected{backends: make(map[string]*backend.Backend, len(configs))}
	for _, o := range outcomes {
		if o == nil {
			continue
		}
		result.backends[o.toolEntry.BackendName] = o.backend
		result.toolEntries = append(result.toolEntries, o.toolEntry)
		result.resourceEntries = append(result.resourceEntries, o.resourceEntry)
		result.resourceTemplateEntries = append(result.resourceTemplateEntries, o.resourceTemplateEntry)
		result.promptEntries = append(result.promptEntries, o.promptEntry)
	}
	return result
}

func toolNameOf(t *mcp.Tool) string { return t.Name }

func toolRename(t *mcp.Tool, name string) *mcp.Tool {
	c := *t
	c.Name = name
	return &c
}

func resourceNameOf(r *mcp.Resource) string { return r.URI }

func resourceRename(r *mcp.Resource, name string) *mcp.Resource {
	c := *r
	c.URI = name
	return &c
}

func resourceTemplateNameOf(t *mcp.ResourceTemplate) string { return t.URITemplate }

func resourceTemplateRename(t *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate {
	c := *t
	c.URITemplate = name
	return &c
}

func promptNameOf(p *mcp.Prompt) string { return p.Name }

func promptRename(p *mcp.Prompt, name string) *mcp.Prompt {
	c := *p
	c.Name = name
	return &c
}
