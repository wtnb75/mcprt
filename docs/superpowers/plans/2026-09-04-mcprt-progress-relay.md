# mcprt: tools/call progress通知中継 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `tools/call` に限り、downstreamクライアントが送る`progressToken`をbackendへの呼び出しに伝播し、backendが返す`notifications/progress`を元のtokenでdownstreamへ中継する。

**Architecture:** `internal/gateway`に新規`ProgressRegistry`（backend向けに払い出す内部token -> {downstream ServerSession, 元のtoken, 件数/最新messageの要約}の相関テーブル）を追加する。`callHandler`はdownstreamが`progressToken`付きで呼んできたときだけ`Register`して内部tokenをbackendへの呼び出しにセットし、`defer`でcleanupする。backend向け`mcp.Client`の`ProgressNotificationHandler`（`backend.ChangeCallbacks.OnProgress`経由）が`ProgressRegistry.Relay`を呼び、内部tokenから元のdownstream session/tokenを引いて中継する。呼び出し完了時、`logCall`に要約(`progressEntry.Summary()`)を渡し、1件以上あれば`progress_count`/`progress_last_message`をログに追加する。

設計書（下記Spec）は`gateway.New`/`connectBackends`に`*ProgressRegistry`を新パラメータとして通す案を示しているが、本プランでは`connectBackends`/`superviseBackends`/`superviseBackend`のシグネチャ変更は避け、既存の`gwHolder`（`internal/cli/server.go`）に`progress *gateway.ProgressRegistry`フィールドを1つ追加する形にする。理由: `connectBackends`は`internal/cli/call.go`・`internal/cli/list.go`からも`gwH=nil`で呼ばれており、そちらはprogress中継と無関係なので触れる必要がない。`gwHolder`は「backend supervisorがbackend接続完了後に参照する共有状態」を束ねる既存の置き場所であり、そこにもう1フィールド足すほうが、3関数のシグネチャとその全テスト呼び出し箇所（9箇所以上）を書き換えるより変更範囲が小さく、`gwH != nil`で「call/listでは常に無効」という設計書の意図もそのまま満たせる。`gateway.New`/`registerTool`/`callHandler`/`logCall`への`*ProgressRegistry`（または要約用の`*progressEntry`）追加は設計書どおり行う。

**Tech Stack:** Go 1.x, `github.com/modelcontextprotocol/go-sdk v1.7.0`（`mcp`パッケージ）, 標準の`go test`（`-race`込み）。

**Spec:** `docs/superpowers/specs/2026-08-25-mcprt-progress-relay-design.md`

## Global Constraints

- 対象は`tools/call`のみ。`resources/read`・`resources/templates/read`・`prompts/get`は対象外（既存の3ハンドラの`logCall`呼び出しには`progress`引数として`nil`を渡すだけで変更しない）。
- 進捗通知1件ごとの個別audit log記録はしない。呼び出し完了時に件数+最新messageの要約のみ`logCall`へ渡す。
- 停止した呼び出し（backendがハングして進捗も返さないケース）の検出・タイムアウトは実装しない（既存の`tools/call`自体のタイムアウト・キャンセル機構に委ねる）。相関エントリは呼び出し完了時に必ず`defer`で削除する。
- `go.mod`のモジュールパスは`github.com/wtnb75/mcprt`。SDKのパッケージ名は`mcp`（import path `github.com/modelcontextprotocol/go-sdk/mcp`）。

---

## ファイル構成

| ファイル | 種別 | 責務 |
|---|---|---|
| `internal/gateway/progress.go` | 新規 | `ProgressRegistry`/`progressEntry`: 内部token <-> downstream session/元tokenの相関、要約集計 |
| `internal/gateway/progress_test.go` | 新規 | `ProgressRegistry`の単体テスト（Register/Relay/Summary、cleanup後の無視、並行アクセス） |
| `internal/gateway/gateway.go` | 変更 | `Server.progress`フィールド追加、`New`/`registerTool`/`callHandler`に`*ProgressRegistry`を通す |
| `internal/gateway/reconcile.go` | 変更 | `updateToolsLocked`内の`registerTool`呼び出しに`s.progress`を渡す |
| `internal/gateway/audit.go` | 変更 | `logCall`に`progress *progressEntry`引数を追加し、件数>0のときログ属性を追加 |
| `internal/gateway/audit_test.go` | 変更 | 既存9箇所の`logCall`呼び出しに末尾`nil`を追加 |
| `internal/gateway/gateway_test.go` | 変更 | 既存13箇所の`gateway.New`呼び出しに末尾`nil`を追加。progress中継の統合テストを2件追加 |
| `internal/gateway/reconcile_test.go` | 変更 | 既存12箇所の`gateway.New`呼び出しに末尾`nil`を追加 |
| `internal/backend/backend.go` | 変更 | `ChangeCallbacks.OnProgress`フィールド追加、`Connect`内で`ClientOptions.ProgressNotificationHandler`に配線 |
| `internal/backend/backend_test.go` | 変更 | `OnProgress`が発火することを確認するテストを追加 |
| `internal/cli/server.go` | 変更 | `gwHolder.progress`フィールド追加、`buildGateway`で構築して`gateway.New`に渡す、`superviseBackend`のコールバック構築で`cb.OnProgress`を配線 |
| `internal/cli/server_internal_test.go` | 変更 | 既存3箇所の`gateway.New`呼び出しに末尾`nil`を追加（`gwHolder{}`は零値のままでよく、他の変更は不要） |
| `internal/cli/server_test.go` | 変更 | e2eテスト`TestServerCommand_RelaysToolCallProgress`を追加 |

---

## Task 1: ProgressRegistry コンポーネント

**Files:**
- Create: `internal/gateway/progress.go`
- Create: `internal/gateway/progress_test.go`

**Interfaces:**
- Produces: `gateway.NewProgressRegistry() *ProgressRegistry`、`(*ProgressRegistry).Register(session *mcp.ServerSession, originalToken any) (internalToken uint64, entry *progressEntry, cleanup func())`、`(*ProgressRegistry).Relay(ctx context.Context, logger *slog.Logger, params *mcp.ProgressNotificationParams)`、`(*progressEntry).Summary() (count int, lastMessage string)`。以降のタスクはこれらのシグネチャをそのまま使う。

- [ ] **Step 1: `internal/gateway/progress.go`を作成する**

```go
package gateway

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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

// progressEntry is one in-flight forwarded tools/call's correlation state.
// session/originalToken are set once at Register and never mutated;
// count/lastMessage are updated by Relay (called from the backend client's
// notification-handling goroutine) and read by Summary (called by
// callHandler, on a different goroutine, after CallTool returns) -- hence
// their own mutex, separate from the registry's.
type progressEntry struct {
	session       *mcp.ServerSession
	originalToken any

	mu          sync.Mutex
	count       int
	lastMessage string
}

// NewProgressRegistry returns an empty registry, ready to use.
func NewProgressRegistry() *ProgressRegistry {
	return &ProgressRegistry{entries: make(map[uint64]*progressEntry)}
}

// Register allocates a fresh internal token for one forwarded tools/call,
// remembers session/originalToken so a later Relay can find its way back,
// and returns the internal token to set on the outgoing CallToolParams,
// plus a cleanup func the caller must defer to remove the entry once the
// call returns (success or error). The returned *progressEntry can be read
// (via Summary) after cleanup to build the audit log line.
func (r *ProgressRegistry) Register(session *mcp.ServerSession, originalToken any) (internalToken uint64, entry *progressEntry, cleanup func()) {
	internalToken = r.next.Add(1)
	entry = &progressEntry{session: session, originalToken: originalToken}

	r.mu.Lock()
	r.entries[internalToken] = entry
	r.mu.Unlock()

	cleanup = func() {
		r.mu.Lock()
		delete(r.entries, internalToken)
		r.mu.Unlock()
	}
	return internalToken, entry, cleanup
}

// normalizeProgressToken converts a decoded JSON-RPC progress token into the
// uint64 form Register handed out, for comparison against the registry's
// keys. JSON numbers decode into float64 when the target type is `any`
// (the common case here, since the token round-trips through JSON on its
// way to and from the backend); int64/uint64 are accepted too in case a
// caller constructs params in-process without going through JSON. Anything
// else (including a negative number) reports ok=false.
func normalizeProgressToken(t any) (token uint64, ok bool) {
	switch v := t.(type) {
	case uint64:
		return v, true
	case int64:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case float64:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	default:
		return 0, false
	}
}

// Relay looks up params.ProgressToken (expected to be one Register handed
// out) and, if still registered, forwards params to the matching
// downstream ServerSession under its original token, and records the event
// in the entry's summary. A token no longer in the registry (the call
// already completed) or not decodable as a Register-issued token is
// silently dropped -- an expected race with a backend's last few in-flight
// notifications, not an error.
func (r *ProgressRegistry) Relay(ctx context.Context, logger *slog.Logger, params *mcp.ProgressNotificationParams) {
	token, ok := normalizeProgressToken(params.ProgressToken)
	if !ok {
		return
	}

	r.mu.Lock()
	entry, found := r.entries[token]
	r.mu.Unlock()
	if !found {
		return
	}

	entry.mu.Lock()
	entry.count++
	if params.Message != "" {
		entry.lastMessage = params.Message
	}
	entry.mu.Unlock()

	err := entry.session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
		ProgressToken: entry.originalToken,
		Progress:      params.Progress,
		Total:         params.Total,
		Message:       params.Message,
	})
	if err != nil {
		logger.Warn("progress relay: downstream NotifyProgress failed", "error", err)
	}
}

// Summary reports how many progress events were relayed for entry, and the
// most recent Message (empty if none carried one or none arrived).
func (e *progressEntry) Summary() (count int, lastMessage string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.count, e.lastMessage
}
```

- [ ] **Step 2: `internal/gateway/progress_test.go`を作成する**

`Relay`は実際に`entry.session.NotifyProgress`を呼ぶため、テストでは零値の`*mcp.ServerSession{}`ではなく、実際に接続された`*mcp.ServerSession`が要る。`mcp.NewServer`にツールを1つ登録し、そのハンドラ内で`req.Session`をキャプチャして使う。

```go
package gateway_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/gateway"
)

// newCapturedDownstreamSession spins up a tiny MCP server with one tool,
// connects a client to it (with progressCh wired as the client's
// ProgressNotificationHandler), calls the tool once to capture the
// server-side *mcp.ServerSession for that connection, and returns it plus
// progressCh and a cleanup func. This mirrors what callHandler sees as
// req.Session for a real downstream call.
func newCapturedDownstreamSession(t *testing.T) (session *mcp.ServerSession, progressCh <-chan *mcp.ProgressNotificationParams, cleanup func()) {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "probe", Version: "v1"}, nil)
	var captured *mcp.ServerSession
	srv.AddTool(&mcp.Tool{Name: "probe", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			captured = req.Session
			return &mcp.CallToolResult{}, nil
		})
	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))

	ch := make(chan *mcp.ProgressNotificationParams, 16)
	client := mcp.NewClient(&mcp.Implementation{Name: "probe-client", Version: "v1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			ch <- req.Params
		},
	})
	clientSession, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: httpSrv.URL}, nil)
	if err != nil {
		t.Fatalf("connect probe client: %v", err)
	}

	if _, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "probe", Arguments: map[string]any{}}); err != nil {
		t.Fatalf("call probe: %v", err)
	}
	if captured == nil {
		t.Fatal("probe tool handler never ran; no server-side session captured")
	}

	return captured, ch, func() {
		_ = clientSession.Close()
		httpSrv.Close()
	}
}

func TestProgressRegistry_RegisterRelaySummary(t *testing.T) {
	session, progressCh, cleanupSession := newCapturedDownstreamSession(t)
	defer cleanupSession()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := gateway.NewProgressRegistry()

	internalToken, entry, cleanup := reg.Register(session, "original-token")
	defer cleanup()

	reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: internalToken, Progress: 1, Total: 2, Message: "step1"})
	// A relay carrying the token as float64 (as it would arrive after a real
	// JSON round-trip) must resolve to the same entry.
	reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: float64(internalToken), Progress: 2, Total: 2, Message: "step2"})

	var got []*mcp.ProgressNotificationParams
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		select {
		case p := <-progressCh:
			got = append(got, p)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(got) != 2 {
		t.Fatalf("received %d progress notifications, want 2", len(got))
	}
	if got[0].ProgressToken != "original-token" || got[0].Message != "step1" {
		t.Fatalf("first notification = %+v, want ProgressToken=original-token Message=step1", got[0])
	}
	if got[1].ProgressToken != "original-token" || got[1].Message != "step2" {
		t.Fatalf("second notification = %+v, want ProgressToken=original-token Message=step2", got[1])
	}

	count, lastMessage := entry.Summary()
	if count != 2 || lastMessage != "step2" {
		t.Fatalf("Summary() = (%d, %q), want (2, \"step2\")", count, lastMessage)
	}
}

func TestProgressRegistry_RelayIgnoredAfterCleanup(t *testing.T) {
	session, progressCh, cleanupSession := newCapturedDownstreamSession(t)
	defer cleanupSession()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := gateway.NewProgressRegistry()

	internalToken, _, cleanup := reg.Register(session, "original-token")
	cleanup() // call already completed before any progress notification arrived

	reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: internalToken, Progress: 1, Message: "too-late"})

	select {
	case p := <-progressCh:
		t.Fatalf("received progress notification %+v after cleanup, want none relayed", p)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestProgressRegistry_RelayIgnoresUnknownToken(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := gateway.NewProgressRegistry()

	// No Register call at all: Relay must not panic on a nil session lookup.
	reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: uint64(999), Progress: 1})
	reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: "not-a-number", Progress: 1})
}

// TestProgressRegistry_ConcurrentRegisterRelayCleanup exercises Register,
// Relay, and cleanup from many goroutines at once against one shared
// session -- go test -race must find nothing.
func TestProgressRegistry_ConcurrentRegisterRelayCleanup(t *testing.T) {
	session, _, cleanupSession := newCapturedDownstreamSession(t)
	defer cleanupSession()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := gateway.NewProgressRegistry()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			internalToken, _, cleanup := reg.Register(session, i)
			defer cleanup()
			for j := 0; j < 3; j++ {
				reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: internalToken, Progress: float64(j)})
			}
		}(i)
	}
	wg.Wait()

	// The registry must still be usable afterward.
	internalToken, entry, cleanup := reg.Register(session, "final")
	defer cleanup()
	reg.Relay(context.Background(), logger, &mcp.ProgressNotificationParams{ProgressToken: internalToken, Progress: 1, Message: "final-msg"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if count, _ := entry.Summary(); count == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if count, lastMessage := entry.Summary(); count != 1 || lastMessage != "final-msg" {
		t.Fatalf("Summary() after concurrent stress = (%d, %q), want (1, \"final-msg\")", count, lastMessage)
	}
}
```

- [ ] **Step 3: テストを実行して確認する**

Run: `go test ./internal/gateway/... -run TestProgressRegistry -race -v`
Expected: `PASS`（`TestProgressRegistry_RegisterRelaySummary`・`TestProgressRegistry_RelayIgnoredAfterCleanup`・`TestProgressRegistry_RelayIgnoresUnknownToken`・`TestProgressRegistry_ConcurrentRegisterRelayCleanup`すべて）。

- [ ] **Step 4: パッケージ全体のビルド・vetを確認する**

Run: `go build ./... && go vet ./...`
Expected: エラーなし（`progress.go`は他のどこからもまだ参照されないので、既存コードに影響しない）。

- [ ] **Step 5: コミット**

```bash
git add internal/gateway/progress.go internal/gateway/progress_test.go
git commit -m "feat(gateway): add ProgressRegistry for tools/call progress correlation"
```

---

## Task 2: `backend.ChangeCallbacks.OnProgress` の配線

**Files:**
- Modify: `internal/backend/backend.go:27-53`
- Test: `internal/backend/backend_test.go`

**Interfaces:**
- Consumes: なし（`mcp.ClientOptions.ProgressNotificationHandler func(context.Context, *mcp.ProgressNotificationClientRequest)`はSDKの既存API）。
- Produces: `backend.ChangeCallbacks.OnProgress func(context.Context, *mcp.ProgressNotificationClientRequest)`。Task 3の`internal/cli/server.go`配線がこのフィールドを使う。

- [ ] **Step 1: `ChangeCallbacks`に`OnProgress`フィールドを追加する**

`internal/backend/backend.go`の`ChangeCallbacks`定義（27-36行目）を変更:

```go
// ChangeCallbacks are invoked when a connected backend reports that its
// tool/prompt/resource list has changed, or sends a progress notification
// for an in-flight call. Each OnXChanged func takes no arguments: MCP's
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
}
```

- [ ] **Step 2: `Connect`内で配線する**

`internal/backend/backend.go`の`Connect`（38-53行目、`clientOpts`組み立て部分）に追加:

```go
	if cb.OnResourcesChanged != nil {
		clientOpts.ResourceListChangedHandler = func(context.Context, *mcp.ResourceListChangedRequest) { cb.OnResourcesChanged() }
	}
	if cb.OnProgress != nil {
		clientOpts.ProgressNotificationHandler = cb.OnProgress
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "mcprt", Version: "v1"}, clientOpts)
```

（`ResourceListChangedHandler`のif節の直後、`mcp.NewClient`呼び出しの直前に挿入する。）

- [ ] **Step 3: 失敗するテストを書く**

`internal/backend/backend_test.go`の末尾（`TestConnect_PromptListChangedCallback`の近く、414行目`TestConnect_NilChangeCallbacks_NoHandlersRegistered`の前）に追加:

```go
// TestConnect_ProgressNotificationCallback checks that ChangeCallbacks.OnProgress
// fires with the backend's notifications/progress payload when the backend
// sends one mid-call, echoing back the progress token the client set on its
// request.
func TestConnect_ProgressNotificationCallback(t *testing.T) {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	fakeServer.AddTool(&mcp.Tool{Name: "slow", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if token := req.Params.GetProgressToken(); token != nil {
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: token, Progress: 1, Total: 2, Message: "working",
				})
			}
			return &mcp.CallToolResult{}, nil
		})

	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil))
	defer srv.Close()

	ctx := context.Background()
	received := make(chan *mcp.ProgressNotificationParams, 1)
	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL},
		backend.ChangeCallbacks{OnProgress: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			received <- req.Params
		}})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	params := &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{}}
	params.SetProgressToken("client-token")
	if _, err := b.Session.CallTool(ctx, params); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	select {
	case p := <-received:
		if p.ProgressToken != "client-token" || p.Message != "working" || p.Progress != 1 || p.Total != 2 {
			t.Fatalf("OnProgress params = %+v, want ProgressToken=client-token Progress=1 Total=2 Message=working", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnProgress did not fire within 5s of the backend sending a progress notification")
	}
}
```

- [ ] **Step 4: テストを実行して失敗を確認する**

Run: `go test ./internal/backend/... -run TestConnect_ProgressNotificationCallback -v`
Expected: FAIL（`ChangeCallbacks`に`OnProgress`フィールドがまだ存在しない、またはコンパイルエラー） -- Step 1/2をまだ適用していない場合。Step 1/2を先に適用した状態でこのテストを書いた場合は、そのままPASSするはずなので、Step 1・2の適用後にこのテストを流して確認する。

- [ ] **Step 5: テストを実行してパスすることを確認する**

Run: `go test ./internal/backend/... -run TestConnect -race -v`
Expected: `PASS`（新規テストに加え、既存の`TestConnect_ToolListChangedCallback`等も影響を受けず通ること）。

- [ ] **Step 6: パッケージ全体のビルドを確認する**

Run: `go build ./... && go vet ./...`
Expected: エラーなし。

- [ ] **Step 7: コミット**

```bash
git add internal/backend/backend.go internal/backend/backend_test.go
git commit -m "feat(backend): wire ChangeCallbacks.OnProgress to the client's ProgressNotificationHandler"
```

---

## Task 3: `gateway`パッケージへの統合（`callHandler`・`registerTool`・`New`・`logCall`）

**Files:**
- Modify: `internal/gateway/gateway.go` (Server struct: 66-88行目, New: 139-186行目, registerTool: 194-209行目, callHandler: 353-369行目, resourceReadHandler/resourceTemplateReadHandler/promptGetHandlerの`logCall`呼び出し: 245行目, 299行目, 343行目)
- Modify: `internal/gateway/reconcile.go:143`
- Modify: `internal/gateway/audit.go:115-146`
- Modify: `internal/gateway/audit_test.go` (9箇所の`logCall`呼び出し)
- Modify: `internal/gateway/gateway_test.go` (13箇所の`gateway.New`呼び出し + 新規テスト2件)
- Modify: `internal/gateway/reconcile_test.go` (12箇所の`gateway.New`呼び出し)

**Interfaces:**
- Consumes: Task 1の`gateway.NewProgressRegistry`/`ProgressRegistry.Register`/`ProgressRegistry.Relay`/`progressEntry.Summary`。
- Produces: `gateway.New(logger, backends, tables, entries, overrides, maskKeys, progress *ProgressRegistry) *Server`（末尾に`progress`引数を追加）。Task 4の`internal/cli/server.go`がこの新シグネチャで呼ぶ。

- [ ] **Step 1: `logCall`に`progress`引数を追加する**

`internal/gateway/audit.go`の`logCall`定義（115-146行目）を変更:

```go
// logCall logs one backend call's outcome — success or failure — in a
// consistent shape, so investigating an incident doesn't require treating
// the success and error paths as separate log formats.
// kind labels the log message ("tool"/"resource"/"resource template"/"prompt");
// nameKey is the field name for name ("tool"/"uri"/"prompt" — resource and
// resource template both use "uri"). args is nil for resource reads, which
// have no call arguments. progress is non-nil only for a tools/call that
// carried a downstream progressToken (see callHandler); a nil progress, or
// one whose Summary() count is zero, adds no progress fields to the log
// line.
func logCall(ctx context.Context, logger *slog.Logger, kind, nameKey, name, backend string, sess *mcp.ServerSession, args any, maskKeys []string, start time.Time, err error, progress *progressEntry) {
	attrs := []any{
		"backend", backend,
		nameKey, name,
		"session_id", sess.ID(),
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if ip := sess.InitializeParams(); ip != nil && ip.ClientInfo != nil {
		attrs = append(attrs, "client_name", ip.ClientInfo.Name, "client_version", ip.ClientInfo.Version)
	}
	if addr, ok := remoteAddrFromContext(ctx); ok {
		attrs = append(attrs, "remote_addr", addr)
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
	}
	if hasArgs(args) {
		attrs = append(attrs, "arguments", maskArguments(args, maskKeys))
	}
	if progress != nil {
		if count, lastMessage := progress.Summary(); count > 0 {
			attrs = append(attrs, "progress_count", count)
			if lastMessage != "" {
				attrs = append(attrs, "progress_last_message", lastMessage)
			}
		}
	}
	if err != nil {
		logger.Error(kind+" call failed", append(attrs, "error", err)...)
		return
	}
	logger.Info(kind+" call", attrs...)
}
```

- [ ] **Step 2: `gateway.go`の3箇所の非tool `logCall`呼び出しに`nil`を足す**

`internal/gateway/gateway.go`の3箇所を編集（対象外なので常に`nil`）:

245行目: `logCall(ctx, logger, "resource", "uri", originalURI, b.Name, req.Session, nil, maskKeys, start, err)` → 末尾に`, nil`を追加。
299行目: `logCall(ctx, logger, "resource template", "uri", req.Params.URI, b.Name, req.Session, nil, maskKeys, start, err)` → 末尾に`, nil`を追加。
343行目: `logCall(ctx, logger, "prompt", "prompt", originalName, b.Name, req.Session, req.Params.Arguments, maskKeys, start, err)` → 末尾に`, nil`を追加。

- [ ] **Step 3: `Server`構造体に`progress`フィールドを追加する**

`internal/gateway/gateway.go`の`Server`構造体（66-88行目）の`maskKeys []string`の直後に追加:

```go
type Server struct {
	mcp      *mcp.Server
	logger   *slog.Logger
	backends map[string]*backend.Backend
	maskKeys []string
	progress *ProgressRegistry

	mu sync.Mutex
	...
```

- [ ] **Step 4: `New`に`progress`引数を追加する**

`internal/gateway/gateway.go`の`New`（139-186行目）を変更:

```go
func New(logger *slog.Logger, backends map[string]*backend.Backend, tables Tables, entries Entries, overrides Overrides, maskKeys []string, progress *ProgressRegistry) *Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{Logger: logger})

	s := &Server{
		mcp:      mcpSrv,
		logger:   logger,
		backends: backends,
		maskKeys: maskKeys,
		progress: progress,

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
			registerTool(mcpSrv, logger, backends, resolved, maskKeys, progress)
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

- [ ] **Step 5: `registerTool`に`progress`引数を追加する**

`internal/gateway/gateway.go`の`registerTool`（194-209行目）を変更:

```go
func registerTool(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Tool], maskKeys []string, progress *ProgressRegistry) (ok bool) {
	candidates := append([]router.Candidate[*mcp.Tool]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addTool(srv, logger, c.Item, callHandler(logger, maskKeys, b, c.OriginalName, progress)) {
			return true
		}
	}
	logger.Error("tool unavailable: every candidate backend had an invalid definition", "tool", resolved.Item.Name)
	return false
}
```

- [ ] **Step 6: `callHandler`に進捗中継ロジックを追加する**

`internal/gateway/gateway.go`の`callHandler`（348-369行目）を変更:

```go
// callHandler forwards a tools/call to originalName on backend b, passing
// the raw arguments through unchanged. It wraps the call in a span
// (startCallSpan is a no-op for stdio-originated calls) and logs it via
// logCall, success or failure, so a dead or erroring backend — and normal
// usage — is visible to the operator. When progress is non-nil and the
// downstream request carries a progressToken, it registers a fresh
// correlation entry so a notifications/progress the backend sends mid-call
// (relayed via progress.Relay, wired through backend.ChangeCallbacks.
// OnProgress) reaches the downstream caller under its own token.
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

- [ ] **Step 7: `reconcile.go`の`registerTool`呼び出しに`s.progress`を渡す**

`internal/gateway/reconcile.go`の143行目を変更:

```go
		if !registerTool(s.mcp, s.logger, s.backends, resolved, s.maskKeys, s.progress) {
```

（これが無いと、`list_changed`での再登録や`ConnectBackend`によるreconnect時の再登録で作られる`callHandler`が`progress=nil`になり、そのtoolだけ進捗中継が効かなくなる。）

- [ ] **Step 8: `go build`でコンパイルエラーを洗い出し、機械的に直す**

Run: `go build ./... 2>&1 | head -60`

このビルドは、シグネチャを変えた`gateway.New`・`logCall`を呼んでいる全箇所でエラーになる。エラーは「too many arguments」ではなく「not enough arguments」の形で出る。対象は:

- `internal/gateway/gateway_test.go`: 13箇所の`gateway.New(...)`呼び出し（90, 124, 188, 251, 383, 436, 498, 548, 617, 683, 729, 792, 840, 908, 982, 1077行目 -- Read済みの内容と一致するもの全て）。それぞれ末尾の引数（`nil`または`[]string{"secret_value"}`）の後ろに`, nil)`を追加する。例（90行目）:
  - Before: `srv := gateway.New(logger, want, gateway.Tables{}, gateway.Entries{}, gateway.Overrides{}, nil)`
  - After: `srv := gateway.New(logger, want, gateway.Tables{}, gateway.Entries{}, gateway.Overrides{}, nil, nil)`
- `internal/gateway/reconcile_test.go`: 12箇所の`gateway.New(...)`呼び出し（65, 95, 138, 171, 199, 245, 293, 328, 360, 387, 421, 444行目付近）。いずれも複数行にまたがる呼び出しで、閉じ括弧の直前の引数（`gateway.Overrides{}`または類似の最終引数）の後ろに`, nil`を追加する。
- `internal/gateway/audit_test.go`: 9箇所の`logCall(...)`呼び出し（164, 197, 221, 239, 257, 324, 342行目付近、複数行にまたがるものも含む）。末尾の`err`引数（`nil`または`errors.New("boom")`）の後ろに`, nil)`を追加する。例（158-165行目）:
  - Before: `logCall(context.Background(), logger, "tool", "tool", "mytool", "backend-a", sess,\n\t\tjson.RawMessage(`{"user":"alice"}`), nil, start, nil)`
  - After: `logCall(context.Background(), logger, "tool", "tool", "mytool", "backend-a", sess,\n\t\tjson.RawMessage(`{"user":"alice"}`), nil, start, nil, nil)`

`go build`が通るまでこの機械的な追記を繰り返す。1回のビルドで複数のエラーが出るので、まとめて直してから再ビルドしてよい。

Expected: 最終的に`go build ./... && go vet ./...`がエラーなしで終わる。

- [ ] **Step 9: 統合テストを`gateway_test.go`に追加する**

`internal/gateway/gateway_test.go`の末尾に追加:

```go
// TestGateway_CallHandlerRelaysProgressAndLogsSummary checks the full
// tools/call progress-relay path: a downstream call carrying a
// progressToken gets a fresh internal token forwarded to the backend, the
// backend's notifications/progress (sent via req.Session.NotifyProgress in
// the fake backend's tool handler) is relayed back to the downstream
// client under its ORIGINAL token, and the resulting "tool call" audit log
// line carries progress_count/progress_last_message.
func TestGateway_CallHandlerRelaysProgressAndLogsSummary(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	backendServer := mcp.NewServer(&mcp.Implementation{Name: "backend-a", Version: "v1"}, nil)
	backendServer.AddTool(&mcp.Tool{Name: "progressive", Description: "progressive", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if token := req.Params.GetProgressToken(); token != nil {
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: 1, Total: 2, Message: "half"})
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: 2, Total: 2, Message: "done"})
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL}, backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-a", Items: toolsA}}, toolNameOf, toolRename, nil)

	progressReg := gateway.NewProgressRegistry()
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, gateway.Entries{}, gateway.Overrides{}, nil, progressReg)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	progressCh := make(chan *mcp.ProgressNotificationParams, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			progressCh <- req.Params
		},
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	params := &mcp.CallToolParams{Name: "progressive", Arguments: map[string]any{}}
	params.SetProgressToken("downstream-token")
	if _, err := session.CallTool(ctx, params); err != nil {
		t.Fatalf("call progressive: %v", err)
	}

	var got []*mcp.ProgressNotificationParams
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		select {
		case p := <-progressCh:
			got = append(got, p)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(got) != 2 {
		t.Fatalf("received %d progress notifications, want 2", len(got))
	}
	if got[0].ProgressToken != "downstream-token" || got[0].Message != "half" {
		t.Fatalf("first notification = %+v, want ProgressToken=downstream-token Message=half", got[0])
	}
	if got[1].ProgressToken != "downstream-token" || got[1].Message != "done" {
		t.Fatalf("second notification = %+v, want ProgressToken=downstream-token Message=done", got[1])
	}

	rec := findLogLine(t, buf.String(), "tool call")
	countVal, ok := rec["progress_count"].(float64) // JSON numbers decode as float64
	if !ok || countVal != 2 {
		t.Fatalf("progress_count = %v, want 2", rec["progress_count"])
	}
	if rec["progress_last_message"] != "done" {
		t.Fatalf("progress_last_message = %v, want done", rec["progress_last_message"])
	}
}

// TestGateway_CallHandlerNoProgressTokenSkipsRelay checks that a tools/call
// without a progressToken is unaffected by a non-nil ProgressRegistry: no
// relay happens, and the audit log carries no progress fields.
func TestGateway_CallHandlerNoProgressTokenSkipsRelay(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	backendServer := newFakeBackendServer("backend-a", "plain")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL}, backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-a", Items: toolsA}}, toolNameOf, toolRename, nil)

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, gateway.Entries{}, gateway.Overrides{}, nil, gateway.NewProgressRegistry())

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "plain", Arguments: map[string]any{}}); err != nil {
		t.Fatalf("call plain: %v", err)
	}

	rec := findLogLine(t, buf.String(), "tool call")
	if _, ok := rec["progress_count"]; ok {
		t.Fatalf("log line %v has progress_count, want it omitted for a call with no progressToken", rec)
	}
}
```

- [ ] **Step 10: 新規テストを実行する**

Run: `go test ./internal/gateway/... -run TestGateway_CallHandler -race -v`
Expected: `PASS`（両テストとも）。

- [ ] **Step 11: パッケージ全体のテストを実行する**

Run: `go test ./internal/gateway/... -race`
Expected: `ok`（既存テストを含め全てPASS -- Step 8の機械的な引数追加が正しく行われていることの確認）。

- [ ] **Step 12: コミット**

```bash
git add internal/gateway/gateway.go internal/gateway/reconcile.go internal/gateway/audit.go \
        internal/gateway/audit_test.go internal/gateway/gateway_test.go internal/gateway/reconcile_test.go
git commit -m "feat(gateway): relay tools/call progress notifications through callHandler"
```

---

## Task 4: `internal/cli/server.go`への配線とe2eテスト

**Files:**
- Modify: `internal/cli/server.go` (`gwHolder`: 304-306行目, `buildGateway`: 252-291行目, `superviseBackend`: 511-519行目)
- Modify: `internal/cli/server_internal_test.go` (3箇所の`gateway.New`呼び出し)
- Modify: `internal/cli/server_test.go` (e2eテスト追加)

**Interfaces:**
- Consumes: Task 3の`gateway.New(..., progress *gateway.ProgressRegistry)`、Task 1の`gateway.NewProgressRegistry()`、Task 2の`backend.ChangeCallbacks.OnProgress`。
- Produces: なし（末端の配線。他タスクから消費されない）。

- [ ] **Step 1: `gwHolder`に`progress`フィールドを追加する**

`internal/cli/server.go`の`gwHolder`定義（296-306行目）を変更:

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
// progress is set once, before connectBackends spawns any supervisor
// goroutine, and never mutated afterward -- so reading it from those
// goroutines needs no lock (the write happens-before every goroutine's
// creation). A nil progress (buildGateway always sets one; only some tests
// construct a bare gwHolder{} without it) means "no progress relay for this
// generation," matching a nil *gateway.ProgressRegistry everywhere else.
type gwHolder struct {
	ptr      atomic.Pointer[gateway.Server]
	progress *gateway.ProgressRegistry
}
```

- [ ] **Step 2: `buildGateway`で`ProgressRegistry`を構築し、`gateway.New`に渡す**

`internal/cli/server.go`の`buildGateway`（247-294行目）を変更。まず`var gwH gwHolder`の直後（252行目）に追加:

```go
	var gwH gwHolder
	gwH.progress = gateway.NewProgressRegistry()
	conn := connectBackends(ctx, logger, cfg.Backends, &gwH)
```

次に`gateway.New(...)`呼び出し（275-290行目）の最終引数に`gwH.progress`を追加:

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
	}, cfg.Logging.MaskKeys, gwH.progress)
	gwH.ptr.Store(srv)
```

- [ ] **Step 3: `superviseBackend`のコールバック構築で`OnProgress`を配線する**

`internal/cli/server.go`の`superviseBackend`内のコールバック構築（511-519行目）を変更:

```go
	cb := backend.ChangeCallbacks{}
	if gwH != nil {
		cb = backend.ChangeCallbacks{
			OnToolsChanged:     toolsChangedCallback(ctx, logger, bc.Name, gwH),
			OnResourcesChanged: resourcesChangedCallback(ctx, logger, bc.Name, gwH),
			OnPromptsChanged:   promptsChangedCallback(ctx, logger, bc.Name, gwH),
		}
		if gwH.progress != nil {
			cb.OnProgress = func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
				gwH.progress.Relay(ctx, logger, req.Params)
			}
		}
	}
```

- [ ] **Step 4: `go build`でコンパイルエラーを洗い出し、既存の`gateway.New`呼び出しを直す**

Run: `go build ./... 2>&1`

`internal/cli/server_internal_test.go`の3箇所（362-363, 502, 780-781行目付近）の`gateway.New(...)`呼び出しが「not enough arguments」で失敗する。それぞれ末尾に`, nil`を追加する（これらのテストは`&gwHolder{}`を直接構築しており`progress`フィールドは零値`nil`のままでよいので、`gateway.New`側も`nil`を渡せばよい -- 進捗中継はこれらのテストの検証対象ではない）。例（362-363行目）:

- Before:
  ```go
  srv := gateway.New(logger, conn.backends, gateway.Tables{Tools: toolTable},
  	gateway.Entries{Tools: conn.toolEntries}, gateway.Overrides{}, nil)
  ```
- After:
  ```go
  srv := gateway.New(logger, conn.backends, gateway.Tables{Tools: toolTable},
  	gateway.Entries{Tools: conn.toolEntries}, gateway.Overrides{}, nil, nil)
  ```

502行目・780-781行目も同様に末尾へ`, nil`を追加する。

Expected: `go build ./... && go vet ./...`がエラーなしで終わる。

- [ ] **Step 5: 既存の`internal/cli`テストを実行して壊れていないことを確認する**

Run: `go test ./internal/cli/... -race`
Expected: `ok`（list_changed・reconnect等の既存e2e/内部テストが全てPASS）。

- [ ] **Step 6: e2eテストを`server_test.go`に追加する**

`internal/cli/server_test.go`の末尾（`TestServerCommand_ToolsListChanged_ReListFailureKeepsPreviousList`の後）に追加:

```go
// TestServerCommand_RelaysToolCallProgress checks the progress-relay
// feature end-to-end through the real server command: a downstream
// tools/call carrying a progressToken reaches a backend that sends two
// notifications/progress mid-call, and both arrive at the downstream
// client under its ORIGINAL progressToken.
func TestServerCommand_RelaysToolCallProgress(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	backendSrv.AddTool(&mcp.Tool{Name: "slow", Description: "slow", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if token := req.Params.GetProgressToken(); token != nil {
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: 1, Total: 2, Message: "step1"})
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: 2, Total: 2, Message: "step2"})
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
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

	progressCh := make(chan *mcp.ProgressNotificationParams, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			progressCh <- req.Params
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
	waitForToolNames(t, ctx, session, []string{"slow"})

	params := &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{}}
	params.SetProgressToken("original-token")
	if _, err := session.CallTool(ctx, params); err != nil {
		t.Fatalf("call slow: %v", err)
	}

	var got []*mcp.ProgressNotificationParams
	deadline = time.Now().Add(5 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		select {
		case p := <-progressCh:
			got = append(got, p)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(got) != 2 {
		t.Fatalf("received %d progress notifications, want 2", len(got))
	}
	if got[0].ProgressToken != "original-token" || got[0].Message != "step1" {
		t.Fatalf("first progress = %+v, want ProgressToken=original-token Message=step1", got[0])
	}
	if got[1].ProgressToken != "original-token" || got[1].Message != "step2" {
		t.Fatalf("second progress = %+v, want ProgressToken=original-token Message=step2", got[1])
	}

	_ = session.Close()
	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}
```

`waitForToolNames`は`TestServerCommand_PropagatesToolsListChanged`が既に使っている既存ヘルパーをそのまま使う（新規追加不要）。

- [ ] **Step 7: 新規e2eテストを実行する**

Run: `go test ./internal/cli/... -run TestServerCommand_RelaysToolCallProgress -race -v`
Expected: `PASS`。

- [ ] **Step 8: モジュール全体のテストを実行する**

Run: `go build ./... && go vet ./... && go test ./... -race`
Expected: 全パッケージ`ok`。

- [ ] **Step 9: コミット**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go internal/cli/server_test.go
git commit -m "feat(cli): wire ProgressRegistry into the server command's backend supervision"
```

---

## Self-Review メモ（実行者向けではなく記録用）

- Spec coverage: スコープに含まれる4項目（progressToken伝播・中継・複数同時呼び出しの相関・audit log要約）は全てTask 1〜3でカバー。スコープ外（resources/prompts中継、個別audit log、タイムアウト検出）は実装しない。
- 設計書と異なる点: `connectBackends`/`superviseBackends`/`superviseBackend`のシグネチャ変更を避け、`gwHolder`に`progress`フィールドを追加する方式にした（Architecture節に理由を明記）。挙動としては設計書のエラーハンドリング表（`progress`が`nil`のときcallHandlerは進捗関連処理をスキップする等）と完全に一致する。
- 型整合性: `ProgressRegistry.Register`が返す`(uint64, *progressEntry, func())`、`Relay(ctx, logger, *mcp.ProgressNotificationParams)`、`Summary() (int, string)`という名前・型はTask 1で定義した通りTask 3のcallHandler/logCallで一貫して使われている。`gateway.New`の末尾引数`progress *ProgressRegistry`もTask 3・Task 4で一貫。
