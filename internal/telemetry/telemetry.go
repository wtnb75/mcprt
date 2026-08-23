// Package telemetry configures mcprt's OpenTelemetry tracing from the
// standard OTEL_* environment variables, so mcprt's tracing behavior
// matches any other OTel-instrumented service without mcprt-specific
// config (see docs/superpowers/specs/2026-08-21-otel-tracing-design.md).
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/contrib/propagators/autoprop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Setup configures the global TracerProvider and propagator from standard
// OTEL_* environment variables (OTEL_EXPORTER_OTLP_ENDPOINT,
// OTEL_TRACES_EXPORTER, OTEL_PROPAGATORS, OTEL_SERVICE_NAME, ...), so
// mcprt's tracing behavior matches any other OTel-instrumented service
// without mcprt-specific config. It returns a shutdown func that flushes
// buffered spans; callers must invoke it before process exit.
func Setup(ctx context.Context) (shutdown func(context.Context) error, err error) {
	exporter, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("configuring span exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("mcprt")),
		resource.WithFromEnv(), // OTEL_SERVICE_NAME etc. override the default
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("building resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(autoprop.NewTextMapPropagator()) // OTEL_PROPAGATORS-driven; default tracecontext+baggage
	return tp.Shutdown, nil
}
