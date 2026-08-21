# mcprt: OpenTelemetryトレーシング 設計

## 背景・目的

MCPプロトコル自体は分散トレーシング用の情報（OpenTelemetryのtrace_id/span_id等）を標準フィールドとして持たない。JSON-RPC 2.0ベースのメッセージフォーマットにトレースコンテキスト専用の枠がなく、唯一の拡張ポイントは実装依存の`_meta`フィールドのみである。

一方、mcprtはtool/resource/promptの呼び出しを別プロセス・別ホストのbackendへ転送するgatewayであり、「クライアント→mcprt→backend」という複数ホップにまたがるレイテンシの内訳や、backend呼び出し個々の成否をObservabilityバックエンド（Jaeger/Tempo等）で追跡できる価値は大きい。

本ドキュメントは、mcprtが単なるトレースコンテキストの中継役ではなく、**自らスパンを生成しOTLP経由でエクスポートする本格的な参加者**として振る舞うための設計を定義する。

## スコープ

含める:
- HTTPトランスポート経由で受けたtool/resource/resourceTemplate/prompt呼び出しに対する、backend呼び出しをラップするspanの生成
- 受信したHTTPリクエストヘッダーからの`traceparent`抽出（親スパンとしての継承）
- backendがHTTPトランスポートの場合の、outboundリクエストへの`traceparent`注入（cross-processなトレース継続）
- `TracerProvider`/exporterの標準OTel環境変数（`OTEL_*`）による自動設定
- 監査ログ（`internal/gateway/audit.go`の`logCall`、別spec・未実装）への`trace_id`/`span_id`フィールド追記

含めない（将来拡張）:
- stdioトランスポートでのトレースコンテキスト伝播（帯域外チャネルがなく、`_meta`を使う非標準の仕組みは費用対効果が低いため見送り）
- mcprt自身の設定ファイル（config.yaml）への専用フィールド追加（有効/無効やendpoint指定は標準`OTEL_*`環境変数に委ねる）
- サンプリング戦略のカスタマイズ（`OTEL_TRACES_SAMPLER`等、標準環境変数のデフォルトに委ねる）
- backend起動時の接続・List呼び出し（`connectBackends`）へのspan付与

## 全体アーキテクチャ

```
                    ┌───────────────────────────────┐
  client ─(HTTP)──▶ │ gateway handlers                │
 traceparent?       │ callHandler / resourceRead      │
                    │ Handler / resourceTemplate      │
                    │ ReadHandler / promptGetHandler  │
                    └───────────────┬─────────────────┘
                     req.Extra.Header (呼び出し単位) │
                                    ▼
                    ┌───────────────────────────────┐
                    │ req.Extra.Header が非空なら:      │
                    │  1. Extract → 親SpanContext復元   │
                    │  2. tracer.Start → span開始       │
                    │  3. backend呼び出し                │
                    │  4. span.End（エラー時はStatus設定）│
                    │ 空なら（stdio）: spanを作らずそのまま│
                    └───────────────┬─────────────────┘
                                    │ span付きctx
                                    ▼
                    ┌───────────────────────────────┐
                    │ internal/backend outbound       │
                    │ HTTPトランスポート                │
                    │  ctxのspanをInjectするだけ        │
                    │ （span生成はしない）               │
                    └───────────────────────────────┘

  internal/telemetry.Setup(ctx) が起動時に一度呼ばれ、
  TracerProvider/Propagatorをグローバル設定する
  （OTEL_* 環境変数から autoexport/autoprop 経由で構成）
```

### 技術的な前提（調査済み）

`github.com/modelcontextprotocol/go-sdk` v1.7.0の`mcp.CallToolRequest`等（`= mcp.ServerRequest[*mcp.CallToolParamsRaw]`等のエイリアス）は`Extra *RequestExtra`フィールドを持ち、`RequestExtra.Header`にその**呼び出し1回分の生HTTPリクエストヘッダー**が入る（`mcp/shared.go:595-606`、`mcp/streamable.go:1553`で`jreq.Extra = &RequestExtra{Header: req.Header, ...}`と設定される）。

これは監査ログ設計が使う「セッション確立時のcontextを以後全呼び出しの基底にする」`xcontext.Detach`ベースの伝播とは独立した、SDKが呼び出しごとに用意する仕組みである。stdioトランスポートでは`Extra.Header`が空になるため、これを「HTTP経由の呼び出しかどうか」の判定に直接使える。context経由の伝播トリックや、追加のミドルウェア配線は不要。

## コンポーネント構成

### `internal/telemetry`（新規パッケージ）

```go
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

`OTEL_TRACES_EXPORTER`/`OTEL_EXPORTER_OTLP_ENDPOINT`等が未設定の場合、OTel SDKの仕様上のデフォルト（OTLP/HTTP、`http://localhost:4318`）への接続が試みられる。コレクタが存在しない環境ではexport失敗ログが定期的に出るが、`BatchSpanProcessor`は非同期のためRPC応答はブロックしない。トレーシングを完全に無効化したい場合は運用側で`OTEL_TRACES_EXPORTER=none`を設定する（mcprt側に専用フラグは持たない）。

### `internal/gateway/gateway.go`（既存4ハンドラの変更）

各ハンドラの先頭・末尾に以下のパターンを追加する（例: `callHandler`）:

```go
var tracer = otel.Tracer("github.com/wtnb75/mcprt/internal/gateway")

func callHandler(logger *slog.Logger, b *backend.Backend, originalName string) mcp.ToolHandler {
    return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
        ctx, span := startCallSpan(ctx, req.Extra, "tools/call",
            attribute.String("mcp.backend", b.Name),
            attribute.String("mcp.tool.name", originalName))
        defer span.End()

        result, err := b.Session.CallTool(ctx, &mcp.CallToolParams{
            Name:      originalName,
            Arguments: req.Params.Arguments,
        })
        recordOutcome(span, err)
        if err != nil {
            logger.Error("backend call failed", "backend", b.Name, "tool", originalName, "error", err)
        }
        return result, err
    }
}
```

`resourceReadHandler`/`resourceTemplateReadHandler`/`promptGetHandler`も同型（spanName・属性キーのみ異なる: `resources/read`+`mcp.resource.uri`、`resources/templates/read`+`mcp.resource.uri`、`prompts/get`+`mcp.prompt.name`）。

### `internal/gateway/tracing.go`（新規）

```go
// startCallSpan starts a span for one backend call if extra carries HTTP
// headers (i.e. the call arrived over the HTTP transport); over stdio,
// extra.Header is empty and this is a no-op returning ctx unchanged and a
// non-recording span, so the rest of the handler behaves exactly as before
// tracing existed.
func startCallSpan(ctx context.Context, extra *mcp.RequestExtra, spanName string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
    if extra == nil || len(extra.Header) == 0 {
        return ctx, trace.SpanFromContext(ctx) // non-recording no-op span
    }
    ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(extra.Header))
    return tracer.Start(ctx, spanName, trace.WithAttributes(attrs...))
}

// recordOutcome marks span as failed when err is non-nil; on success it
// leaves the default (unset) status, per OTel convention.
func recordOutcome(span trace.Span, err error) {
    if err == nil {
        return
    }
    span.RecordError(err)
    span.SetStatus(codes.Error, err.Error())
}
```

`tracer.Start`が返す`trace.Span`が非recordingの場合（`extra`が空だった場合の`trace.SpanFromContext(ctx)`）でも、`span.End()`/`recordOutcome`の呼び出しは安全（no-op）。ハンドラ側の分岐を増やさずに済む。

### `internal/backend/backend.go`（outboundトランスポートの変更）

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

`Connect`内、`http.Client{Transport: headerRoundTripper{...}}`の構築箇所を以下に変更する:

```go
HTTPClient: &http.Client{Transport: tracingRoundTripper{base: headerRoundTripper{headers: cfg.Headers, base: base}}},
```

既存の`httpBaseTransport`（プロキシ設定）・`headerRoundTripper`（固定ヘッダー注入）のテストには影響しない（外側に1層足すだけ）。

### `internal/cli/server.go`（起動時の配線）

`runServer`の先頭で`internal/telemetry.Setup(ctx)`を呼び、返る`shutdown`をgraceful shutdown経路（既存の`shutdownTimeout`パターン）で呼ぶ。`Setup`が失敗した場合はサーバ起動前にエラーとして終了する。

### `internal/gateway/audit.go`（audit-logging実装後の追記、本specの対象）

`logCall`（audit-logging-designで新設される想定）の`attrs`組み立てに、以下を追加する:

```go
if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
    attrs = append(attrs, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
}
```

`ctx`はハンドラの`startCallSpan`が返したもの（stdio経由なら`sc.IsValid()`が`false`になるため何も付与されない）。

## データフロー

**HTTP経由のtool呼び出し**

1. clientが`tools/call`をPOST（`traceparent`ヘッダは任意）
2. SDKが`jreq.Extra = &RequestExtra{Header: req.Header, ...}`を設定し、`callHandler`が返す`mcp.ToolHandler`に`req`（`req.Extra.Header`込み）を渡す
3. `startCallSpan`: `req.Extra.Header`が非空なので、`Extract`で親SpanContextを復元（ヘッダがなければ空のまま＝新規rootスパンとして開始される）、`tracer.Start`でspan開始
4. `b.Session.CallTool(ctx, ...)`をspan付きctxで呼ぶ
5. backendがHTTP transportなら、`tracingRoundTripper.RoundTrip`が現在のspanを`traceparent`としてoutbound requestに`Inject` → backend側プロセスまでトレースが継続する
6. backendがstdio transport（子プロセス）なら伝播チャネルがないため繋がらないが、step3のspan自体はローカルで完結してexportされる（呼び出し時間・成否は見える）
7. `recordOutcome`でエラー時はspanをError状態にし、`span.End()`
8. （audit-logging実装後）`logCall`が`ctx`から`trace_id`/`span_id`を読み取りログ行に追加

**stdio経由の呼び出し**

1. clientが標準入力から`tools/call`を送信
2. `req.Extra`が`nil`（またはHeaderが空）→ `startCallSpan`は何もせず、非recordingのno-op spanを返す
3. backendがHTTPでも`Inject`は何も注入しない（ctxに有効なspanがないため）
4. 監査ログにも`trace_id`/`span_id`は出ない

resource/resourceTemplate/promptの3ハンドラも同型。

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| `req.Extra`が`nil`または`Extra.Header`が空（stdio、または将来の非HTTP transport） | `startCallSpan`が非recordingのno-op spanを返し、トレーシングは完全にno-op。従来の挙動と変わらない |
| `traceparent`ヘッダが不正な形式 | `Extract`が無視して空のSpanContextを返す（OTel SDK標準挙動）。新規rootスパンとして開始される |
| exporter未到達（OTLPエンドポイントに接続できない） | `BatchSpanProcessor`が非同期でexportするためRPC応答には影響しない。SDK内部で定期的にエラーログが出る（標準挙動として許容） |
| `internal/telemetry.Setup`自体が失敗（無効な`OTEL_*`環境変数など） | 起動時にエラーを返し、`runServer`はサーバ起動前に失敗として終了する |
| backend呼び出し自体がpanicする | 対象外（既存の`addTool`等のpanic-recoveryとは別レイヤーの話）。`defer span.End()`はpanic時にも実行されるためspanのリークはない |

## テスト方針

- **`internal/telemetry`**: `Setup`が正常系でエラーなく`shutdown`関数を返すこと。`OTEL_TRACES_EXPORTER=console`など外部接続不要な設定でのユニットテスト。
- **`internal/gateway/tracing_test.go`（新規）**
  - `sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))`をテスト用にセットし、`otel.SetTracerProvider`で差し替える
  - `req.Extra.Header`に`traceparent`を含めて`startCallSpan`を呼ぶ → 記録されたspanの親TraceIDが継承されていることを確認
  - `req.Extra`が`nil`の場合、spanが一切記録されない（in-memory exporterに何も溜まらない）ことを確認
  - `recordOutcome`がエラー時に`codes.Error`を設定することを確認
- **`internal/backend`**: `tracingRoundTripper`が、ctxに有効なspanがある場合のみ`traceparent`ヘッダを注入することを`httptest.Server`で検証（既存の`headerRoundTripper`テストと同じパターンで、span無しctxでは注入されないケースも確認）。
- **`internal/gateway`の既存ハンドラテスト拡張**: HTTP経由の呼び出しでspanが1つ生成されること、stdio経由（`req.Extra`なし）では生成されないことをend-to-endで確認。
- 外部のOTLPコレクタには依存しない（`tracetest.NewInMemoryExporter`でexport先を差し替える）。

## 実装順序（audit-loggingとの依存関係）

1. **audit-logging-design（別spec、既存・未実装）を先に実装する** — `internal/gateway/audit.go`の`logCall`と4ハンドラへの組み込み、`--log-format`等。既にレビュー・詳細設計済みのため先行させる。
2. **本トレーシング設計を実装する** — `internal/telemetry`新設、4ハンドラへの`startCallSpan`/`recordOutcome`組み込み、`internal/backend`の`tracingRoundTripper`、`internal/cli/server.go`の起動時配線、そして**`logCall`へ`trace_id`/`span_id`の2行を追記する差分**を含める。

両者は同じ4ハンドラの同じ箇所（backend呼び出しの直前・直後）を触るため、並行実装するとマージコンフリクトが起きやすい。この順序を守ることで、トレーシング側のPRは「`logCall`呼び出しの前後にspan開始/終了を挟み、`logCall`自体に2行足す」という小さな差分に収まる。

## 将来拡張（本ドキュメントのスコープ外）

- stdioトランスポートでの`_meta`ベースのトレースコンテキスト伝播
- config.yamlでの明示的な有効/無効切り替え（現状は`OTEL_TRACES_EXPORTER=none`に委ねる）
- `connectBackends`（起動時のbackend接続・List呼び出し）へのspan付与
- サンプリング戦略のカスタム設定（現状は`OTEL_TRACES_SAMPLER`等の標準環境変数のデフォルトに委ねる）
- outbound HTTP呼び出し自体の詳細な子span化（現状はハンドラのspanに一本化し、ネットワーク層の内訳は取らない）
