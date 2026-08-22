package gateway

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracer is the package-wide Tracer every handler's startCallSpan call
// uses. It is bound to whatever global TracerProvider is installed at the
// time each Start call happens (otel.Tracer's delegation semantics), so
// this can be initialized before internal/telemetry.Setup runs — or before
// a test's own TracerProvider is installed, in tests that reassign it.
var tracer = otel.Tracer("github.com/wtnb75/mcprt/internal/gateway")

// startCallSpan starts a span for one backend call if extra carries HTTP
// headers (i.e. the call arrived over the HTTP transport); over stdio,
// extra is nil (or its Header is empty) and this is a no-op returning ctx
// unchanged and a non-recording span, so the rest of the handler behaves
// exactly as before tracing existed.
func startCallSpan(ctx context.Context, extra *mcp.RequestExtra, spanName string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if extra == nil || len(extra.Header) == 0 {
		return ctx, trace.SpanFromContext(ctx) // non-recording no-op span
	}
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(extra.Header))
	return tracer.Start(ctx, spanName, trace.WithAttributes(attrs...))
}

// recordOutcome marks span as failed when err is non-nil; on success it
// leaves the default (unset) status, per OTel convention. Safe to call on
// a non-recording span (the no-op case from startCallSpan): it's a no-op.
func recordOutcome(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
