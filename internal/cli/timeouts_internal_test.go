package cli

import (
	"testing"
	"time"

	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
)

// saveTimeoutVars snapshots every package-level timeout/backoff var
// applyTimeouts can touch, restoring them on test cleanup so this test can't
// leak its overrides into whichever test runs next.
func saveTimeoutVars(t *testing.T) {
	t.Helper()
	origShutdown := gateway.ShutdownTimeout
	origProgressRelay := gateway.ProgressRelayTimeout
	origBackendConnect := backendConnectTimeout
	origReloadDrain := reloadDrainTimeout
	origElicit := elicitTimeout
	origTelemetryShutdown := telemetryShutdownTimeout
	origBackoffMin := backendBackoffMin
	origBackoffMax := backendBackoffMax
	t.Cleanup(func() {
		gateway.ShutdownTimeout = origShutdown
		gateway.ProgressRelayTimeout = origProgressRelay
		backendConnectTimeout = origBackendConnect
		reloadDrainTimeout = origReloadDrain
		elicitTimeout = origElicit
		telemetryShutdownTimeout = origTelemetryShutdown
		backendBackoffMin = origBackoffMin
		backendBackoffMax = origBackoffMax
	})
}

func TestApplyTimeouts_OverridesWhenSet(t *testing.T) {
	saveTimeoutVars(t)

	applyTimeouts(config.TimeoutsConfig{
		Shutdown:          config.Duration(11 * time.Second),
		TelemetryShutdown: config.Duration(12 * time.Second),
		BackendConnect:    config.Duration(13 * time.Second),
		ReloadDrain:       config.Duration(14 * time.Minute),
		Elicit:            config.Duration(15 * time.Minute),
		ProgressRelay:     config.Duration(16 * time.Second),
		BackendBackoffMin: config.Duration(17 * time.Second),
		BackendBackoffMax: config.Duration(18 * time.Second),
	})

	if gateway.ShutdownTimeout != 11*time.Second {
		t.Errorf("gateway.ShutdownTimeout = %v, want 11s", gateway.ShutdownTimeout)
	}
	if telemetryShutdownTimeout != 12*time.Second {
		t.Errorf("telemetryShutdownTimeout = %v, want 12s", telemetryShutdownTimeout)
	}
	if backendConnectTimeout != 13*time.Second {
		t.Errorf("backendConnectTimeout = %v, want 13s", backendConnectTimeout)
	}
	if reloadDrainTimeout != 14*time.Minute {
		t.Errorf("reloadDrainTimeout = %v, want 14m", reloadDrainTimeout)
	}
	if elicitTimeout != 15*time.Minute {
		t.Errorf("elicitTimeout = %v, want 15m", elicitTimeout)
	}
	if gateway.ProgressRelayTimeout != 16*time.Second {
		t.Errorf("gateway.ProgressRelayTimeout = %v, want 16s", gateway.ProgressRelayTimeout)
	}
	if backendBackoffMin != 17*time.Second {
		t.Errorf("backendBackoffMin = %v, want 17s", backendBackoffMin)
	}
	if backendBackoffMax != 18*time.Second {
		t.Errorf("backendBackoffMax = %v, want 18s", backendBackoffMax)
	}
}

func TestApplyTimeouts_KeepsDefaultsWhenUnset(t *testing.T) {
	saveTimeoutVars(t)
	origShutdown := gateway.ShutdownTimeout
	origProgressRelay := gateway.ProgressRelayTimeout
	origBackendConnect := backendConnectTimeout
	origReloadDrain := reloadDrainTimeout
	origElicit := elicitTimeout
	origTelemetryShutdown := telemetryShutdownTimeout
	origBackoffMin := backendBackoffMin
	origBackoffMax := backendBackoffMax

	applyTimeouts(config.TimeoutsConfig{})

	if gateway.ShutdownTimeout != origShutdown {
		t.Errorf("gateway.ShutdownTimeout changed to %v, want unchanged %v", gateway.ShutdownTimeout, origShutdown)
	}
	if gateway.ProgressRelayTimeout != origProgressRelay {
		t.Errorf("gateway.ProgressRelayTimeout changed to %v, want unchanged %v", gateway.ProgressRelayTimeout, origProgressRelay)
	}
	if backendConnectTimeout != origBackendConnect {
		t.Errorf("backendConnectTimeout changed to %v, want unchanged %v", backendConnectTimeout, origBackendConnect)
	}
	if reloadDrainTimeout != origReloadDrain {
		t.Errorf("reloadDrainTimeout changed to %v, want unchanged %v", reloadDrainTimeout, origReloadDrain)
	}
	if elicitTimeout != origElicit {
		t.Errorf("elicitTimeout changed to %v, want unchanged %v", elicitTimeout, origElicit)
	}
	if telemetryShutdownTimeout != origTelemetryShutdown {
		t.Errorf("telemetryShutdownTimeout changed to %v, want unchanged %v", telemetryShutdownTimeout, origTelemetryShutdown)
	}
	if backendBackoffMin != origBackoffMin {
		t.Errorf("backendBackoffMin changed to %v, want unchanged %v", backendBackoffMin, origBackoffMin)
	}
	if backendBackoffMax != origBackoffMax {
		t.Errorf("backendBackoffMax changed to %v, want unchanged %v", backendBackoffMax, origBackoffMax)
	}
}
