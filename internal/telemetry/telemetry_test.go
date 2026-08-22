package telemetry_test

import (
	"context"
	"testing"

	"github.com/wtnb75/mcprt/internal/telemetry"
)

// TestSetup_ReturnsWorkingShutdown checks Setup's happy path without
// depending on a reachable OTLP collector: OTEL_TRACES_EXPORTER=none makes
// autoexport.NewSpanExporter return a genuine no-op exporter, so Setup
// still runs its full configuration path (building a Resource, a
// TracerProvider, registering the global provider/propagator) and returns
// a shutdown func that itself succeeds.
func TestSetup_ReturnsWorkingShutdown(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")

	shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Setup returned a nil shutdown func")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestSetup_InvalidExporterNameFails checks the error-handling contract the
// spec requires: an invalid OTEL_TRACES_EXPORTER value must make Setup
// return an error (which internal/cli/server.go's runServer, wired up in
// Task 5, turns into "fail before starting any listener"), not silently
// fall back to a default.
func TestSetup_InvalidExporterNameFails(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "bogus-exporter-name")

	if _, err := telemetry.Setup(context.Background()); err == nil {
		t.Fatal("Setup with an invalid OTEL_TRACES_EXPORTER: expected an error, got nil")
	}
}
