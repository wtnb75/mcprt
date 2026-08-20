package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
	"github.com/wtnb75/mcprt/internal/router"
)

// backendConnectTimeout bounds how long connectBackends waits on any single
// backend's Connect+ListTools, so one hung backend can't stall the whole
// gateway's startup (see connectBackends). A var so tests can shrink it.
var backendConnectTimeout = 30 * time.Second

func newServerCmd() *cobra.Command {
	var configPath string
	var logLevel string

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
			logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))
			return runServer(cmd.Context(), logger, configPath)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to the gateway config file (required)")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, error")
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

func runServer(ctx context.Context, logger *slog.Logger, configPath string) error {
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

	backends, entries := connectBackends(ctx, logger, cfg.Backends)
	defer func() {
		for _, b := range backends {
			_ = b.Close()
		}
	}()

	table := router.Resolve(entries, toolNameOf, toolRename, cfg.Overrides)
	for _, c := range table.Conflicts {
		logger.Warn("tool name conflict", "tool", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	srv := gateway.New(logger, backends, table)

	running := 0
	errCh := make(chan error, 2)
	if cfg.Listen.Stdio {
		running++
		go func() { errCh <- gateway.ServeStdio(ctx, srv) }()
	}
	if cfg.Listen.HTTP != "" {
		running++
		go func() { errCh <- gateway.ServeHTTP(ctx, srv, cfg.Listen.HTTP) }()
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

// connectBackends connects to every configured backend concurrently and
// lists its tools. A backend that fails to connect, list tools, or exceed
// backendConnectTimeout is logged and excluded (best-effort); it does not
// fail or stall the whole startup. The returned entries preserve configs'
// order, since router.Resolve treats that order as priority (index 0 =
// highest).
func connectBackends(ctx context.Context, logger *slog.Logger, configs []config.BackendConfig) (map[string]*backend.Backend, []router.Entry[*mcp.Tool]) {
	type outcome struct {
		backend *backend.Backend
		entry   router.Entry[*mcp.Tool]
	}
	outcomes := make([]*outcome, len(configs))

	var wg sync.WaitGroup
	for i, bc := range configs {
		wg.Add(1)
		go func(i int, bc config.BackendConfig) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
			defer cancel()

			b, err := backend.Connect(ctx, bc)
			if err != nil {
				logger.Error("skipping backend: connect failed", "backend", bc.Name, "error", err)
				return
			}
			tools, err := b.ListTools(ctx)
			if err != nil {
				logger.Error("skipping backend: list tools failed", "backend", bc.Name, "error", err)
				_ = b.Close()
				return
			}
			outcomes[i] = &outcome{
				backend: b,
				entry:   router.Entry[*mcp.Tool]{BackendName: bc.Name, Prefix: bc.Prefix, Items: tools},
			}
		}(i, bc)
	}
	wg.Wait()

	backends := make(map[string]*backend.Backend, len(configs))
	entries := make([]router.Entry[*mcp.Tool], 0, len(configs))
	for _, o := range outcomes {
		if o == nil {
			continue
		}
		backends[o.entry.BackendName] = o.backend
		entries = append(entries, o.entry)
	}
	return backends, entries
}

func toolNameOf(t *mcp.Tool) string { return t.Name }

func toolRename(t *mcp.Tool, name string) *mcp.Tool {
	c := *t
	c.Name = name
	return &c
}
