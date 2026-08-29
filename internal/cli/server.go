package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
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

// reloadDrainTimeout bounds how long a superseded generation's backend
// connections are kept alive after a hot-reload swap, so sessions still
// bound to it can finish naturally. A var so tests can shrink it.
var reloadDrainTimeout = 5 * time.Minute

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

	// A child context we can cancel ourselves: if one listener fails while
	// another is still healthy, cancelling here tells the healthy one to
	// shut down too instead of leaving runServer blocked waiting on it. It
	// also bounds every generation's genCtx below, so process shutdown
	// cancels all of them regardless of hot-reload state.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// genCtx/genCancel scope generation 0's backend connections and
	// list_changed callbacks (see buildGateway) separately from ctx itself,
	// so a later SIGHUP-triggered reload can supersede this generation (via
	// watchSIGHUP's scheduleDrain, Task 5) without tearing down the
	// listeners themselves.
	genCtx, genCancel := context.WithCancel(ctx)
	// watchSIGHUP takes ownership of genCancel once spawned below (calling
	// it via scheduleDrain on the generation's first supersession), but this
	// defer still runs at process shutdown regardless -- e.g. when no HTTP
	// listener is configured and watchSIGHUP never runs at all, or when the
	// process exits before any reload happens. Calling an already-cancelled
	// context.CancelFunc again is a safe no-op, so this is just a backstop,
	// not a double-cancellation hazard.
	defer genCancel()
	srv, err := buildGateway(genCtx, logger, cfg)
	if err != nil {
		return err
	}

	// current is where new HTTP connections get routed (see
	// gateway.ServeHTTP below): generation 0 until a SIGHUP-triggered
	// reload (watchSIGHUP) swaps it.
	current := new(atomic.Pointer[gateway.Server])
	current.Store(srv)
	// live tracks every generation whose backend connections are still open
	// -- current plus any superseded generation still inside its
	// reloadDrainTimeout window -- so shutdown below closes all of them, not
	// just current. Backends are real subprocesses (stdio/ssh/docker run) that
	// don't reliably die with their parent, so leaving a draining generation's
	// connections to scheduleDrain's timer alone would leak them whenever the
	// process exits before that timer fires.
	live := new(generations)
	live.add(srv)

	// startup is the listener configuration this process actually bound.
	// Listener re-binding is out of hot-reload's scope, so watchSIGHUP
	// compares every reloaded config against these values rather than
	// believing whatever the new config says about listeners.
	startup := startupListen{http: cfg.Listen.HTTP, stdio: cfg.Listen.Stdio}

	// sighupDone is closed when watchSIGHUP returns; nil when no HTTP
	// listener is configured and it never runs at all.
	var sighupDone chan struct{}
	defer func() {
		// Wait for watchSIGHUP to exit before closing anything: it runs
		// fire-and-forget, so a reload still in flight when ctx was cancelled
		// could otherwise Swap in a brand-new generation after this loop had
		// already run, leaking its backends with nothing left to close them.
		// cancel() here (rather than relying on the outer `defer cancel()`,
		// which runs later) is what guarantees watchSIGHUP actually returns.
		cancel()
		if sighupDone != nil {
			<-sighupDone
		}
		for _, s := range live.takeAll() {
			for _, b := range s.Backends() {
				_ = b.Close()
			}
		}
	}()

	logger.Info("listening", "stdio", cfg.Listen.Stdio, "http", cfg.Listen.HTTP)

	running := 0
	errCh := make(chan error, 2)
	if cfg.Listen.Stdio {
		running++
		// Unlike the HTTP listener, ServeStdio is pinned to generation 0's
		// srv for its whole lifetime -- stdio hot-reload is out of scope
		// (see this plan's Global Constraints and watchSIGHUP below).
		go func() { errCh <- gateway.ServeStdio(ctx, srv.MCP()) }()
	}
	if cfg.Listen.HTTP != "" {
		running++
		go func() {
			errCh <- gateway.ServeHTTP(ctx, func() *mcp.Server { return current.Load().MCP() }, cfg.Listen.HTTP)
		}()
		sighupDone = make(chan struct{})
		go func() {
			defer close(sighupDone)
			watchSIGHUP(ctx, logger, configPath, startup, current, live, genCancel)
		}()
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

// buildGateway connects to every configured backend (see connectBackends)
// and builds a fresh *gateway.Server from scratch. Called once at startup,
// and (from Task 5's watchSIGHUP) once per SIGHUP-triggered reload with a
// freshly-loaded cfg -- the two call sites are otherwise identical, which is
// the whole point of the graceful-restart design: nothing about
// config-derived state is patched piecemeal, it's all rebuilt the same way
// every time.
func buildGateway(ctx context.Context, logger *slog.Logger, cfg *config.Config) (*gateway.Server, error) {
	if !cfg.Listen.Stdio && cfg.Listen.HTTP == "" {
		return nil, errors.New("no listener configured: enable listen.stdio or set listen.http")
	}

	var gwH gwHolder
	conn := connectBackends(ctx, logger, cfg.Backends, &gwH)

	toolTable := router.Resolve(conn.toolEntries, gateway.ToolNameOf, gateway.ToolRename, cfg.Overrides)
	for _, c := range toolTable.Conflicts {
		logger.Warn("tool name conflict", "tool", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	resourceTable := router.Resolve(conn.resourceEntries, gateway.ResourceNameOf, gateway.ResourceRename, cfg.ResourceOverrides)
	for _, c := range resourceTable.Conflicts {
		logger.Warn("resource URI conflict", "uri", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	resourceTemplateTable := router.Resolve(conn.resourceTemplateEntries, gateway.ResourceTemplateNameOf, gateway.ResourceTemplateRename, cfg.ResourceTemplateOverrides)
	for _, c := range resourceTemplateTable.Conflicts {
		logger.Warn("resource template URI conflict", "uriTemplate", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	promptTable := router.Resolve(conn.promptEntries, gateway.PromptNameOf, gateway.PromptRename, cfg.PromptOverrides)
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

	return srv, nil
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

// startupListen is the listener configuration the process actually started
// with: the address its HTTP listener is bound to, and whether it really
// started a stdio listener. Changing either of those requires a process
// restart (listener re-binding is out of hot-reload's scope), so a reloaded
// config's listen.* values describe what the operator *asked for*, never what
// the process is doing -- watchSIGHUP needs both to tell them apart.
type startupListen struct {
	http  string
	stdio bool
}

// generations tracks every gateway generation whose backend connections are
// still open: the live one plus any superseded generation still inside its
// reloadDrainTimeout window. Removing a generation from the set is what
// confers the right to close it, so scheduleDrain's drain timer (which runs
// on its own goroutine) and runServer's shutdown path can never both close
// the same connections.
type generations struct {
	mu   sync.Mutex
	live []*gateway.Server
}

func (g *generations) add(srv *gateway.Server) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.live = append(g.live, srv)
}

// take removes srv from the set, reporting whether this caller is the one
// that removed it -- i.e. whether it now owns closing srv's backends.
func (g *generations) take(srv *gateway.Server) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, s := range g.live {
		if s == srv {
			g.live = append(g.live[:i], g.live[i+1:]...)
			return true
		}
	}
	return false
}

// takeAll empties the set and returns everything it held, transferring
// ownership of closing all of them to the caller.
func (g *generations) takeAll() []*gateway.Server {
	g.mu.Lock()
	defer g.mu.Unlock()
	all := g.live
	g.live = nil
	return all
}

// watchSIGHUP blocks until ctx is cancelled, rebuilding the gateway (via
// buildGateway) and swapping current on every SIGHUP. initialGenCancel is
// the cancel func for the generation runServer already built before
// spawning this loop -- watchSIGHUP takes ownership of it so that
// generation 0 is cancelled on its first supersession exactly like every
// later one; without this, the very first reload would leak generation 0's
// connectBackends-spawned goroutines' resources forever. Every generation it
// builds is registered in live so process shutdown can close it even while it
// drains. Only spawned by runServer when cfg.Listen.HTTP != "" -- hot-reload
// only makes sense for HTTP (see this plan's Global Constraints).
func watchSIGHUP(ctx context.Context, logger *slog.Logger, configPath string, startup startupListen, current *atomic.Pointer[gateway.Server], live *generations, initialGenCancel context.CancelFunc) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)

	genCancel := initialGenCancel // the currently-live generation's cancel func

	for {
		select {
		case <-ctx.Done():
			return
		case <-sighup:
			cfg, err := config.Load(configPath)
			if err != nil {
				logger.Error("config reload failed, keeping current config", "error", err)
				continue
			}
			// buildGateway itself validates that SOME listener is
			// configured; this check is specifically about whether
			// hot-reload can do anything USEFUL with the new config, which
			// is a stricter, hot-reload-specific condition on top of that.
			if cfg.Listen.HTTP == "" {
				logger.Warn("SIGHUP received, but hot-reload is only supported for HTTP listeners; ignoring")
				continue
			}
			// Listener re-binding is out of hot-reload's scope: this process
			// keeps serving on the address it bound at startup no matter what
			// the reloaded config says. Say so explicitly -- otherwise an
			// operator who edited listen.http sees nothing but "config
			// reloaded" and keeps waiting for an address that will never come
			// up.
			if cfg.Listen.HTTP != startup.http {
				logger.Warn("listen.http change requires a process restart and was NOT applied; still serving on the startup address",
					"listening", startup.http, "requested", cfg.Listen.HTTP)
			}
			// Gated on what the PROCESS actually started, not on the reloaded
			// config's listen.stdio: a process started HTTP-only never has a
			// stdio session to warn about, however the new config reads.
			if startup.stdio {
				logger.Warn("SIGHUP received: only the HTTP listener sees the new config; the existing stdio session keeps running under the previous generation, whose backend connections are force-closed once the drain timeout elapses -- every stdio call fails permanently from that point on, and only a process restart restores it",
					"drainTimeout", reloadDrainTimeout)
			}

			genCtx, newGenCancel := context.WithCancel(ctx)
			newSrv, err := buildGateway(genCtx, logger, cfg)
			if err != nil {
				logger.Error("config reload failed, keeping current config", "error", err)
				newGenCancel()
				continue
			}

			// Register before the swap: from here on the new generation has
			// open backend connections, and shutdown must close them whether
			// or not it ever became current.
			live.add(newSrv)
			oldSrv := current.Swap(newSrv)
			logger.Info("config reloaded")

			scheduleDrain(logger, oldSrv, genCancel, live) // supersede and drain the generation this reload replaced
			genCancel = newGenCancel                       // track the new generation for the NEXT reload (or process shutdown, which cancels ctx and makes newGenCancel a no-op)
		}
	}
}

// scheduleDrain cancels the just-superseded generation's long-lived ctx
// (stopping its backends' list_changed callback contexts) and, after
// reloadDrainTimeout, force-closes every one of its backend connections --
// so a session still bound to oldSrv gets a normal "backend disconnected"
// error from that point on, instead of the old generation's connections
// staying open indefinitely. The timer only closes what it can still take
// out of live: if process shutdown already claimed (and closed) oldSrv, this
// callback has nothing left to do.
func scheduleDrain(logger *slog.Logger, oldSrv *gateway.Server, oldGenCancel context.CancelFunc, live *generations) {
	oldGenCancel()
	time.AfterFunc(reloadDrainTimeout, func() {
		if !live.take(oldSrv) {
			return
		}
		for name, b := range oldSrv.Backends() {
			if err := b.Close(); err != nil {
				logger.Warn("closing superseded backend connection", "backend", name, "error", err)
			}
		}
	})
}
