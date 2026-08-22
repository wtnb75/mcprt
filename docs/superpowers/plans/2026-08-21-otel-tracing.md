# OpenTelemetry Tracing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make mcprt a genuine OpenTelemetry tracing participant — not just a context relay — by generating a span around every backend call made over the HTTP transport, continuing an inbound `traceparent`, propagating trace context to HTTP backends, and surfacing `trace_id`/`span_id` in the audit log.

**Architecture:** A new `internal/telemetry` package configures the global `TracerProvider`/propagator once at startup from standard `OTEL_*` environment variables. A new `internal/gateway/tracing.go` provides `startCallSpan` (extracts an inbound `traceparent` from the per-call HTTP headers the SDK already exposes via `req.Extra.Header`, then starts a span — a no-op for stdio calls, which never carry `Extra.Header`) and `recordOutcome` (marks a span as failed). The four existing `internal/gateway/gateway.go` handlers each wrap their backend call with these two calls, without changing their exported signatures. `internal/backend/backend.go` gains a `tracingRoundTripper` that injects the active span's trace context into outbound HTTP requests to backends, continuing the trace across the hop. `internal/cli/server.go` calls `internal/telemetry.Setup` once at startup and its returned `shutdown` func on exit. Finally, `internal/gateway/audit.go`'s existing `logCall` (from the already-implemented audit-logging feature) gains two extra fields, `trace_id`/`span_id`, read from the span-bearing `ctx` the handlers already pass it — no call-site changes needed there.

**Tech Stack:** Go 1.25, `go.opentelemetry.io/otel` v1.45.x (`otel`, `otel/sdk`, `otel/trace`, `otel/attribute`, `otel/codes`, `otel/propagation`, `otel/semconv/v1.43.0`, `otel/sdk/resource`, `otel/sdk/trace`, `otel/sdk/trace/tracetest`), `go.opentelemetry.io/contrib/exporters/autoexport`, `go.opentelemetry.io/contrib/propagators/autoprop`, `github.com/modelcontextprotocol/go-sdk` v1.7.0, `log/slog`, standard `testing`.

**Spec:** `docs/superpowers/specs/2026-08-21-otel-tracing-design.md`

**Depends on:** the audit-logging feature (`docs/superpowers/specs/2026-08-21-audit-logging-design.md`), already implemented on `main` — this plan's Task 6 modifies its `internal/gateway/audit.go`/`logCall`.

## Global Constraints

- Tracing applies only to calls served over the HTTP transport. A call arrives with a non-empty `req.Extra.Header` if and only if it came in over HTTP (the go-sdk populates `RequestExtra.Header` from the real HTTP request per call; stdio calls never set `Extra` or leave `Extra.Header` empty). `startCallSpan` must treat an empty/nil `Extra.Header` as "no tracing for this call" and return `ctx` unchanged plus a non-recording span — never error, never behave differently from pre-tracing code for stdio.
- No mcprt-specific config field for enabling/disabling tracing or picking an exporter/endpoint. All of that is driven by standard `OTEL_*` environment variables (`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_TRACES_EXPORTER`, `OTEL_PROPAGATORS`, `OTEL_SERVICE_NAME`, ...) via `autoexport`/`autoprop`/`resource.WithFromEnv()`. To fully disable tracing, operators set `OTEL_TRACES_EXPORTER=none`.
- Every test in this plan must be self-contained and never depend on a reachable OTLP collector: use `tracetest.NewInMemoryExporter()` (or `sdktrace.WithSyncer`) as a test-local `TracerProvider`, and/or set `OTEL_TRACES_EXPORTER=none` via `t.Setenv` (or, for `internal/cli`, via a package-wide `TestMain`) wherever code under test might otherwise attempt a live OTLP connection. Test output must stay pristine — no stray "connection refused" / export-failure noise.
- `internal/telemetry.Setup` failing (e.g. malformed `OTEL_*` env vars) must fail `runServer` before any listener starts, with a wrapped error.
- `logCall`'s existing behavior for callers that don't wire tracing (this plan's audit.go change is additive) is unaffected: `trace_id`/`span_id` are appended only when `trace.SpanContextFromContext(ctx).IsValid()` is true; they are simply absent otherwise, exactly parallel to how `remote_addr` is already handled.
- `go build ./...`, `go vet ./...`, `gofmt -l .`, and `go test ./...` must all be clean at the end of every task.
- Run `golangci-lint run ./...` at the end of every task if the `golangci-lint` binary is available (`command -v golangci-lint`); skip silently if not.
- New external dependencies ARE expected and required for this plan (unlike the audit-logging plan) — `go.opentelemetry.io/*` and its transitive dependency tree. Only add packages this plan's tasks actually import; don't pull in exporters/propagators beyond `autoexport`/`autoprop` "speak for themselves" from `OTEL_*` env vars.

---

### Task 1: `internal/telemetry.Setup`

**Files:**
- Create: `internal/telemetry/telemetry.go`
- Test: `internal/telemetry/telemetry_test.go`
- Modify: `go.mod`, `go.sum` (via `go get`/`go mod tidy`)

**Interfaces:**
- Produces: `telemetry.Setup(ctx context.Context) (shutdown func(context.Context) error, err error)`, consumed by Task 5 (`internal/cli/server.go`).
- Consumes: nothing from other tasks.

- [ ] **Step 1: Write the failing test**

Create `internal/telemetry/telemetry_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/telemetry/... -v`
Expected: FAIL — `internal/telemetry` doesn't exist yet (`no Go files in ...` / `cannot find package`).

- [ ] **Step 3: Add the OpenTelemetry dependencies**

Run:
```bash
go get go.opentelemetry.io/otel@latest \
  go.opentelemetry.io/otel/sdk@latest \
  go.opentelemetry.io/contrib/exporters/autoexport@latest \
  go.opentelemetry.io/contrib/propagators/autoprop@latest
```

This pulls in a sizeable transitive dependency tree (gRPC, Prometheus client, multiple OTLP/stdout exporter packages, several propagator packages) — this is expected and inherent to `autoexport`'s multi-exporter, env-var-driven design (it must be able to construct any exporter `OTEL_TRACES_EXPORTER` might name); don't try to trim it.

Before writing `telemetry.go`, confirm the semconv package's latest available version directory matches this plan's `v1.43.0` (it may have moved on since this plan was written):

```bash
ls "$(go env GOMODCACHE)"/go.opentelemetry.io/otel@*/semconv | sort -V | tail -1
```

If it prints something other than `v1.43.0`, use that version's import path instead everywhere `semconv/v1.43.0` appears below.

- [ ] **Step 4: Write the implementation**

Create `internal/telemetry/telemetry.go`:

```go
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
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/telemetry/... -v`
Expected: PASS.

- [ ] **Step 6: Tidy go.mod/go.sum**

Run: `go mod tidy`
Expected: no errors; `go.mod`/`go.sum` gain the direct requires above plus their transitive tree.

- [ ] **Step 7: Format, vet, lint**

Run: `gofmt -l internal/telemetry/telemetry.go internal/telemetry/telemetry_test.go && go build ./... && go vet ./... && (command -v golangci-lint >/dev/null && golangci-lint run ./... || true)`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/telemetry/telemetry.go internal/telemetry/telemetry_test.go go.mod go.sum
git commit -m "feat: add internal/telemetry.Setup for OTel tracing configuration"
```

---

### Task 2: `internal/gateway/tracing.go` — span helpers

**Files:**
- Create: `internal/gateway/tracing.go`
- Test: `internal/gateway/tracing_test.go`
- Modify: `go.mod`, `go.sum` (`go.opentelemetry.io/otel/trace` gets promoted to a direct require)

**Interfaces:**
- Produces: `startCallSpan(ctx context.Context, extra *mcp.RequestExtra, spanName string, attrs ...attribute.KeyValue) (context.Context, trace.Span)`, `recordOutcome(span trace.Span, err error)`, and a package-level `var tracer trace.Tracer` — consumed by Task 3 (the four gateway handlers).
- Consumes: nothing from other tasks. (This task adds no dependency on Task 1's `internal/telemetry` package — it uses `go.opentelemetry.io/otel`'s global `Tracer`/`GetTextMapPropagator` functions directly, which pick up whatever `TracerProvider`/propagator Task 1's `Setup` — or a test — installs later, since `otel.Tracer(name)` returns a delegating tracer bound to the *current* global provider at call time, not at var-init time.)

- [ ] **Step 1: Write the failing tests**

Create `internal/gateway/tracing_test.go` (package `gateway`, since it exercises unexported `startCallSpan`/`recordOutcome`):

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gateway/... -run 'TestStartCallSpan|TestRecordOutcome' -v`
Expected: FAIL to compile — `startCallSpan`, `recordOutcome`, and the package-level `tracer` var don't exist yet.

- [ ] **Step 3: Write the implementation**

Create `internal/gateway/tracing.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/gateway/... -run 'TestStartCallSpan|TestRecordOutcome' -v`
Expected: PASS.

- [ ] **Step 5: Run the full gateway package test suite**

Run: `go test ./internal/gateway/... -v`
Expected: PASS, every existing test still green (this task adds a new file; it doesn't touch any existing one).

- [ ] **Step 6: Tidy go.mod/go.sum**

Run: `go mod tidy`
Expected: `go.opentelemetry.io/otel/trace` is promoted from indirect to a direct require (it was already vendored transitively via Task 1's `otel/sdk`, but this file is the first to import it directly).

- [ ] **Step 7: Format, vet, lint**

Run: `gofmt -l internal/gateway/tracing.go internal/gateway/tracing_test.go && go build ./... && go vet ./... && (command -v golangci-lint >/dev/null && golangci-lint run ./... || true)`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/gateway/tracing.go internal/gateway/tracing_test.go go.mod go.sum
git commit -m "feat: add startCallSpan/recordOutcome span helpers for gateway handlers"
```

---

### Task 3: Wire spans into the four gateway handlers

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/gateway_test.go`

**Interfaces:**
- Consumes: `startCallSpan`/`recordOutcome`/`tracer` (Task 2).
- Produces: nothing new — none of the four handlers' or `registerX`/`New`'s signatures change. Later tasks don't depend on anything from this task beyond "spans now exist in production," which Task 6's tests exercise indirectly through `logCall`.

This task is a pure body edit inside each of the four handler closures in `internal/gateway/gateway.go`: `callHandler`, `resourceReadHandler`, `resourceTemplateReadHandler`, `promptGetHandler`. Each gets two new lines — a `startCallSpan` call right after the existing `start := time.Now()` line (reassigning `ctx`), a `defer span.End()`, and a `recordOutcome(span, err)` call right after the backend call, before the existing `logCall` call. `logCall` itself needs no changes here (Task 6 handles it) — it already receives `ctx` as its first parameter, and once handlers pass a span-bearing `ctx` into it, `logCall` will pick up trace/span IDs automatically as soon as Task 6 lands.

**A verified gotcha this task's tests must account for:** `gateway_test.go` is a black-box test file (`package gateway_test`) and cannot reassign `internal/gateway`'s unexported package-level `tracer` var the way Task 2's internal tests do. The OTel Go SDK's global package only rebinds an already-obtained `trace.Tracer` (like that package-level var, created once at program-init time) to a new `TracerProvider` the *first* time `otel.SetTracerProvider` ever replaces the initial default delegating provider in the process — a *second* `otel.SetTracerProvider` call from a later test does **not** retarget tracers obtained earlier; it only updates what `otel.GetTracerProvider()` returns to *future* callers of `otel.Tracer(...)`. Confirmed empirically while writing this plan: two black-box tests that each independently call `otel.SetTracerProvider(freshProvider)` and then check their own `tracetest.InMemoryExporter` pass individually but the second one silently observes zero spans when run together, because the package-level `tracer` var stayed bound to the *first* test's (already torn-down) provider. The fix is to install exactly one shared `TracerProvider`/`InMemoryExporter` pair once, via a package-level `TestMain`, and have every span-checking test `Reset()` the shared exporter at its own start.

- [ ] **Step 1: Write the failing tests**

Add to `internal/gateway/gateway_test.go` (add `"go.opentelemetry.io/otel"`, `"go.opentelemetry.io/otel/attribute"`, `"go.opentelemetry.io/otel/sdk/trace/tracetest"`, and `sdktrace "go.opentelemetry.io/otel/sdk/trace"` to its import block):

```go
// testSpanExporter is the single, shared in-memory span exporter for this
// whole test binary -- see TestMain and the Task 3 plan note above for why
// each test that checks spans must Reset() it rather than installing its
// own fresh TracerProvider.
var testSpanExporter = tracetest.NewInMemoryExporter()

// TestMain installs testSpanExporter's TracerProvider exactly once, before
// any test runs, and sets a TraceContext propagator. This is the only
// otel.SetTracerProvider call in this test binary.
func TestMain(m *testing.M) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(testSpanExporter))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	os.Exit(m.Run())
}

// TestGateway_CallCreatesSpanWithParentFromTraceparent checks the core
// behavior change: a tool call arriving over HTTP with a traceparent
// header now produces one span, continuing that trace, with the expected
// name and attributes.
func TestGateway_CallCreatesSpanWithParentFromTraceparent(t *testing.T) {
	testSpanExporter.Reset()
	exp := testSpanExporter

	backendServer := newFakeBackendServer("backend-a", "ping")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-a", Items: toolsA}}, toolNameOf, toolRename, nil)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	// Every outbound request from this client (including the tools/call
	// POST) carries a fixed traceparent, simulating an already-traced
	// upstream caller.
	traceHeaders := http.Header{}
	traceHeaders.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   gw.URL,
		HTTPClient: &http.Client{Transport: headerSetRoundTripper{headers: traceHeaders, base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ping", Arguments: map[string]any{}}); err != nil {
		t.Fatalf("call ping: %v", err)
	}

	var callSpan *tracetest.SpanStub
	for i, s := range exp.GetSpans() {
		if s.Name == "tools/call" {
			callSpan = &exp.GetSpans()[i]
			break
		}
	}
	if callSpan == nil {
		t.Fatalf("no \"tools/call\" span recorded; spans = %+v", exp.GetSpans())
	}
	if callSpan.Parent.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("parent trace id = %s, want 4bf92f3577b34da6a3ce929d0e0e4736", callSpan.Parent.TraceID().String())
	}
	attrs := attribute.NewSet(callSpan.Attributes...)
	if v, ok := attrs.Value("mcp.backend"); !ok || v.AsString() != "backend-a" {
		t.Fatalf("mcp.backend attribute = %v (ok=%v), want backend-a", v, ok)
	}
	if v, ok := attrs.Value("mcp.tool.name"); !ok || v.AsString() != "ping" {
		t.Fatalf("mcp.tool.name attribute = %v (ok=%v), want ping", v, ok)
	}
}

// headerSetRoundTripper sets fixed headers on every outbound request,
// simulating a client that already carries trace context (or any other
// fixed header) on every request it sends — distinct from
// internal/backend's headerRoundTripper, which is production code for
// injecting configured backend-auth headers; this is test-only and lives
// in the gateway_test package, testing the gateway's inbound side.
type headerSetRoundTripper struct {
	headers http.Header
	base    http.RoundTripper
}

func (h headerSetRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, vs := range h.headers {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	return h.base.RoundTrip(req)
}

// TestGateway_ResourceReadCreatesSpan checks that the resource-read
// handler (a different attribute set than the tool handler) also produces
// a span when called over HTTP, without a traceparent header this time
// (exercising the "no inbound parent, so a new root span starts" path).
func TestGateway_ResourceReadCreatesSpan(t *testing.T) {
	testSpanExporter.Reset()
	exp := testSpanExporter

	backendServer := newFakeResourceBackendServer("backend-a", "file:///a.txt")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	resourcesA, err := connA.ListResources(ctx)
	if err != nil {
		t.Fatalf("list backend-a resources: %v", err)
	}
	resourceNameOf := func(r *mcp.Resource) string { return r.URI }
	resourceRename := func(r *mcp.Resource, name string) *mcp.Resource { c := *r; c.URI = name; return &c }
	table := router.Resolve([]router.Entry[*mcp.Resource]{{BackendName: "backend-a", Items: resourcesA}}, resourceNameOf, resourceRename, nil)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Resources: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///a.txt"}); err != nil {
		t.Fatalf("read resource: %v", err)
	}

	var readSpan *tracetest.SpanStub
	for i, s := range exp.GetSpans() {
		if s.Name == "resources/read" {
			readSpan = &exp.GetSpans()[i]
			break
		}
	}
	if readSpan == nil {
		t.Fatalf("no \"resources/read\" span recorded; spans = %+v", exp.GetSpans())
	}
	attrs := attribute.NewSet(readSpan.Attributes...)
	if v, ok := attrs.Value("mcp.resource.uri"); !ok || v.AsString() != "file:///a.txt" {
		t.Fatalf("mcp.resource.uri attribute = %v (ok=%v), want file:///a.txt", v, ok)
	}
}
```

Also add `"io"`, `"os"`, and `"go.opentelemetry.io/otel/propagation"` to `gateway_test.go`'s import block if not already present (`io.Discard` is used to keep this test's own logger silent; `os` is for `TestMain`'s `os.Exit`; `slog`/`http`/`httptest`/`context` etc. are already imported). There is no pre-existing `TestMain` in this file or package to conflict with.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gateway/... -run 'TestGateway_CallCreatesSpanWithParentFromTraceparent|TestGateway_ResourceReadCreatesSpan' -v`
Expected: FAIL — zero spans recorded (`callSpan`/`readSpan` is nil), since the handlers don't create spans yet. (The new `TestMain` itself doesn't fail anything by existing — it just installs the shared exporter — so this step's failure comes entirely from the assertions inside the two new tests, exactly as with any other new-test RED step.)

- [ ] **Step 3: Wire span creation into the four handlers**

In `internal/gateway/gateway.go`, add `"go.opentelemetry.io/otel/attribute"` to the import block, then change each handler:

`resourceReadHandler`:

```go
func resourceReadHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalURI string) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		start := time.Now()
		ctx, span := startCallSpan(ctx, req.Extra, "resources/read",
			attribute.String("mcp.backend", b.Name),
			attribute.String("mcp.resource.uri", originalURI))
		defer span.End()

		result, err := b.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: originalURI})
		recordOutcome(span, err)
		logCall(ctx, logger, "resource", "uri", originalURI, b.Name, req.Session, nil, maskKeys, start, err)
		return result, err
	}
}
```

`resourceTemplateReadHandler`:

```go
func resourceTemplateReadHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		start := time.Now()
		ctx, span := startCallSpan(ctx, req.Extra, "resources/templates/read",
			attribute.String("mcp.backend", b.Name),
			attribute.String("mcp.resource.uri", req.Params.URI))
		defer span.End()

		result, err := b.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
		recordOutcome(span, err)
		logCall(ctx, logger, "resource template", "uri", req.Params.URI, b.Name, req.Session, nil, maskKeys, start, err)
		return result, err
	}
}
```

`promptGetHandler`:

```go
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
		logCall(ctx, logger, "prompt", "prompt", originalName, b.Name, req.Session, req.Params.Arguments, maskKeys, start, err)
		return result, err
	}
}
```

`callHandler`:

```go
// callHandler forwards a tools/call to originalName on backend b, passing
// the raw arguments through unchanged. It wraps the call in a span
// (startCallSpan is a no-op for stdio-originated calls) and logs it via
// logCall, success or failure, so a dead or erroring backend — and normal
// usage — is visible to the operator.
func callHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalName string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		ctx, span := startCallSpan(ctx, req.Extra, "tools/call",
			attribute.String("mcp.backend", b.Name),
			attribute.String("mcp.tool.name", originalName))
		defer span.End()

		result, err := b.Session.CallTool(ctx, &mcp.CallToolParams{
			Name:      originalName,
			Arguments: req.Params.Arguments,
		})
		recordOutcome(span, err)
		logCall(ctx, logger, "tool", "tool", originalName, b.Name, req.Session, req.Params.Arguments, maskKeys, start, err)
		return result, err
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/gateway/... -v`
Expected: PASS, every test in the package (existing ones plus the two new ones).

- [ ] **Step 5: Format, vet, lint**

Run: `gofmt -l internal/gateway/gateway.go internal/gateway/gateway_test.go && go build ./... && go vet ./... && (command -v golangci-lint >/dev/null && golangci-lint run ./... || true)`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go
git commit -m "feat: wrap backend calls in OTel spans across the four gateway handlers"
```

---

### Task 4: `internal/backend` outbound trace propagation

**Files:**
- Modify: `internal/backend/backend.go`
- Modify: `internal/backend/backend_test.go`

**Interfaces:**
- Consumes: nothing from other tasks (uses `go.opentelemetry.io/otel`/`otel/propagation` directly, already in `go.mod` since Task 1).
- Produces: nothing consumed by later tasks.

This task is independent of Tasks 2–3's gateway-side wiring: it only changes how an HTTP backend's outbound requests are built, continuing whatever trace context (if any) is present in the `ctx` passed to `backend.Connect`/`b.Session.*` calls.

- [ ] **Step 1: Write the failing tests**

Add to `internal/backend/backend_test.go` (add `"go.opentelemetry.io/otel"`, `"go.opentelemetry.io/otel/propagation"`, and `sdktrace "go.opentelemetry.io/otel/sdk/trace"` to its import block):

```go
func TestConnect_HTTP_InjectsTraceparentWhenSpanActive(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	var gotHeader string
	seen := false
	mcpHandler := newFakeMCPHandler()
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !seen {
			gotHeader = r.Header.Get("traceparent")
			seen = true
		}
		mcpHandler.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	tracer := otel.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "test-call")
	defer span.End()

	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if gotHeader == "" {
		t.Fatal("no traceparent header was injected on the outbound request despite an active span in ctx")
	}
}

func TestConnect_HTTP_NoTraceparentWithoutActiveSpan(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	var gotHeader string
	seen := false
	mcpHandler := newFakeMCPHandler()
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !seen {
			gotHeader = r.Header.Get("traceparent")
			seen = true
		}
		mcpHandler.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	ctx := context.Background()
	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if gotHeader != "" {
		t.Fatalf("traceparent = %q, want none for a ctx with no active span", gotHeader)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/backend/... -run 'TestConnect_HTTP_InjectsTraceparent|TestConnect_HTTP_NoTraceparent' -v`
Expected: `TestConnect_HTTP_InjectsTraceparentWhenSpanActive` FAILs (`gotHeader` is empty — nothing injects the header yet); `TestConnect_HTTP_NoTraceparentWithoutActiveSpan` trivially passes already (no code path adds any header). That's expected: only the first test drives the real behavior change.

- [ ] **Step 3: Add `tracingRoundTripper` and wire it in**

In `internal/backend/backend.go`, add `"go.opentelemetry.io/otel"` and `"go.opentelemetry.io/otel/propagation"` to the import block, then change the `http` case in `Connect`:

```go
	case "http":
		base, err := httpBaseTransport(cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("backend %q: %w", cfg.Name, err)
		}
		transport = &mcp.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: &http.Client{Transport: tracingRoundTripper{base: headerRoundTripper{headers: cfg.Headers, base: base}}},
		}
```

Then add the new type at the end of the file (after `headerRoundTripper`'s `RoundTrip` method):

```go
// tracingRoundTripper injects the current span's trace context into
// outbound requests to an HTTP backend, so a call that arrived over HTTP
// (and therefore has an active span in ctx) continues the same trace on the
// backend side. It does not start its own span -- the handler's span
// already covers the call's duration; a bare stdio-originated ctx carries
// no span, so Inject is a no-op and no traceparent header is sent.
type tracingRoundTripper struct {
	base http.RoundTripper
}

func (t tracingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	otel.GetTextMapPropagator().Inject(req.Context(), propagation.HeaderCarrier(req.Header))
	return t.base.RoundTrip(req)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/backend/... -v`
Expected: PASS, every test in the package (existing ones — including `TestConnect_HTTPWithHeaders`, unaffected since `tracingRoundTripper` just wraps `headerRoundTripper` without changing its behavior — plus the two new ones).

- [ ] **Step 5: Format, vet, lint**

Run: `gofmt -l internal/backend/backend.go internal/backend/backend_test.go && go build ./... && go vet ./... && (command -v golangci-lint >/dev/null && golangci-lint run ./... || true)`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/backend/backend.go internal/backend/backend_test.go
git commit -m "feat: inject trace context into outbound HTTP backend requests"
```

---

### Task 5: Wire `internal/telemetry.Setup` into `internal/cli/server.go`

**Files:**
- Modify: `internal/cli/server.go`
- Modify: `internal/cli/server_internal_test.go`

**Interfaces:**
- Consumes: `telemetry.Setup` (Task 1).
- Produces: nothing new consumed by later tasks.

`runServer` calls `telemetry.Setup(ctx)` as its very first action — before `config.Load` — so a broken `OTEL_*` environment fails the server before it does anything else, per the spec. Its `shutdown` is deferred, bounded by a new `telemetryShutdownTimeout` var (same pattern as `internal/gateway`'s existing `shutdownTimeout`).

Because every existing test that calls `runServer` (directly, or via `cli.Execute`) will now also call `telemetry.Setup`, and `Setup`'s default behavior (no `OTEL_*` env vars set) targets `http://localhost:4318` and logs periodic connection-refused noise from a background goroutine when nothing is listening there, this task also adds a package-wide `TestMain` to force `OTEL_TRACES_EXPORTER=none` for every test in `internal/cli`, keeping test output pristine and tests network-independent — this is not scope creep on unrelated tests, it's this task's own environment-hygiene requirement for the change it's making.

- [ ] **Step 1: Write the failing test**

Add to `internal/cli/server_internal_test.go` (add `"go.opentelemetry.io/otel"` to its import block):

```go
// TestMain forces OTEL_TRACES_EXPORTER=none for every test in this
// package's test binary: once runServer wires internal/telemetry.Setup,
// every test that calls runServer (directly, or via cli.Execute) would
// otherwise attempt the OTel SDK's default OTLP/HTTP export target
// (http://localhost:4318), which nothing here listens on -- Setup itself
// wouldn't fail (BatchSpanProcessor exports asynchronously), but its
// background goroutine would log periodic connection-refused noise into
// test output. Setting "none" makes autoexport.NewSpanExporter return a
// genuine no-op exporter, keeping every test in this package fast,
// deterministic, and independent of network access.
func TestMain(m *testing.M) {
	os.Setenv("OTEL_TRACES_EXPORTER", "none")
	os.Exit(m.Run())
}

// TestRunServer_ConfiguresGlobalTracerProvider checks that runServer wires
// internal/telemetry.Setup: after it starts, a span started via the
// package-level otel.Tracer (which delegates to whatever global provider
// is currently installed) is recording -- the default, pre-Setup global
// provider always returns a non-recording no-op span, so recording=true
// is only possible once Setup has installed a real SDK TracerProvider.
func TestRunServer_ConfiguresGlobalTracerProvider(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stdin pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = origStdin
		_ = w.Close()
		_ = r.Close()
	})

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("listen:\n  stdio: true\n\nbackends: []\n"), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, logger, configPath) }()

	time.Sleep(200 * time.Millisecond)
	_, span := otel.Tracer("probe").Start(context.Background(), "probe")
	recording := span.IsRecording()
	span.End()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServer exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer did not exit within 5s of context cancellation")
	}

	if !recording {
		t.Fatal("global TracerProvider was not configured by runServer (span.IsRecording() = false)")
	}
}
```

This test must not run with `t.Parallel()` (nor alongside another test touching `os.Stdin`), same constraint as the existing `TestRunServer_LogsListening`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli/... -run TestRunServer_ConfiguresGlobalTracerProvider -v`
Expected: FAIL — `recording` is `false` (`runServer` doesn't call `telemetry.Setup` yet, so the global `TracerProvider` stays the default no-op one).

- [ ] **Step 3: Wire `telemetry.Setup` into `runServer`**

In `internal/cli/server.go`, add `"github.com/wtnb75/mcprt/internal/telemetry"` to the import block, then add a shutdown-timeout var near the existing `backendConnectTimeout`:

```go
// telemetryShutdownTimeout bounds internal/telemetry.Setup's returned
// shutdown func, which flushes buffered spans to the configured OTLP
// exporter. A var so tests can shrink it.
var telemetryShutdownTimeout = 5 * time.Second
```

Then change the start of `runServer`:

```go
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
```

(The rest of `runServer` is unchanged; this only adds the new leading block and reuses the existing `err` name in the next statement.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/... -v`
Expected: PASS, every test in the package — including all the pre-existing ones that call `runServer`/`cli.Execute`, now implicitly covered by the new `TestMain`'s `OTEL_TRACES_EXPORTER=none`.

- [ ] **Step 5: Run the full build**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Format, vet, lint**

Run: `gofmt -l internal/cli/server.go internal/cli/server_internal_test.go && go vet ./internal/cli/... && (command -v golangci-lint >/dev/null && golangci-lint run ./internal/cli/... || true)`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go
git commit -m "feat: configure OTel tracing at server startup and flush spans on shutdown"
```

---

### Task 6: `trace_id`/`span_id` in the audit log

**Files:**
- Modify: `internal/gateway/audit.go`
- Modify: `internal/gateway/audit_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: a span-bearing `ctx` from Task 3's handlers (indirectly, at the whole-feature level — this task's own tests exercise `logCall` directly with a synthetic span context, so it has no hard code dependency on Task 3).
- Produces: nothing consumed by later tasks (this is the last task).

`logCall` already takes `ctx` as its first parameter (from the audit-logging feature); this task only adds one more conditional block reading from it, exactly parallel to the existing `remote_addr` block.

- [ ] **Step 1: Write the failing tests**

Add to `internal/gateway/audit_test.go` (add `"go.opentelemetry.io/otel/trace"` to its import block):

```go
func TestLogCall_IncludesTraceIDAndSpanIDWhenSpanValid(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("TraceIDFromHex: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("SpanIDFromHex: %v", err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logCall(ctx, logger, "tool", "tool", "x", "backend-a", &mcp.ServerSession{}, nil, nil, time.Now(), nil)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if rec["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace_id = %v, want 4bf92f3577b34da6a3ce929d0e0e4736", rec["trace_id"])
	}
	if rec["span_id"] != "00f067aa0ba902b7" {
		t.Fatalf("span_id = %v, want 00f067aa0ba902b7", rec["span_id"])
	}
}

func TestLogCall_OmitsTraceIDAndSpanIDWhenNoSpan(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	logCall(context.Background(), logger, "tool", "tool", "x", "backend-a", &mcp.ServerSession{}, nil, nil, time.Now(), nil)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if _, ok := rec["trace_id"]; ok {
		t.Fatalf("trace_id present = %v, want omitted for a ctx with no span", rec["trace_id"])
	}
	if _, ok := rec["span_id"]; ok {
		t.Fatalf("span_id present = %v, want omitted for a ctx with no span", rec["span_id"])
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gateway/... -run 'TestLogCall_IncludesTraceIDAndSpanID|TestLogCall_OmitsTraceIDAndSpanID' -v`
Expected: `TestLogCall_IncludesTraceIDAndSpanIDWhenSpanValid` FAILs (`rec["trace_id"]` is `nil`, since `logCall` doesn't add it yet); `TestLogCall_OmitsTraceIDAndSpanIDWhenNoSpan` trivially passes already.

- [ ] **Step 3: Add the `trace_id`/`span_id` fields to `logCall`**

In `internal/gateway/audit.go`, add `"go.opentelemetry.io/otel/trace"` to the import block, then change `logCall`:

```go
func logCall(ctx context.Context, logger *slog.Logger, kind, nameKey, name, backend string, sess *mcp.ServerSession, args any, maskKeys []string, start time.Time, err error) {
	attrs := []any{
		"backend", backend,
		nameKey, name,
		"session_id", sess.ID(),
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if ip := sess.InitializeParams(); ip != nil && ip.ClientInfo != nil {
		attrs = append(attrs, "client_name", ip.ClientInfo.Name, "client_version", ip.ClientInfo.Version)
	}
	if addr, ok := remoteAddrFromContext(ctx); ok {
		attrs = append(attrs, "remote_addr", addr)
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
	}
	if hasArgs(args) {
		attrs = append(attrs, "arguments", maskArguments(args, maskKeys))
	}
	if err != nil {
		logger.Error(kind+" call failed", append(attrs, "error", err)...)
		return
	}
	logger.Info(kind+" call", attrs...)
}
```

(Only the new `if sc := ...` block is added; every other line is unchanged.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/gateway/... -v`
Expected: PASS, every test in the package.

- [ ] **Step 5: Document tracing in the README**

In `README.md`, after the paragraph added by the audit-logging feature that begins "Every backend call (`tools/call`, `resources/read`, `prompts/get`) is logged..." (ends with "...replaced with `***` before logging."), add a new paragraph:

```
mcprt also participates in distributed tracing via OpenTelemetry: a call
served over the HTTP transport gets wrapped in a span (continuing an
inbound `traceparent` header if present), and if the routed backend is
itself an HTTP backend, the active span's trace context is injected into
the outbound request, so the trace continues across the hop. Tracing is
configured entirely through standard `OTEL_*` environment variables
(`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_TRACES_EXPORTER`, `OTEL_SERVICE_NAME`,
...) — there is no mcprt-specific config for it. Set
`OTEL_TRACES_EXPORTER=none` to disable it entirely. Calls made over the
stdio transport are never traced (there is no out-of-band channel to carry
trace context on stdio). When a call is traced, its audit log line (see
above) also carries `trace_id`/`span_id` fields.
```

- [ ] **Step 6: Format, vet, lint**

Run: `gofmt -l internal/gateway/audit.go internal/gateway/audit_test.go && go build ./... && go vet ./... && go test ./... && (command -v golangci-lint >/dev/null && golangci-lint run ./... || true)`
Expected: clean across the whole repo.

- [ ] **Step 7: Commit**

```bash
git add internal/gateway/audit.go internal/gateway/audit_test.go README.md
git commit -m "feat: add trace_id/span_id fields to the audit log"
```
