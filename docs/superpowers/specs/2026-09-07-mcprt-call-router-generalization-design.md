# mcprt: sampling/roots 実装に先立つ共通基盤化（ElicitationRouter → CallRouter）設計

## 背景・目的

現在未対応のMCP機能のうち`sampling`（`sampling/createMessage`）と`roots`（`roots/list`）は、いずれも「backendが、mcprtしか答えを持っていないdownstreamクライアントに何かを要求する」という、`elicitation/create`と全く同じ形の問題を抱えている:

- `sampling/createMessage`・`roots/list`とも、`elicitation/create`と同様に**特定の`tools/call`呼び出しへの相関情報を一切持たない**（`CreateMessageParams`にコールIDのようなフィールドは存在しない。`roots/list`はそもそもパラメータを取らない）。
- したがって、どちらも既存の`ElicitationRouter`が解いている「対象backendへの`tools/call`が同時にちょうど1件のときだけ相関を取り、それ以外は安全側に倒してエラーにする」というルールに、そのまま乗せられる。

現状の`ElicitationRouter`（`internal/gateway/elicitation.go`）は、実装を見るとelicitation固有の処理を一切含んでおらず、「backend名 → 進行中の`tools/call`のセッション集合」を追跡するだけの汎用コンポーネントになっている。elicitation・sampling・rootsそれぞれが個別に同じ追跡ロジックを再実装（`callHandler`から3回`Enter`/`leave`を呼ぶ、など）するのは無駄であるだけでなく、3つの相関状態が食い違うバグの温床になる。

本ドキュメントは、sampling・rootsそれぞれの個別仕様を書く**前**に、`ElicitationRouter`を汎用化し、3機能が同じ実装・同じ状態を共有できるようにする土台を設計する。sampling・rootsの個別のリレー仕様（`ChangeCallbacks`への`OnSampling`/`OnRootsList`追加、実際の`session.CreateMessage`/`ListRoots`呼び出しの配線）は本ドキュメントのスコープ外とし、別途書く。

## スコープ

含める:
- `ElicitationRouter`を`CallRouter`（仮称）にリネームし、`gateway.Relays`上の置き場を`Elicit`から`Calls`に変更する
- `callHandler`内の`Enter`/`leave`呼び出しを、elicitation専用ではなく「`tools/call`が存在する限り常に」呼ぶよう変更する（`relays.Elicit != nil`という条件分岐をなくす）
- 既存のelicitation中継の**振る舞いは一切変えない**、純粋なリファクタリングとして行う

含めない（将来拡張、それぞれ別ドキュメント）:
- sampling（`sampling/createMessage`）自体の中継仕様
- roots（`roots/list`）自体の中継仕様
- `Route`の「ちょうど1件」ルール自体の改善（`ElicitationRouter`の既存doc commentが述べている、呼び出しキャンセル時の相関ズレの是正など）

## 全体アーキテクチャ

現状（elicitation専用）:

```
callHandler ──Enter/leave──▶ ElicitationRouter ◀──Route── OnElicit (elicitation専用)
```

変更後（共有）:

```
                              ┌──Route── OnElicit         (elicitation)
callHandler ──Enter/leave──▶ CallRouter ◀──Route── OnSampling  (将来、sampling)
                              └──Route── OnRootsList     (将来、roots)
```

`CallRouter`自体の内部実装（`Enter`/`Route`/`backendCalls`の構造）は変更しない。呼び出し側の配線と名前だけを変える。

## コンポーネント構成

### `internal/gateway/elicitation.go` → `internal/gateway/call_router.go`（リネーム）

型・メソッドをリネームするのみ、ロジックは無変更:

```go
// CallRouter tracks, per backend, which downstream ServerSessions
// currently have a tools/call in flight against that backend -- so that a
// backend-initiated request with no built-in call correlation (MCP defines
// several: elicitation/create, sampling/createMessage, roots/list) can be
// routed to the right downstream session when exactly one call is in
// flight, and refused otherwise. Shared by every such feature rather than
// each tracking its own copy of the same in-flight-call state.
type CallRouter struct {
	mu    sync.Mutex
	calls map[string]*backendCalls
}

func NewCallRouter() *CallRouter
func (r *CallRouter) Enter(backendName string, session *mcp.ServerSession) (leave func())
func (r *CallRouter) Route(backendName string) (*mcp.ServerSession, error)
```

`backendCalls`型・`Enter`/`Route`の実装（`sync.Mutex`+`map[uint64]*mcp.ServerSession`による、キャンセル安全な参照カウント）はそのまま移動する。doc commentの「ちょうど1件」ルールとその既知の限界（キャンセル直後の相関ズレ）の説明も、elicitation固有ではなく機能横断の説明として書き直す。

### `internal/gateway/gateway.go`: `Relays`

```go
type Relays struct {
	Progress *ProgressRegistry
	Calls    *CallRouter // was: Elicit *ElicitationRouter
}
```

`Elicit`という名前をなくすことで、「elicitation専用の仕組みではない」ことを型定義自体が表すようにする。

### `internal/gateway/gateway.go`: `callHandler`

```go
func callHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalName string, relays Relays) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// ...
		if relays.Calls != nil {
			leave := relays.Calls.Enter(b.Name, req.Session)
			defer leave()
		}
		// ...（Progress周りは変更なし）
	}
}
```

`relays.Elicit != nil`だった条件が`relays.Calls != nil`になるだけで、呼び出し位置・タイミングは変わらない。

### `internal/cli/server.go`: 配線

`elicitRouter := gateway.NewElicitationRouter()` → `callRouter := gateway.NewCallRouter()`。`OnElicit`の中身（`callRouter.Route(bc.Name)`を呼んで`session.Elicit(...)`する部分）はロジック上は無変更、参照する変数名だけが変わる。

## データフロー

既存のelicitation中継のデータフロー（`docs/superpowers/specs/2026-08-25-mcprt-elicitation-relay-design.md`のデータフロー節）と完全に同一。変わるのは型名・フィールド名のみ。

## エラーハンドリング

既存のelicitation中継のエラーハンドリング表と完全に同一（変更なし）。

## ロギング

変更なし。

## テスト方針

このリファクタリングの正しさは「**既存のelicitation関連テストが、一切の変更なしにそのまま通ること**」で証明する（振る舞いを変えないリファクタリングであることの直接的な検証）:

- `internal/gateway`の`ElicitationRouter`単体テスト → `CallRouter`単体テストとして、テストコード内の型名参照だけを機械的に置換する。
- `internal/gateway`・`internal/cli`のelicitation統合/e2eテストは、内部で`ElicitationRouter`/`Elicit`という識別子を直接参照していない限り、無改修で通るはず（外部から見た`ChangeCallbacks.OnElicit`・`session.Elicit`の呼び出し経路自体は変わらないため）。
- `go vet`・`golangci-lint`でリネーム漏れ（未使用の旧識別子、importの整合性）がないことを確認。
- 新規のふるまいテストは追加しない（本ドキュメントのスコープでは新機能を導入しない）。

## 将来拡張（本ドキュメントのスコープ外、次に書く2本のドキュメント）

- **sampling中継**: `backend.ChangeCallbacks`への`OnSampling func(context.Context, *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error)`追加、`mcp.ClientOptions.CreateMessageHandler`への配線、`CallRouter.Route`で解決したセッションへの`session.CreateMessage(ctx, params)`呼び出し。elicitationのタイムアウト（`elicitTimeout`）と同様の、sampling専用タイムアウトの要否を検討する。
- **roots中継**: backendからの`roots/list`要求を、`CallRouter.Route`で解決したdownstreamセッションへ`session.ListRoots(ctx)`相当で問い合わせ、結果を返す。downstreamクライアントが`notifications/roots/list_changed`を送ってきた場合の扱い（mcprtが保持する「そのセッションの最新roots」をどう扱うか、複数backendへの伝播要否）は別途検討する。
