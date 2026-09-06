package cli

import (
	"time"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
)

// applyTimeouts overrides this process's package-level timeout/backoff
// variables from t, leaving each one at its built-in default wherever the
// corresponding field is unset (config.Duration's zero value). Callers must
// run this once, before any goroutine that reads one of these vars is
// spawned -- see its call sites (runServer's initial startup, and each
// one-shot command that calls connectBackends). It is deliberately NOT
// called again on a SIGHUP-triggered reload: these vars are read
// concurrently by every already-running generation's supervisor goroutines,
// and re-assigning them post-startup would race with those reads.
func applyTimeouts(t config.TimeoutsConfig) {
	if t.Shutdown > 0 {
		gateway.ShutdownTimeout = time.Duration(t.Shutdown)
	}
	if t.TelemetryShutdown > 0 {
		telemetryShutdownTimeout = time.Duration(t.TelemetryShutdown)
	}
	if t.BackendConnect > 0 {
		backendConnectTimeout = time.Duration(t.BackendConnect)
	}
	if t.ReloadDrain > 0 {
		reloadDrainTimeout = time.Duration(t.ReloadDrain)
	}
	if t.Elicit > 0 {
		elicitTimeout = time.Duration(t.Elicit)
	}
	if t.ProgressRelay > 0 {
		gateway.ProgressRelayTimeout = time.Duration(t.ProgressRelay)
	}
	if t.BackendBackoffMin > 0 {
		backendBackoffMin = time.Duration(t.BackendBackoffMin)
	}
	if t.BackendBackoffMax > 0 {
		backendBackoffMax = time.Duration(t.BackendBackoffMax)
	}
	if t.BackendKeepAlive > 0 {
		backend.KeepAlive = time.Duration(t.BackendKeepAlive)
	}
	if t.BackendKeepAliveFailureThreshold > 0 {
		backend.KeepAliveFailureThreshold = t.BackendKeepAliveFailureThreshold
	}
}
