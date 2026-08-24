# mcprt: backend 切断検知・自動再接続 設計

## 背景・目的

list_changed機能（`2026-08-21-mcprt-list-changed-design.md`）は、backendの通知を受けて再Listし集約テーブルを再計算する仕組みを実装したが、「backendへの自動再接続・切断検知」は明示的にスコープ外としていた。現状の`internal/cli/server.go`の`connectBackends`は、backendへの接続（または`ListTools`）に一度失敗すると、そのbackendを永久に除外する。稼働中に接続が切れた場合の検知・復旧の仕組みも存在しない。

本ドキュメントでは、backendへの接続を継続的に監視し、切断を検知したら該当backendの項目をルーティングテーブルから除去し、バックグラウンドで再接続を試み続け、成功したら自動的に復帰させる仕組みを設計する。**起動時に接続失敗したbackendも同じリトライループの対象とする**（コードパスの一本化）。

## スコープ

含める:
- backendごとの永続的な監視goroutineによる切断検知（`mcp.ClientSession.Wait()`を利用）
- 切断時、該当backendの全項目（tools/resources/resource templates/prompts）をルーティングテーブルから除去する
- 指数backoff（上限あり）による、期限なしの自動再接続リトライ
- 起動時に接続（または`ListTools`）に失敗したbackendも、同じ監視・リトライ対象に含める
- 再接続成功時、該当backendを1回のreconcileで復帰させる

含めない（将来拡張）:
- configのホットリロード（別ドキュメントとして今後設計する）
- リトライ間隔・上限のYAML設定への露出（既存の`backendConnectTimeout`等と同じくハードコードされたpackage変数とする）
- 複数backendの同時切断・復旧のjitter/協調制御（各backendは独立したgoroutineで完全に非同期に動くため、今回は考慮しない）
- `ListResources`/`ListResourceTemplates`/`ListPrompts`の失敗をリトライ対象にすること（現状通りソフト失敗＝空リスト扱いのまま。今回リトライ対象にするのは「接続失敗」「`ListTools`失敗」という、現状すでに"backendを丸ごと除外"扱いになっている2ケースのみ）

## 技術的な前提

`go-sdk@v1.7.0`を調査した結果、`mcp.ClientSession.Wait() error`が、接続がどのような理由（正常終了・エラー）であれ閉じられるまでブロックし、その理由を返す。これを切断検知のトリガーとして使う。

## 全体アーキテクチャ

```
   internal/cli/server.go
   ┌─────────────────────────────────────────────┐
   │ runServer                                     │
   │  1. progressReg/elicitRouter同様、gwHolderを   │
   │     connectBackendsより前に構築                │
   │  2. superviseBackend を全backend分spawn        │
   │  3. backendConnectTimeoutだけ「初回接続完了」を │
   │     待ち、揃った分でgateway.New()を呼ぶ          │
   │  4. gwHolder.ptr.Store(srv)                    │
   └─────────────────────────────────────────────┘
                       │ spawn (backendごとに1つ、プロセス生存中ずっと動く)
                       ▼
   ┌─────────────────────────────────────────────┐
   │ superviseBackend(ctx, logger, bc, gwH)         │
   │                                                │
   │  for {                                         │
   │    b, tools, ... := connectAndList(bc)  ← backoff│
   │    gw := gwH.ptr.Load()                        │
   │    if gw != nil { gw.ConnectBackend(...) }      │
   │    else { 初回成功を呼び出し元へ report }         │
   │                                                │
   │    err := b.Session.Wait()  ← 切断まで待機       │
   │                                                │
   │    if gw := gwH.ptr.Load(); gw != nil {         │
   │        gw.UpdateTools(bc.Name, nil)             │
   │        gw.UpdateResources(bc.Name, nil, nil)    │
   │        gw.UpdatePrompts(bc.Name, nil)           │
   │    }                                            │
   │    // ループ先頭に戻り再接続                      │
   │  }                                              │
   └─────────────────────────────────────────────┘
```

## コンポーネント構成

### `internal/gateway/reconcile.go`: `ConnectBackend`（新規メソッド）と`upsertEntry`（新規ヘルパー）

list_changed機能が作った`replaceEntry`（既知のbackend名のitemsを差し替える、未知なら何もしない）は変更しない。代わりに、「未知のbackend名なら新規追加する」`upsertEntry`を新設し、既存の`UpdateTools`/`UpdateResources`/`UpdatePrompts`は一切変更せず再利用する:

```go
// upsertEntry is replaceEntry's counterpart for a backend connecting: it
// replaces backendName's entry if one already exists (a reconnect), or
// appends a new one with the given prefix if this is the first time
// backendName has ever appeared in entries (a backend that failed to
// connect at mcprt startup, connecting for the first time). Unlike
// replaceEntry, this needs prefix because a first-time entry has no prior
// Prefix to inherit.
func upsertEntry[T any](entries []router.Entry[T], backendName, prefix string, items []T) []router.Entry[T] {
	out := make([]router.Entry[T], len(entries))
	copy(out, entries)
	for i, e := range out {
		if e.BackendName == backendName {
			out[i].Items = items
			return out
		}
	}
	return append(out, router.Entry[T]{BackendName: backendName, Prefix: prefix, Items: items})
}

// ConnectBackend registers (or re-registers) backendName's live connection
// and reconciles its full item set -- used both when a backend that failed
// to connect at mcprt startup finally connects for the first time, and
// when a previously-connected backend reconnects after a disconnect.
// prefix is the backend's configured Prefix (from config.BackendConfig;
// resources and resource templates never carry one, matching
// connectBackends' existing convention).
//
// This first ensures an entry (seeded with nil Items) exists for
// backendName under s.mu, in one atomic step with setting s.backends[name]
// -- then delegates the actual item reconciliation to the existing
// UpdateTools/UpdateResources/UpdatePrompts, each of which takes its own
// lock. A backend name is only ever driven by one supervisor goroutine at
// a time (see internal/cli/server.go), so this two-phase locking is safe:
// the only other concurrent writers are other backends' own reconciles,
// which touch different slice elements.
func (s *Server) ConnectBackend(name string, b *backend.Backend, prefix string, tools []*mcp.Tool, resources []*mcp.Resource, templates []*mcp.ResourceTemplate, prompts []*mcp.Prompt) {
	s.mu.Lock()
	s.backends[name] = b
	s.toolEntries = upsertEntry(s.toolEntries, name, prefix, nil)
	s.resourceEntries = upsertEntry(s.resourceEntries, name, "", nil)
	s.resourceTemplateEntries = upsertEntry(s.resourceTemplateEntries, name, "", nil)
	s.promptEntries = upsertEntry(s.promptEntries, name, prefix, nil)
	s.mu.Unlock()

	s.UpdateTools(name, tools)
	s.UpdateResources(name, resources, templates)
	s.UpdatePrompts(name, prompts)
}
```

### `internal/cli/server.go`: `superviseBackend`（新規）が`connectBackends`の接続ロジックを置き換える

現在の`connectBackends`は「全backend分goroutineをspawnし`wg.Wait()`で全員の結果を待つ」構造。これを「全backend分の永続監視goroutineをspawnし、`backendConnectTimeout`だけ最初の接続完了を待つ（間に合わなかった分は後で`gwHolder`経由で追いつく）」構造に変える。

```go
// backendBackoffMin/Max bound superviseBackend's exponential retry delay.
// A var so tests can shrink both.
var (
	backendBackoffMin = 1 * time.Second
	backendBackoffMax = 60 * time.Second
)

// superviseBackend owns backendName's whole connection lifecycle for the
// life of the process: connect (retrying with exponential backoff on
// failure, forever -- there is no give-up), report success, wait for
// disconnect, clear the backend's items, and reconnect. ctx is the
// long-lived server context (NOT a per-attempt timeout-derived one --
// see the list_changed plan's Task 5 for why this distinction matters:
// a callback/loop that captures a timeout-bound context would silently
// stop working once that timeout expires, long before a real disconnect
// could occur).
//
// connectResult is connectAndList's successful result: the live backend plus
// its four freshly-listed item sets, as raw slices (not yet wrapped in
// router.Entry -- that wrapping only makes sense once the caller knows
// whether it's building the very first Entries for gateway.New, or
// calling gateway.Server.ConnectBackend, which does its own wrapping via
// upsertEntry).
type connectResult struct {
	backend            *backend.Backend
	tools              []*mcp.Tool
	resources          []*mcp.Resource
	resourceTemplates  []*mcp.ResourceTemplate
	prompts            []*mcp.Prompt
}

// onFirstConnect, if non-nil, is called exactly once, the first time this
// backend ever connects -- used by connectBackends' startup path to learn
// about a backend that connected within the initial window. It is nil
// once that window has passed (superviseBackend running for a backend
// that was still retrying, or reconnecting after a later disconnect, or
// a subsequent instance would have gwH already populated).
func superviseBackend(ctx context.Context, logger *slog.Logger, bc config.BackendConfig, gwH *gwHolder, onFirstConnect func(*connectResult)) {
	backoff := backendBackoffMin
	first := true
	for {
		c, err := connectAndList(ctx, logger, bc)
		if err != nil {
			logger.Error("backend connect failed, retrying", "backend", bc.Name, "error", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, backendBackoffMax)
			continue
		}
		backoff = backendBackoffMin

		if first && onFirstConnect != nil {
			first = false
			onFirstConnect(c)
		} else if gw := gwH.ptr.Load(); gw != nil {
			gw.ConnectBackend(bc.Name, c.backend, bc.Prefix, c.tools, c.resources, c.resourceTemplates, c.prompts)
		}
		// gw == nil here means the initial window hasn't closed yet AND
		// this wasn't the first connect (e.g. a fast reconnect racing
		// gateway.New()) -- exceedingly unlikely, and tolerated the same
		// way list_changed's own startup race is: the pending initial
		// List that runServer is about to do anyway will pick it up.

		if err := c.backend.Session.Wait(); err != nil {
			logger.Warn("backend disconnected", "backend", bc.Name, "error", err)
		} else {
			logger.Info("backend disconnected", "backend", bc.Name)
		}
		if gw := gwH.ptr.Load(); gw != nil {
			gw.UpdateTools(bc.Name, nil)
			gw.UpdateResources(bc.Name, nil, nil)
			gw.UpdatePrompts(bc.Name, nil)
		}
		// loop: reconnect
	}
}
```

（`connectAndList`は、現行`connectBackends`の1backendぶんの「Connect→ListTools→ListResources→ListResourceTemplates→ListPrompts」ロジックをそのまま切り出したヘルパー。`ListTools`失敗は`err`として返しリトライ対象にする。`ListResources`等の失敗は現状通りソフト失敗として扱い、`err`にはしない。）

`connectBackends`（`runServer`から呼ばれるエントリポイント）は、全backend分`superviseBackend`をspawnし、`onFirstConnect`コールバック経由で届いた結果を`backendConnectTimeout`だけ集める。集まった時点（タイムアウトでも全員揃っても）で、既存の`connected`型（`backends`マップと4種の`router.Entry`スライスをまとめたもの。シグネチャ自体は変更しない）を返す。以降は各`superviseBackend`がバックグラウンドで動き続ける。

## データフロー

**起動時**:
1. `runServer`が`gwHolder`を構築する。
2. `connectBackends`が全backend分`superviseBackend`をspawnし、`backendConnectTimeout`だけ待つ。
3. 間に合ったbackendで`gateway.New(...)`を呼び、配信開始。`gwH.ptr.Store(srv)`。
4. 間に合わなかったbackendの`superviseBackend`は、backoffで再試行を続けている。接続に成功すると`gwH.ptr.Load()`が非nilになっているので、`gw.ConnectBackend(...)`で1回のreconcileにより自動的に参加する。

**稼働中の切断→再接続**:
1. `superviseBackend`が`b.Session.Wait()`から復帰（切断検知）。
2. `gw.UpdateTools(name, nil)` / `UpdateResources(name, nil, nil)` / `UpdatePrompts(name, nil)`を呼び、該当backendの全項目を即座に消す。
3. 指数backoffで再接続を試み続ける（1秒開始・倍々・上限60秒、無期限）。
4. 成功したら`gw.ConnectBackend(...)`で復帰。
5. 1に戻る。

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| 起動時、接続または`ListTools`に失敗 | 現状通りログを出しつつ、その回の起動には含めない。ただしバックグラウンドで無期限にリトライを続ける（新規） |
| 起動時、`ListResources`/`ListResourceTemplates`/`ListPrompts`に失敗 | 現状通りソフト失敗（空リスト）。リトライ対象にはしない |
| 稼働中の切断 | 即座に該当backendの全項目を除去。バックグラウンドで無期限リトライ |
| 再接続後、以前と全く同じ項目を返す | `UpdateTools`等の既存の差分ロジックがそのまま働き、無駄な再登録はしない |
| 再接続後、conflictの勝敗が変わる | 既存の`logNewConflicts`がそのまま働き、新規に発生した分だけWARNログ |
| gateway shutdown中 | 各`superviseBackend`は`ctx`キャンセルで終了し、リトライ・監視を止める |
| 切断検知直後、まだ処理中のdownstream呼び出し | 変更なし。既存の`TestGateway_CallOnDeadBackendReturnsError`と同じ形でエラーになる |

## ロギング

- 接続失敗・リトライ開始: `logger.Error("backend connect failed, retrying", "backend", ..., "error", ..., "retry_in", ...)`
- 切断検知: `logger.Warn("backend disconnected", "backend", ..., "error", ...)`（正常クローズなら`logger.Info`）
- 再接続成功: 既存の`logger.Info("backend connected", ...)`をそのまま流用

## テスト方針

- **`internal/gateway`**: `upsertEntry`の単体テスト（既知backend名の更新・未知backend名の新規追加）。`ConnectBackend`の単体テスト（新規backend追加のフルフロー、既存backendの再接続フル・conflict絡みのケース）、`-race`。
- **`internal/cli`**: `superviseBackend`の単体テスト — fakeバックエンドを意図的に切断させる（テスト用HTTPサーバーを閉じる、またはstdioプロセスをkillする）ことで`b.Session.Wait()`を発火させ、`UpdateTools`等が空になることを確認。その後fakeバックエンドを復活させ、backoffの上限を小さくしたテスト用の値で自動復帰することを確認。起動時に繋がらなかったbackendが後から`onFirstConnect`を経由せず`ConnectBackend`経由で参加するケースも確認。
- **`internal/cli`（e2e）**: `mcprt server`起動→fakeバックエンド切断→ツールが消える→復活→ツールが復帰、という一連をdownstreamクライアント視点で確認。
- `go test -race ./...`で完結、外部サービス依存なし。

## 将来拡張（本ドキュメントのスコープ外）

- configのホットリロード（別ドキュメント）
- リトライ間隔・上限のYAML設定への露出
- `ListResources`等のソフト失敗もリトライ対象にする選択肢
