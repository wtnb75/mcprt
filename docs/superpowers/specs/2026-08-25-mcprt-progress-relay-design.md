# mcprt: tools/call の progress通知 中継 設計

## 背景・目的

MCP仕様は、リクエストの`_meta.progressToken`を通じてサーバー側が`notifications/progress`を送り返す「進捗通知」の仕組みを定義している。現状のmcprtは、`internal/gateway/gateway.go`の`callHandler`がbackendへ転送する`CallToolParams`を都度ゼロから組み立てており（`&mcp.Tool{Name: originalName, Arguments: req.Params.Arguments}`）、downstreamが送ってきた`progressToken`を一切引き継いでいない。そのため、backend側が進捗通知を送ってきても、mcprtを経由するdownstreamクライアントには一切届かない。

本ドキュメントでは、`tools/call`に限定して、downstreamの`progressToken`をbackendへの呼び出しに伝播し、backendから返る`notifications/progress`を元のtokenで downstreamへ中継する仕組みを設計する。

## スコープ

含める:
- `tools/call`のみを対象に、downstreamの`progressToken`をbackendへの呼び出しに伝播する
- backendが送る`notifications/progress`を、対応するdownstreamの元の`progressToken`に付け替えて中継する
- 同一backendへの複数同時呼び出しでも、進捗通知の相関を取り違えないようにする（mcprtが呼び出しごとに新しい内部tokenを発行）
- 呼び出し完了時、進捗通知の件数と最新messageを既存のaudit log（`logCall`）へ要約として付加する

含めない（将来拡張）:
- `resources/read`・`resources/templates/read`・`prompts/get`への同様の中継（まずは`tools/call`のみで実績を作る）
- downstream側からbackendへの進捗通知（MCPの仕様上、進捗はリクエストを受けた側からリクエスト元へ一方向に流れるものであり、この方向の通知は存在しない）
- 進捗通知1件ごとの個別audit log記録（要約のみ。1回の`tools/call`実行中に高頻度で発生しうるイベントであり、個別記録するとaudit logが肥大化するため）
- 停止した呼び出し（backendがハングしたまま進捗も返さないケース）の検出・タイムアウト（`tools/call`自体の既存のタイムアウト・キャンセル機構に委ねる。相関エントリは呼び出し完了時に必ず`defer`で削除されるため、呼び出し自体が終われば漏れなく片付く）

## 全体アーキテクチャ

```
                    ┌───────────────────────────┐
  client        ──▶ │   gateway.Server           │
 (progressToken付き) │   - callHandler (tools/call)│
                    └─────────────┬───────────────┘
                                  │ ProgressRegistry.Register
                                  │ (内部token発行・相関登録)
                                  ▼
                    ┌───────────────────────────┐
                    │  ProgressRegistry           │
                    │  内部token -> {ServerSession,│
                    │  元のtoken, 要約(件数/message)}│
                    └─────────────┬───────────────┘
                                  ▲
                                  │ ProgressRegistry.Relay
                                  │ (backendのProgressNotificationHandlerから)
              ┌───────────────────┼───────────────────┐
              ▼                   ▼                   ▼
        Backend Client      Backend Client       Backend Client
        (mcp.ClientOptions.  (同左)                (同左)
         ProgressNotificationHandler
         経由でChangeCallbacks.OnProgressを起動)
```

`ProgressRegistry`は`internal/gateway`の新規ファイル`progress.go`に置く、依存の少ない独立コンポーネントとして設計する。`gateway.Server`（reconcile状態を持つ型）とは別の関心事であり、entries/tables/overridesのようなbackend接続完了後に確定する状態を必要としない — 空のマップとして即座に構築できるため、`gwHolder`のような遅延解決（`atomic.Pointer`）は不要。`internal/cli/server.go`の`runServer`が`connectBackends`を呼ぶ**前**に1つ構築し、`connectBackends`（backend側の配線）と`gateway.New`（downstream側の`callHandler`）の両方に同じインスタンスをそのまま渡す。

## コンポーネント構成

### `internal/gateway/progress.go`（新規）

```go
// ProgressRegistry correlates a backend-facing progress token (which mcprt
// generates fresh for every forwarded tools/call that carries one, to
// guarantee uniqueness across concurrent calls to the same backend) with
// the downstream ServerSession and progress token that originated the
// call, so a notifications/progress a backend sends mid-call can be
// relayed back to the right downstream request under its own token.
type ProgressRegistry struct {
	mu      sync.Mutex
	next    atomic.Uint64
	entries map[uint64]*progressEntry
}

type progressEntry struct {
	session       *mcp.ServerSession
	originalToken any

	mu          sync.Mutex // protects count/lastMessage: relayed from the backend client's read loop, read by callHandler after CallTool returns
	count       int
	lastMessage string
}

// NewProgressRegistry returns an empty registry, ready to use.
func NewProgressRegistry() *ProgressRegistry

// Register allocates a fresh internal token for one forwarded tools/call,
// remembers session/originalToken so a later Relay can find its way back,
// and returns the internal token to set on the outgoing CallToolParams,
// plus a cleanup func the caller must defer to remove the entry once the
// call returns (success or error). Returns a *progressEntry the caller can
// read (via Summary) after cleanup to build the audit log line.
func (r *ProgressRegistry) Register(session *mcp.ServerSession, originalToken any) (internalToken uint64, entry *progressEntry, cleanup func())

// Relay looks up params.ProgressToken (expected to be one Register handed
// out, boxed as an int64/float64 per JSON-RPC's number decoding -- see
// Data flow) and, if still registered, forwards params to the matching
// downstream ServerSession under its original token, and records the event
// in the entry's summary. A token no longer in the registry (the call
// already completed) is silently dropped -- an expected race with a
// backend's last few in-flight notifications, not an error.
func (r *ProgressRegistry) Relay(ctx context.Context, logger *slog.Logger, params *mcp.ProgressNotificationParams)

// Summary reports how many progress events were relayed for entry, and the
// most recent Message (empty if none carried one or none arrived).
func (e *progressEntry) Summary() (count int, lastMessage string)
```

### `internal/backend`: `ChangeCallbacks`への`OnProgress`追加

`ChangeCallbacks`（list_changed機能で追加済み）に1フィールド追加する:

```go
type ChangeCallbacks struct {
	OnToolsChanged     func()
	OnPromptsChanged   func()
	OnResourcesChanged func()
	// OnProgress, if non-nil, is wired as the backend-facing mcp.Client's
	// ProgressNotificationHandler -- unlike the three notification
	// callbacks above, progress notifications carry a payload, so this
	// field's signature matches the SDK handler's exactly.
	OnProgress func(context.Context, *mcp.ProgressNotificationClientRequest)
}
```

`Connect`内の配線もパターンを合わせて追加する:

```go
if cb.OnProgress != nil {
	clientOpts.ProgressNotificationHandler = cb.OnProgress
}
```

### `internal/gateway/gateway.go`: `callHandler`・`logCall`の変更

`registerTool`・`callHandler`に`*ProgressRegistry`を引数として通す（`gateway.New`から`registerTool`経由で渡る）。`callHandler`:

```go
func callHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalName string, progress *ProgressRegistry) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		ctx, span := startCallSpan(ctx, req.Extra, "tools/call",
			attribute.String("mcp.backend", b.Name),
			attribute.String("mcp.tool.name", originalName))
		defer span.End()

		params := &mcp.CallToolParams{Name: originalName, Arguments: req.Params.Arguments}

		var entry *progressEntry
		if progress != nil {
			if token := req.Params.GetProgressToken(); token != nil {
				var internalToken uint64
				var cleanup func()
				internalToken, entry, cleanup = progress.Register(req.Session, token)
				defer cleanup()
				params.SetProgressToken(internalToken)
			}
		}

		result, err := b.Session.CallTool(ctx, params)
		recordOutcome(span, err)
		logCall(ctx, logger, "tool", "tool", originalName, b.Name, req.Session, req.Params.Arguments, maskKeys, start, err, entry)
		return result, err
	}
}
```

`logCall`（`internal/gateway/audit.go`）は末尾に`progress *progressEntry`引数を追加する。`resourceReadHandler`・`resourceTemplateReadHandler`・`promptGetHandler`の3呼び出しは`nil`を渡す（対象外のため変更なし）。`progress != nil`かつ`count > 0`のときのみ、ログ属性に`"progress_count"`・`"progress_last_message"`を追加する。

### `internal/cli/server.go`: 配線

`runServer`内、`connectBackends`を呼ぶ前に構築する:

```go
progressReg := gateway.NewProgressRegistry()
```

`connectBackends`のシグネチャに`progressReg *gateway.ProgressRegistry`を追加し（`gwH`と同様、`call`/`list`は`nil`を渡す）、各backend用`ChangeCallbacks`構築時に:

```go
cb.OnProgress = func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
	progressReg.Relay(ctx, logger, req.Params)
}
```

（`gwH != nil`のガードと同じ条件分岐に相乗りする — list_changedの3コールバックと一緒に、`progressReg != nil`のときだけ設定する。）

`gateway.New(...)`の呼び出しに`progressReg`を渡す（`New`のシグネチャに`progress *ProgressRegistry`パラメータを追加し、`registerTool`へそのまま渡す）。

## データフロー

1. downstreamクライアントが`tools/call`を`_meta.progressToken`付きで送信する。
2. `callHandler`が`req.Params.GetProgressToken()`でtokenを取得する。
   - tokenが`nil`（downstreamが進捗通知を要求していない）なら、以降の手順は一切実行せず、既存通りbackendへ転送する。
3. tokenがあれば`progress.Register(req.Session, token)`を呼び、新しい内部token（`uint64`）・`*progressEntry`・`cleanup func()`を受け取る。`defer cleanup()`を登録し、backend向け`CallToolParams`に`SetProgressToken(internalToken)`で内部tokenをセットする。
4. `b.Session.CallTool(ctx, params)`を実行する（既存通り）。backendは処理中、任意回数`notifications/progress`（内部token付き）を送ってよい。
5. backend向け`mcp.Client`の`ProgressNotificationHandler`（`ChangeCallbacks.OnProgress`経由で配線）が発火し、`progressReg.Relay(ctx, logger, params)`を呼ぶ。
   - `params.ProgressToken`で内部エントリを検索する。JSON-RPCの数値はfloat64またはint64としてデコードされる可能性があるため、`Relay`側で`uint64`への型変換を行う（`Register`が払い出した値と一致するように、比較前に正規化する）。
   - 見つかれば、エントリの要約（件数+1、`Message`があれば最新値で上書き）を更新し、`entry.session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: entry.originalToken, Progress: params.Progress, Total: params.Total, Message: params.Message})`でdownstreamへ中継する。
   - 見つからなければ（呼び出しが既に完了しエントリが削除された後に届いた遅延通知）、何もせず破棄する。
6. `CallTool`が完了したら、`defer cleanup()`でレジストリからエントリを削除する。`logCall`に`entry`（`nil`可）を渡し、`entry.Summary()`の件数が1件以上あればログに要約を追加する。

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| downstreamが`progressToken`を送ってこない | 何もしない（既存動作のまま、オーバーヘッドなし） |
| backendが進捗通知を送らないtool/backend | 何も中継されない。ログにも進捗要約は付かない |
| `ServerSession.NotifyProgress`がエラーを返す（downstreamが既に切断済みなど） | `logger.Warn`で記録し、`tools/call`自体は失敗させない（進捗中継の失敗で本処理を止めない） |
| `tools/call`完了後に遅れて届いた進捗通知 | レジストリに見つからないため無視（ログも出さない — 正常に起こりうるレース） |
| 同一backendへの複数の同時呼び出し | 呼び出しごとに新しい内部tokenを発行するため、相関を取り違えない |
| `progress`（`*ProgressRegistry`）が`nil`（`mcprt call`/`mcprt list`経由、あるいは万一の未配線） | `callHandler`は進捗関連の処理を一切スキップし、既存動作のまま動く |

## ロギング

`logCall`が出す`"tool call"`/`"tool call failed"`ログに、進捗通知が1件以上あった場合のみ`"progress_count"`（件数）・`"progress_last_message"`（最新のmessage、空なら省略）を追加する。それ以外のログ形式・レベルは変更しない。

## テスト方針

- **`internal/gateway`（`ProgressRegistry`単体）**: `Register`→`Relay`→`Summary`で件数・最新messageが正しく積み上がること、`cleanup`後に届いた`Relay`が無視されること、複数goroutineからの同時`Register`/`Relay`/`cleanup`が`-race`でクリーンであること。
- **`internal/gateway`（`callHandler`経由の統合テスト）**: fakeバックエンドのtoolハンドラ内から`req.Extra.Session`経由（またはSDKが提供する手段）で複数回`NotifyProgress`を送出させ、`downstream`側のテスト用クライアントが元の`progressToken`で、正しい`Progress`/`Total`/`Message`を受け取れることを確認する。progressTokenを送らない呼び出しでは何も届かないことも確認する。
- **`internal/cli`（e2e）**: `mcprt server`を起動し、fakeバックエンドが`tools/call`処理中に複数回進捗を送るシナリオで、downstreamに正しいtokenで届くこと、および最終audit logの1行に`progress_count`が含まれることを確認する。
- `go test ./...`で完結、外部サービス依存なし。

## 将来拡張（本ドキュメントのスコープ外）

- `resources/read`・`resources/templates/read`・`prompts/get`への同様の中継
- 進捗通知の頻度制限（backendが極端に高頻度で送ってくる場合のレート制限・間引き）
