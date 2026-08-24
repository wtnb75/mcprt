# mcprt: list_changed 動的リレー 設計

## 背景・目的

`2026-08-19-mcprt-gateway-design.md`（tool中継）、`2026-08-20-mcprt-resources-design.md`（resource中継）、`2026-08-20-mcprt-prompts-design.md`（prompt中継）はいずれも「起動時に一度だけ全backendへ接続し、`ListTools`/`ListResources`/`ListResourceTemplates`/`ListPrompts`を実行して`router.Table`を構築し、以後は不変」という静的スナップショット設計を採っている。backend側の一覧が実行中に変化しても、mcprtは再起動するまでそれを反映しない。

MCP仕様は`notifications/tools/list_changed`・`notifications/prompts/list_changed`・`notifications/resources/list_changed`（resourceとresource templateの両方をこの1種類の通知でカバーする）を定義しており、backendがこれを送ってきた場合にmcprtが再Listしてdownstreamへ伝播する動的リレーを、本ドキュメントで設計する。

SDK（`github.com/modelcontextprotocol/go-sdk` v1.7.0）を調査した結果、以下を確認済み:

- downstreamへの`list_changed`通知の送信自体はSDKの`Server`が自動で行う。`Server.AddTool`/`RemoveTools`（および`AddResource`/`RemoveResources`、`AddResourceTemplate`/`RemoveResourceTemplates`、`AddPrompt`/`RemovePrompts`）はいずれも内部で`changeAndNotify`を呼んでおり、これが接続中のdownstreamセッションへ該当する`list_changed`を自動送信する（`mcp/server.go`）。mcprtが起動後にこれらのメソッドを呼びさえすれば、downstreamへの伝播はSDKの責務として無償で得られる。
- `Server.capabilities()`は、1件でも該当カテゴリの項目が登録されていれば自動的に`ListChanged: true`をdownstreamへ広告する。mcprt側で明示的なcapability opt-inは不要。
- `Add*`系メソッドは同名項目に対して暗黙にupsertする（前回の最終レビューで`AddPrompt`について確認済み。`AddTool`/`AddResource`/`AddResourceTemplate`も同様のstore実装であり、同じ振る舞いをすると設計上は前提とし、実装時に確認する）。したがって「消えた項目をRemove」「新規・変更された項目をAdd」の2操作だけで反映できる。

これらの発見により、mcprt側が新規実装するのは「backendの通知を受けて→そのbackendだけ再List→全backend分の既知データをマージして`router.Resolve`→前回の`Table`との差分を`srv.Add*`/`Remove*`に変換」という**reconcile処理**に限定される。

## スコープ

含める:
- `notifications/tools/list_changed`・`notifications/prompts/list_changed`・`notifications/resources/list_changed`をbackendから受信し、該当backendのみ再Listして、mcprtが公開する集約済みルーティングテーブル（tool/resource/resource template/prompt の4種）を再計算・再登録する
- 反映後の変更をdownstreamへ伝播する（SDKの自動送信に乗るだけで、mcprtが明示的に何か送るわけではない）
- 再List失敗時に直前の既知の一覧を保持するフォールバック

含めない（将来拡張）:
- `resources/subscribe` / `notifications/resources/updated`（別スペックとして今後設計する）
- backendへの自動再接続・切断検知（起動時にしか接続失敗を検出しない、という既存の制約を維持）
- configのホットリロード（`overrides`系はconnectBackends実行時点の値のまま。ランタイム中に設定ファイルを変更しても反映しない）
- `mcprt list`コマンドへの対応（サーバーループを持たない一発コマンドであり、そもそもlist_changedを受信し続ける機会がない）
- 通知のデバウンス／コーリアレス（1backendからの連続通知はそのまま連続でreconcileする。既存コードのスタイルに合わせ、まずはシンプルな実装を優先する）

## 全体アーキテクチャ

```
                    ┌───────────────────────────┐
  client(s) ──────▶ │   gateway.Server           │
 (stdio/HTTP)       │   - tools/resources/       │
                    │     resource templates/     │
                    │     prompts の各ハンドラ     │
                    └─────────────┬───────────────┘
                                  │ mu で保護された
                                  │ 4種のEntry/Table状態
                                  ▼
                    ┌───────────────────────────┐
                    │ UpdateTools/UpdateResources │
                    │ /UpdatePrompts               │
                    │  (router.Resolve 再実行      │
                    │   + 新旧Table差分をAdd/Remove)│
                    └─────────────┬───────────────┘
                                  ▲
                                  │ 再List結果 (I/Oはmu外)
              ┌───────────────────┼───────────────────┐
              ▼                   ▼                   ▼
        Backend Client      Backend Client       Backend Client
        (ChangeCallbacks     (ChangeCallbacks      (ChangeCallbacks
         経由でcli層の         経由でcli層の          経由でcli層の
         クロージャを起動)      クロージャを起動)       クロージャを起動)
```

起動時のシーケンス自体（全backend並行接続→初回List→初回Resolve→登録）はv1〜prompts中継と変わらない。変わるのは、その後もbackendからの通知を受けて同じ登録処理を再実行できるようになる点。

## コンポーネント構成

### `internal/backend`: 通知コールバックの配線

`Backend.Connect`に、backend単位の変更通知を受け取るコールバックを渡せるようにする。`mcp.NewClient`に渡す`ClientOptions`に`ToolListChangedHandler`/`PromptListChangedHandler`/`ResourceListChangedHandler`をセットする（`ResourceUpdatedHandler`はsubscribe機能のスコープなので今回は未設定のまま）。

```go
// ChangeCallbacks are invoked when a connected backend reports that its
// tool/prompt/resource list has changed. Each func takes no arguments: MCP's
// list_changed notifications carry no payload, they only signal "go re-list."
// A nil field means "not interested" and leaves the corresponding SDK
// handler unset.
type ChangeCallbacks struct {
    OnToolsChanged     func()
    OnPromptsChanged   func()
    OnResourcesChanged func() // fires for notifications/resources/list_changed, which covers BOTH resources and resource templates per the MCP spec -- there is no separate resource-template notification
}

func Connect(ctx context.Context, cfg config.BackendConfig, cb ChangeCallbacks) (*Backend, error)
```

`internal/backend`は`internal/router`・`internal/gateway`に依存させない。コールバックの型を引数なし`func()`にとどめ、「何が変わったか」の解釈や再Listの実行、下流への反映はすべて呼び出し側（cli層）に委ねる。

### `internal/gateway`: reconcile状態を持つ`Server`型

現在の`gateway.New(...) *mcp.Server`を、内部状態（backendごとのEntry、現在のTable、overrides、排他用mutex）を保持する`*gateway.Server`を返すように変更する。`*mcp.Server`自体は`Server.MCP() *mcp.Server`のようなアクセサで取り出し、`ServeStdio`/`ServeHTTP`は引き続き`*mcp.Server`を受け取る形を維持する。

```go
type Server struct {
    mcp      *mcp.Server
    logger   *slog.Logger
    backends map[string]*backend.Backend

    mu sync.Mutex // 以下4カテゴリぶんのentries/currentTableをまとめて保護する。
                  // 保護区間はrouter.Resolve(インメモリ)とSDKのAdd/Remove呼び出し
                  // (バックエンドI/Oを含まない)だけなので、1本のmutexで足りる。

    toolEntries       []router.Entry[*mcp.Tool]
    toolTable         *router.Table[*mcp.Tool]
    toolOverrides     map[string]string

    resourceEntries         []router.Entry[*mcp.Resource]
    resourceTable           *router.Table[*mcp.Resource]
    resourceOverrides       map[string]string
    resourceTemplateEntries []router.Entry[*mcp.ResourceTemplate]
    resourceTemplateTable   *router.Table[*mcp.ResourceTemplate]
    resourceTemplateOverrides map[string]string

    promptEntries   []router.Entry[*mcp.Prompt]
    promptTable     *router.Table[*mcp.Prompt]
    promptOverrides map[string]string
}

// UpdateTools replaces backendName's tool entry with items, re-resolves the
// merged table, and applies the diff (Remove vanished names, Add
// new/changed names) to the underlying *mcp.Server.
func (s *Server) UpdateTools(backendName string, items []*mcp.Tool)

// UpdateResources replaces backendName's resource AND resource-template
// entries together (MCP fires one notification for both), re-resolves both
// tables, and applies both diffs, all under one lock acquisition.
func (s *Server) UpdateResources(backendName string, resources []*mcp.Resource, templates []*mcp.ResourceTemplate)

func (s *Server) UpdatePrompts(backendName string, items []*mcp.Prompt)
```

`New(...)`は、既存の`registerTool`/`registerResource`/`registerResourceTemplate`/`registerPrompt`ループと同じ内容を初期状態の構築として使う。差分適用ロジックの本体は次の共通手順（tool/resource/resource template/promptで対称）:

1. 該当backendのEntryを新しいitemsで差し替える
2. `router.Resolve`で全体を再計算し、新しい`*router.Table[T]`を得る
3. 旧`Table.Items`と新`Table.Items`をexposed name（tool/prompt名、resource/resource templateのURI）で突き合わせる
   - 旧にあって新にないexposed name → `srv.RemoveTools`/`RemoveResources`/`RemoveResourceTemplates`/`RemovePrompts`
   - 新にあって、旧に存在しないか内容が異なる（`reflect.DeepEqual`不一致、または勝者backend名が変わった）exposed name → 既存の`registerTool`/`registerResource`/`registerResourceTemplate`/`registerPrompt`を呼ぶ（`Add*`はupsertなので明示的なRemoveは不要）
4. 新しい`Conflicts`があれば起動時と同じ形で`logger.Warn`
5. `toolTable`（等）を新しいTableに差し替える

### `internal/cli/server.go`: 再List実行と配線

`connectBackends`で`backend.Connect`を呼ぶ際、`gwServer`（`gateway.New`の戻り値）への参照を閉じ込めた`backend.ChangeCallbacks`を渡す。各コールバックは概ね次の形:

```go
func toolsChangedCallback(ctx context.Context, logger *slog.Logger, b *backend.Backend, gw *gateway.Server) func() {
    return func() {
        ctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
        defer cancel()
        tools, err := b.ListTools(ctx)
        if err != nil {
            logger.Warn("list_changed: re-list failed, keeping previous list", "backend", b.Name, "error", err)
            return
        }
        gw.UpdateTools(b.Name, tools)
    }
}
```

ここには実在の競合状態がある: backendの通知読み取りループは`backend.Connect`が返った直後から動き出す（=list_changed通知をすでに受け取れる状態になる）が、`*gateway.Server`は全backendの初回List完了後でなければ構築できない（`router.Resolve`が全backend分のEntryを必要とするため）。したがって、コールバッククロージャが作られる時点（`backend.Connect`呼び出し時）ではまだ`gw`は存在しない。

これは`gwHolder`のような1段の間接参照で解決する:

```go
// gwHolder lets a backend's ChangeCallbacks closures (created before the
// *gateway.Server exists) reference it once it does.
type gwHolder struct {
    ptr atomic.Pointer[gateway.Server]
}
```

`connectBackends`は各backend用に共有の`gwHolder`を1つ作り、コールバックはそこから`ptr.Load()`する。`ptr`がまだ`nil`（＝全backendの初回List〜`gateway.New`呼び出しがまだ完了していない）の間に通知が届いた場合は、そのコールバックは何もせず戻る（reconcileを1回取りこぼす）。これは許容する: list_changed通知は「何かが変わったので再Listせよ」という単発シグナルであり、配送保証や順序保証を前提にした設計ではない。取りこぼした変更は、直後に控えている初回`ListTools`等（`gateway.New`の直前に必ず実行される）で最終的に反映される。`runServer`は全backend接続・初回List・`gateway.New`完了後に`ptr.Store(gw)`する。

## データフロー

**tools の例（resources/promptsも対称）**

1. backendが`notifications/tools/list_changed`を送信
2. backendの`mcp.Client`の読み取りループが`ToolListChangedHandler`を呼ぶ（backendのSessionに紐づくgoroutine上で実行される）
3. cli層のクロージャが起動。`backendConnectTimeout`相当のタイムアウト付きで`b.ListTools(ctx)`を実行する（`gateway.Server`のmutex外・I/O）
   - 失敗したら`logger.Warn`して終了（直前の既知の一覧を保持し、このreconcileはスキップする）
4. 成功したら`gw.UpdateTools(backendName, tools)`を呼ぶ
5. `UpdateTools`内: mutex取得 → 該当backendの`toolEntries`を更新 → `router.Resolve`で全体再計算 → 新旧`Table.Items`を突き合わせて`RemoveTools`/`AddTool`を必要な分だけ呼ぶ → 新しいconflictがあればWARNログ → mutex解放
6. SDKが登録内容の変化を検知し、自動的にdownstreamへ`notifications/tools/list_changed`を送信する（実装確認：SDK内部は`changeAndNotify`が`notificationDelay`＝10msの`time.AfterFunc`タイマーで送信をまとめており、1回の`UpdateTools`呼び出し内でRemove/Addが複数回発生してもdownstreamへの送信は1回に集約される。これはSDK側の実装詳細であり、mcprt自身は何のデバウンス／コーリアレスも実装しない、というスコープ外方針とは矛盾しない）

resources用コールバックのみ、`ListResources`と`ListResourceTemplates`の両方を再実行し、`UpdateResources`が両方のTableを1回のロック区間内でまとめて差分適用する。

**起動シーケンス**は既存のprompts中継の設計から変わらない（全backend並行接続 → 各種List並行実行 → `router.Resolve` → `gateway.New`でTableを登録してServer起動）。変わるのは、起動後もbackend単位のコールバックが生きている点のみ。

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| 起動時にbackend接続失敗 | 既存どおりERRORログを出してそのbackendを除外、他は起動続行 |
| 起動時の初回List失敗（tools以外） | 既存どおりWARNログを出し「そのカテゴリは空」として扱う |
| 実行中、list_changed通知を受けての再List失敗 | WARNログを出し、直前の既知の一覧を保持（reconcileをスキップ）。downstreamには何も伝播しない |
| 実行中、再List成功後の`router.Resolve`でconflictが発生（新規または解消） | 起動時と同じ形でWARNログ（発生時のみ・解消時は特にログしない） |
| 実行中にbackendが切断される | 本ドキュメントのスコープ外。切断検知・自動再接続は行わない（既存の制約を維持） |
| clientが存在しないtool/resource/resource template/prompt名を呼ぶ | 標準的なMCPの「not found」エラーを返す（変更なし） |

## ロギング

v1から変更なし（`log/slog`、`--log-level`フラグ、人間可読テキストハンドラでstderr）。

## テスト方針

- **`internal/backend`**: `ClientOptions`に渡したハンドラが、fake HTTP MCPサーバーから`notifications/tools/list_changed`等を受信したときにコールバックが呼ばれることを確認する（既存の`newFakeMCPHandler`パターンを流用し、テスト用サーバー側で`srv.AddTool`等を呼んで通知を誘発する）。`OnResourcesChanged`が`notifications/resources/list_changed`1回の受信で1回だけ呼ばれることも確認する。
- **`internal/gateway`**: `Server.UpdateTools`等を直接呼び、
  - 新規追加・削除・内容変更（同名だがdescriptionが変わる等）の3パターンで、`*mcp.Server`に実際に登録されているtool一覧（`mcp.Server`の内部状態を直接見る手段がなければ、テスト用downstreamクライアントで`tools/list`して確認）が正しく変わること
  - 複数backend間のconflict勝敗が再Resolveで正しく切り替わること（backend Aの項目が消えたらbackend Bのfallbackが昇格する、等）
  - `-race`付きで複数回の`Update*`呼び出しを並行実行してもraceしないこと
- **`internal/cli`（e2e）**: 既存の`TestServerCommand_ServesAggregatedTools`系のfakeサーバーパターンを拡張し、テスト内でfakeサーバー側の`mcp.Server`に対して起動後に`AddTool`/`RemoveTools`を呼んで実際に通知を発火させ、mcprt gatewayに繋いだテスト用downstreamクライアントが`list_changed`を受け取り再Listした結果、期待通りのtools一覧になることを確認する統合テスト。再List失敗時に直前の一覧を保持するケースも、通知後に対象backendへの以降のList呼び出しだけ失敗させるfakeサーバーで再現する。resources/resource templates/promptsについても同型のテストを用意する。
- `go test ./...`で完結、外部サービス依存なし。

## 将来拡張（本ドキュメントのスコープ外）

- `resources/subscribe` / `notifications/resources/updated`（downstream購読の受付、backendへの購読転送、購読者数のrefcounting）
- backendへの自動再接続、切断検知
- configのホットリロード
