# mcprt: tools/call の elicitation中継 設計

## 背景・目的

MCP仕様は、サーバー（本ドキュメントの文脈ではbackend）がクライアント（downstream）に対して構造化された入力をその場で求める`elicitation/create`リクエストを定義している。mcprtは各backendに対して`internal/backend.Backend.Session`という1本の`*mcp.ClientSession`のみを保持し、複数のdownstreamクライアントからの`tools/call`を同一backend・同一セッション上で並行に処理しうる。backendが`elicitation/create`を送ってきても、mcprtの`mcp.Client`にその要求を受け取る`ElicitationHandler`が配線されていないため、現状は「クライアントはelicitationをサポートしない」というエラーを即座にbackendへ返してしまい、機能しない。

本ドキュメントでは、`tools/call`に限定して、backendからのelicitation要求を対応するdownstreamクライアントへ中継し、その応答をbackendへ返す仕組みを設計する。

## スコープ

含める:
- `tools/call`のみを対象に、backendの`elicitation/create`をdownstreamへ中継し、応答をbackendへ返す
- 同一backendへの`tools/call`が同時に複数進行中で、どの呼び出しに対するelicitationか判別できない場合は、安全側に倒してエラーを返す（誤ったdownstreamへ配送しない）
- mcprt独自の応答待ちタイムアウト（backendのキャンセルに依存しない）

含めない（将来拡張）:
- `resources/read`・`resources/templates/read`・`prompts/get`への同様の中継
- 相関の曖昧さを解消するための、より高度な仕組み（例: backend側プロトコルの拡張によるリクエストID相関）。MCP仕様自体がelicitationをコネクション単位のものと定義しており、mcprt側だけで解決できる話ではない
- タイムアウト値のYAML設定への露出（`backendConnectTimeout`等、既存の同種タイムアウトと同じくハードコードされたpackage変数とする。既存の慣習に合わせる）

## 技術的な制約（設計の前提）

`go-sdk@v1.7.0`を調査した結果:
- `mcp.ClientOptions.ElicitationHandler func(context.Context, *ElicitRequest) (*ElicitResult, error)`が、backendからの`elicitation/create`受信時にmcprtの`mcp.Client`側で呼ばれる。この`ctx`は接続（セッション）単位のものであり、特定の`tools/call`呼び出しに紐づく相関情報を一切含まない（`ElicitParams`にそのようなフィールドはない）。
- `mcp.ServerSession.Elicit(ctx, params *ElicitParams) (*ElicitResult, error)`が、mcprt（downstreamに対してはサーバー）から特定のdownstreamセッションへelicitationリクエストを送り、応答を同期的に受け取れる。

この2点から、mcprtは「backendからのelicitation要求を、どのdownstreamセッションへ転送すべきか」を**自前で追跡**する必要がある。追跡の粒度は、同一backendへの`tools/call`が同時に何件進行中かというカウントであり、ちょうど1件のときのみ一意に相関が取れる。

## 全体アーキテクチャ

```
                    ┌───────────────────────────┐
  client(s)     ──▶ │   gateway.Server           │
 (tools/call)       │   - callHandler             │
                    └─────────────┬───────────────┘
                                  │ ElicitationRouter.Enter / leave()
                                  │ (backend名 -> 進行中セッションの集合)
                                  ▼
                    ┌───────────────────────────┐
                    │  ElicitationRouter           │
                    │  backend名 -> []*ServerSession│
                    └─────────────┬───────────────┘
                                  ▲
                                  │ ElicitationRouter.Route
                                  │ (backendのElicitationHandlerから)
              ┌───────────────────┼───────────────────┐
              ▼                   ▼                   ▼
        Backend Client      Backend Client       Backend Client
        (mcp.ClientOptions.  (同左)                (同左)
         ElicitationHandler
         経由でChangeCallbacks.OnElicitを起動)
```

`ElicitationRouter`は`internal/gateway`の新規ファイル`elicitation.go`に置く、`ProgressRegistry`（別途設計済み・未実装）と同型の独立コンポーネントとして設計する。entries/tables/overridesに依存しないため、`runServer`が`connectBackends`を呼ぶ**前**に構築し、`connectBackends`（backend側の配線）と`gateway.New`（`callHandler`側の配線）の両方に同じインスタンスを渡す。

## コンポーネント構成

### `internal/gateway/elicitation.go`（新規）

```go
// ElicitationRouter tracks, per backend, which downstream ServerSessions
// currently have a tools/call in flight against that backend -- so that
// when the backend sends an elicitation/create request (which carries no
// correlation to any specific call), mcprt can route it to the right
// downstream session when exactly one call is in flight, and refuse to
// guess otherwise.
type ElicitationRouter struct {
	mu    sync.Mutex
	calls map[string]*backendCalls // keyed by backend name, created lazily
}

type backendCalls struct {
	mu       sync.Mutex
	sessions []*mcp.ServerSession // one entry per in-flight tools/call; may repeat the same session
}

// NewElicitationRouter returns an empty router, ready to use.
func NewElicitationRouter() *ElicitationRouter

// Enter records one in-flight tools/call for backendName, owned by
// session. The caller must call the returned leave func exactly once (via
// defer) when the call returns, success or failure.
func (r *ElicitationRouter) Enter(backendName string, session *mcp.ServerSession) (leave func())

// Route reports the single downstream session to forward an elicitation
// request to, for the given backend. It returns an error -- and forwards
// nothing -- unless exactly one tools/call is currently in flight for
// backendName: zero in-flight calls means there's nothing to correlate to
// (the elicitation arrived too late, or the backend is misbehaving); more
// than one means mcprt cannot tell which call it belongs to (MCP's
// elicitation/create carries no per-call correlation token), and guessing
// wrong would route a backend's question to an unrelated client.
func (r *ElicitationRouter) Route(backendName string) (*mcp.ServerSession, error)
```

### `internal/backend`: `ChangeCallbacks`への`OnElicit`追加

```go
type ChangeCallbacks struct {
	OnToolsChanged     func()
	OnPromptsChanged   func()
	OnResourcesChanged func()
	OnProgress         func(context.Context, *mcp.ProgressNotificationClientRequest) // 別ドキュメント（progress中継設計）
	// OnElicit, if non-nil, is wired as the backend-facing mcp.Client's
	// ElicitationHandler. Unlike the OnXChanged callbacks, this one both
	// takes a payload and returns a result -- its signature matches the
	// SDK handler's exactly.
	OnElicit func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)
}
```

`Connect`内の配線:

```go
if cb.OnElicit != nil {
	clientOpts.ElicitationHandler = cb.OnElicit
}
```

### `internal/gateway/gateway.go`: `callHandler`の変更

`registerTool`・`callHandler`に`*ElicitationRouter`を引数として通す（`gateway.New`から渡る）。

```go
func callHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalName string, elicit *ElicitationRouter) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		ctx, span := startCallSpan(ctx, req.Extra, "tools/call",
			attribute.String("mcp.backend", b.Name),
			attribute.String("mcp.tool.name", originalName))
		defer span.End()

		if elicit != nil {
			leave := elicit.Enter(b.Name, req.Session)
			defer leave()
		}

		result, err := b.Session.CallTool(ctx, &mcp.CallToolParams{
			Name:      originalName,
			Arguments: req.Params.Arguments,
		})
		recordOutcome(span, err)
		logCall(ctx, logger, "tool", "tool", originalName, b.Name, req.Session, req.Params.Arguments, maskKeys, start, err, nil)
		return result, err
	}
}
```

（`logCall`の末尾引数は、別途設計済みのprogress中継が追加する`*progressEntry`パラメータ。ここでは常に`nil`。）

### `internal/cli/server.go`: 配線

`runServer`内、`connectBackends`を呼ぶ前に構築する:

```go
elicitRouter := gateway.NewElicitationRouter()
```

`connectBackends`のシグネチャに`elicitRouter *gateway.ElicitationRouter`を追加し（`call`/`list`は`nil`）、各backend用`ChangeCallbacks`構築時に:

```go
cb.OnElicit = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	session, err := elicitRouter.Route(bc.Name)
	if err != nil {
		logger.Warn("elicitation: cannot route to a downstream session, refusing", "backend", bc.Name, "error", err)
		return nil, err
	}
	ectx, cancel := context.WithTimeout(ctx, elicitTimeout)
	defer cancel()
	res, err := session.Elicit(ectx, req.Params)
	if err != nil {
		logger.Warn("elicitation: downstream did not respond", "backend", bc.Name, "error", err)
	}
	return res, err
}
```

`gateway.New(...)`の呼び出しに`elicitRouter`を渡す（`New`のシグネチャに`elicit *ElicitationRouter`パラメータを追加し、`registerTool`経由で`callHandler`まで渡す）。

`elicitTimeout`は`internal/cli/server.go`（もしくは`internal/gateway`）に、既存の`backendConnectTimeout`/`shutdownTimeout`と同じ形のpackage変数として定義する: `var elicitTimeout = 5 * time.Minute`（テストで上書き可能）。人間の応答を待つため、`backendConnectTimeout`（30秒）より大幅に長い値をデフォルトとする。

## データフロー

1. downstreamが`tools/call`を送信する。
2. `callHandler`が`elicit.Enter(b.Name, req.Session)`を呼び、`defer leave()`を登録する。
3. `b.Session.CallTool(ctx, params)`を実行する（既存通り）。
4. backendが処理中に`elicitation/create`を送ってくる。backend向け`mcp.Client`の`ElicitationHandler`（`ChangeCallbacks.OnElicit`経由）が発火する。
5. `elicitRouter.Route(b.Name)`を呼ぶ:
   - 対象backendへの進行中`tools/call`が**ちょうど1件**なら、そのセッションを返す。`elicitTimeout`でboundした`ctx`を作り、`session.Elicit(ectx, req.Params)`を呼んでdownstreamへ中継し、結果（`*mcp.ElicitResult`または`error`）をそのままbackendへの応答として返す。
   - **0件または2件以上**なら、エラーを返す（`logger.Warn`で件数とともに記録）。downstreamには何も届かない。
6. backendはelicitationの応答（またはエラー）を受け取り、`tools/call`の処理を続行する。
7. `CallTool`が完了したら、`defer leave()`で`ElicitationRouter`から該当セッションの1エントリを除去する。

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| 対象backendへの`tools/call`が同時に0件（タイミングのずれ、backendの不具合など） | `Route`がエラーを返す。`logger.Warn`。backendへエラー応答 |
| 対象backendへの`tools/call`が同時に2件以上 | `Route`がエラーを返す（件数を記録）。`logger.Warn`。backendへエラー応答。downstreamは一切関与しない |
| downstreamクライアントがelicitation capabilityを持たない | `session.Elicit`がSDKレベルでエラーを返す → そのままbackendへ伝播。`logger.Warn` |
| `elicitTimeout`超過（人間が応答しない） | `ectx`がタイムアウトし`session.Elicit`がエラーを返す → backendへ伝播。`logger.Warn` |
| downstreamセッションが応答前に切断 | `session.Elicit`がエラーを返す → backendへ伝播。`logger.Warn` |
| `elicit`（`*ElicitationRouter`）が`nil`（`mcprt call`/`mcprt list`経由） | `callHandler`は`Enter`/`leave`をスキップする（既存動作のまま。`OnElicit`自体も配線されないため、backendからのelicitationはSDKレベルの「サポートしない」エラーになる） |

## ロギング

`Route`が失敗した場合と、`session.Elicit`自体がエラーを返した場合、それぞれ`logger.Warn`で記録する（backend名・理由）。elicitationの成否そのものをaudit logの`tools/call`ログ行に含めるかどうかは、progress中継設計の`progress_count`のような要約フィールドとは別に扱い、本ドキュメントのスコープでは追加しない（elicitationは1回の`tools/call`中に高々数回であり、Warnログで十分把握できるため）。

## テスト方針

- **`internal/gateway`（`ElicitationRouter`単体）**: `Enter`→`Route`で0件・1件・2件以上それぞれの挙動を確認。`leave`後に件数が正しく減ること、同じセッションで2回`Enter`しても個別に`leave`できること、`-race`での並行`Enter`/`Route`/`leave`。
- **`internal/gateway`（`callHandler`経由の統合テスト）**: fakeバックエンドのtoolハンドラ内から`elicitation/create`を送出させ、fake downstreamクライアントの`ElicitationHandler`が応答し、それがbackendまで正しく返ることを確認する。同一backendへ2つの`tools/call`を同時に実行中に片方がelicitationを送るケースでは、エラーになりdownstreamへは何も届かないことを確認する。
- **`internal/cli`（e2e）**: `mcprt server`を起動し、fakeバックエンドが`tools/call`中に1回elicitationを送り、テスト用downstreamクライアントが応答し、それがbackendの`tools/call`結果に反映されるところまでを1シナリオ確認する。
- `go test ./...`で完結、外部サービス依存なし。

## 将来拡張（本ドキュメントのスコープ外）

- `resources/read`・`resources/templates/read`・`prompts/get`への同様の中継
- `elicitTimeout`のYAML設定への露出
