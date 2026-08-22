package gateway

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// setTestTracerProvider installs an in-memory-exporting TracerProvider as
// the global provider for the duration of the test, restoring the prior
// global provider/propagator afterward, and rebinds the package-level
// tracer var to it (otel.Tracer's delegation only kicks in for tracers
// obtained AFTER SetTracerProvider in some SDK versions' test doubles, so
// tests rebind explicitly rather than relying on delegation timing).
func setTestTracerProvider(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	prevTracer := tracer
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	tracer = otel.Tracer("test")
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		tracer = prevTracer
	})
	return exp
}

func TestStartCallSpan_ExtractsParentFromHeader(t *testing.T) {
	exp := setTestTracerProvider(t)

	h := http.Header{}
	h.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	ctx, span := startCallSpan(context.Background(), &mcp.RequestExtra{Header: h}, "tools/call",
		attribute.String("mcp.tool.name", "x"))
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Parent.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("parent trace id = %s, want 4bf92f3577b34da6a3ce929d0e0e4736", spans[0].Parent.TraceID().String())
	}
	if !trace.SpanContextFromContext(ctx).IsValid() {
		t.Fatal("ctx has no valid span context after startCallSpan")
	}
}

func TestStartCallSpan_NilExtraIsNoop(t *testing.T) {
	exp := setTestTracerProvider(t)

	ctx, span := startCallSpan(context.Background(), nil, "tools/call")
	span.End()

	if len(exp.GetSpans()) != 0 {
		t.Fatalf("got %d spans, want 0 for a nil Extra (stdio call)", len(exp.GetSpans()))
	}
	if trace.SpanContextFromContext(ctx).IsValid() {
		t.Fatal("ctx should carry no valid span for a nil Extra")
	}
}

func TestStartCallSpan_EmptyHeaderIsNoop(t *testing.T) {
	exp := setTestTracerProvider(t)

	ctx, span := startCallSpan(context.Background(), &mcp.RequestExtra{Header: http.Header{}}, "tools/call")
	span.End()

	if len(exp.GetSpans()) != 0 {
		t.Fatalf("got %d spans, want 0 for an empty Header (stdio call)", len(exp.GetSpans()))
	}
	if trace.SpanContextFromContext(ctx).IsValid() {
		t.Fatal("ctx should carry no valid span for an empty Header")
	}
}

func TestRecordOutcome_SetsErrorStatusOnFailure(t *testing.T) {
	exp := setTestTracerProvider(t)

	_, span := tracer.Start(context.Background(), "x")
	recordOutcome(span, errors.New("boom"))
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Fatalf("status code = %v, want Error", spans[0].Status.Code)
	}
	if spans[0].Status.Description != "boom" {
		t.Fatalf("status description = %q, want boom", spans[0].Status.Description)
	}
}

func TestRecordOutcome_LeavesStatusUnsetOnSuccess(t *testing.T) {
	exp := setTestTracerProvider(t)

	_, span := tracer.Start(context.Background(), "x")
	recordOutcome(span, nil)
	span.End()

	spans := exp.GetSpans()
	if spans[0].Status.Code != codes.Unset {
		t.Fatalf("status code = %v, want Unset", spans[0].Status.Code)
	}
}
