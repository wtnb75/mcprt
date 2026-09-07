# mcprt: gateway.New 引数構造体化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `internal/gateway.New`（および`registerTool`・`callHandler`・`Server`構造体）と`internal/cli`の`gwHolder`が抱える「肥大化した位置引数リスト」を、`gateway.Relays`/`gateway.NewConfig`という2つの構造体に置き換える。**振る舞いは一切変更しない**——純粋なシグネチャ・呼び出し規約のリファクタである。

**Architecture:** `internal/gateway`に`Relays{Progress, Elicit}`・`NewConfig{Logger, Backends, Tables, Entries, Overrides, MaskKeys, Relays}`を新設し、`New(cfg NewConfig) *Server`に変更する。`Server`構造体・`registerTool`・`callHandler`は`progress`/`elicit`の2フィールド・2引数を`relays Relays`1つに統合する。`internal/cli/server.go`の`gwHolder`も同じ`gateway.Relays`型を使って`progress`/`elicit`の2フィールドを`relays gateway.Relays`1つに統合する。詳細は spec を参照。

**Tech Stack:** Go 1.x, `github.com/modelcontextprotocol/go-sdk v1.7.0`（`mcp`パッケージ）, 標準の`go test`（`-race`込み）。

**Spec:** `docs/superpowers/specs/2026-09-05-mcprt-gateway-config-refactor-design.md`

## Global Constraints

- **振る舞いを一切変更しない。** progress中継・elicitation中継のロジック（`ProgressRegistry`・`ElicitationRouter`自体、`callHandler`内の登録/相関/クリーンアップ処理、`logCall`への要約付加、`superviseBackend`内の`OnProgress`/`OnElicit`のタイムアウト・エラーハンドリング）は一切変更しない。変更するのはシグネチャと呼び出し規約のみ。
- 新規テストは不要。既存のテストスイート（progress中継・elicitation中継それぞれのユニット/統合/e2eテスト）が、新しい呼び出し規約に書き換えた後も全てPASSすることが、このリファクタが振る舞いを壊していないことの唯一の検証手段である。
- `registerResource`・`registerResourceTemplate`・`registerPrompt`とそれぞれのハンドラ関数、および`resourceReadHandler`・`resourceTemplateReadHandler`・`promptGetHandler`は`Relays`を受け取らない——変更しない。
- `maskKeys []string`は`Relays`に含めない。`NewConfig`の独立したフィールドのまま。
- `go.mod`のモジュールパスは`github.com/wtnb75/mcprt`。

---

## ファイル構成

| ファイル | 種別 | 責務 |
|---|---|---|
| `internal/gateway/gateway.go` | 変更 | `Relays`/`NewConfig`型の新設、`Server`構造体、`New`、`registerTool`、`callHandler`のシグネチャ変更 |
| `internal/gateway/reconcile.go` | 変更 | `updateToolsLocked`内の`registerTool`呼び出しを`s.relays`1引数に変更 |
| `internal/gateway/gateway_test.go` | 変更 | 全ての`gateway.New(...)`呼び出しを`gateway.New(gateway.NewConfig{...})`形式に書き換え |
| `internal/gateway/reconcile_test.go` | 変更 | 同上 |
| `internal/cli/server.go` | 変更 | `gwHolder`構造体、`buildGateway`、`superviseBackend`内の`cb.OnProgress`/`cb.OnElicit`配線を`gwH.relays`経由に変更 |
| `internal/cli/server_internal_test.go` | 変更 | `gateway.New(...)`呼び出し・`&gwHolder{elicit: ...}`直接構築箇所を書き換え |

---

## Task 1: `internal/gateway`: `Relays`/`NewConfig`導入と全呼び出し箇所の書き換え

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/reconcile.go`
- Modify: `internal/gateway/gateway_test.go`
- Modify: `internal/gateway/reconcile_test.go`

**Interfaces:**
- Produces: `gateway.Relays{Progress *ProgressRegistry, Elicit *ElicitationRouter}`、`gateway.NewConfig{Logger, Backends, Tables, Entries, Overrides, MaskKeys, Relays}`、`gateway.New(cfg NewConfig) *Server`。Task 2の`internal/cli/server.go`がこれらをそのまま使う。

- [ ] **Step 1: `Relays`・`NewConfig`型を追加する**

`internal/gateway/gateway.go`の`Overrides`型定義（`Server`構造体の直前）の後に追加:

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

- [ ] **Step 2: `Server`構造体の`progress`/`elicit`フィールドを`relays`に統合する**

`internal/gateway/gateway.go`の`Server`構造体を変更:

```go
type Server struct {
	mcp      *mcp.Server
	logger   *slog.Logger
	backends map[string]*backend.Backend
	maskKeys []string
	relays   Relays

	mu sync.Mutex

	// ...以下（toolEntries〜promptOverrides）は変更なし
```

- [ ] **Step 3: `New`のシグネチャを`NewConfig`受け取りに変更する**

`internal/gateway/gateway.go`の`New`を変更:

```go
// New builds a Server that exposes cfg.Tables' resolved tools/resources/
// prompts, forwarding each call to the backend that owns it, and retains
// cfg.Entries and cfg.Overrides so a later UpdateTools/UpdateResources/
// UpdatePrompts call can re-run router.Resolve when a backend reports its
// list has changed. cfg.Backends must contain an entry for every
// BackendName referenced in cfg.Tables (the caller builds both from the
// same set of connected backends).
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

		resourceEntries:           cfg.Entries.Resources,
		resourceTable:             emptyTable(cfg.Tables.Resources),
		resourceOverrides:         cfg.Overrides.Resources,
		resourceTemplateEntries:   cfg.Entries.ResourceTemplates,
		resourceTemplateTable:     emptyTable(cfg.Tables.ResourceTemplates),
		resourceTemplateOverrides: cfg.Overrides.ResourceTemplates,

		promptEntries:   cfg.Entries.Prompts,
		promptTable:     emptyTable(cfg.Tables.Prompts),
		promptOverrides: cfg.Overrides.Prompts,
	}

	if cfg.Tables.Tools != nil {
		for _, resolved := range cfg.Tables.Tools.Items {
			registerTool(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys, cfg.Relays)
		}
	}
	if cfg.Tables.Resources != nil {
		for _, resolved := range cfg.Tables.Resources.Items {
			registerResource(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys)
		}
	}
	if cfg.Tables.ResourceTemplates != nil {
		for _, resolved := range cfg.Tables.ResourceTemplates.Items {
			registerResourceTemplate(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys)
		}
	}
	if cfg.Tables.Prompts != nil {
		for _, resolved := range cfg.Tables.Prompts.Items {
			registerPrompt(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys)
		}
	}

	return s
}
```

（`emptyTable`関数自体は変更しない。）

- [ ] **Step 4: `registerTool`のシグネチャを変更する**

`internal/gateway/gateway.go`の`registerTool`を変更（末尾の`progress *ProgressRegistry, elicit *ElicitationRouter`を`relays Relays`に置き換え、`callHandler`呼び出しも合わせる）:

```go
func registerTool(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Tool], maskKeys []string, relays Relays) (ok bool) {
	candidates := append([]router.Candidate[*mcp.Tool]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addTool(srv, logger, c.Item, callHandler(logger, maskKeys, b, c.OriginalName, relays)) {
			return true
		}
	}
	logger.Error("tool unavailable: every candidate backend had an invalid definition", "tool", resolved.Item.Name)
	return false
}
```

- [ ] **Step 5: `callHandler`のシグネチャを変更する**

`internal/gateway/gateway.go`の`callHandler`を変更（末尾の`progress *ProgressRegistry, elicit *ElicitationRouter`を`relays Relays`に置き換え、本体の`progress`/`elicit`参照を`relays.Progress`/`relays.Elicit`に置き換える。ロジック自体は一切変更しない）:

```go
// callHandler forwards a tools/call to originalName on backend b, passing
// the raw arguments through unchanged. It wraps the call in a span
// (startCallSpan is a no-op for stdio-originated calls) and logs it via
// logCall, success or failure, so a dead or erroring backend — and normal
// usage — is visible to the operator. When relays.Progress is non-nil and
// the downstream request carries a progressToken, it registers a fresh
// correlation entry so a notifications/progress the backend sends mid-call
// (relayed via relays.Progress.Relay, wired through backend.ChangeCallbacks.
// OnProgress) reaches the downstream caller under its own token. When
// relays.Elicit is non-nil, it records this call as in-flight against b for
// the whole duration of the backend call, so a backend's elicitation/create
// (relayed via relays.Elicit.Route, wired through backend.ChangeCallbacks.
// OnElicit) can be routed back to req.Session when -- and only when --
// this is the sole tools/call in flight against b.
func callHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalName string, relays Relays) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		ctx, span := startCallSpan(ctx, req.Extra, "tools/call",
			attribute.String("mcp.backend", b.Name),
			attribute.String("mcp.tool.name", originalName))
		defer span.End()

		if relays.Elicit != nil {
			leave := relays.Elicit.Enter(b.Name, req.Session)
			defer leave()
		}

		params := &mcp.CallToolParams{Name: originalName, Arguments: req.Params.Arguments}

		var entry *progressEntry
		if relays.Progress != nil {
			if token := req.Params.GetProgressToken(); token != nil {
				var internalToken uint64
				var cleanup func()
				internalToken, entry, cleanup = relays.Progress.Register(req.Session, token, b.Name)
				defer cleanup()
				// SetProgressToken only accepts int/int32/int64/string (see
				// go-sdk's setProgressToken); Register hands out a uint64, so
				// it must be narrowed to int64 here. normalizeProgressToken
				// (progress.go) accepts int64 back, so this round-trips
				// correctly on the relay side.
				params.SetProgressToken(int64(internalToken))
			}
		}

		result, err := b.Session.CallTool(ctx, params)
		recordOutcome(span, err)
		logCall(ctx, logger, "tool", "tool", originalName, b.Name, req.Session, req.Params.Arguments, maskKeys, start, err, entry)
		return result, err
	}
}
```

- [ ] **Step 6: `reconcile.go`の`registerTool`呼び出しを変更する**

`internal/gateway/reconcile.go`の`updateToolsLocked`内、`registerTool`呼び出しを変更:

```go
		if !registerTool(s.mcp, s.logger, s.backends, resolved, s.maskKeys, s.relays) {
```

- [ ] **Step 7: `go build`でコンパイルエラーを洗い出し、呼び出し箇所を書き換える**

Run: `go build ./internal/gateway/... 2>&1 | head -80`

`internal/gateway/gateway_test.go`（約18箇所）・`internal/gateway/reconcile_test.go`（約12箇所）の`gateway.New(...)`呼び出しがコンパイルエラーになる。それぞれ位置引数呼び出しを`gateway.NewConfig{...}`構造体リテラルに書き換える。**ゼロ値のフィールドは省略する**（`gateway.Tables{}`・`gateway.Entries{}`・`gateway.Overrides{}`・`nil`だけの引数は、対応するフィールドごと書かない）——これによりテストの大半はリライト後の方が短くなる。

例1（`gateway_test.go`、progress/elicitともに使わない典型的な呼び出し）:

- Before: `srv := gateway.New(logger, want, gateway.Tables{}, gateway.Entries{}, gateway.Overrides{}, nil, nil, nil)`
- After: `srv := gateway.New(gateway.NewConfig{Logger: logger, Backends: want})`

例2（`Tools`テーブルとmaskKeysを使う呼び出し）:

- Before: `srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, gateway.Entries{}, gateway.Overrides{}, []string{"secret_value"}, nil, nil)`
- After:
  ```go
  srv := gateway.New(gateway.NewConfig{
  	Logger:   logger,
  	Backends: map[string]*backend.Backend{"backend-a": connA},
  	Tables:   gateway.Tables{Tools: table},
  	MaskKeys: []string{"secret_value"},
  })
  ```

例3（progress中継のテスト、`Relays.Progress`を使う呼び出し。`gateway_test.go`内の`TestGateway_CallHandlerRelaysProgressAndLogsSummary`等）:

- Before: `srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, gateway.Entries{}, gateway.Overrides{}, nil, progressReg, nil)`
- After:
  ```go
  srv := gateway.New(gateway.NewConfig{
  	Logger:   logger,
  	Backends: map[string]*backend.Backend{"backend-a": connA},
  	Tables:   gateway.Tables{Tools: table},
  	Relays:   gateway.Relays{Progress: progressReg},
  })
  ```

例4（elicitation中継のテスト、`Relays.Elicit`を使う呼び出し。`gateway_test.go`内の`TestGateway_CallHandlerRoutesElicitationWhenExactlyOneCallInFlight`等）:

- Before: `srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, gateway.Entries{}, gateway.Overrides{}, nil, nil, elicitRouter)`
- After:
  ```go
  srv := gateway.New(gateway.NewConfig{
  	Logger:   logger,
  	Backends: map[string]*backend.Backend{"backend-a": connA},
  	Tables:   gateway.Tables{Tools: table},
  	Relays:   gateway.Relays{Elicit: elicitRouter},
  })
  ```

`reconcile_test.go`の呼び出しは複数行にまたがっており、`Entries`（`gateway.Entries{Tools: conn.toolEntries}`のような値）を実際に使っているものが多い——その場合はそのフィールドも`NewConfig`に含める（省略できるのは本当にゼロ値のフィールドだけ）。上記の変換パターンをそのまま当てはめて、`go build ./internal/gateway/...`が通るまで全箇所を直す。

Expected: `go build ./internal/gateway/... && go vet ./internal/gateway/...`がエラーなしで終わる。

- [ ] **Step 8: `internal/gateway`パッケージ全体のテストを実行する**

Run: `go test ./internal/gateway/... -race`
Expected: `ok`（既存の全テスト——progress中継・elicitation中継それぞれのユニット/統合テストを含む——がPASSすること。これが振る舞い不変の検証）。

- [ ] **Step 9: コミット**

```bash
git add internal/gateway/gateway.go internal/gateway/reconcile.go \
        internal/gateway/gateway_test.go internal/gateway/reconcile_test.go
git commit -m "refactor(gateway): bundle New's parameters into NewConfig/Relays structs"
```

---

## Task 2: `internal/cli/server.go`: `gwHolder`の統合と呼び出し箇所の書き換え

**Files:**
- Modify: `internal/cli/server.go`
- Modify: `internal/cli/server_internal_test.go`

**Interfaces:**
- Consumes: Task 1の`gateway.Relays`・`gateway.NewConfig`・`gateway.New(cfg NewConfig) *Server`。

- [ ] **Step 1: `gwHolder`構造体の`progress`/`elicit`フィールドを`relays`に統合する**

`internal/cli/server.go`の`gwHolder`定義を変更:

```go
// gwHolder lets a backend's ChangeCallbacks closures (built inside
// connectBackends, before the *gateway.Server exists) reference it once
// runServer finishes building it. A nil Load() means the initial
// connect-and-list sequence gateway.New's caller runs hasn't completed yet;
// a notification that fires in that window is dropped -- the pending
// initial ListTools/ListResources/ListResourceTemplates/ListPrompts that
// runServer is about to do anyway will reflect the same change, so nothing
// is permanently lost.
//
// relays is set once, before connectBackends spawns any supervisor
// goroutine, and never mutated afterward -- so reading it from those
// goroutines needs no lock (the write happens-before every goroutine's
// creation). A zero-value relays (buildGateway always sets both of its
// fields; only some tests construct a bare gwHolder{} without them) means
// "no progress relay/elicitation routing for this generation," matching a
// nil *gateway.ProgressRegistry/*gateway.ElicitationRouter everywhere else.
type gwHolder struct {
	ptr    atomic.Pointer[gateway.Server]
	relays gateway.Relays
}
```

- [ ] **Step 2: `buildGateway`を変更する**

`internal/cli/server.go`の`buildGateway`内、`gwH.progress = ...`・`gwH.elicit = ...`の2行を1つの`gwH.relays`代入に置き換える:

```go
	var gwH gwHolder
	gwH.relays = gateway.Relays{
		Progress: gateway.NewProgressRegistry(),
		Elicit:   gateway.NewElicitationRouter(),
	}
	conn := connectBackends(ctx, logger, cfg.Backends, &gwH)
```

`gateway.New(...)`呼び出しを`NewConfig`経由に変更:

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

- [ ] **Step 3: `superviseBackend`内の`cb.OnProgress`/`cb.OnElicit`配線を変更する**

`internal/cli/server.go`の`superviseBackend`内、`gwH.progress`/`gwH.elicit`への参照を`gwH.relays.Progress`/`gwH.relays.Elicit`に置き換える（ロジック・エラーハンドリング・ログメッセージは一切変更しない）:

```go
		if gwH.relays.Progress != nil {
			cb.OnProgress = func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
				gwH.relays.Progress.Relay(ctx, logger, bc.Name, req.Params)
			}
		}
		if gwH.relays.Elicit != nil {
			cb.OnElicit = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				session, err := gwH.relays.Elicit.Route(bc.Name)
				if err != nil {
					logger.Warn("elicitation: cannot route to a downstream session, refusing", "backend", bc.Name, "error", err)
					return nil, err
				}
				ectx, cancel := context.WithTimeout(ctx, elicitTimeout)
				defer cancel()
				res, err := session.Elicit(ectx, req.Params)
				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) {
						logger.Warn("elicitation: downstream did not respond within timeout", "backend", bc.Name, "error", err)
					} else {
						logger.Warn("elicitation: downstream request failed", "backend", bc.Name, "error", err)
					}
				}
				return res, err
			}
		}
```

（このブロックの周辺にあるコメント——タイムアウト・プロトコルバージョンに関する説明——はそのまま残す。変更するのは`gwH.progress`/`gwH.elicit`を`gwH.relays.Progress`/`gwH.relays.Elicit`に読み替える箇所だけ。）

- [ ] **Step 4: `go build`でコンパイルエラーを洗い出し、呼び出し箇所を直す**

Run: `go build ./... 2>&1`

`internal/cli/server_internal_test.go`で以下2種類の箇所が壊れる:

1. **`gateway.New(...)`の直接呼び出し**（3箇所）。Task 1のStep 7と同じ要領で、位置引数呼び出しを`gateway.NewConfig{...}`構造体リテラルに書き換える。例:
   - Before: `srv := gateway.New(logger, conn.backends, gateway.Tables{}, gateway.Entries{}, gateway.Overrides{}, nil, nil, nil)`
   - After: `srv := gateway.New(gateway.NewConfig{Logger: logger, Backends: conn.backends})`

2. **`&gwHolder{elicit: gateway.NewElicitationRouter()}`の直接構築**（`TestSuperviseBackend_OnElicit_BoundedByElicitTimeout`内、1箇所）と、それに続く`gwH.elicit.Enter(...)`呼び出し（1箇所）。以下のように書き換える:
   - Before: `gwH := &gwHolder{elicit: gateway.NewElicitationRouter()}`
   - After: `gwH := &gwHolder{relays: gateway.Relays{Elicit: gateway.NewElicitationRouter()}}`
   - Before: `leave := gwH.elicit.Enter("fake", capturedSession)`
   - After: `leave := gwH.relays.Elicit.Enter("fake", capturedSession)`

   このテストのdocコメント内にも`gwH.elicit`という表記が複数箇所現れる（`in gwH.elicit`・`into gwH.elicit`等）——コードの参照先が変わったことに合わせて`gwH.relays.Elicit`に更新する。

`go build ./... && go vet ./...`がエラーなしで終わるまで、上記2種類の書き換えを全箇所に適用する（`rg -n 'gwH\.progress|gwH\.elicit|gateway\.New\(' internal/cli/server_internal_test.go`で書き換え漏れがないか確認できる）。

Expected: `go build ./... && go vet ./...`が全モジュールでエラーなく終わる。

- [ ] **Step 5: モジュール全体のテストを実行する**

Run: `go test ./... -race`
Expected: 全パッケージ`ok`。特に`internal/cli`のprogress中継・elicitation中継それぞれのe2eテスト（`TestServerCommand_RelaysToolCallProgress`・`TestServerCommand_RoutesElicitationToDownstreamClient`）と、Step 4で書き換えた`TestSuperviseBackend_OnElicit_BoundedByElicitTimeout`が全てPASSすること。

- [ ] **Step 6: コミット**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go
git commit -m "refactor(cli): fold gwHolder's progress/elicit fields into gateway.Relays"
```

---

## Self-Review メモ（実行者向けではなく記録用）

- Spec coverage: specの「含める」項目（`Relays`/`NewConfig`新設、`New`/`registerTool`/`callHandler`/`Server`/`reconcile.go`/`gwHolder`の変更、全呼び出し箇所の書き換え）は全てTask 1・Task 2でカバー。「含めない」項目（`registerResource`等の非対象化、`maskKeys`を`Relays`に含めない、README更新、将来のコードベース全体レビュー）はどのタスクでも触れない。
- 型整合性: `Relays{Progress, Elicit}`・`NewConfig{...}`のフィールド名・型はTask 1で定義した通り、Task 2の`buildGateway`・`superviseBackend`で一貫して使われている。
- 振る舞い不変の確認手段: 新規テストを一切追加せず、既存テストスイートの全PASSのみで検証する設計になっている——このリファクタの性質（純粋なシグネチャ変更）に合致している。
