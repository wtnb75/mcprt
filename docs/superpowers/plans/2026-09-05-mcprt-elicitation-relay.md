# mcprt: tools/call elicitation中継 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `tools/call` に限り、backendが送る`elicitation/create`要求を、ちょうど1件だけ進行中のdownstream呼び出しへ中継し、その応答をbackendへ返す。同時に複数の`tools/call`が同一backendへ進行中で相関が取れない場合は、安全側に倒してエラーを返す。

**Architecture:** `internal/gateway`に新規`ElicitationRouter`（backend名 -> 現在進行中のdownstream`*mcp.ServerSession`の集合、というだけの独立コンポーネント）を追加する。`callHandler`は`tools/call`開始時に`elicit.Enter(b.Name, req.Session)`を呼び、`defer leave()`を登録するだけで、tools/call自体の処理には一切割り込まない。backend向け`mcp.Client`の`ElicitationHandler`（`backend.ChangeCallbacks.OnElicit`経由で配線）が発火すると、`elicitRouter.Route(backendName)`で「ちょうど1件」の場合だけdownstream sessionを特定し、`session.Elicit(...)`で中継する。0件または2件以上ならエラーを返し、backendへそのままエラーとして伝播する（downstreamには一切届かない）。

設計書（下記Spec）は`ElicitationRouter`の内部状態を「backend名 -> `[]*mcp.ServerSession`」という素朴なスライスとして示しているが、本プランでは`internal/gateway/progress.go`（progress中継実装、既にmerge済み）の`ProgressRegistry`と同じ設計方針——「単調増加カウンタをキーにしたmapで1エントリずつ安全に追加/削除する」——を採用する。理由: スライスから特定の1エントリだけを安全に削除するには、並行な`leave`呼び出し間でインデックスがずれない工夫が必要になり、mapの方が明らかにシンプルで、progress中継の`ProgressRegistry`が既に確立した「backend名/呼び出しごとに単調カウンタで一意なキーを払い出す」パターンとも一貫する。

配線についても、`docs/superpowers/plans/2026-09-04-mcprt-progress-relay.md`（既にmerge済み）が確立した方針を踏襲する: 設計書は`connectBackends`/`superviseBackends`/`superviseBackend`のシグネチャに`*ElicitationRouter`を新パラメータとして通す案を示しているが、本プランでは既存の`gwHolder`（`internal/cli/server.go`、progress中継で`progress *gateway.ProgressRegistry`フィールドを既に持つ）に`elicit *gateway.ElicitationRouter`フィールドをもう1つ追加するだけにする。`internal/cli/call.go`・`internal/cli/list.go`（`connectBackends`を`gwH=nil`で呼ぶ）は今回も一切変更しない。

**Tech Stack:** Go 1.x, `github.com/modelcontextprotocol/go-sdk v1.7.0`（`mcp`パッケージ）, 標準の`go test`（`-race`込み）。

**Spec:** `docs/superpowers/specs/2026-08-25-mcprt-elicitation-relay-design.md`

## Global Constraints

- 対象は`tools/call`のみ。`resources/read`・`resources/templates/read`・`prompts/get`は対象外（一切変更しない）。
- elicitationの成否をaudit log（`logCall`）へ記録する仕組みは追加しない（progress中継の`progress_count`のような要約フィールドは作らない）。失敗時は`logger.Warn`のみ。
- `elicitTimeout`（downstreamの応答を待つ上限）はYAML設定に露出しない。既存の`backendConnectTimeout`/`reloadDrainTimeout`と同じ形の、`internal/cli/server.go`のpackage変数（テストで上書き可能）としてハードコードする。デフォルトは`5 * time.Minute`（人間の応答を待つため、既存の`backendConnectTimeout`の30秒より大幅に長い）。
- `ElicitationRouter.Route`は、対象backendへの進行中`tools/call`が**ちょうど1件**のときだけ一意にセッションを返す。0件・2件以上はどちらもエラーであり、絶対に「推測」してはならない（同一セッションからの2件同時呼び出しでも、2件は2件としてエラーにする——スライスの中身が同じセッションかどうかは関係ない。件数だけを見る）。
- `go.mod`のモジュールパスは`github.com/wtnb75/mcprt`。SDKのパッケージ名は`mcp`（import path `github.com/modelcontextprotocol/go-sdk/mcp`）。

---

## ファイル構成

| ファイル | 種別 | 責務 |
|---|---|---|
| `internal/gateway/elicitation.go` | 新規 | `ElicitationRouter`: backend名ごとの進行中`tools/call`セッション集合、`Enter`/`Route` |
| `internal/gateway/elicitation_test.go` | 新規 | `ElicitationRouter`の単体テスト |
| `internal/gateway/gateway.go` | 変更 | `Server.elicit`フィールド追加、`New`/`registerTool`/`callHandler`に`*ElicitationRouter`を通す |
| `internal/gateway/reconcile.go` | 変更 | `updateToolsLocked`内の`registerTool`呼び出しに`s.elicit`を渡す |
| `internal/gateway/gateway_test.go` | 変更 | 既存18箇所の`gateway.New`呼び出しに末尾`nil`を追加。elicitation中継の統合テストを2件追加 |
| `internal/gateway/reconcile_test.go` | 変更 | 既存12箇所の`gateway.New`呼び出しに末尾`nil`を追加 |
| `internal/backend/backend.go` | 変更 | `ChangeCallbacks.OnElicit`フィールド追加、`Connect`内で`ClientOptions.ElicitationHandler`に配線 |
| `internal/backend/backend_test.go` | 変更 | `OnElicit`が発火し、backendへ応答が返ることを確認するテストを追加 |
| `internal/cli/server.go` | 変更 | `gwHolder.elicit`フィールド追加、`elicitTimeout`変数追加、`buildGateway`で構築して`gateway.New`に渡す、`superviseBackend`のコールバック構築で`cb.OnElicit`を配線 |
| `internal/cli/server_internal_test.go` | 変更 | 既存3箇所の`gateway.New`呼び出しに末尾`nil`を追加 |
| `internal/cli/server_test.go` | 変更 | e2eテスト`TestServerCommand_RoutesElicitationToDownstreamClient`を追加 |

---

## Task 1: ElicitationRouter コンポーネント

**Files:**
- Create: `internal/gateway/elicitation.go`
- Create: `internal/gateway/elicitation_test.go`

**Interfaces:**
- Produces: `gateway.NewElicitationRouter() *ElicitationRouter`、`(*ElicitationRouter).Enter(backendName string, session *mcp.ServerSession) (leave func())`、`(*ElicitationRouter).Route(backendName string) (*mcp.ServerSession, error)`。以降のタスクはこれらのシグネチャをそのまま使う。

- [ ] **Step 1: `internal/gateway/elicitation.go`を作成する**

```go
package gateway

import (
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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

// backendCalls tracks one backend's in-flight tools/call sessions, keyed by
// a per-backendCalls monotonic counter rather than stored in a plain slice:
// a slice index shifts under concurrent removal (leave), so identifying an
// entry by a stable key that Enter hands out and leave deletes by is what
// makes concurrent Enter/leave safe without any index-bookkeeping.
type backendCalls struct {
	mu   sync.Mutex
	next uint64
	live map[uint64]*mcp.ServerSession
}

// NewElicitationRouter returns an empty router, ready to use.
func NewElicitationRouter() *ElicitationRouter {
	return &ElicitationRouter{calls: make(map[string]*backendCalls)}
}

// Enter records one in-flight tools/call for backendName, owned by
// session -- the same session may Enter more than once, for two concurrent
// calls from the same downstream client to the same backend, and each
// counts as a separate in-flight call for Route's purposes. The caller
// must call the returned leave func exactly once (via defer) when the call
// returns, success or failure.
func (r *ElicitationRouter) Enter(backendName string, session *mcp.ServerSession) (leave func()) {
	r.mu.Lock()
	bc, ok := r.calls[backendName]
	if !ok {
		bc = &backendCalls{live: make(map[uint64]*mcp.ServerSession)}
		r.calls[backendName] = bc
	}
	r.mu.Unlock()

	bc.mu.Lock()
	id := bc.next
	bc.next++
	bc.live[id] = session
	bc.mu.Unlock()

	return func() {
		bc.mu.Lock()
		delete(bc.live, id)
		bc.mu.Unlock()
	}
}

// Route reports the single downstream session to forward an elicitation
// request to, for the given backend. It returns an error -- and forwards
// nothing -- unless exactly one tools/call is currently in flight for
// backendName: zero in-flight calls means there's nothing to correlate to
// (the elicitation arrived too late, or the backend is misbehaving); more
// than one means mcprt cannot tell which call it belongs to (MCP's
// elicitation/create carries no per-call correlation token), and guessing
// wrong would route a backend's question to an unrelated client -- even
// when every in-flight call happens to belong to the same session, the
// count alone decides, never the sessions' identity.
func (r *ElicitationRouter) Route(backendName string) (*mcp.ServerSession, error) {
	r.mu.Lock()
	bc, ok := r.calls[backendName]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("elicitation: no in-flight tools/call for backend %q", backendName)
	}

	bc.mu.Lock()
	defer bc.mu.Unlock()
	switch len(bc.live) {
	case 0:
		return nil, fmt.Errorf("elicitation: no in-flight tools/call for backend %q", backendName)
	case 1:
		for _, s := range bc.live {
			return s, nil
		}
	}
	return nil, fmt.Errorf("elicitation: %d concurrent tools/call in flight for backend %q, cannot disambiguate", len(bc.live), backendName)
}
```

- [ ] **Step 2: `internal/gateway/elicitation_test.go`を作成する**

`Enter`/`Route`/`leave`は`*mcp.ServerSession`の値そのものを一切呼び出さず、識別子として保持・比較するだけなので、テストは接続済みの本物のセッションを用意する必要がなく、`&mcp.ServerSession{}`をopaqueな識別子として使うだけで十分（progress中継の`ProgressRegistry`のテストとは異なり、`Relay`が実際に`session.NotifyProgress`を呼び出すのと違って、このルータは何もI/Oしない）。

```go
package gateway_test

import (
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/gateway"
)

func TestElicitationRouter_RouteWithZeroInFlightErrors(t *testing.T) {
	r := gateway.NewElicitationRouter()
	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route with zero in-flight calls: got nil error, want an error")
	}
}

func TestElicitationRouter_RouteWithOneInFlightReturnsSession(t *testing.T) {
	r := gateway.NewElicitationRouter()
	session := &mcp.ServerSession{}
	leave := r.Enter("backend-a", session)
	defer leave()

	got, err := r.Route("backend-a")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got != session {
		t.Fatalf("Route returned %v, want %v", got, session)
	}
}

func TestElicitationRouter_RouteWithMultipleInFlightErrors(t *testing.T) {
	r := gateway.NewElicitationRouter()
	leave1 := r.Enter("backend-a", &mcp.ServerSession{})
	defer leave1()
	leave2 := r.Enter("backend-a", &mcp.ServerSession{})
	defer leave2()

	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route with two in-flight calls: got nil error, want an error (ambiguous)")
	}
}

// TestElicitationRouter_SameSessionTwiceIsStillAmbiguous checks that Route
// counts in-flight CALLS, not distinct sessions: two concurrent tools/call
// from the very same downstream session against the same backend must
// still refuse to route, since MCP's elicitation/create carries no
// per-call correlation -- mcprt genuinely cannot tell which of the two
// calls the elicitation belongs to, even though routing it to "the" session
// would happen to reach the right client.
func TestElicitationRouter_SameSessionTwiceIsStillAmbiguous(t *testing.T) {
	r := gateway.NewElicitationRouter()
	session := &mcp.ServerSession{}
	leave1 := r.Enter("backend-a", session)
	leave2 := r.Enter("backend-a", session)

	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route with two in-flight calls from the same session: got nil error, want an error")
	}

	leave1()
	got, err := r.Route("backend-a")
	if err != nil {
		t.Fatalf("Route after one leave: %v", err)
	}
	if got != session {
		t.Fatalf("Route after one leave = %v, want %v", got, session)
	}

	leave2()
	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route after both leaves: got nil error, want an error (zero in-flight)")
	}
}

func TestElicitationRouter_DifferentBackendsAreIndependent(t *testing.T) {
	r := gateway.NewElicitationRouter()
	sessionA := &mcp.ServerSession{}
	leaveA := r.Enter("backend-a", sessionA)
	defer leaveA()

	if _, err := r.Route("backend-b"); err == nil {
		t.Fatal("Route(backend-b) with zero in-flight calls for backend-b: got nil error, want an error")
	}
	got, err := r.Route("backend-a")
	if err != nil {
		t.Fatalf("Route(backend-a): %v", err)
	}
	if got != sessionA {
		t.Fatalf("Route(backend-a) = %v, want %v", got, sessionA)
	}
}

// TestElicitationRouter_ConcurrentEnterRouteLeave exercises Enter, Route,
// and leave from many goroutines at once -- go test -race must find
// nothing. It does not assert on Route's outcome mid-stress (the in-flight
// count is nondeterministic while goroutines are still entering/leaving),
// only that the router is race-free and left in a correct empty state
// afterward.
func TestElicitationRouter_ConcurrentEnterRouteLeave(t *testing.T) {
	r := gateway.NewElicitationRouter()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leave := r.Enter("backend-a", &mcp.ServerSession{})
			_, _ = r.Route("backend-a")
			leave()
		}()
	}
	wg.Wait()

	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route after all goroutines left: got nil error, want an error (zero in-flight)")
	}
}
```

- [ ] **Step 3: テストを実行して確認する**

Run: `go test ./internal/gateway/... -run TestElicitationRouter -race -v`
Expected: `PASS`（6つのテスト全て）。

- [ ] **Step 4: パッケージ全体のビルド・vetを確認する**

Run: `go build ./... && go vet ./...`
Expected: エラーなし（`elicitation.go`はまだどこからも参照されないので、既存コードに影響しない）。

- [ ] **Step 5: コミット**

```bash
git add internal/gateway/elicitation.go internal/gateway/elicitation_test.go
git commit -m "feat(gateway): add ElicitationRouter for tools/call elicitation correlation"
```

---

## Task 2: `backend.ChangeCallbacks.OnElicit` の配線

**Files:**
- Modify: `internal/backend/backend.go`（`ChangeCallbacks`定義と`Connect`内の配線）
- Test: `internal/backend/backend_test.go`

**Interfaces:**
- Consumes: なし（SDKの`mcp.ClientOptions.ElicitationHandler func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)`は既存API）。
- Produces: `backend.ChangeCallbacks.OnElicit func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)`。Task 4の`internal/cli/server.go`配線がこのフィールドを使う。

- [ ] **Step 1: `ChangeCallbacks`に`OnElicit`フィールドを追加する**

`internal/backend/backend.go`の`ChangeCallbacks`定義を変更（現状は`OnProgress`まで持っている）:

```go
// ChangeCallbacks are invoked when a connected backend reports that its
// tool/prompt/resource list has changed, sends a progress notification for
// an in-flight call, or asks for structured input mid-call via
// elicitation/create. Each OnXChanged func takes no arguments: MCP's
// list_changed notifications carry no payload, they only signal "go
// re-list." A nil field means "not interested" and leaves the corresponding
// SDK handler unset.
type ChangeCallbacks struct {
	OnToolsChanged     func()
	OnPromptsChanged   func()
	OnResourcesChanged func() // fires for notifications/resources/list_changed, which covers BOTH resources and resource templates per the MCP spec -- there is no separate resource-template notification
	// OnProgress, if non-nil, is wired as the backend-facing mcp.Client's
	// ProgressNotificationHandler -- unlike the three notification
	// callbacks above, progress notifications carry a payload, so this
	// field's signature matches the SDK handler's exactly.
	OnProgress func(context.Context, *mcp.ProgressNotificationClientRequest)
	// OnElicit, if non-nil, is wired as the backend-facing mcp.Client's
	// ElicitationHandler. Like OnProgress, this one both takes a payload
	// and returns a result -- its signature matches the SDK handler's
	// exactly. Setting it also causes the SDK to automatically advertise
	// the elicitation capability to the backend (see go-sdk's
	// ClientOptions.ElicitationHandler doc).
	OnElicit func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)
}
```

- [ ] **Step 2: `Connect`内で配線する**

`internal/backend/backend.go`の`Connect`内、`OnProgress`の配線ブロック直後、`mcp.NewClient`呼び出しの直前に追加:

```go
	if cb.OnProgress != nil {
		clientOpts.ProgressNotificationHandler = cb.OnProgress
	}
	if cb.OnElicit != nil {
		clientOpts.ElicitationHandler = cb.OnElicit
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "mcprt", Version: "v1"}, clientOpts)
```

- [ ] **Step 3: 失敗するテストを書く**

`internal/backend/backend_test.go`の`TestConnect_ProgressNotificationCallback`の直後、`TestConnect_NilChangeCallbacks_NoHandlersRegistered`（465行目付近）の前に追加:

```go
// TestConnect_ElicitationCallback checks that ChangeCallbacks.OnElicit
// fires with the backend's elicitation/create payload when the backend
// asks for input mid-call, and that the handler's response reaches the
// backend as the tool call's result.
func TestConnect_ElicitationCallback(t *testing.T) {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	fakeServer.AddTool(&mcp.Tool{Name: "ask", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, err := req.Session.Elicit(ctx, &mcp.ElicitParams{
				Message:         "confirm?",
				RequestedSchema: map[string]any{"type": "object"},
			})
			if err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Action}}}, nil
		})

	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil))
	defer srv.Close()

	ctx := context.Background()
	var gotMessage string
	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL},
		backend.ChangeCallbacks{OnElicit: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			gotMessage = req.Params.Message
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirmed": true}}, nil
		}})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	res, err := b.Session.CallTool(ctx, &mcp.CallToolParams{Name: "ask", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if gotMessage != "confirm?" {
		t.Fatalf("OnElicit message = %q, want \"confirm?\"", gotMessage)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "accept" {
		t.Fatalf("tool result = %+v, want text \"accept\"", res.Content)
	}
}
```

- [ ] **Step 4: テストを実行してパスすることを確認する**

Run: `go test ./internal/backend/... -run TestConnect -race -v`
Expected: `PASS`（新規テスト`TestConnect_ElicitationCallback`に加え、既存の`TestConnect_*`全てが通ること）。

- [ ] **Step 5: パッケージ全体のビルドを確認する**

Run: `go build ./... && go vet ./...`
Expected: エラーなし。

- [ ] **Step 6: コミット**

```bash
git add internal/backend/backend.go internal/backend/backend_test.go
git commit -m "feat(backend): wire ChangeCallbacks.OnElicit to the client's ElicitationHandler"
```

---

## Task 3: `gateway`パッケージへの統合（`callHandler`・`registerTool`・`New`）

**Files:**
- Modify: `internal/gateway/gateway.go`（`Server`構造体、`New`、`registerTool`、`callHandler`）
- Modify: `internal/gateway/reconcile.go`（`updateToolsLocked`内の`registerTool`呼び出し1箇所）
- Modify: `internal/gateway/gateway_test.go`（既存18箇所の`gateway.New`呼び出し + 新規テスト2件）
- Modify: `internal/gateway/reconcile_test.go`（既存12箇所の`gateway.New`呼び出し）

**Interfaces:**
- Consumes: Task 1の`gateway.NewElicitationRouter`/`ElicitationRouter.Enter`/`ElicitationRouter.Route`。
- Produces: `gateway.New(logger, backends, tables, entries, overrides, maskKeys, progress, elicit *ElicitationRouter) *Server`（末尾に`elicit`引数を追加）。Task 4の`internal/cli/server.go`がこの新シグネチャで呼ぶ。

- [ ] **Step 1: `Server`構造体に`elicit`フィールドを追加する**

`internal/gateway/gateway.go`の`Server`構造体、`progress *ProgressRegistry`の直後に追加:

```go
type Server struct {
	mcp      *mcp.Server
	logger   *slog.Logger
	backends map[string]*backend.Backend
	maskKeys []string
	progress *ProgressRegistry
	elicit   *ElicitationRouter

	mu sync.Mutex
	...
```

- [ ] **Step 2: `New`に`elicit`引数を追加する**

`internal/gateway/gateway.go`の`New`を変更（末尾に`elicit *ElicitationRouter`引数を追加、`Server`構築時に格納、tools登録ループで`registerTool`へ渡す）:

```go
func New(logger *slog.Logger, backends map[string]*backend.Backend, tables Tables, entries Entries, overrides Overrides, maskKeys []string, progress *ProgressRegistry, elicit *ElicitationRouter) *Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{Logger: logger})

	s := &Server{
		mcp:      mcpSrv,
		logger:   logger,
		backends: backends,
		maskKeys: maskKeys,
		progress: progress,
		elicit:   elicit,

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
			registerTool(mcpSrv, logger, backends, resolved, maskKeys, progress, elicit)
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

- [ ] **Step 3: `registerTool`に`elicit`引数を追加する**

`internal/gateway/gateway.go`の`registerTool`を変更:

```go
func registerTool(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Tool], maskKeys []string, progress *ProgressRegistry, elicit *ElicitationRouter) (ok bool) {
	candidates := append([]router.Candidate[*mcp.Tool]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addTool(srv, logger, c.Item, callHandler(logger, maskKeys, b, c.OriginalName, progress, elicit)) {
			return true
		}
	}
	logger.Error("tool unavailable: every candidate backend had an invalid definition", "tool", resolved.Item.Name)
	return false
}
```

- [ ] **Step 4: `callHandler`にelicitation相関ロジックを追加する**

`internal/gateway/gateway.go`の`callHandler`を変更（`elicit.Enter`/`defer leave()`を、既存のprogress登録ロジックの前に追加する — tools/callの範囲全体を覆うようにするため、spanを開始した直後、progress登録より前に置く）:

```go
// callHandler forwards a tools/call to originalName on backend b, passing
// the raw arguments through unchanged. It wraps the call in a span
// (startCallSpan is a no-op for stdio-originated calls) and logs it via
// logCall, success or failure, so a dead or erroring backend — and normal
// usage — is visible to the operator. When progress is non-nil and the
// downstream request carries a progressToken, it registers a fresh
// correlation entry so a notifications/progress the backend sends mid-call
// (relayed via progress.Relay, wired through backend.ChangeCallbacks.
// OnProgress) reaches the downstream caller under its own token. When
// elicit is non-nil, it records this call as in-flight against b for the
// whole duration of the backend call, so a backend's elicitation/create
// (relayed via elicit.Route, wired through backend.ChangeCallbacks.
// OnElicit) can be routed back to req.Session when -- and only when --
// this is the sole tools/call in flight against b.
func callHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalName string, progress *ProgressRegistry, elicit *ElicitationRouter) mcp.ToolHandler {
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

		params := &mcp.CallToolParams{Name: originalName, Arguments: req.Params.Arguments}

		var entry *progressEntry
		if progress != nil {
			if token := req.Params.GetProgressToken(); token != nil {
				var internalToken uint64
				var cleanup func()
				internalToken, entry, cleanup = progress.Register(req.Session, token, b.Name)
				defer cleanup()
				// SetProgressToken only accepts int/int32/int64/string (see
				// go-sdk's setProgressToken); progress.Register hands out a
				// uint64, so it must be narrowed to int64 here.
				// normalizeProgressToken (progress.go) accepts int64 back,
				// so this round-trips correctly on the relay side.
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

- [ ] **Step 5: `reconcile.go`の`registerTool`呼び出しに`s.elicit`を渡す**

`internal/gateway/reconcile.go`の`registerTool`呼び出しを変更:

```go
		if !registerTool(s.mcp, s.logger, s.backends, resolved, s.maskKeys, s.progress, s.elicit) {
```

（これが無いと、`list_changed`での再登録や`ConnectBackend`によるreconnect時の再登録で作られる`callHandler`が`elicit=nil`になり、そのtoolだけelicitation中継が効かなくなる。）

- [ ] **Step 6: `go build`でコンパイルエラーを洗い出し、機械的に直す**

Run: `go build ./... 2>&1 | head -80`

`gateway.New`のシグネチャ変更により、`internal/gateway/gateway_test.go`（18箇所）・`internal/gateway/reconcile_test.go`（12箇所）の`gateway.New(...)`呼び出しがコンパイルエラーになる。エラーは「not enough arguments」の形で出る。それぞれ末尾の引数（`nil`、`gateway.NewProgressRegistry()`、`progressReg`など、progress中継の実装で既に7引数になっているもの）の後ろに`, nil`を追加する。例:

- Before: `srv := gateway.New(logger, want, gateway.Tables{}, gateway.Entries{}, gateway.Overrides{}, nil, nil)`
- After: `srv := gateway.New(logger, want, gateway.Tables{}, gateway.Entries{}, gateway.Overrides{}, nil, nil, nil)`

複数行にまたがる呼び出し（`reconcile_test.go`はほぼ全てこの形）も同様に、最後の実引数（多くは`nil)`で終わる行）の直前に`, nil`を追加する。

`internal/cli/server.go`（本番コード、`buildGateway`内の`gateway.New`呼び出し）と`internal/cli/server_internal_test.go`（3箇所）も同様にコンパイルエラーになるが、これらはTask 4の担当なので**このタスクでは触らない**。`internal/cli`パッケージが壊れたままになるのは想定通り（Task 4で直る）。

このタスクの検証は`internal/gateway`パッケージにスコープする:

Run: `go build ./internal/gateway/... && go vet ./internal/gateway/...`
Expected: エラーなし。`go build ./...`（モジュール全体）は`internal/cli`のエラーで失敗するのが正しい状態 — 実行して確認してもよいが、失敗を修正しようとしないこと。

- [ ] **Step 7: 統合テストを`gateway_test.go`に追加する**

`internal/gateway/gateway_test.go`の末尾に追加。1つ目はちょうど1件の`tools/call`が進行中のときの正常系、2つ目は同時に2件進行中のときの拒否を確認する。どちらも`req.Session.Elicit`（backend側）と`session.Elicit`（gateway callHandler経由でrouterが見つけたdownstream session側）の実際の呼び出しを、実接続の`mcp.Server`/`mcp.Client`同士でエンドツーエンドに確認する — sleepやタイミング調整は一切不要（`elicit.Enter`は`b.Session.CallTool`を呼ぶ*前*に同期的に完了しているため、backendのツールハンドラが動き始めた時点でEnterは既に反映済み）。

```go
// TestGateway_CallHandlerRoutesElicitationWhenExactlyOneCallInFlight checks
// the full tools/call elicitation-relay path: while exactly one tools/call
// is in flight against a backend, that backend's elicitation/create
// (sent via req.Session.Elicit in the fake backend's tool handler) is
// routed to the ORIGINAL downstream session and its response reaches the
// backend as the elicitation result.
func TestGateway_CallHandlerRoutesElicitationWhenExactlyOneCallInFlight(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := mcp.NewServer(&mcp.Implementation{Name: "backend-a", Version: "v1"}, nil)
	backendServer.AddTool(&mcp.Tool{Name: "ask", Description: "ask", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, err := req.Session.Elicit(ctx, &mcp.ElicitParams{
				Message:         "confirm?",
				RequestedSchema: map[string]any{"type": "object"},
			})
			if err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Action}}}, nil
		})

	elicitRouter := gateway.NewElicitationRouter()
	var gotMessage string
	cb := backend.ChangeCallbacks{
		OnElicit: func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			session, err := elicitRouter.Route("backend-a")
			if err != nil {
				return nil, err
			}
			gotMessage = req.Params.Message
			return session.Elicit(ctx, req.Params)
		},
	}

	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL}, cb)
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-a", Items: toolsA}}, toolNameOf, toolRename, nil)

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, gateway.Entries{}, gateway.Overrides{}, nil, nil, elicitRouter)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"ok": true}}, nil
		},
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ask", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call ask: %v", err)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "accept" {
		t.Fatalf("call ask result = %+v, want text \"accept\"", res.Content)
	}
	if gotMessage != "confirm?" {
		t.Fatalf("OnElicit message = %q, want \"confirm?\"", gotMessage)
	}
}

// TestGateway_CallHandlerRefusesAmbiguousElicitation checks that when two
// tools/call are concurrently in flight against the same backend, that
// backend's elicitation/create is refused (never reaches the downstream
// client) -- mcprt cannot tell which of the two calls it belongs to.
func TestGateway_CallHandlerRefusesAmbiguousElicitation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	started := make(chan struct{}, 2)
	proceed := make(chan struct{})
	backendServer := mcp.NewServer(&mcp.Implementation{Name: "backend-a", Version: "v1"}, nil)
	backendServer.AddTool(&mcp.Tool{Name: "ask", Description: "ask", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			started <- struct{}{}
			<-proceed
			_, err := req.Session.Elicit(ctx, &mcp.ElicitParams{
				Message:         "confirm?",
				RequestedSchema: map[string]any{"type": "object"},
			})
			return &mcp.CallToolResult{}, err
		})

	elicitRouter := gateway.NewElicitationRouter()
	cb := backend.ChangeCallbacks{
		OnElicit: func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			session, err := elicitRouter.Route("backend-a")
			if err != nil {
				return nil, err
			}
			return session.Elicit(ctx, req.Params)
		},
	}

	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL}, cb)
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-a", Items: toolsA}}, toolNameOf, toolRename, nil)

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, gateway.Entries{}, gateway.Overrides{}, nil, nil, elicitRouter)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	// This ElicitationHandler must never be called: Route must refuse
	// before the request ever reaches the downstream client.
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			t.Error("downstream ElicitationHandler was called; want Route to refuse before ever reaching downstream")
			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ask", Arguments: map[string]any{}})
			results[i] = err
		}(i)
	}

	<-started
	<-started // both tool handlers are now past elicit.Enter and blocked before calling Elicit
	close(proceed)
	wg.Wait()

	for i, err := range results {
		if err == nil {
			t.Fatalf("call %d: got no error, want elicitation to fail (ambiguous routing refuses it)", i)
		}
	}
}
```

（`sync`パッケージが`gateway_test.go`にまだimportされていなければ追加すること。`config`・`router`・`backend`・`gateway`パッケージは既にimport済み。）

- [ ] **Step 8: 新規テストを実行する**

Run: `go test ./internal/gateway/... -run TestGateway_CallHandlerRoutesElicitation -race -v`
Run: `go test ./internal/gateway/... -run TestGateway_CallHandlerRefusesAmbiguousElicitation -race -v`
Expected: 両方`PASS`。

- [ ] **Step 9: `internal/gateway`パッケージ全体のテストを実行する**

Run: `go test ./internal/gateway/... -race`
Expected: `ok`（既存テストを含め全てPASS — Step 6の機械的な引数追加が正しく行われていることの確認）。

- [ ] **Step 10: コミット**

```bash
git add internal/gateway/gateway.go internal/gateway/reconcile.go \
        internal/gateway/gateway_test.go internal/gateway/reconcile_test.go
git commit -m "feat(gateway): route tools/call elicitation requests through callHandler"
```

---

## Task 4: `internal/cli/server.go`への配線とe2eテスト

**Files:**
- Modify: `internal/cli/server.go`（`gwHolder`、`elicitTimeout`変数、`buildGateway`、`superviseBackend`）
- Modify: `internal/cli/server_internal_test.go`（3箇所の`gateway.New`呼び出し）
- Modify: `internal/cli/server_test.go`（e2eテスト追加）

**Interfaces:**
- Consumes: Task 3の`gateway.New(..., progress, elicit *gateway.ElicitationRouter)`、Task 1の`gateway.NewElicitationRouter()`、Task 2の`backend.ChangeCallbacks.OnElicit`。
- Produces: なし（末端の配線）。

- [ ] **Step 1: `elicitTimeout`変数を追加する**

`internal/cli/server.go`の`backendConnectTimeout`の近く（`reloadDrainTimeout`の後、`telemetryShutdownTimeout`の前あたり）に追加:

```go
// elicitTimeout bounds how long superviseBackend's OnElicit callback waits
// for a downstream client to respond to a relayed elicitation/create
// request -- it blocks the backend's in-flight tools/call for as long as
// it runs. Deliberately much longer than backendConnectTimeout: this waits
// on a human answering a prompt, not a network round trip. Not exposed in
// YAML config, matching the existing convention for this class of
// hardcoded timeout (see backendConnectTimeout/reloadDrainTimeout). A var
// so tests can shrink it.
var elicitTimeout = 5 * time.Minute
```

- [ ] **Step 2: `gwHolder`に`elicit`フィールドを追加する**

`internal/cli/server.go`の`gwHolder`定義を変更（既に`progress *gateway.ProgressRegistry`フィールドを持つ）:

```go
type gwHolder struct {
	ptr      atomic.Pointer[gateway.Server]
	progress *gateway.ProgressRegistry
	elicit   *gateway.ElicitationRouter
}
```

- [ ] **Step 3: `buildGateway`で`ElicitationRouter`を構築し、`gateway.New`に渡す**

`internal/cli/server.go`の`buildGateway`内、`gwH.progress = gateway.NewProgressRegistry()`の直後に追加:

```go
	var gwH gwHolder
	gwH.progress = gateway.NewProgressRegistry()
	gwH.elicit = gateway.NewElicitationRouter()
	conn := connectBackends(ctx, logger, cfg.Backends, &gwH)
```

`gateway.New(...)`呼び出しの最終引数に`gwH.elicit`を追加:

```go
	}, cfg.Logging.MaskKeys, gwH.progress, gwH.elicit)
	gwH.ptr.Store(srv)
```

- [ ] **Step 4: `superviseBackend`のコールバック構築で`OnElicit`を配線する**

`internal/cli/server.go`の`superviseBackend`内、`cb.OnProgress`配線の直後に追加:

```go
		if gwH.progress != nil {
			cb.OnProgress = func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
				gwH.progress.Relay(ctx, logger, bc.Name, req.Params)
			}
		}
		if gwH.elicit != nil {
			cb.OnElicit = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				session, err := gwH.elicit.Route(bc.Name)
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
		}
```

- [ ] **Step 5: `go build`でコンパイルエラーを洗い出し、既存の`gateway.New`呼び出しを直す**

Run: `go build ./... 2>&1`

`internal/cli/server_internal_test.go`の3箇所の`gateway.New(...)`呼び出しが「not enough arguments」で失敗する（Task 3の時点で既に`internal/cli`は壊れていたはずなので、progress分の`, nil`も合わせてこの時点で確認しながら直すこと）。それぞれ末尾に`, nil`を追加する。例:

- Before: `srv := gateway.New(logger, conn.backends, gateway.Tables{}, gateway.Entries{}, gateway.Overrides{}, nil, nil)`
- After: `srv := gateway.New(logger, conn.backends, gateway.Tables{}, gateway.Entries{}, gateway.Overrides{}, nil, nil, nil)`

複数行にまたがる呼び出しも同様に、最後の実引数の直後に`, nil`を追加する。これらのテストは`&gwHolder{}`を直接構築しており`elicit`フィールドは零値`nil`のままでよいので、`gateway.New`側も`nil`を渡せばよい（elicitation中継はこれらのテストの検証対象ではない）。

Expected: `go build ./... && go vet ./...`がエラーなしで終わる（これでモジュール全体が正常にビルドできる状態に戻る）。

- [ ] **Step 6: 既存の`internal/cli`テストを実行して壊れていないことを確認する**

Run: `go test ./internal/cli/... -race`
Expected: `ok`。

- [ ] **Step 7: e2eテストを`server_test.go`に追加する**

`internal/cli/server_test.go`の末尾に追加:

```go
// TestServerCommand_RoutesElicitationToDownstreamClient checks the
// elicitation-relay feature end-to-end through the real server command: a
// backend's elicitation/create (sent mid-tools/call) reaches the
// downstream client, and the client's response reaches the backend as the
// elicitation result -- exercising the real production wiring
// (superviseBackend's OnElicit, ElicitationRouter.Route, elicitTimeout).
func TestServerCommand_RoutesElicitationToDownstreamClient(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	backendSrv.AddTool(&mcp.Tool{Name: "ask", Description: "ask", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, err := req.Session.Elicit(ctx, &mcp.ElicitParams{
				Message:         "confirm?",
				RequestedSchema: map[string]any{"type": "object"},
			})
			if err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Action}}}, nil
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

	var gotMessage string
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			gotMessage = req.Params.Message
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"ok": true}}, nil
		},
	})
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
	waitForToolNames(t, ctx, session, []string{"ask"})

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ask", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call ask: %v", err)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "accept" {
		t.Fatalf("call ask result = %+v, want text \"accept\"", res.Content)
	}
	if gotMessage != "confirm?" {
		t.Fatalf("downstream ElicitationHandler message = %q, want \"confirm?\"", gotMessage)
	}

	_ = session.Close()
	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}
```

`waitForToolNames`は既存のヘルパーをそのまま使う（新規追加不要）。

- [ ] **Step 8: 新規e2eテストを実行する**

Run: `go test ./internal/cli/... -run TestServerCommand_RoutesElicitationToDownstreamClient -race -v`
Expected: `PASS`。

- [ ] **Step 9: モジュール全体のテストを実行する**

Run: `go build ./... && go vet ./... && go test ./... -race`
Expected: 全パッケージ`ok`。

- [ ] **Step 10: コミット**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go internal/cli/server_test.go
git commit -m "feat(cli): route tools/call elicitation requests through the server command's backend supervision"
```

---

## Self-Review メモ（実行者向けではなく記録用）

- Spec coverage: スコープに含まれる3項目（progressToken伝播ならぬelicitation要求の中継、複数同時呼び出し時の安全側エラー、mcprt独自のタイムアウト）は全てTask 1〜4でカバー。スコープ外（resources/prompts中継、より高度な相関の曖昧さ解消、タイムアウト値のYAML露出）は実装しない。
- 設計書と異なる点: (1) `ElicitationRouter`の内部状態をスライスではなく単調カウンタ+mapで実装（progress中継の`ProgressRegistry`と同じ設計方針、並行`leave`の安全性のため）。(2) `connectBackends`/`superviseBackends`/`superviseBackend`のシグネチャ変更を避け、`gwHolder`に`elicit`フィールドを追加する方式にした（progress中継と同じ理由: `internal/cli/call.go`/`list.go`を無変更に保つ）。挙動としては設計書のエラーハンドリング表と完全に一致する。
- 型整合性: `ElicitationRouter.Enter`が返す`func()`、`Route`が返す`(*mcp.ServerSession, error)`という名前・型はTask 1で定義した通りTask 3のcallHandlerで一貫して使われている。`gateway.New`の末尾引数`elicit *ElicitationRouter`もTask 3・Task 4で一貫。
