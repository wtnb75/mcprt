# mcprt: `gateway.New` 引数構造体化 設計

## 背景・目的

`internal/gateway.New`は、`tools/call` progress通知中継（`docs/superpowers/plans/2026-09-04-mcprt-progress-relay.md`）とelicitation中継（`docs/superpowers/plans/2026-09-05-mcprt-elicitation-relay.md`）の実装を経て、以下のシグネチャになっている：

```go
func New(logger *slog.Logger, backends map[string]*backend.Backend, tables Tables, entries Entries, overrides Overrides, maskKeys []string, progress *ProgressRegistry, elicit *ElicitationRouter) *Server
```

末尾の`maskKeys`・`progress`・`elicit`のうち、`progress`・`elicit`は「その機能を使わないなら`nil`」という省略可能な値であり、この2つがトップレベルの`New`と、内部で使われる`registerTool`・`callHandler`の両方に、常にペアで引き回されている。elicitation中継のplanの最終ブランチレビュー（Minor finding #4）で以下の指摘を受けた：

> `gateway.New`が8個の位置引数（末尾3つがnil可）になっている。3つ目の同種機能が来る前にオプション構造体化を推奨。

厳密には、`progress`・`elicit`は型が異なる（`*ProgressRegistry`と`*ElicitationRouter`）ため、値を取り違えて渡してもGoのコンパイラが型不一致として弾く。したがって「取り違えてもコンパイルが通ってしまう」という意味での実行時バグのリスクは実際には低い。本来の課題は、位置引数が増え続けることによる**可読性・保守性の低下**である：

- 呼び出し箇所（本ドキュメント作成時点でモジュール内に約35箇所）で、「何番目の引数が何を意味するか」を数えないと読めない。
- 3つ目の同種機能（例: サンプリング中継）が将来追加された場合、既存の全呼び出し箇所に機械的な引数追加が必要になる——これは本ドキュメント執筆時点までに実際に2回（progress中継・elicitation中継の導入時）発生しており、パターン化してしまっている。

本ドキュメントは、`gateway.New`とその内部呼び出し経路（`registerTool`・`callHandler`・`Server`構造体）、および同じ問題を抱える`internal/cli/server.go`の`gwHolder`を、構造体渡しに置き換える設計を定義する。**振る舞いは一切変更しない**——純粋なシグネチャ・呼び出し規約のリファクタである。

## スコープ

含める:
- `internal/gateway`に`Relays`構造体（`Progress`・`Elicit`フィールド）と`NewConfig`構造体（`New`の全引数を束ねる）を新設する。
- `New`のシグネチャを`func New(cfg NewConfig) *Server`に変更する。
- `registerTool`・`callHandler`の末尾`progress *ProgressRegistry, elicit *ElicitationRouter`を`relays Relays`1引数にまとめる。
- `Server`構造体の`progress *ProgressRegistry`・`elicit *ElicitationRouter`フィールドを`relays Relays`1フィールドに統合する。
- `internal/gateway/reconcile.go`の`registerTool`呼び出し（`updateToolsLocked`内）を新シグネチャに合わせる。
- `internal/cli/server.go`の`gwHolder`構造体も同じ`gateway.Relays`型を使って`ptr`・`relays gateway.Relays`の2フィールドに統合する（`buildGateway`・`superviseBackend`の該当箇所を合わせて更新）。
- モジュール内の全呼び出し箇所（`internal/gateway/gateway_test.go`・`internal/gateway/reconcile_test.go`・`internal/cli/server.go`・`internal/cli/server_internal_test.go`）を新しい呼び出し規約に書き換える。

含めない（将来拡張・別の課題）:
- `registerResource`・`registerResourceTemplate`・`registerPrompt`・それぞれのハンドラ関数は`progress`・`elicit`を元々受け取っていないため、変更しない。
- `maskKeys []string`は`Relays`には含めない——resource/prompt系のregister関数など、`Relays`を必要としない箇所でも共通して使われる引数であり、性質が異なるため。
- README.mdの更新（別途、両中継機能の説明と合わせて対応する）。
- 本リファクタ後に予定している、モジュール全体を対象にした改めてのコードレビュー（類似の「肥大化した位置引数リスト」パターンが他にもないか）。これは本リファクタの完了後、別セッションで行う。
- `Tables`・`Entries`・`Overrides`自体の構造は変更しない（既にカテゴリごとに構造体化済みであり、今回の問題の対象外）。

## 全体アーキテクチャ

```
                    internal/gateway
┌──────────────────────────────────────────────────────┐
│  type Relays struct {                                  │
│      Progress *ProgressRegistry                        │
│      Elicit   *ElicitationRouter                        │
│  }                                                       │
│                                                           │
│  type NewConfig struct {                                 │
│      Logger    *slog.Logger                              │
│      Backends  map[string]*backend.Backend               │
│      Tables    Tables                                    │
│      Entries   Entries                                   │
│      Overrides Overrides                                 │
│      MaskKeys  []string                                  │
│      Relays    Relays                                    │
│  }                                                        │
│                                                            │
│  func New(cfg NewConfig) *Server                          │
│      └─ registerTool(srv, logger, backends, resolved,     │
│                       maskKeys, relays Relays)             │
│           └─ callHandler(logger, maskKeys, b, name,        │
│                          relays Relays)                     │
│                                                              │
│  type Server struct {                                       │
│      ...                                                     │
│      relays Relays   // was: progress *ProgressRegistry;      │
│                       //      elicit   *ElicitationRouter      │
│  }                                                              │
└──────────────────────────────────────────────────────────────┘
                    internal/cli (server.go)
┌──────────────────────────────────────────────────────┐
│  type gwHolder struct {                                 │
│      ptr    atomic.Pointer[gateway.Server]              │
│      relays gateway.Relays  // was: progress, elicit      │
│  }                                                          │
│                                                               │
│  buildGateway:                                                │
│      gwH.relays = gateway.Relays{                              │
│          Progress: gateway.NewProgressRegistry(),                │
│          Elicit:   gateway.NewElicitationRouter(),                 │
│      }                                                               │
│      ... gateway.New(gateway.NewConfig{..., Relays: gwH.relays})      │
│                                                                          │
│  superviseBackend:                                                       │
│      gwH.relays.Progress.Relay(...)  /  gwH.relays.Elicit.Route(...)      │
└──────────────────────────────────────────────────────────────────────────┘
```

## コンポーネント構成

### `internal/gateway`: `Relays`・`NewConfig`（新規、`gateway.go`に追加）

```go
// Relays bundles the optional cross-call correlation services a gateway
// can wire in. A nil field means that feature is disabled, matching the
// existing nil-means-disabled convention each of *ProgressRegistry and
// *ElicitationRouter already had as standalone parameters.
type Relays struct {
	Progress *ProgressRegistry
	Elicit   *ElicitationRouter
}

// NewConfig bundles New's construction parameters. Fields left at their
// zero value behave exactly as an omitted/nil positional argument did
// before this type existed: a nil Tables/Entries/Overrides sub-field means
// that category has no items, a nil MaskKeys means no extra masking, and a
// nil Relays.Progress/Relays.Elicit means that relay feature is disabled.
type NewConfig struct {
	Logger    *slog.Logger
	Backends  map[string]*backend.Backend
	Tables    Tables
	Entries   Entries
	Overrides Overrides
	MaskKeys  []string
	Relays    Relays
}
```

### `internal/gateway`: `New`

```go
func New(cfg NewConfig) *Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{Logger: cfg.Logger})

	s := &Server{
		mcp:      mcpSrv,
		logger:   cfg.Logger,
		backends: cfg.Backends,
		maskKeys: cfg.MaskKeys,
		relays:   cfg.Relays,

		toolEntries:   cfg.Entries.Tools,
		toolTable:     emptyTable(cfg.Tables.Tools),
		toolOverrides: cfg.Overrides.Tools,
		// ...resource/resourceTemplate/prompt fields unchanged, just read from cfg.* instead of the old positional names
	}

	if cfg.Tables.Tools != nil {
		for _, resolved := range cfg.Tables.Tools.Items {
			registerTool(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys, cfg.Relays)
		}
	}
	// ...resource/resourceTemplate/prompt registration loops unchanged (they don't take Relays)

	return s
}
```

`Tables`・`Entries`・`Overrides`という型自体、および`emptyTable`ヘルパーは変更しない——`cfg.Tables`・`cfg.Entries`・`cfg.Overrides`として参照するだけ。

### `internal/gateway`: `Server`構造体

```go
type Server struct {
	mcp      *mcp.Server
	logger   *slog.Logger
	backends map[string]*backend.Backend
	maskKeys []string
	relays   Relays // was: progress *ProgressRegistry; elicit *ElicitationRouter

	mu sync.Mutex
	// ...以下変更なし
}
```

### `internal/gateway`: `registerTool`・`callHandler`

```go
func registerTool(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Tool], maskKeys []string, relays Relays) (ok bool) {
	// 中身は progress/elicit を relays.Progress/relays.Elicit に読み替えるだけ
	for _, c := range candidates {
		b := backends[c.BackendName]
		if addTool(srv, logger, c.Item, callHandler(logger, maskKeys, b, c.OriginalName, relays)) {
			return true
		}
	}
	// ...
}

func callHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalName string, relays Relays) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// ...
		if relays.Elicit != nil {
			leave := relays.Elicit.Enter(b.Name, req.Session)
			defer leave()
		}
		// ...
		if relays.Progress != nil {
			if token := req.Params.GetProgressToken(); token != nil {
				internalToken, entry, cleanup = relays.Progress.Register(req.Session, token, b.Name)
				// ...
			}
		}
		// ...
	}
}
```

`resourceReadHandler`・`resourceTemplateReadHandler`・`promptGetHandler`・`registerResource`・`registerResourceTemplate`・`registerPrompt`は`Relays`を一切受け取らない——変更なし。

### `internal/gateway/reconcile.go`

`updateToolsLocked`内の`registerTool`呼び出し（`s.progress`・`s.elicit`の2引数）を`s.relays`1引数に置き換える：

```go
if !registerTool(s.mcp, s.logger, s.backends, resolved, s.maskKeys, s.relays) {
```

### `internal/cli/server.go`: `gwHolder`

```go
type gwHolder struct {
	ptr    atomic.Pointer[gateway.Server]
	relays gateway.Relays // was: progress *gateway.ProgressRegistry; elicit *gateway.ElicitationRouter
}
```

`buildGateway`内、`connectBackends`を呼ぶ前に構築する箇所：

```go
var gwH gwHolder
gwH.relays = gateway.Relays{
	Progress: gateway.NewProgressRegistry(),
	Elicit:   gateway.NewElicitationRouter(),
}
conn := connectBackends(ctx, logger, cfg.Backends, &gwH)
```

`gateway.New`呼び出しは`NewConfig`経由に変更：

```go
srv := gateway.New(gateway.NewConfig{
	Logger:   logger,
	Backends: conn.backends,
	Tables: gateway.Tables{
		Tools:             toolTable,
		Resources:         resourceTable,
		ResourceTemplates: resourceTemplateTable,
		Prompts:           promptTable,
	},
	Entries: gateway.Entries{
		Tools:             conn.toolEntries,
		Resources:         conn.resourceEntries,
		ResourceTemplates: conn.resourceTemplateEntries,
		Prompts:           conn.promptEntries,
	},
	Overrides: gateway.Overrides{
		Tools:             cfg.Overrides,
		Resources:         cfg.ResourceOverrides,
		ResourceTemplates: cfg.ResourceTemplateOverrides,
		Prompts:           cfg.PromptOverrides,
	},
	MaskKeys: cfg.Logging.MaskKeys,
	Relays:   gwH.relays,
})
gwH.ptr.Store(srv)
```

`superviseBackend`内のコールバック構築（`cb.OnProgress`・`cb.OnElicit`）は、`gwH.progress`/`gwH.elicit`への参照を`gwH.relays.Progress`/`gwH.relays.Elicit`に読み替えるだけ：

```go
if gwH.relays.Progress != nil {
	cb.OnProgress = func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
		gwH.relays.Progress.Relay(ctx, logger, bc.Name, req.Params)
	}
}
if gwH.relays.Elicit != nil {
	cb.OnElicit = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		session, err := gwH.relays.Elicit.Route(bc.Name)
		// ...
	}
}
```

## データフロー（呼び出し規約の変化）

すべて構造体リテラルへの機械的な書き換えであり、実行時の動作・順序に変化はない。Go言語の構造体リテラルはゼロ値フィールドを省略できるため、多くのテスト呼び出しは**書き換え後の方が短くなる**：

Before（`internal/gateway/gateway_test.go`の典型的な呼び出し）:
```go
srv := gateway.New(logger, want, gateway.Tables{}, gateway.Entries{}, gateway.Overrides{}, nil, nil, nil)
```
After:
```go
srv := gateway.New(gateway.NewConfig{Logger: logger, Backends: want})
```

Before（Relaysを使うテスト、progress中継のTask3で追加されたもの）:
```go
srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, gateway.Entries{}, gateway.Overrides{}, nil, progressReg, nil)
```
After:
```go
srv := gateway.New(gateway.NewConfig{
	Logger:   logger,
	Backends: map[string]*backend.Backend{"backend-a": connA},
	Tables:   gateway.Tables{Tools: table},
	Relays:   gateway.Relays{Progress: progressReg},
})
```

## エラーハンドリング

振る舞いの変更なし。`New`・`registerTool`・`callHandler`のロジック自体（progress登録・elicitation相関・エラー時のフォールバック等）は一切変更しない——シグネチャと呼び出し規約のみが変わる。既存のnilチェック（`if relays.Progress != nil`等）は既存の`if progress != nil`と等価。

## テスト方針

- 振る舞いに変更がないため、新規テストは不要。既存のテスト（`internal/gateway/gateway_test.go`・`reconcile_test.go`・`internal/cli/server_test.go`・`server_internal_test.go`）が、progress中継・elicitation中継それぞれの機能を既にカバーしている——これらが新しい呼び出し規約に書き換えた後も全てPASSすることが、リファクタが振る舞いを壊していないことの検証になる。
- `go build ./... && go vet ./... && go test ./... -race`がモジュール全体でエラーなく通ることを最終的な受け入れ基準とする。
- 呼び出し箇所の書き換え漏れは`go build`のコンパイルエラーとして機械的に検出できる（progress中継・elicitation中継の実装時に確立済みのパターン）。

## 将来拡張（本ドキュメントのスコープ外）

- 3つ目の同種機能（例: サンプリング中継）が将来追加される場合、`Relays`構造体に1フィールド追加するだけで済み、`NewConfig`・`registerTool`・`callHandler`のシグネチャ自体は変更不要になる——本リファクタの主目的である。
- 本リファクタ完了後、モジュール全体を対象にした改めてのコードレビューを別途行い、他の箇所に同様の「肥大化した位置引数リスト」パターンがないか確認する。
