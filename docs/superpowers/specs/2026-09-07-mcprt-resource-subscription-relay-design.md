# mcprt: resources/subscribe 中継 設計

## 背景・目的

MCP仕様は、クライアントが特定のリソースURIの更新を購読する`resources/subscribe`/`resources/unsubscribe`と、更新時に届く`notifications/resources/updated`を定義している。mcprtは現状これらを一切配線しておらず、`resources/subscribe`を送るとSDKレベルで「サポートしない」エラーになる（READMEにも明記済み）。

`tools/call`のelicitation中継・progress中継が「1回の呼び出しに閉じた」相関だったのに対し、リソース購読は**セッションの生存期間全体にわたって持続する状態**であり、mcprtにとって新しい種類の中継になる。

## スコープ

含める:
- downstreamの`resources/subscribe`/`resources/unsubscribe`を受け付け、該当リソースを提供するbackendへの実際の購読を（参照カウント方式で）管理する
- backendからの`notifications/resources/updated`を、そのURIを購読している全downstreamセッションへリレーする
- backend再接続時に、その時点で購読されている全URIを自動的に再購読する
- downstreamセッション切断時に、そのセッションの購読を除去し、購読者がゼロになったURIはbackendへ`unsubscribe`する

含めない（将来拡張）:
- リソーステンプレート由来のURI（動的に生成されるURI）の購読 — 本設計は`resourceTable`に登録された具体リソースのURIのみを対象とする
- 購読状態の永続化（プロセス再起動・SIGHUP再読み込みで購読は失われる。既存のmcprtの全状態がインメモリであることと一貫する）

## 技術的な制約（設計の前提）

`go-sdk@v1.7.0`を調査した結果:
- `mcp.ServerOptions.SubscribeHandler func(context.Context, *SubscribeRequest) error` / `UnsubscribeHandler func(context.Context, *UnsubscribeRequest) error` — 両方セットしないと`NewServer`がpanicする（片方だけの設定は不可）。
- `mcp.ClientSession.Subscribe(ctx, params *SubscribeParams) error` / `Unsubscribe(ctx, params *UnsubscribeParams) error` — mcprtがbackendへ実際に購読/解除を送るクライアント側メソッド。
- `mcp.ClientOptions.ResourceUpdatedHandler func(context.Context, *ResourceUpdatedNotificationRequest)` — backendからの`notifications/resources/updated`受信時に呼ばれる（`backend.ChangeCallbacks`に`OnResourceUpdated`相当を追加する）。
- **downstreamセッションのクローズを検知する公開APIはgo-sdkにない**（`ServerSessionOptions.onClose`は非公開フィールド）。既存のmcprtが`superviseBackend`で`Session.Wait()`をbackend切断検知に使っているのと同じ手法を、ここでは**downstream側の`*mcp.ServerSession`**に対して使う：購読ハンドラで初めて見るセッションごとに`session.Wait()`を待つgoroutineを1つ立て、返ってきたらそのセッションの購読を全部片付ける。

## 全体アーキテクチャ

```
                    resources/subscribe / unsubscribe
  downstream(s) ──────────────────────────────────────┐
                                                        ▼
                                            ┌───────────────────────┐
                                            │  gateway.Server         │
                                            │  SubscribeHandler /      │
                                            │  UnsubscribeHandler      │
                                            └───────────┬─────────────┘
                                                        │ Register / Unregister
                                                        ▼
                                            ┌───────────────────────┐
                                            │ SubscriptionRegistry     │
                                            │ URI -> {backend,         │
                                            │  originalURI,            │
                                            │  []*ServerSession}       │
                                            └───────────┬─────────────┘
                              refcount 0→1: Subscribe   │  refcount 1→0: Unsubscribe
                                                        ▼
                                                  Backend Client
                                     (notifications/resources/updated
                                      → OnResourceUpdated → Relay)
```

`ProgressRegistry`・（将来`CallRouter`に一般化される）`ElicitationRouter`と並ぶ、`gateway.Relays`の第3の独立コンポーネントとして`SubscriptionRegistry`を追加する。

## コンポーネント構成

### `internal/gateway/subscription.go`（新規）

```go
// SubscriptionRegistry tracks, per resource URI, which downstream
// ServerSessions currently want notifications/resources/updated for it --
// and, per URI, whether mcprt itself has an active upstream subscription
// with the owning backend (reference-counted: mcprt subscribes upstream
// once, on the first downstream subscriber, and unsubscribes once the
// last one leaves).
type SubscriptionRegistry struct {
	mu   sync.Mutex
	subs map[string]*subscription // keyed by the exposed (downstream-facing) URI
}

type subscription struct {
	backendName string
	originalURI string // the URI as the backend itself knows it
	sessions    map[*mcp.ServerSession]bool
}

// NewSubscriptionRegistry returns an empty registry, ready to use.
func NewSubscriptionRegistry() *SubscriptionRegistry

// Subscribe registers session's interest in uri (backendName/originalURI
// describe the owning backend, resolved the same way resourceReadHandler
// resolves them). Reports needUpstream=true exactly when this is the URI's
// first subscriber -- the caller must then call session's backend's
// Subscribe itself; SubscriptionRegistry never touches a *backend.Backend
// directly, keeping it free of I/O and easy to test in isolation (same
// design as ElicitationRouter/ProgressRegistry).
func (r *SubscriptionRegistry) Subscribe(session *mcp.ServerSession, uri, backendName, originalURI string) (needUpstream bool)

// Unsubscribe removes session's interest in uri, reporting
// needUpstreamUnsubscribe=true exactly when session was the URI's last
// subscriber.
func (r *SubscriptionRegistry) Unsubscribe(session *mcp.ServerSession, uri string) (needUpstreamUnsubscribe bool)

// SessionClosed removes every subscription session held, across every URI,
// reporting the URIs that lost their last subscriber (each needing an
// upstream Unsubscribe) alongside their backend name/original URI. Called
// once, when session's Wait() returns (see gateway.go's wiring).
func (r *SubscriptionRegistry) SessionClosed(session *mcp.ServerSession) []subscriptionToClose

type subscriptionToClose struct {
	BackendName string
	OriginalURI string
}

// Relay looks up uri's current subscribers and calls NotifyResourceUpdated
// on each, bounded by the same progressRelayTimeout-style deadline
// progress relay already uses (a stalled downstream client must not block
// this backend's whole notification pipeline). Unlike ElicitationRouter's
// Route, this fans out to potentially many sessions, so partial failure
// (one downstream write times out) must not stop delivery to the others.
func (r *SubscriptionRegistry) Relay(ctx context.Context, logger *slog.Logger, backendName, originalURI string)

// BackendReconnected reports every URI currently subscribed against
// backendName, for the reconnect path (see superviseBackend's wiring) to
// re-issue Subscribe on the fresh *backend.Backend -- an existing
// subscription must survive a backend disconnect/reconnect cycle exactly
// like tools/resources/prompts list-changed handlers already do.
func (r *SubscriptionRegistry) BackendReconnected(backendName string) []subscriptionToClose // same shape: {BackendName, OriginalURI}
```

`needUpstream`/`needUpstreamUnsubscribe`をレジストリ自身が返し、実際の`b.Session.Subscribe`/`Unsubscribe`呼び出しはレジストリの**外**（`SubscribeHandler`/`UnsubscribeHandler`/`SessionClosed`の呼び出し元）で行う。これは`ElicitationRouter`/`ProgressRegistry`が徹底している「相関の追跡だけを行い、実際のI/Oは持たない」設計を踏襲する。

`gateway.Relays`に新しいフィールドとして追加する（以下、本ドキュメント中の`s.relays.Subscriptions`はこれを指す）:

```go
type Relays struct {
	Progress      *ProgressRegistry
	Elicit        *ElicitationRouter // または将来のCallRouter（別ドキュメント参照）
	Subscriptions *SubscriptionRegistry
}
```

### `internal/gateway/gateway.go`: `SubscribeHandler`/`UnsubscribeHandler`

```go
func (s *Server) subscribeHandler(ctx context.Context, req *mcp.SubscribeRequest) error {
	s.mu.Lock()
	resolved, ok := s.resourceTable.Items[req.Params.URI]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("subscribe: unknown resource %q", req.Params.URI)
	}
	b := s.backends[resolved.BackendName]
	s.mu.Unlock()

	s.startSessionCloseWatcherOnce(req.Session) // 後述

	needUpstream := s.relays.Subscriptions.Subscribe(req.Session, req.Params.URI, resolved.BackendName, resolved.OriginalName)
	if needUpstream {
		return b.Session.Subscribe(ctx, &mcp.SubscribeParams{URI: resolved.OriginalName})
	}
	return nil
}
```

`unsubscribeHandler`は対称。`needUpstreamUnsubscribe`のとき`b.Session.Unsubscribe`を呼ぶ。

### downstreamセッション切断の検知

`SubscribeHandler`が新規セッション（それまで一度も購読していないセッション）を見た瞬間に、そのセッション専用のwatcherを一度だけ起動する:

```go
func (s *Server) startSessionCloseWatcherOnce(session *mcp.ServerSession) {
	s.mu.Lock()
	if s.watchedSessions[session] {
		s.mu.Unlock()
		return
	}
	s.watchedSessions[session] = true
	s.mu.Unlock()

	go func() {
		_ = session.Wait()
		toClose := s.relays.Subscriptions.SessionClosed(session)
		for _, c := range toClose {
			if b := s.Backend(c.BackendName); b != nil {
				_ = b.Session.Unsubscribe(context.Background(), &mcp.UnsubscribeParams{URI: c.OriginalURI})
			}
		}
		s.mu.Lock()
		delete(s.watchedSessions, session)
		s.mu.Unlock()
	}()
}
```

`watchedSessions map[*mcp.ServerSession]bool`は`Server`に追加する新しいフィールド（既存の`mu`で保護、購読機能が無効(`relays.Subscriptions == nil`)なら一切使わない）。

### backend再接続時の再購読

`internal/cli/server.go`の`superviseBackend`が再接続に成功した直後（`gw.ConnectBackend`呼び出しの近く）に:

```go
if gwH.relays.Subscriptions != nil {
	for _, c := range gwH.relays.Subscriptions.BackendReconnected(bc.Name) {
		if err := c.backend.Session.Subscribe(ctx, &mcp.SubscribeParams{URI: c.OriginalURI}); err != nil {
			logger.Warn("resubscribe after reconnect failed", "backend", bc.Name, "uri", c.OriginalURI, "error", err)
		}
	}
}
```

### `internal/backend`: `ChangeCallbacks`への`OnResourceUpdated`追加

```go
type ChangeCallbacks struct {
	// ...既存フィールド...
	OnResourceUpdated func(context.Context, *mcp.ResourceUpdatedNotificationRequest)
}
```

`Connect`内: `clientOpts.ResourceUpdatedHandler = cb.OnResourceUpdated`（nilなら未設定のまま、既存の`OnProgress`/`OnElicit`と同じパターン）。

## データフロー

1. downstreamが`resources/subscribe`（URI指定）を送る。
2. `subscribeHandler`が`resourceTable`でbackendを解決し、`SubscriptionRegistry.Subscribe`を呼ぶ。
3. そのURIの最初の購読者なら、`b.Session.Subscribe`をbackendへ実際に送る。2人目以降は何もしない（既にbackend側は購読済み）。
4. backendがリソース更新時に`notifications/resources/updated`を送ってくる。`OnResourceUpdated`（`ChangeCallbacks`経由）が発火し、`SubscriptionRegistry.Relay`を呼ぶ。
5. `Relay`はそのURIの購読者全員へ`NotifyResourceUpdated`を送る（1人の遅延が他を巻き込まないよう、各送信を独立してタイムアウト管理）。
6. downstreamが`resources/unsubscribe`を送るか、セッションが切断される（`session.Wait()`が返る）と、該当エントリを除去し、購読者がゼロになったURIだけbackendへ`Unsubscribe`を送る。
7. backendが切断・再接続すると、`superviseBackend`が再接続直後に、そのbackend宛の全購読URIを新しい`Session`へ再送信する。

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| 未登録URIへの`subscribe` | `subscribe: unknown resource "<uri>"`エラーを返す |
| backendが`resources/subscribe`自体をサポートしない | `b.Session.Subscribe`がSDKレベルのエラーを返す → downstreamへ伝播。`SubscriptionRegistry`側の状態は登録済みのままになる点に注意（次にRelayが呼ばれても実際には更新が来ないだけで実害はないが、次回同URIへの新規購読者がいてもneedUpstream判定は「既に購読者あり」のままなので再試行されない） — 将来、Subscribe失敗時はレジストリからも即座に外す方が誠実。本設計では初版のシンプルさを優先し、TODOとして残す |
| backend切断中に再購読が必要になった | `BackendReconnected`はbackendが実際に繋がった後に呼ばれる想定のため発生しない。再接続自体が失敗し続ける間は購読も届かないが、それは他のtools/call等と同じ既存の挙動 |
| downstreamセッションが購読中に切断 | `session.Wait()`が返り、`SessionClosed`で自動的にクリーンアップされる |

## ロギング

- 再購読失敗（`BackendReconnected`後の`Subscribe`エラー）: `logger.Warn`
- それ以外（通常の`Relay`、正常な`Subscribe`/`Unsubscribe`成功）はログしない（progress中継と同様、高頻度で副作用のない経路のため）

## テスト方針

- **`internal/gateway`（`SubscriptionRegistry`単体）**: 参照カウントの増減（1人目でneedUpstream=true、2人目以降false、最後の1人が抜けたときだけneedUpstreamUnsubscribe=true）。`SessionClosed`が該当セッションの全URIを正しく返すこと。`BackendReconnected`が該当backendの全URIを返すこと。`-race`での並行`Subscribe`/`Unsubscribe`/`SessionClosed`。
- **`internal/gateway`（統合テスト）**: fakeバックエンドで実際にリソース更新を発生させ、購読中のdownstreamクライアントに`notifications/resources/updated`が届くこと。2つのdownstreamセッションが同じURIを購読しているとき、片方が`unsubscribe`してももう片方には引き続き届くこと。
- **`internal/cli`（e2e）**: `mcprt server`起動 → downstreamが`subscribe` → fakeバックエンドが更新通知 → downstreamに届く、を1シナリオ。backend切断・再接続後も購読が生き続けることを確認するシナリオ（`TestSuperviseBackend_ReconnectsAfterDisconnect`と同種の切断シミュレーションを流用）。
- `go test ./...`で完結、外部サービス依存なし。

## 将来拡張（本ドキュメントのスコープ外）

- リソーステンプレート由来の動的URIの購読
- 購読状態の永続化・SIGHUP再読み込みをまたいだ引き継ぎ
- Subscribe失敗時にレジストリ状態を即座に巻き戻す（上記エラーハンドリング表のTODO）
