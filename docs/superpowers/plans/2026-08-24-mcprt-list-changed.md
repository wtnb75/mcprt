# mcprt: list_changed 動的リレー Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** backendが送る `notifications/{tools,resources,prompts}/list_changed` を受け取り、該当backendだけ再Listして、mcprtが公開する集約ルーティングテーブルを再計算・再登録し、downstreamへ変更を伝播する。

**Architecture:** `internal/backend.Connect` に `ChangeCallbacks`（backend単位の通知コールバック）を追加配線し、`internal/gateway` の `New` が返す型を素の `*mcp.Server` から reconcile 状態（backendごとのEntry/現在のTable/overrides + mutex）を保持する `*gateway.Server` に変更、`UpdateTools`/`UpdateResources`/`UpdatePrompts` で「再Resolve→新旧Table差分をAdd/Remove」を行う。`internal/cli/server.go` はbackend接続時に `*gateway.Server` への遅延参照（`gwHolder`、`atomic.Pointer` 経由）を閉じ込めたコールバックを配線し、通知受信→再List→`Update*`呼び出しをつなぐ。

**Tech Stack:** Go, `github.com/modelcontextprotocol/go-sdk` v1.7.0 (`mcp.ClientOptions.{Tool,Prompt,Resource}ListChangedHandler` および `mcp.Server.{Add,Remove}{Tool,Resource,ResourceTemplate,Prompt}` を使用)。テストは標準 `testing` + 既存の `httptest`/fakeサーバーパターン。

**Spec:** `docs/superpowers/specs/2026-08-21-mcprt-list-changed-design.md`

## Global Constraints

- 対象SDKは `github.com/modelcontextprotocol/go-sdk@v1.7.0`固定（新規依存追加なし）。
- スコープ外（実装しない）: `resources/subscribe`/`notifications/resources/updated`、backendへの自動再接続・切断検知、configのホットリロード、`mcprt list`コマンドへの対応、通知のデバウンス／コーリアレス。
- 再List失敗時は直前の既知の一覧を保持し、downstreamには何も伝播しない（`logger.Warn`のみ）。
- conflict（新規発生分のみ）は起動時と同じ形で`logger.Warn`。解消時はログしない。
- 検証コマンドは `task test`（`go test -cover ./...`）と `task lint`（`gofmt -l .` / `go vet ./...` / `golangci-lint run ./...`）。データレース検証が要る箇所は個別に `go test -race ./internal/gateway/...` を使う。
- ソースコードのコメント・ログメッセージは英語（既存コードの慣習と一致させる）。

---

## Task 1: `internal/backend` — ChangeCallbacks とConnectへの配線

**Files:**
- Modify: `internal/backend/backend.go`
- Test: `internal/backend/backend_test.go`
- Modify (compile-fix only, see Step 5): `internal/cli/server.go`, `internal/cli/ping.go`, `internal/gateway/gateway_test.go`

**Interfaces:**
- Produces: `backend.ChangeCallbacks{ OnToolsChanged, OnPromptsChanged, OnResourcesChanged func() }`、`backend.Connect(ctx, cfg, cb ChangeCallbacks) (*Backend, error)`（第3引数を追加した新シグネチャ）。

- [ ] **Step 1: 失敗するテストを書く**

`internal/backend/backend_test.go` の末尾に追記する（`newFakeMCPHandler`のすぐ後、`TestConnect_HTTPWithHeaders`の前あたりでよい）:

```go
// TestConnect_ToolListChangedCallback checks that ChangeCallbacks.OnToolsChanged
// fires when the connected backend sends notifications/tools/list_changed
// after the initial connection is established.
func TestConnect_ToolListChangedCallback(t *testing.T) {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	mcp.AddTool(fakeServer, &mcp.Tool{Name: "a", Description: "a"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})

	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil))
	defer srv.Close()

	ctx := context.Background()
	fired := make(chan struct{}, 1)
	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL},
		backend.ChangeCallbacks{OnToolsChanged: func() { fired <- struct{}{} }})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	// Registering a second tool on the already-connected fake server makes
	// the SDK emit notifications/tools/list_changed to b's session.
	mcp.AddTool(fakeServer, &mcp.Tool{Name: "b", Description: "b"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("OnToolsChanged did not fire within 5s of the backend adding a tool")
	}
}

// TestConnect_ResourceListChangedCallback_FiresOnceForOneNotification checks
// that OnResourcesChanged fires exactly once for a single
// notifications/resources/list_changed (which the MCP spec fires for both
// resources AND resource templates -- there is no separate template
// notification, so this same handler covers both).
func TestConnect_ResourceListChangedCallback_FiresOnceForOneNotification(t *testing.T) {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	readHandler := func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{Text: "stub"}}}, nil
	}
	fakeServer.AddResource(&mcp.Resource{URI: "file:///a", Name: "a"}, readHandler)

	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil))
	defer srv.Close()

	ctx := context.Background()
	var fireCount atomic.Int32
	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL},
		backend.ChangeCallbacks{OnResourcesChanged: func() { fireCount.Add(1) }})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	fakeServer.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "file:///dir/{f}", Name: "dir"}, readHandler)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && fireCount.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if fireCount.Load() != 1 {
		t.Fatalf("OnResourcesChanged fired %d times, want exactly 1", fireCount.Load())
	}
}

// TestConnect_PromptListChangedCallback mirrors
// TestConnect_ToolListChangedCallback for OnPromptsChanged.
func TestConnect_PromptListChangedCallback(t *testing.T) {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	fakeServer.AddPrompt(&mcp.Prompt{Name: "greet", Description: "say hello"},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{}}, nil
		})

	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil))
	defer srv.Close()

	ctx := context.Background()
	fired := make(chan struct{}, 1)
	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL},
		backend.ChangeCallbacks{OnPromptsChanged: func() { fired <- struct{}{} }})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	fakeServer.AddPrompt(&mcp.Prompt{Name: "farewell", Description: "say bye"},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{}}, nil
		})

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("OnPromptsChanged did not fire within 5s of the backend adding a prompt")
	}
}

// TestConnect_NilChangeCallbacks_NoHandlersRegistered checks that a nil
// field on ChangeCallbacks leaves the corresponding SDK handler unset
// (rather than, say, panicking or wiring a no-op that still advertises
// interest) -- Connect with a zero-value ChangeCallbacks{} must keep working
// exactly like the pre-list_changed Connect(ctx, cfg) did.
func TestConnect_NilChangeCallbacks_NoHandlersRegistered(t *testing.T) {
	b, err := backend.Connect(context.Background(),
		config.BackendConfig{Name: "fake", Transport: "http", URL: httptest.NewServer(newFakeMCPHandler()).URL},
		backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
}
```

`backend_test.go` の import に `"sync/atomic"` を追加する（`sync/atomic` は現状未importなので）。

- [ ] **Step 2: テストが失敗する（コンパイルエラーになる）ことを確認する**

Run: `go test ./internal/backend/... -run TestConnect_ToolListChangedCallback -v`
Expected: FAIL — `backend.ChangeCallbacks` が存在せず、`backend.Connect` が3引数を受け付けないためコンパイルエラーになる。

- [ ] **Step 3: `ChangeCallbacks`型とConnectへの配線を実装する**

`internal/backend/backend.go` の `Backend` struct 定義の直後に追加:

```go
// ChangeCallbacks are invoked when a connected backend reports that its
// tool/prompt/resource list has changed. Each func takes no arguments: MCP's
// list_changed notifications carry no payload, they only signal "go
// re-list." A nil field means "not interested" and leaves the corresponding
// SDK handler unset.
type ChangeCallbacks struct {
	OnToolsChanged     func()
	OnPromptsChanged   func()
	OnResourcesChanged func() // fires for notifications/resources/list_changed, which covers BOTH resources and resource templates per the MCP spec -- there is no separate resource-template notification
}
```

`Connect`のシグネチャと最初の2行を置き換える:

```go
// Connect starts (for stdio) or dials (for http) the backend described by
// cfg, and performs the MCP initialize handshake. cb's non-nil fields are
// wired as the corresponding notification handlers on the client, so the
// caller finds out when the backend's tool/prompt/resource list changes.
func Connect(ctx context.Context, cfg config.BackendConfig, cb ChangeCallbacks) (*Backend, error) {
	clientOpts := &mcp.ClientOptions{}
	if cb.OnToolsChanged != nil {
		clientOpts.ToolListChangedHandler = func(context.Context, *mcp.ToolListChangedRequest) { cb.OnToolsChanged() }
	}
	if cb.OnPromptsChanged != nil {
		clientOpts.PromptListChangedHandler = func(context.Context, *mcp.PromptListChangedRequest) { cb.OnPromptsChanged() }
	}
	if cb.OnResourcesChanged != nil {
		clientOpts.ResourceListChangedHandler = func(context.Context, *mcp.ResourceListChangedRequest) { cb.OnResourcesChanged() }
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "mcprt", Version: "v1"}, clientOpts)
```

（この後に続く `var transport mcp.Transport` 以下は変更なし。）

- [ ] **Step 4: テストを再実行し、この4つが通ることを確認する**

Run: `go test ./internal/backend/... -run 'TestConnect_(ToolListChangedCallback|ResourceListChangedCallback_FiresOnceForOneNotification|PromptListChangedCallback|NilChangeCallbacks_NoHandlersRegistered)' -v`
Expected: PASS（4つとも）

- [ ] **Step 5: 残り全ての呼び出し元をコンパイルが通るまで修正する**

`Connect`のシグネチャ変更で、以下がコンパイルエラーになる:
- `internal/cli/server.go:236` の `b, err := backend.Connect(ctx, bc)`
- `internal/cli/ping.go:61` の `b, err := backend.Connect(ctx, bc)`
- `internal/backend/backend_test.go` の既存の呼び出し全部（Step 1で追加した新規テスト以外）
- `internal/gateway/gateway_test.go` の既存の呼び出し全部（12箇所）

この段階では**すべて `backend.ChangeCallbacks{}`（ゼロ値）を第3引数として追加するだけ**でよい（本物のコールバック配線はTask 5で行う）。

Run: `go build ./... 2>&1 | head -50` を実行し、`not enough arguments in call to backend.Connect` のエラーが出た箇所を1つずつ開き、呼び出しの閉じ括弧の直前に `, backend.ChangeCallbacks{}` を追加する。複数行にまたがる `config.BackendConfig{...}` リテラルの場合は、リテラルを閉じる `})` の行を `}, backend.ChangeCallbacks{})` に変える。1行で完結している呼び出し（例: `backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL})`）も同様に末尾へ追加する。

`go build ./...` がエラーなく通るまで繰り返す。

- [ ] **Step 6: 全テストを実行して確認する**

Run: `go test ./... -run . 2>&1 | tail -30` （もしくは `task test`）
Expected: PASS（`internal/gateway`はTask 2でさらに変更するため、この時点でもgatewayパッケージのテストは全て通っているはず — Connectの呼び出し引数を1つ増やしただけで意味的な変更はない）

- [ ] **Step 7: lintを実行する**

Run: `task lint`
Expected: エラーなし

- [ ] **Step 8: コミット**

```bash
git add internal/backend/backend.go internal/backend/backend_test.go internal/cli/server.go internal/cli/ping.go internal/gateway/gateway_test.go
git commit -m "feat(backend): wire list_changed notification callbacks into Connect"
```

---

## Task 2: `internal/gateway` — reconcile状態を持つ`Server`型への変更

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/gateway_test.go`（15箇所の`gateway.New(...)`呼び出しと14箇所の`return srv }`）
- Modify: `internal/cli/server.go`（`gateway.New`の呼び出しと`ServeStdio`/`ServeHTTP`の呼び出し）

**Interfaces:**
- Consumes: なし（Task 1の`backend.ChangeCallbacks`はこのタスクでは使わない）
- Produces:
  - `gateway.Entries{ Tools []router.Entry[*mcp.Tool], Resources []router.Entry[*mcp.Resource], ResourceTemplates []router.Entry[*mcp.ResourceTemplate], Prompts []router.Entry[*mcp.Prompt] }`
  - `gateway.Overrides{ Tools, Resources, ResourceTemplates, Prompts map[string]string }`
  - `gateway.New(logger *slog.Logger, backends map[string]*backend.Backend, tables Tables, entries Entries, overrides Overrides, maskKeys []string) *gateway.Server`（戻り値の型が`*mcp.Server`から`*gateway.Server`に変更）
  - `(*gateway.Server).MCP() *mcp.Server`
  - `(*gateway.Server).Backend(name string) *backend.Backend`
  - Task 3/4が`UpdateTools`/`UpdateResources`/`UpdatePrompts`を追加する際に使う内部フィールド: `mu sync.Mutex`、`toolEntries`/`toolTable`/`toolOverrides`、`resourceEntries`/`resourceTable`/`resourceOverrides`、`resourceTemplateEntries`/`resourceTemplateTable`/`resourceTemplateOverrides`、`promptEntries`/`promptTable`/`promptOverrides`、`logger`、`backends`、`maskKeys`

このタスクは新しい振る舞いを追加しない純粋なリファクタリングなので、TDDのRed/Greenではなく「変更後に既存テストが全て通ることを確認する」進め方を取る。

- [ ] **Step 1: `gateway.go`に`Entries`・`Overrides`・`Server`型を追加し、`New`を書き換える**

`internal/gateway/gateway.go`の`Tables`型定義の直後（`New`関数の直前）に追加:

```go
// Entries bundles the raw, pre-resolution per-backend item lists Server
// needs in order to re-run router.Resolve when a backend's list changes
// (see Server.UpdateTools/UpdateResources/UpdatePrompts). It must be built
// from the same connected backend set as tables passed to New.
type Entries struct {
	Tools             []router.Entry[*mcp.Tool]
	Resources         []router.Entry[*mcp.Resource]
	ResourceTemplates []router.Entry[*mcp.ResourceTemplate]
	Prompts           []router.Entry[*mcp.Prompt]
}

// Overrides bundles the exposed-name -> winning-backend overrides for each
// category, as loaded from config.Config. router.Resolve takes overrides as
// an explicit argument rather than storing it, so Server retains these to
// re-supply on every reconcile.
type Overrides struct {
	Tools             map[string]string
	Resources         map[string]string
	ResourceTemplates map[string]string
	Prompts           map[string]string
}

// Server wraps an *mcp.Server with the reconcile state needed to react to a
// backend's list_changed notification: its per-backend raw item lists, the
// currently-registered routing table, and the exposed-name overrides -- all
// four independently for tools/resources/resource templates/prompts. mu
// protects all eight of those fields together; the protected section is
// always in-memory work (router.Resolve plus the SDK's Add/Remove calls),
// never backend I/O, so one mutex is enough.
type Server struct {
	mcp      *mcp.Server
	logger   *slog.Logger
	backends map[string]*backend.Backend
	maskKeys []string

	mu sync.Mutex

	toolEntries   []router.Entry[*mcp.Tool]
	toolTable     *router.Table[*mcp.Tool]
	toolOverrides map[string]string

	resourceEntries           []router.Entry[*mcp.Resource]
	resourceTable             *router.Table[*mcp.Resource]
	resourceOverrides         map[string]string
	resourceTemplateEntries   []router.Entry[*mcp.ResourceTemplate]
	resourceTemplateTable     *router.Table[*mcp.ResourceTemplate]
	resourceTemplateOverrides map[string]string

	promptEntries   []router.Entry[*mcp.Prompt]
	promptTable     *router.Table[*mcp.Prompt]
	promptOverrides map[string]string
}

// MCP returns the underlying *mcp.Server, for ServeStdio/ServeHTTP.
func (s *Server) MCP() *mcp.Server { return s.mcp }

// Backend looks up a connected backend by name, for the cli layer's
// list_changed callbacks (see internal/cli/server.go) to re-list from
// without keeping their own separate reference.
func (s *Server) Backend(name string) *backend.Backend { return s.backends[name] }

// emptyTable returns t, or a fresh empty table if t is nil -- New is called
// with tables.X == nil when a category has no items anywhere (see New's
// existing nil checks below), and Update* assumes toolTable etc are never
// nil so its diff loops don't need their own nil guards.
func emptyTable[T any](t *router.Table[T]) *router.Table[T] {
	if t != nil {
		return t
	}
	return &router.Table[T]{}
}
```

`New`関数を以下に置き換える（シグネチャと本体の両方が変わる）:

```go
// New builds a Server that exposes tables' resolved tools/resources/prompts,
// forwarding each call to the backend that owns it, and retains entries and
// overrides so a later UpdateTools/UpdateResources/UpdatePrompts call can
// re-run router.Resolve when a backend reports its list has changed.
// backends must contain an entry for every BackendName referenced in
// tables (the caller builds both from the same set of connected backends).
func New(logger *slog.Logger, backends map[string]*backend.Backend, tables Tables, entries Entries, overrides Overrides, maskKeys []string) *Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{Logger: logger})

	s := &Server{
		mcp:      mcpSrv,
		logger:   logger,
		backends: backends,
		maskKeys: maskKeys,

		toolEntries:   entries.Tools,
		toolTable:     emptyTable(tables.Tools),
		toolOverrides: overrides.Tools,

		resourceEntries:           entries.Resources,
		resourceTable:             emptyTable(tables.Resources),
		resourceOverrides:         overrides.Resources,
		resourceTemplateEntries:   entries.ResourceTemplates,
		resourceTemplateTable:     emptyTable(tables.ResourceTemplates),
		resourceTemplateOverrides: overrides.ResourceTemplates,

		promptEntries:   entries.Prompts,
		promptTable:     emptyTable(tables.Prompts),
		promptOverrides: overrides.Prompts,
	}

	if tables.Tools != nil {
		for _, resolved := range tables.Tools.Items {
			registerTool(mcpSrv, logger, backends, resolved, maskKeys)
		}
	}
	if tables.Resources != nil {
		for _, resolved := range tables.Resources.Items {
			registerResource(mcpSrv, logger, backends, resolved, maskKeys)
		}
	}
	if tables.ResourceTemplates != nil {
		for _, resolved := range tables.ResourceTemplates.Items {
			registerResourceTemplate(mcpSrv, logger, backends, resolved, maskKeys)
		}
	}
	if tables.Prompts != nil {
		for _, resolved := range tables.Prompts.Items {
			registerPrompt(mcpSrv, logger, backends, resolved, maskKeys)
		}
	}

	return s
}
```

`internal/gateway/gateway.go`の`import`ブロックに`"sync"`を追加する（現状未import）。

- [ ] **Step 2: `gateway_test.go`の全呼び出しを機械的に更新する**

`internal/gateway/gateway_test.go`で、`gateway.Tables{...}, `という文字列で終わる15箇所すべてに`gateway.Entries{}, gateway.Overrides{}, `を挟み込む。`sd`で一括置換する:

```bash
sd 'gateway\.Tables\{([^}]*)\}, ' 'gateway.Tables{$1}, gateway.Entries{}, gateway.Overrides{}, ' internal/gateway/gateway_test.go
```

続けて、`New`の戻り値を`*mcp.Server`として直接返している14箇所（`func(*http.Request) *mcp.Server { return srv }`）を`.MCP()`経由に変える:

```bash
sd 'return srv \}' 'return srv.MCP() }' internal/gateway/gateway_test.go
```

`rg -n "gateway\.New\(|return srv" internal/gateway/gateway_test.go`で置換結果を目視確認する（`gateway.Entries{}, gateway.Overrides{},`が全15箇所に、`return srv.MCP()`が全14箇所に入っていること）。

- [ ] **Step 3: `internal/cli/server.go`を最小限だけ追従させる**

このタスクではまだ本物のreconcile配線はしない（Task 5で行う）。`runServer`内の`gateway.New`呼び出し（71行目付近）を、空の`Entries{}`/`Overrides{}`を渡す形に変える:

```go
	srv := gateway.New(logger, conn.backends, gateway.Tables{
		Tools:             toolTable,
		Resources:         resourceTable,
		ResourceTemplates: resourceTemplateTable,
		Prompts:           promptTable,
	}, gateway.Entries{}, gateway.Overrides{}, cfg.Logging.MaskKeys)
```

続けて、`ServeStdio`/`ServeHTTP`の呼び出しを`.MCP()`経由に変える:

```go
	if cfg.Listen.Stdio {
		running++
		go func() { errCh <- gateway.ServeStdio(ctx, srv.MCP()) }()
	}
	if cfg.Listen.HTTP != "" {
		running++
		go func() { errCh <- gateway.ServeHTTP(ctx, srv.MCP(), cfg.Listen.HTTP) }()
	}
```

- [ ] **Step 4: ビルドとテストを確認する**

Run: `go build ./... 2>&1`
Expected: エラーなし

Run: `task test`
Expected: PASS（全パッケージ）— 振る舞いは変えていないので、既存テストは無修正で全て通るはず。

- [ ] **Step 5: lintを実行する**

Run: `task lint`
Expected: エラーなし

- [ ] **Step 6: コミット**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go internal/cli/server.go
git commit -m "refactor(gateway): New returns a *Server carrying reconcile state"
```

---

## Task 3: `internal/gateway` — `UpdateTools`によるreconcile

**Files:**
- Create: `internal/gateway/reconcile.go`
- Create: `internal/gateway/reconcile_test.go`

**Interfaces:**
- Consumes: Task 2の`gateway.Server`（`toolEntries`/`toolTable`/`toolOverrides`/`mu`/`mcp`/`logger`/`backends`/`maskKeys`フィールドと`registerTool`関数）
- Produces: `(*gateway.Server).UpdateTools(backendName string, items []*mcp.Tool)`。Task 4がこのファイルに`UpdateResources`/`UpdatePrompts`を追記する際に再利用する内部ヘルパー: `toolNameOf`/`toolRename`/`resourceNameOf`/`resourceRename`/`resourceTemplateNameOf`/`resourceTemplateRename`/`promptNameOf`/`promptRename`、`replaceEntry[T any](entries []router.Entry[T], backendName string, items []T) []router.Entry[T]`、`logNewConflicts(logger *slog.Logger, msg, field string, oldConflicts, newConflicts []router.Conflict)`

- [ ] **Step 1: 失敗するテストを書く**

`internal/gateway/reconcile_test.go`を新規作成する:

```go
package gateway_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/gateway"
	"github.com/wtnb75/mcprt/internal/router"
)

// downstreamToolNames connects a fresh test client to srv and returns the
// sorted list of tool names it currently sees, for asserting on the effect
// of an UpdateTools call.
func downstreamToolNames(t *testing.T, ctx context.Context, srv *mcp.Server) []string {
	t.Helper()
	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var names []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestUpdateTools_AddsRemovesAndChangesItems(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "keep", Description: "v1"}, {Name: "gone", Description: "v1"}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"a": {Name: "a"}},
		gateway.Tables{Tools: table}, gateway.Entries{Tools: entries}, gateway.Overrides{}, nil)

	if got := downstreamToolNames(t, ctx, srv.MCP()); !equalStrings(got, []string{"gone", "keep"}) {
		t.Fatalf("initial tools = %v, want [gone keep]", got)
	}

	// "gone" disappears, "keep" changes description, "new" appears.
	srv.UpdateTools("a", []*mcp.Tool{{Name: "keep", Description: "v2"}, {Name: "new", Description: "v1"}})

	if got := downstreamToolNames(t, ctx, srv.MCP()); !equalStrings(got, []string{"keep", "new"}) {
		t.Fatalf("tools after UpdateTools = %v, want [keep new]", got)
	}
}

func TestUpdateTools_ConflictFallbackPromotesWhenWinnerRemoved(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "search", Description: "from a"}}},
		{BackendName: "b", Items: []*mcp.Tool{{Name: "search", Description: "from b"}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	if len(table.Conflicts) != 1 || table.Conflicts[0].Winner != "a" {
		t.Fatalf("initial table.Conflicts = %+v, want one conflict won by \"a\"", table.Conflicts)
	}
	srv := gateway.New(logger, map[string]*backend.Backend{"a": {Name: "a"}, "b": {Name: "b"}},
		gateway.Tables{Tools: table}, gateway.Entries{Tools: entries}, gateway.Overrides{}, nil)

	// backend "a" no longer serves "search": "b"'s definition should take over.
	srv.UpdateTools("a", nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var found *mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		if tool.Name == "search" {
			found = tool
		}
	}
	if found == nil || found.Description != "from b" {
		t.Fatalf("tool \"search\" = %+v, want description \"from b\" (promoted fallback)", found)
	}
}

func TestUpdateTools_LogsOnlyNewConflicts(t *testing.T) {
	var buf logBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "search"}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"a": {Name: "a"}},
		gateway.Tables{Tools: table}, gateway.Entries{Tools: entries}, gateway.Overrides{}, nil)

	buf.reset() // discard whatever New itself may have logged (nothing, in this case, but keep the assertion below scoped to UpdateTools)

	// First reconcile introduces a brand-new conflict: must log.
	srv.UpdateTools("b", []*mcp.Tool{{Name: "search"}})
	if !buf.contains("tool name conflict") {
		t.Fatalf("log output = %q, want a \"tool name conflict\" warning for the newly-introduced conflict", buf.String())
	}
	buf.reset()

	// Second reconcile touches an unrelated tool; the SAME conflict persists
	// but must NOT be re-logged.
	srv.UpdateTools("a", []*mcp.Tool{{Name: "search"}, {Name: "other"}})
	if buf.contains("tool name conflict") {
		t.Fatalf("log output = %q, want no \"tool name conflict\" warning for an already-known conflict", buf.String())
	}
}

func TestUpdateTools_ConcurrentCallsDoNotRace(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "x"}}},
		{BackendName: "b", Items: []*mcp.Tool{{Name: "y"}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"a": {Name: "a"}, "b": {Name: "b"}},
		gateway.Tables{Tools: table}, gateway.Entries{Tools: entries}, gateway.Overrides{}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			srv.UpdateTools("a", []*mcp.Tool{{Name: "x"}, {Name: "extra-a"}})
		}(i)
		go func(i int) {
			defer wg.Done()
			srv.UpdateTools("b", []*mcp.Tool{{Name: "y"}, {Name: "extra-b"}})
		}(i)
	}
	wg.Wait()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

`internal/gateway/gateway_test.go`の末尾（`toolNameOf`/`toolRename`定義のすぐ後）に、この新ファイルからも使う小さなテスト用ロガーバッファを追加する:

```go
// logBuffer is an io.Writer that lets tests both feed a slog.Handler and
// inspect/reset what's been written, without pulling in a mutex-free race
// (slog serializes Handler.Handle calls per-logger already).
type logBuffer struct {
	bytes.Buffer
}

func (b *logBuffer) reset()                  { b.Buffer.Reset() }
func (b *logBuffer) contains(s string) bool  { return bytes.Contains(b.Buffer.Bytes(), []byte(s)) }
```

`gateway_test.go`は既に`"bytes"`をimportしているので追加import不要。

- [ ] **Step 2: テストが失敗する（コンパイルエラーになる）ことを確認する**

Run: `go test ./internal/gateway/... -run TestUpdateTools -v`
Expected: FAIL — `(*gateway.Server).UpdateTools`が存在しないためコンパイルエラー。

- [ ] **Step 3: `reconcile.go`を実装する**

`internal/gateway/reconcile.go`を新規作成する:

```go
package gateway

import (
	"log/slog"
	"reflect"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/router"
)

func toolNameOf(t *mcp.Tool) string { return t.Name }

func toolRename(t *mcp.Tool, name string) *mcp.Tool {
	c := *t
	c.Name = name
	return &c
}

func resourceNameOf(r *mcp.Resource) string { return r.URI }

func resourceRename(r *mcp.Resource, name string) *mcp.Resource {
	c := *r
	c.URI = name
	return &c
}

func resourceTemplateNameOf(t *mcp.ResourceTemplate) string { return t.URITemplate }

func resourceTemplateRename(t *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate {
	c := *t
	c.URITemplate = name
	return &c
}

func promptNameOf(p *mcp.Prompt) string { return p.Name }

func promptRename(p *mcp.Prompt, name string) *mcp.Prompt {
	c := *p
	c.Name = name
	return &c
}

// replaceEntry returns a copy of entries with backendName's Items replaced
// by items. If no entry for backendName exists yet, entries is returned
// unchanged (a list_changed callback only fires for a backend that was
// already connected and entered into entries at startup).
func replaceEntry[T any](entries []router.Entry[T], backendName string, items []T) []router.Entry[T] {
	out := make([]router.Entry[T], len(entries))
	copy(out, entries)
	for i, e := range out {
		if e.BackendName == backendName {
			out[i].Items = items
			break
		}
	}
	return out
}

// logNewConflicts warns about every conflict in newConflicts whose exposed
// name wasn't already conflicting in oldConflicts -- an already-known
// conflict isn't re-logged, and a conflict that disappears isn't logged at
// all, matching startup's one-shot conflict logging.
func logNewConflicts(logger *slog.Logger, msg, field string, oldConflicts, newConflicts []router.Conflict) {
	seen := make(map[string]bool, len(oldConflicts))
	for _, c := range oldConflicts {
		seen[c.ExposedName] = true
	}
	for _, c := range newConflicts {
		if !seen[c.ExposedName] {
			logger.Warn(msg, field, c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
		}
	}
}

// UpdateTools replaces backendName's tool entry with items, re-resolves the
// merged table, and applies the diff (Remove vanished names, Add
// new/changed names) to the underlying *mcp.Server. Called from the cli
// layer when a backend reports notifications/tools/list_changed.
func (s *Server) UpdateTools(backendName string, items []*mcp.Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.toolEntries = replaceEntry(s.toolEntries, backendName, items)
	newTable := router.Resolve(s.toolEntries, toolNameOf, toolRename, s.toolOverrides)

	for name := range s.toolTable.Items {
		if _, ok := newTable.Items[name]; !ok {
			s.mcp.RemoveTools(name)
		}
	}
	for name, resolved := range newTable.Items {
		if old, ok := s.toolTable.Items[name]; !ok || !reflect.DeepEqual(old, resolved) {
			registerTool(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
		}
	}
	logNewConflicts(s.logger, "tool name conflict", "tool", s.toolTable.Conflicts, newTable.Conflicts)

	s.toolTable = newTable
}
```

- [ ] **Step 4: テストを再実行し、通ることを確認する**

Run: `go test ./internal/gateway/... -run TestUpdateTools -v`
Expected: PASS（4つとも）

- [ ] **Step 5: `-race`で確認する**

Run: `go test -race ./internal/gateway/... -run TestUpdateTools_ConcurrentCallsDoNotRace -v`
Expected: PASS、データレースの報告なし

- [ ] **Step 6: 全体テストとlint**

Run: `task test && task lint`
Expected: PASS、エラーなし

- [ ] **Step 7: コミット**

```bash
git add internal/gateway/reconcile.go internal/gateway/reconcile_test.go internal/gateway/gateway_test.go
git commit -m "feat(gateway): add UpdateTools to reconcile a backend's tool list_changed"
```

---

## Task 4: `internal/gateway` — `UpdateResources`と`UpdatePrompts`によるreconcile

**Files:**
- Modify: `internal/gateway/reconcile.go`
- Modify: `internal/gateway/reconcile_test.go`

**Interfaces:**
- Consumes: Task 3の`replaceEntry`/`logNewConflicts`/`resourceNameOf`/`resourceRename`/`resourceTemplateNameOf`/`resourceTemplateRename`/`promptNameOf`/`promptRename`
- Produces: `(*gateway.Server).UpdateResources(backendName string, resources []*mcp.Resource, templates []*mcp.ResourceTemplate)`、`(*gateway.Server).UpdatePrompts(backendName string, items []*mcp.Prompt)`

- [ ] **Step 1: 失敗するテストを書く**

`internal/gateway/reconcile_test.go`に追記する（`equalStrings`の前に挿入してよい）。`gateway_test`パッケージには`toolNameOf`/`toolRename`/`promptNameOf`/`promptRename`は既にパッケージレベルで定義済み（`gateway_test.go`）だが、`resourceNameOf`/`resourceRename`はテスト関数内のローカルクロージャとしてしか存在せず、`resourceTemplateNameOf`/`resourceTemplateRename`はどこにも存在しないので、まずこの4つをパッケージレベル関数として追加する:

```go
func resourceNameOf(r *mcp.Resource) string { return r.URI }

func resourceRename(r *mcp.Resource, name string) *mcp.Resource {
	c := *r
	c.URI = name
	return &c
}

func resourceTemplateNameOf(t *mcp.ResourceTemplate) string { return t.URITemplate }

func resourceTemplateRename(t *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate {
	c := *t
	c.URITemplate = name
	return &c
}

func TestUpdateResources_AddsRemovesResourcesAndTemplatesInOneCall(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	resourceEntries := []router.Entry[*mcp.Resource]{
		{BackendName: "a", Items: []*mcp.Resource{{URI: "file:///keep", Name: "keep"}, {URI: "file:///gone", Name: "gone"}}},
	}
	templateEntries := []router.Entry[*mcp.ResourceTemplate]{
		{BackendName: "a", Items: []*mcp.ResourceTemplate{{URITemplate: "file:///dir/{f}", Name: "dir"}}},
	}
	resourceTable := router.Resolve(resourceEntries, resourceNameOf, resourceRename, nil)
	templateTable := router.Resolve(templateEntries, resourceTemplateNameOf, resourceTemplateRename, nil)

	srv := gateway.New(logger, map[string]*backend.Backend{"a": {Name: "a"}},
		gateway.Tables{Resources: resourceTable, ResourceTemplates: templateTable},
		gateway.Entries{Resources: resourceEntries, ResourceTemplates: templateEntries},
		gateway.Overrides{}, nil)

	srv.UpdateResources("a",
		[]*mcp.Resource{{URI: "file:///keep", Name: "keep"}, {URI: "file:///new", Name: "new"}},
		nil, // templates all removed
	)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var uris []string
	for r, err := range session.Resources(ctx, nil) {
		if err != nil {
			t.Fatalf("listing resources: %v", err)
		}
		uris = append(uris, r.URI)
	}
	sort.Strings(uris)
	if !equalStrings(uris, []string{"file:///keep", "file:///new"}) {
		t.Fatalf("resources after UpdateResources = %v, want [file:///keep file:///new]", uris)
	}

	var templateCount int
	for range session.ResourceTemplates(ctx, nil) {
		templateCount++
	}
	if templateCount != 0 {
		t.Fatalf("resource templates after UpdateResources = %d, want 0", templateCount)
	}
}

func TestUpdatePrompts_AddsRemovesAndChangesItems(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	entries := []router.Entry[*mcp.Prompt]{
		{BackendName: "a", Items: []*mcp.Prompt{{Name: "keep", Description: "v1"}, {Name: "gone", Description: "v1"}}},
	}
	table := router.Resolve(entries, promptNameOf, promptRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"a": {Name: "a"}},
		gateway.Tables{Prompts: table}, gateway.Entries{Prompts: entries}, gateway.Overrides{}, nil)

	srv.UpdatePrompts("a", []*mcp.Prompt{{Name: "keep", Description: "v2"}, {Name: "new", Description: "v1"}})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var names []string
	for p, err := range session.Prompts(ctx, nil) {
		if err != nil {
			t.Fatalf("listing prompts: %v", err)
		}
		names = append(names, p.Name)
	}
	sort.Strings(names)
	if !equalStrings(names, []string{"keep", "new"}) {
		t.Fatalf("prompts after UpdatePrompts = %v, want [keep new]", names)
	}
}

func TestUpdateResourcesAndUpdatePrompts_ConcurrentCallsDoNotRace(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	resourceEntries := []router.Entry[*mcp.Resource]{{BackendName: "a", Items: []*mcp.Resource{{URI: "file:///x", Name: "x"}}}}
	promptEntries := []router.Entry[*mcp.Prompt]{{BackendName: "a", Items: []*mcp.Prompt{{Name: "p"}}}}
	resourceTable := router.Resolve(resourceEntries, resourceNameOf, resourceRename, nil)
	promptTable := router.Resolve(promptEntries, promptNameOf, promptRename, nil)

	srv := gateway.New(logger, map[string]*backend.Backend{"a": {Name: "a"}},
		gateway.Tables{Resources: resourceTable, Prompts: promptTable},
		gateway.Entries{Resources: resourceEntries, Prompts: promptEntries},
		gateway.Overrides{}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			srv.UpdateResources("a", []*mcp.Resource{{URI: "file:///x", Name: "x"}, {URI: "file:///y", Name: "y"}}, nil)
		}()
		go func() {
			defer wg.Done()
			srv.UpdatePrompts("a", []*mcp.Prompt{{Name: "p"}, {Name: "q"}})
		}()
	}
	wg.Wait()
}
```

- [ ] **Step 2: テストが失敗する（コンパイルエラーになる）ことを確認する**

Run: `go test ./internal/gateway/... -run 'TestUpdateResources|TestUpdatePrompts' -v`
Expected: FAIL — `UpdateResources`/`UpdatePrompts`が存在しない。

- [ ] **Step 3: `reconcile.go`に2つのメソッドを追記する**

`internal/gateway/reconcile.go`の末尾に追記する:

```go
// UpdateResources replaces backendName's resource AND resource-template
// entries together (MCP fires one notification, notifications/resources
// /list_changed, for both), re-resolves both tables, and applies both
// diffs under one lock acquisition.
func (s *Server) UpdateResources(backendName string, resources []*mcp.Resource, templates []*mcp.ResourceTemplate) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.resourceEntries = replaceEntry(s.resourceEntries, backendName, resources)
	newResourceTable := router.Resolve(s.resourceEntries, resourceNameOf, resourceRename, s.resourceOverrides)
	for name := range s.resourceTable.Items {
		if _, ok := newResourceTable.Items[name]; !ok {
			s.mcp.RemoveResources(name)
		}
	}
	for name, resolved := range newResourceTable.Items {
		if old, ok := s.resourceTable.Items[name]; !ok || !reflect.DeepEqual(old, resolved) {
			registerResource(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
		}
	}
	logNewConflicts(s.logger, "resource URI conflict", "uri", s.resourceTable.Conflicts, newResourceTable.Conflicts)
	s.resourceTable = newResourceTable

	s.resourceTemplateEntries = replaceEntry(s.resourceTemplateEntries, backendName, templates)
	newTemplateTable := router.Resolve(s.resourceTemplateEntries, resourceTemplateNameOf, resourceTemplateRename, s.resourceTemplateOverrides)
	for name := range s.resourceTemplateTable.Items {
		if _, ok := newTemplateTable.Items[name]; !ok {
			s.mcp.RemoveResourceTemplates(name)
		}
	}
	for name, resolved := range newTemplateTable.Items {
		if old, ok := s.resourceTemplateTable.Items[name]; !ok || !reflect.DeepEqual(old, resolved) {
			registerResourceTemplate(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
		}
	}
	logNewConflicts(s.logger, "resource template URI conflict", "uriTemplate", s.resourceTemplateTable.Conflicts, newTemplateTable.Conflicts)
	s.resourceTemplateTable = newTemplateTable
}

// UpdatePrompts replaces backendName's prompt entry with items, re-resolves
// the merged table, and applies the diff to the underlying *mcp.Server.
func (s *Server) UpdatePrompts(backendName string, items []*mcp.Prompt) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.promptEntries = replaceEntry(s.promptEntries, backendName, items)
	newTable := router.Resolve(s.promptEntries, promptNameOf, promptRename, s.promptOverrides)

	for name := range s.promptTable.Items {
		if _, ok := newTable.Items[name]; !ok {
			s.mcp.RemovePrompts(name)
		}
	}
	for name, resolved := range newTable.Items {
		if old, ok := s.promptTable.Items[name]; !ok || !reflect.DeepEqual(old, resolved) {
			registerPrompt(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
		}
	}
	logNewConflicts(s.logger, "prompt name conflict", "prompt", s.promptTable.Conflicts, newTable.Conflicts)

	s.promptTable = newTable
}
```

- [ ] **Step 4: テストを再実行し、通ることを確認する**

Run: `go test ./internal/gateway/... -run 'TestUpdateResources|TestUpdatePrompts' -v`
Expected: PASS（全て）

- [ ] **Step 5: `-race`で全`gateway`パッケージを確認する**

Run: `go test -race ./internal/gateway/...`
Expected: PASS、データレースの報告なし

- [ ] **Step 6: 全体テストとlint**

Run: `task test && task lint`
Expected: PASS、エラーなし

- [ ] **Step 7: コミット**

```bash
git add internal/gateway/reconcile.go internal/gateway/reconcile_test.go
git commit -m "feat(gateway): add UpdateResources and UpdatePrompts to reconcile list_changed"
```

---

## Task 5: `internal/cli` — backend通知からreconcileへの配線

**Files:**
- Modify: `internal/cli/server.go`
- Modify: `internal/cli/call.go`、`internal/cli/list.go`（`connectBackends`呼び出しに引数追加）
- Modify: `internal/cli/server_internal_test.go`（`connectBackends`呼び出し4箇所に引数追加）

**Interfaces:**
- Consumes: Task 1の`backend.ChangeCallbacks`/`backend.Connect`、Task 2の`gateway.Entries`/`gateway.Overrides`/`(*gateway.Server).Backend`、Task 3/4の`UpdateTools`/`UpdateResources`/`UpdatePrompts`
- Produces: `connectBackends(ctx, logger, configs, gwH *gwHolder) connected`（シグネチャに`*gwHolder`を追加。`nil`可）

- [ ] **Step 1: `gwHolder`型とコールバック生成ヘルパーを追加する**

`internal/cli/server.go`の`connected`型定義の直前に追加する:

```go
// gwHolder lets a backend's ChangeCallbacks closures (built inside
// connectBackends, before the *gateway.Server exists) reference it once
// runServer finishes building it. A nil Load() means the initial
// connect-and-list sequence gateway.New's caller runs hasn't completed yet;
// a notification that fires in that window is dropped -- the pending
// initial ListTools/ListResources/ListResourceTemplates/ListPrompts that
// runServer is about to do anyway will reflect the same change, so nothing
// is permanently lost.
type gwHolder struct {
	ptr atomic.Pointer[gateway.Server]
}

// toolsChangedCallback returns a func to use as backend.ChangeCallbacks.
// OnToolsChanged for backendName: on fire, it re-lists that backend's tools
// (bounded by backendConnectTimeout) and reconciles gwH's Server, or logs a
// warning and keeps the previous list if either isn't ready yet.
func toolsChangedCallback(ctx context.Context, logger *slog.Logger, backendName string, gwH *gwHolder) func() {
	return func() {
		gw := gwH.ptr.Load()
		if gw == nil {
			return
		}
		b := gw.Backend(backendName)
		if b == nil {
			return
		}
		lctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
		defer cancel()
		tools, err := b.ListTools(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "tools", "error", err)
			return
		}
		gw.UpdateTools(backendName, tools)
	}
}

// resourcesChangedCallback is toolsChangedCallback's counterpart for
// notifications/resources/list_changed, which the MCP spec fires for BOTH
// resources and resource templates -- it re-lists both and reconciles them
// together via a single UpdateResources call.
func resourcesChangedCallback(ctx context.Context, logger *slog.Logger, backendName string, gwH *gwHolder) func() {
	return func() {
		gw := gwH.ptr.Load()
		if gw == nil {
			return
		}
		b := gw.Backend(backendName)
		if b == nil {
			return
		}
		lctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
		defer cancel()
		resources, err := b.ListResources(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "resources", "error", err)
			return
		}
		templates, err := b.ListResourceTemplates(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "resource templates", "error", err)
			return
		}
		gw.UpdateResources(backendName, resources, templates)
	}
}

// promptsChangedCallback is toolsChangedCallback's counterpart for
// notifications/prompts/list_changed.
func promptsChangedCallback(ctx context.Context, logger *slog.Logger, backendName string, gwH *gwHolder) func() {
	return func() {
		gw := gwH.ptr.Load()
		if gw == nil {
			return
		}
		b := gw.Backend(backendName)
		if b == nil {
			return
		}
		lctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
		defer cancel()
		prompts, err := b.ListPrompts(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "prompts", "error", err)
			return
		}
		gw.UpdatePrompts(backendName, prompts)
	}
}
```

`internal/cli/server.go`のimportブロックに`"sync/atomic"`を追加する。

- [ ] **Step 2: `connectBackends`に`gwH *gwHolder`パラメータを追加し、`backend.Connect`へ渡すコールバックを配線する**

`connectBackends`のシグネチャと、ゴルーチン起動部分を書き換える:

```go
func connectBackends(ctx context.Context, logger *slog.Logger, configs []config.BackendConfig, gwH *gwHolder) connected {
	type outcome struct {
		backend               *backend.Backend
		toolEntry             router.Entry[*mcp.Tool]
		resourceEntry         router.Entry[*mcp.Resource]
		resourceTemplateEntry router.Entry[*mcp.ResourceTemplate]
		promptEntry           router.Entry[*mcp.Prompt]
	}
	outcomes := make([]*outcome, len(configs))

	var wg sync.WaitGroup
	for i, bc := range configs {
		wg.Add(1)
		// Callbacks are built here, using ctx (this func's own long-lived
		// context), NOT the timeout-bounded ctx the goroutine below derives
		// for the initial Connect+List sequence -- a callback captured from
		// that derived context would already be expired by the time a real
		// list_changed notification could plausibly arrive.
		cb := backend.ChangeCallbacks{}
		if gwH != nil {
			cb = backend.ChangeCallbacks{
				OnToolsChanged:     toolsChangedCallback(ctx, logger, bc.Name, gwH),
				OnResourcesChanged: resourcesChangedCallback(ctx, logger, bc.Name, gwH),
				OnPromptsChanged:   promptsChangedCallback(ctx, logger, bc.Name, gwH),
			}
		}
		go func(i int, bc config.BackendConfig, cb backend.ChangeCallbacks) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
			defer cancel()

			b, err := backend.Connect(ctx, bc, cb)
			if err != nil {
				logger.Error("skipping backend: connect failed", "backend", bc.Name, "error", err)
				return
			}
			logger.Info("backend connected", "backend", bc.Name, "transport", bc.Transport)
			tools, err := b.ListTools(ctx)
			if err != nil {
				logger.Error("skipping backend: list tools failed", "backend", bc.Name, "error", err)
				_ = b.Close()
				return
			}
			resources, err := b.ListResources(ctx)
			if err != nil {
				logger.Warn("backend lists no resources", "backend", bc.Name, "error", err)
				resources = nil
			}
			resourceTemplates, err := b.ListResourceTemplates(ctx)
			if err != nil {
				logger.Warn("backend lists no resource templates", "backend", bc.Name, "error", err)
				resourceTemplates = nil
			}
			prompts, err := b.ListPrompts(ctx)
			if err != nil {
				logger.Warn("backend lists no prompts", "backend", bc.Name, "error", err)
				prompts = nil
			}
			outcomes[i] = &outcome{
				backend: b,
				toolEntry: router.Entry[*mcp.Tool]{
					BackendName: bc.Name, Prefix: bc.Prefix, Items: tools,
				},
				resourceEntry: router.Entry[*mcp.Resource]{
					BackendName: bc.Name, Items: resources,
				},
				resourceTemplateEntry: router.Entry[*mcp.ResourceTemplate]{
					BackendName: bc.Name, Items: resourceTemplates,
				},
				promptEntry: router.Entry[*mcp.Prompt]{
					BackendName: bc.Name, Prefix: bc.Prefix, Items: prompts,
				},
			}
		}(i, bc, cb)
	}
	wg.Wait()

	result := connected{backends: make(map[string]*backend.Backend, len(configs))}
	for _, o := range outcomes {
		if o == nil {
			continue
		}
		result.backends[o.toolEntry.BackendName] = o.backend
		result.toolEntries = append(result.toolEntries, o.toolEntry)
		result.resourceEntries = append(result.resourceEntries, o.resourceEntry)
		result.resourceTemplateEntries = append(result.resourceTemplateEntries, o.resourceTemplateEntry)
		result.promptEntries = append(result.promptEntries, o.promptEntry)
	}
	return result
}
```

（変更点は関数シグネチャ、`cb`の構築、`go func(...)`に`cb`を渡す3箇所のみ。ループ本体のList呼び出し以降は元のまま。）

- [ ] **Step 3: `runServer`で`gwHolder`を組み立て、`gateway.New`に本物の`Entries`/`Overrides`を渡す**

`runServer`内、`conn := connectBackends(ctx, logger, cfg.Backends)`の行を置き換える:

```go
	var gwH gwHolder
	conn := connectBackends(ctx, logger, cfg.Backends, &gwH)
```

`srv := gateway.New(...)`の呼び出し（Task 2で`gateway.Entries{}, gateway.Overrides{}`という空値にしていた箇所）を、本物の値に置き換える:

```go
	srv := gateway.New(logger, conn.backends, gateway.Tables{
		Tools:             toolTable,
		Resources:         resourceTable,
		ResourceTemplates: resourceTemplateTable,
		Prompts:           promptTable,
	}, gateway.Entries{
		Tools:             conn.toolEntries,
		Resources:         conn.resourceEntries,
		ResourceTemplates: conn.resourceTemplateEntries,
		Prompts:           conn.promptEntries,
	}, gateway.Overrides{
		Tools:             cfg.Overrides,
		Resources:         cfg.ResourceOverrides,
		ResourceTemplates: cfg.ResourceTemplateOverrides,
		Prompts:           cfg.PromptOverrides,
	}, cfg.Logging.MaskKeys)
	gwH.ptr.Store(srv)
```

（`gwH.ptr.Store(srv)`は`srv`を作った直後、`logger.Info("listening", ...)`より前に置く。）

- [ ] **Step 4: `connectBackends`の他の呼び出し元を修正する**

`internal/cli/call.go`の`conn := connectBackends(ctx, logger, cfg.Backends)`を`conn := connectBackends(ctx, logger, cfg.Backends, nil)`に変える。

`internal/cli/list.go`の`conn := connectBackends(ctx, logger, cfg.Backends)`を`conn := connectBackends(ctx, logger, cfg.Backends, nil)`に変える。

`internal/cli/server_internal_test.go`の4箇所（88, 171, 212, 243行目付近、いずれも`connectBackends(context.Background(), logger, configs)`という形）を、それぞれ`connectBackends(context.Background(), logger, configs, nil)`に変える。

- [ ] **Step 5: ビルドを確認する**

Run: `go build ./... 2>&1`
Expected: エラーなし

- [ ] **Step 6: 既存テストが全て通ることを確認する**

Run: `task test`
Expected: PASS（全パッケージ。既存の振る舞いは変えていない — `gwH`が`nil`でない場合でも、初回接続シーケンスが終わるまで`gwH.ptr`は`nil`のままなので、起動直後に届く通知は単に無視される）

- [ ] **Step 7: `-race`で`internal/cli`を確認する**

Run: `go test -race ./internal/cli/...`
Expected: PASS、データレースの報告なし

- [ ] **Step 8: lintを実行する**

Run: `task lint`
Expected: エラーなし

- [ ] **Step 9: コミット**

```bash
git add internal/cli/server.go internal/cli/call.go internal/cli/list.go internal/cli/server_internal_test.go
git commit -m "feat(cli): wire backend list_changed notifications to gateway reconcile"
```

---

## Task 6: `internal/cli` — list_changed のend-to-end統合テスト

**Files:**
- Modify: `internal/cli/server_test.go`

**Interfaces:**
- Consumes: Task 5で完成した`runServer`のフルパイプライン（`cli.Execute(ctx, []string{"server", "--config", configPath})`経由）

- [ ] **Step 1: tools/list_changedが実際に伝播することを確認するテストを書く**

`internal/cli/server_test.go`の末尾に追記する:

```go
// waitForToolNames polls session.Tools until it returns exactly want (order-
// independent), or fails the test after 5s. list_changed propagation is
// asynchronous (backend notification -> re-list -> gateway.Update* -> SDK's
// own downstream notification), so tests must poll rather than assert
// immediately after triggering a change.
func waitForToolNames(t *testing.T, ctx context.Context, session *mcp.ClientSession, want []string) {
	t.Helper()
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)

	deadline := time.Now().Add(5 * time.Second)
	var lastGot []string
	for time.Now().Before(deadline) {
		var got []string
		for tool, err := range session.Tools(ctx, nil) {
			if err != nil {
				t.Fatalf("listing tools: %v", err)
			}
			got = append(got, tool.Name)
		}
		sort.Strings(got)
		lastGot = got
		if fmt.Sprint(got) == fmt.Sprint(wantSorted) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("tools/list = %v after 5s, want %v", lastGot, wantSorted)
}

func TestServerCommand_PropagatesToolsListChanged(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: fake
    transport: http
    url: %q
`, gatewayAddr, backendHTTP.URL))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + gatewayAddr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}
	waitForToolNames(t, ctx, session, []string{"ping"})

	// Registering a new tool on the backend AFTER the gateway already
	// connected must reach the downstream client via list_changed, without
	// restarting anything.
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "pong", Description: "pong"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	waitForToolNames(t, ctx, session, []string{"ping", "pong"})

	// Removing it again must also propagate.
	backendSrv.RemoveTools("ping")
	waitForToolNames(t, ctx, session, []string{"pong"})

	_ = session.Close()
	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}

// waitForResourceURIs is waitForToolNames's counterpart for resources.
func waitForResourceURIs(t *testing.T, ctx context.Context, session *mcp.ClientSession, want []string) {
	t.Helper()
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)

	deadline := time.Now().Add(5 * time.Second)
	var lastGot []string
	for time.Now().Before(deadline) {
		var got []string
		for r, err := range session.Resources(ctx, nil) {
			if err != nil {
				t.Fatalf("listing resources: %v", err)
			}
			got = append(got, r.URI)
		}
		sort.Strings(got)
		lastGot = got
		if fmt.Sprint(got) == fmt.Sprint(wantSorted) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("resources/list = %v after 5s, want %v", lastGot, wantSorted)
}

func TestServerCommand_PropagatesResourcesListChanged(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	readHandler := func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "hello"}}}, nil
	}
	backendSrv.AddResource(&mcp.Resource{URI: "file:///a", Name: "a"}, readHandler)
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: fake
    transport: http
    url: %q
`, gatewayAddr, backendHTTP.URL))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + gatewayAddr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}
	waitForResourceURIs(t, ctx, session, []string{"file:///a"})

	backendSrv.AddResource(&mcp.Resource{URI: "file:///b", Name: "b"}, readHandler)
	waitForResourceURIs(t, ctx, session, []string{"file:///a", "file:///b"})

	_ = session.Close()
	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}

// waitForPromptNames is waitForToolNames's counterpart for prompts.
func waitForPromptNames(t *testing.T, ctx context.Context, session *mcp.ClientSession, want []string) {
	t.Helper()
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)

	deadline := time.Now().Add(5 * time.Second)
	var lastGot []string
	for time.Now().Before(deadline) {
		var got []string
		for p, err := range session.Prompts(ctx, nil) {
			if err != nil {
				t.Fatalf("listing prompts: %v", err)
			}
			got = append(got, p.Name)
		}
		sort.Strings(got)
		lastGot = got
		if fmt.Sprint(got) == fmt.Sprint(wantSorted) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("prompts/list = %v after 5s, want %v", lastGot, wantSorted)
}

func TestServerCommand_PropagatesPromptsListChanged(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	getHandler := func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{}}, nil
	}
	backendSrv.AddPrompt(&mcp.Prompt{Name: "greet", Description: "say hello"}, getHandler)
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: fake
    transport: http
    url: %q
`, gatewayAddr, backendHTTP.URL))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + gatewayAddr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}
	waitForPromptNames(t, ctx, session, []string{"greet"})

	backendSrv.RemovePrompts("greet")
	backendSrv.AddPrompt(&mcp.Prompt{Name: "farewell", Description: "say bye"}, getHandler)
	waitForPromptNames(t, ctx, session, []string{"farewell"})

	_ = session.Close()
	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}

// toggleFailHandler wraps an MCP StreamableHTTP handler so that, once
// failing.Store(true) is called, any JSON-RPC request whose method is
// method gets a synthetic "Internal error" (-32603) response instead of
// reaching the real server. Used to simulate a re-list that fails only
// AFTER the initial (successful) list, to test that mcprt keeps the
// previous list rather than dropping it.
type toggleFailHandler struct {
	inner   http.Handler
	method  string
	failing *atomic.Bool
}

func (h *toggleFailHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !h.failing.Load() {
		h.inner.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var probe struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Method == h.method {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      probe.ID,
			"error":   map[string]any{"code": -32603, "message": "Internal error"},
		})
		return
	}
	h.inner.ServeHTTP(w, r)
}

func TestServerCommand_ToolsListChanged_ReListFailureKeepsPreviousList(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	realHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil)
	var failing atomic.Bool
	backendHTTP := httptest.NewServer(&toggleFailHandler{inner: realHandler, method: "tools/list", failing: &failing})
	defer backendHTTP.Close()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: fake
    transport: http
    url: %q
`, gatewayAddr, backendHTTP.URL))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + gatewayAddr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}
	waitForToolNames(t, ctx, session, []string{"ping"})

	// From now on, any tools/list the gateway sends (i.e. the re-list
	// triggered by the notification below) fails. The notification itself
	// still reaches the client fine -- only the follow-up re-list fails.
	failing.Store(true)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "pong", Description: "pong"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})

	// Give the (failing) reconcile attempt time to run, then assert the
	// downstream list is still exactly the pre-change list.
	time.Sleep(500 * time.Millisecond)
	var got []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		got = append(got, tool.Name)
	}
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint([]string{"ping"}) {
		t.Fatalf("tools/list = %v, want [ping] (the failed re-list must not have dropped or changed the previous list)", got)
	}

	// Once the backend recovers, the SAME notification mechanism isn't
	// re-triggered (list_changed already fired once), but a fresh add does
	// fire again and this time succeeds.
	failing.Store(false)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "extra", Description: "extra"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	waitForToolNames(t, ctx, session, []string{"ping", "pong", "extra"})

	_ = session.Close()
	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}
```

`internal/cli/server_test.go`のimportブロックに`"bytes"`、`"encoding/json"`、`"io"`、`"sync/atomic"`を追加する（現状未import分のみ）。

- [ ] **Step 2: テストを実行し、通ることを確認する**

Run: `go test ./internal/cli/... -run 'TestServerCommand_Propagates|TestServerCommand_ToolsListChanged_ReListFailureKeepsPreviousList' -v -timeout 60s`
Expected: PASS（4テストすべて）

もし`TestServerCommand_ToolsListChanged_ReListFailureKeepsPreviousList`がflakyな場合（"Give the reconcile attempt time to run"の`500ms`が短すぎて再List未着手のまま緑になる誤検知など）、`time.Sleep(500 * time.Millisecond)`を`1 * time.Second`に伸ばして再実行する。

- [ ] **Step 3: 全体テストを実行する**

Run: `task test`
Expected: PASS（全パッケージ）

- [ ] **Step 4: `-race`で確認する**

Run: `go test -race ./internal/cli/... -timeout 120s`
Expected: PASS、データレースの報告なし

- [ ] **Step 5: lintを実行する**

Run: `task lint`
Expected: エラーなし

- [ ] **Step 6: コミット**

```bash
git add internal/cli/server_test.go
git commit -m "test(cli): add end-to-end coverage for list_changed propagation"
```

---

## 完了確認

全タスク完了後、以下を実行して最終確認する:

```bash
task test
task lint
go test -race ./...
```

すべて成功したら、設計ドキュメント（`docs/superpowers/specs/2026-08-21-mcprt-list-changed-design.md`）のスコープに記載された3種の通知（tools/resources(+templates)/prompts）すべてが再List・再登録・downstream伝播されることが、ユニットテスト（`internal/gateway`）と統合テスト（`internal/cli`）の両レベルで確認されている。
