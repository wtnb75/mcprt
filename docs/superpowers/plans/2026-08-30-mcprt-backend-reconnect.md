# mcprt backend 切断検知・自動再接続 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** backendへの接続を継続的に監視し、切断を検知したら該当backendの全項目をルーティングテーブルから除去、バックグラウンドで指数backoffによる無期限再接続を試み、成功したら自動的に復帰させる。起動時に接続失敗したbackendも同じリトライループの対象にする。

**Architecture:** `internal/cli/server.go`の`connectBackends`を、全backend分の永続監視goroutine（`superviseBackend`）をspawnする構造に置き換える。各`superviseBackend`は`connectAndList`（1回分の接続+List）→（初回接続の間だけ）呼び出し元へreport、または`gateway.Server.ConnectBackend`（新規メソッド）で復帰→`Session.Wait()`で切断検知→該当backendの項目を除去→リトライ、を無限ループする。`buildGateway`/`connectBackends`のシグネチャは変更しない — 引数の`ctx`は既にhot-reloadの世代(generation)スコープの`genCtx`なので、supervisorは自然にその世代と運命を共にする。

**Tech Stack:** Go 1.25, `github.com/modelcontextprotocol/go-sdk/mcp` v1.7.0

**Spec:** docs/superpowers/specs/2026-08-25-mcprt-backend-reconnect-design.md

## 重要: このspecはhot-reload実装(2026-08-29 plan)より前に書かれている

specの本文・アーキテクチャ図は`gwHolder`を`runServer`が直接構築する前提（hot-reload導入前の`connectBackends`の姿）で書かれている。しかし2026-08-29の実装により、`internal/cli/server.go`は既に`buildGateway(ctx, logger, cfg) (*gateway.Server, error)`に一本化され、`gwHolder`は`buildGateway`内のローカル変数になっている（`connectBackends(ctx, logger, cfg.Backends, &gwH)`という呼び出し）。**このplanは指示に反する変更を一切加えない**: `buildGateway`・`connectBackends`のシグネチャは今のまま、内部実装だけを変える。理由:

- `buildGateway`は起動時（`runServer`から`genCtx`付きで）とSIGHUP reload時（`watchSIGHUP`から新しい`genCtx`付きで）の両方から呼ばれる。今`connectBackends`が受け取る`ctx`は、どちらの呼び出しでも既に「その世代（generation）のライフタイムだけ生きる`genCtx`」になっている。
- specの`superviseBackend`は「ctx is the long-lived server context ... プロセス生存中ずっと動く」と書いているが、これはhot-reload導入前の話。今の`genCtx`はプロセスではなく**世代**のライフタイムを持つ。この違いは実は都合が良い: `superviseBackend`がそのまま`genCtx`を使えば、世代が置き換えられた（SIGHUP reloadで`scheduleDrain`が呼ばれた、またはプロセスshutdown）ときに`genCtx`がキャンセルされ、supervisorは新しいコードなしで自然に終了する。詳しい追跡は以下の通り:
  1. reloadで世代が置き換わると`scheduleDrain`が即座に`oldGenCancel()`を呼ぶ（`genCtx`キャンセル）。
  2. しかしgo-sdkのstreamable HTTPクライアントは接続のcontextをConnect時のcontextから切り離す（`internal/cli/server.go`の既存コメント参照）ので、`genCtx`キャンセルだけでは生きている接続は閉じない。`c.backend.Session.Wait()`はブロックされたまま。
  3. `scheduleDrain`の`time.AfterFunc(reloadDrainTimeout, ...)`が発火してその世代の全backendを強制`Close()`すると、`Session.Wait()`がようやく復帰する。
  4. supervisorのループは次に`connectAndList(ctx, ...)`を試みるが、`ctx`（`genCtx`）は既にキャンセル済みなので即座に失敗し、backoff待ちの`select`が`<-ctx.Done()`を拾って即returnする。
  5. プロセスshutdown時も同様: `runServer`の`cancel()`が全ての`genCtx`の親を辿ってキャンセルし、`live.takeAll()`が全世代のbackendを`Close()`する。同じ経路でsupervisorは終了する。
  - この一連の流れは新規コード不要（`ctx.Done()`の自然な帰結）。ただし**テストでは`ctx`を必ずキャンセル可能にすること** — `context.Background()`を渡したまま`connectBackends`を呼ぶテストは、supervisor goroutineとその再試行（stdioなら子プロセスの再spawnも）がテストバイナリの生存中ずっとリークする。既存の4つの`TestConnectBackends_*`テストは全て`context.Background()`を直接渡しているため、Task 2でキャンセル可能なcontextに直す。

## Global Constraints

- 含める: backendごとの永続監視goroutine（`Session.Wait()`利用）による切断検知。切断時、該当backendの全項目（tools/resources/resource templates/prompts）をルーティングテーブルから除去。指数backoff（初期1秒・上限60秒、上限なしリトライ）による無期限自動再接続。起動時に接続（または`ListTools`）に失敗したbackendも同じ監視・リトライ対象に含める。再接続成功時、該当backendを1回のreconcileで復帰させる。
- 含めない: configのホットリロード連携の新規実装（既存のhot-reload実装とは共存するのみで変更しない）。リトライ間隔・上限のYAML設定への露出（`backendConnectTimeout`と同じくハードコードされたpackage変数、テスト用に上書き可能）。複数backendの同時切断・復旧のjitter/協調制御。`ListResources`/`ListResourceTemplates`/`ListPrompts`の失敗をリトライ対象にすること（現状通りソフト失敗＝空リスト扱いのまま）。
- `buildGateway(ctx, logger, cfg) (*gateway.Server, error)`と`connectBackends(ctx, logger, configs, gwH) connected`のシグネチャは変更しない。
- `gwH *gwHolder`は`nil`でありうる（既存の複数テストが`nil`を渡す）。`superviseBackend`・`connectAndList`は`gwH == nil`のとき、gatewayへのreconcile呼び出しを一切スキップして安全に動作すること。
- `go test -race ./...`がグリーンであること。外部サービス依存なし（切断シミュレーションはテスト用HTTPサーバーの`Close()`／`CloseClientConnections()`で完結させる）。
- テストで意図的に切断をトリガーする際、`httptest.Server.CloseClientConnections()`（コネクションのリセットのみ）はgo-sdkのstreamable HTTPクライアント自身が「一時的障害」として**内部的に自動再接続してしまう**ため、`superviseBackend`の再接続ロジックを検証する目的には使えない（go-sdk v1.7.0の`mcp/streamable_client.go`のドキュメントコメント: "Network interruption during SSE streaming ... Triggers reconnection" は transient/recoverable 扱い）。`superviseBackend`の切断検知を確実にトリガーするには、クライアント側で`(*backend.Backend).Close()`を呼ぶ（"Context cancellation: Client closed connection"はterminal扱い、既存の`TestGateway_CallOnDeadBackendReturnsError`と同じ手法）か、テスト用サーバー自体を`Close()`で完全に停止させること。

---

## Task 1: `internal/gateway/reconcile.go` — `upsertEntry` と `Server.ConnectBackend`

**Files:**
- Modify: `internal/gateway/reconcile.go`（末尾に追加）
- Test (internal, unexported helperの単体テスト): Create `internal/gateway/reconcile_internal_test.go`
- Test (external, `ConnectBackend`の統合テスト): Modify `internal/gateway/reconcile_test.go`

**Interfaces:**
- Consumes: 既存の`Server.mu`/`s.backends`/`s.toolEntries`等のフィールド（同package内）、既存の`replaceEntry`（変更しない）、既存の`UpdateTools`/`UpdateResources`/`UpdatePrompts`（変更しない）。
- Produces: `func (s *Server) ConnectBackend(name string, b *backend.Backend, prefix string, tools []*mcp.Tool, resources []*mcp.Resource, templates []*mcp.ResourceTemplate, prompts []*mcp.Prompt)` — Task 2の`superviseBackend`がこれを呼ぶ。

- [ ] **Step 1: `upsertEntry`の失敗するテストを書く**

`internal/gateway/reconcile_internal_test.go`を新規作成:

```go
package gateway

import (
	"testing"

	"github.com/wtnb75/mcprt/internal/router"
)

func TestUpsertEntry_ReplacesExistingBackendName(t *testing.T) {
	entries := []router.Entry[string]{
		{BackendName: "a", Prefix: "a-", Items: []string{"old"}},
		{BackendName: "b", Prefix: "b-", Items: []string{"other"}},
	}
	got := upsertEntry(entries, "a", "a-", []string{"new"})
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].BackendName != "a" || got[0].Prefix != "a-" || len(got[0].Items) != 1 || got[0].Items[0] != "new" {
		t.Fatalf("got[0] = %+v, want {a a- [new]}", got[0])
	}
	if got[1].BackendName != "b" || len(got[1].Items) != 1 || got[1].Items[0] != "other" {
		t.Fatalf("got[1] = %+v, want unchanged {b b- [other]}", got[1])
	}
	// entries itself must be unmodified -- upsertEntry copies, like replaceEntry.
	if entries[0].Items[0] != "old" {
		t.Fatalf("original entries mutated: entries[0] = %+v", entries[0])
	}
}

func TestUpsertEntry_AppendsUnknownBackendName(t *testing.T) {
	entries := []router.Entry[string]{
		{BackendName: "a", Prefix: "a-", Items: []string{"x"}},
	}
	got := upsertEntry(entries, "new-backend", "nb-", []string{"y", "z"})
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[1].BackendName != "new-backend" || got[1].Prefix != "nb-" || len(got[1].Items) != 2 || got[1].Items[0] != "y" || got[1].Items[1] != "z" {
		t.Fatalf("got[1] = %+v, want {new-backend nb- [y z]}", got[1])
	}
}

func TestUpsertEntry_EmptyEntriesAppends(t *testing.T) {
	got := upsertEntry[string](nil, "only", "o-", []string{"a"})
	if len(got) != 1 || got[0].BackendName != "only" || got[0].Prefix != "o-" || len(got[0].Items) != 1 || got[0].Items[0] != "a" {
		t.Fatalf("got = %+v, want one entry {only o- [a]}", got)
	}
}
```

- [ ] **Step 2: テストを実行して失敗を確認**

Run: `go test ./internal/gateway/... -run TestUpsertEntry -v`
Expected: FAIL（`upsertEntry` undefined）

- [ ] **Step 3: `upsertEntry`を実装**

`internal/gateway/reconcile.go`の末尾（`UpdatePrompts`の後）に追加:

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
```

- [ ] **Step 4: テストを実行して成功を確認**

Run: `go test ./internal/gateway/... -run TestUpsertEntry -v`
Expected: PASS

- [ ] **Step 5: `ConnectBackend`の失敗するテストを書く**

`internal/gateway/reconcile_test.go`の末尾（`equalStrings`の前でも後でもよいが、末尾に追加）に追加。既存の`toolSchema`/`toolNameOf`/`toolRename`/`downstreamToolNames`（gateway_test.goで定義済み）を再利用する:

```go
// TestConnectBackend_AddsNewBackendNotInEntries checks that ConnectBackend
// registers a backend that was never part of the Server's founding entries
// -- the case of a backend that failed to connect at mcprt startup and
// joins later via superviseBackend (see internal/cli/server.go).
func TestConnectBackend_AddsNewBackendNotInEntries(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "existing", InputSchema: toolSchema}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"a": {Name: "a"}},
		gateway.Tables{Tools: table}, gateway.Entries{Tools: entries}, gateway.Overrides{}, nil)

	newConn := &backend.Backend{Name: "new"}
	srv.ConnectBackend("new", newConn, "new-",
		[]*mcp.Tool{{Name: "fresh", InputSchema: toolSchema}}, nil, nil, nil)

	if srv.Backends()["new"] != newConn {
		t.Fatalf("Backends()[\"new\"] = %v, want %v", srv.Backends()["new"], newConn)
	}
	if got := downstreamToolNames(t, ctx, srv.MCP()); !equalStrings(got, []string{"existing", "fresh"}) {
		t.Fatalf("tools after ConnectBackend = %v, want [existing fresh]", got)
	}
}

// TestConnectBackend_ReconnectsExistingBackend checks that ConnectBackend
// replaces an already-known backend's connection and item set -- the case
// of a backend that disconnected and successfully reconnected.
func TestConnectBackend_ReconnectsExistingBackend(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Prefix: "a-", Items: []*mcp.Tool{{Name: "old", InputSchema: toolSchema}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	oldConn := &backend.Backend{Name: "a"}
	srv := gateway.New(logger, map[string]*backend.Backend{"a": oldConn},
		gateway.Tables{Tools: table}, gateway.Entries{Tools: entries}, gateway.Overrides{}, nil)

	// Simulate a disconnect (as superviseBackend does) before the reconnect.
	srv.UpdateTools("a", nil)
	if got := downstreamToolNames(t, ctx, srv.MCP()); len(got) != 0 {
		t.Fatalf("tools after simulated disconnect = %v, want []", got)
	}

	newConn := &backend.Backend{Name: "a"}
	srv.ConnectBackend("a", newConn, "a-",
		[]*mcp.Tool{{Name: "reconnected", InputSchema: toolSchema}}, nil, nil, nil)

	if srv.Backends()["a"] != newConn {
		t.Fatalf("Backends()[\"a\"] = %v, want the new connection %v", srv.Backends()["a"], newConn)
	}
	if got := downstreamToolNames(t, ctx, srv.MCP()); !equalStrings(got, []string{"reconnected"}) {
		t.Fatalf("tools after ConnectBackend reconnect = %v, want [reconnected]", got)
	}
}

// TestConnectBackend_ReconnectIntroducingConflictLogsIt checks that a
// reconnect which introduces a brand-new name conflict logs it, exactly
// like UpdateTools does (ConnectBackend delegates to UpdateTools for the
// actual reconcile).
func TestConnectBackend_ReconnectIntroducingConflictLogsIt(t *testing.T) {
	var buf logBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "search", InputSchema: toolSchema}}},
		{BackendName: "b", Items: nil}, // "b" starts with nothing, matching UpdateTools' precedent
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"a": {Name: "a"}, "b": {Name: "b"}},
		gateway.Tables{Tools: table}, gateway.Entries{Tools: entries}, gateway.Overrides{}, nil)

	buf.reset()
	srv.ConnectBackend("b", &backend.Backend{Name: "b"}, "",
		[]*mcp.Tool{{Name: "search", InputSchema: toolSchema}}, nil, nil, nil)
	if !buf.contains("tool name conflict") {
		t.Fatalf("log output = %q, want a \"tool name conflict\" warning", buf.String())
	}
}

// TestConnectBackend_ConcurrentWithUpdateToolsDoesNotRace checks that
// ConnectBackend for a brand-new backend and UpdateTools for an existing
// one can run concurrently without racing -- superviseBackend spawns one
// goroutine per backend, so a new backend's first connect can land at the
// same moment another backend's list_changed fires.
func TestConnectBackend_ConcurrentWithUpdateToolsDoesNotRace(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "x", InputSchema: toolSchema}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"a": {Name: "a"}},
		gateway.Tables{Tools: table}, gateway.Entries{Tools: entries}, gateway.Overrides{}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			srv.UpdateTools("a", []*mcp.Tool{{Name: "x", InputSchema: toolSchema}, {Name: "extra", InputSchema: toolSchema}})
		}()
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("new-%d", i)
			srv.ConnectBackend(name, &backend.Backend{Name: name}, "",
				[]*mcp.Tool{{Name: fmt.Sprintf("tool-%d", i), InputSchema: toolSchema}}, nil, nil, nil)
		}(i)
	}
	wg.Wait()
}
```

`reconcile_test.go`の先頭のimportに`"fmt"`を追加する必要がある（他のimportは既存のもので足りる）。

- [ ] **Step 6: テストを実行して失敗を確認**

Run: `go test ./internal/gateway/... -run TestConnectBackend -v`
Expected: FAIL（`ConnectBackend` undefined）

- [ ] **Step 7: `ConnectBackend`を実装**

`internal/gateway/reconcile.go`の末尾（`upsertEntry`の後）に追加。ファイル先頭のimportに`"github.com/wtnb75/mcprt/internal/backend"`を追加すること（現状のreconcile.goはbackendパッケージをimportしていない）:

```go
// ConnectBackend registers (or re-registers) backendName's live connection
// and reconciles its full item set -- used both when a backend that failed
// to connect at mcprt startup finally connects for the first time, and
// when a previously-connected backend reconnects after a disconnect (see
// internal/cli/server.go's superviseBackend). prefix is the backend's
// configured Prefix (from config.BackendConfig; resources and resource
// templates never carry one, matching connectBackends' existing
// convention).
//
// This first ensures an entry (seeded with nil Items) exists for
// backendName under s.mu, in one atomic step with setting s.backends[name]
// -- then delegates the actual item reconciliation to the existing
// UpdateTools/UpdateResources/UpdatePrompts, each of which takes its own
// lock. A backend name is only ever driven by one supervisor goroutine at
// a time (see internal/cli/server.go), so nothing else touches this
// backend's entry between the two phases.
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

- [ ] **Step 8: テストを実行して成功を確認**

Run: `go test ./internal/gateway/... -run 'TestConnectBackend|TestUpsertEntry' -v -race`
Expected: PASS（全ケース）

- [ ] **Step 9: パッケージ全体のテストと静的チェック**

Run:
```bash
go build ./...
go vet ./internal/gateway/...
gofmt -l internal/gateway/
go test ./internal/gateway/... -race
```
Expected: すべてクリーン（`gofmt -l`は出力なし）

- [ ] **Step 10: コミット**

```bash
git add internal/gateway/reconcile.go internal/gateway/reconcile_internal_test.go internal/gateway/reconcile_test.go
git commit -m "feat(gateway): add ConnectBackend for backend reconnect/late-join reconcile"
```

---

## Task 2: `internal/cli/server.go` — `superviseBackend`/`connectAndList` と `connectBackends`の書き換え

**Files:**
- Modify: `internal/cli/server.go`（`connectBackends`の実装を置き換え、`connectResult`/`connectAndList`/`superviseBackend`/`backendBackoffMin`/`backendBackoffMax`を追加）
- Test: Modify `internal/cli/server_internal_test.go`（既存4テストの`context.Background()`をキャンセル可能なcontextに直し、新規テストを追加）

**Interfaces:**
- Consumes: Task 1の`gateway.Server.ConnectBackend`。既存の`gwHolder`型（server.go内、変更しない）。既存の`toolsChangedCallback`/`resourcesChangedCallback`/`promptsChangedCallback`（変更しない）。既存の`backend.Connect`/`backend.ChangeCallbacks`/`(*backend.Backend).ListTools`等/`(*backend.Backend).Close`/`(*backend.Backend).Session.Wait()`（すべて変更しない）。
- Produces: `connectBackends(ctx, logger, configs, gwH) connected`は**シグネチャ不変**、内部実装のみ変更。新規: `superviseBackend(ctx context.Context, logger *slog.Logger, bc config.BackendConfig, gwH *gwHolder, onFirstConnect func(*connectResult))`（外部から直接呼ばれるのはこのplan内のテストのみ）。`backendBackoffMin`/`backendBackoffMax`（package var、テスト用に上書き可能）。

### 設計判断: `onFirstConnect`呼び出し条件

specの疑似コードは「`first`という一度きりのbool」で`onFirstConnect`呼び出しを制御しているが、これには落とし穴がある: `backendConnectTimeout`のcollectウィンドウを過ぎてから初めて接続に成功したbackendが、`first==true`のまま`onFirstConnect`を呼んでしまうと、その結果を受け取る`connectBackends`の収集ループは既にreturn済みで誰も読んでいない ―― そのbackendのデータが（次に切断・再接続するまで）永久に失われる。

正しい条件は「`first`かどうか」ではなく「**`gwH.ptr.Load()`がまだnilかどうか**」: `buildGateway`は`connectBackends`が返ってからごく短時間（router.Resolveの計算のみ、I/Oなし）で`gwH.ptr.Store(srv)`を呼ぶので、collectウィンドウを過ぎてから成功した接続は、ほぼ確実に`gwH.ptr`が既に非nilになっている ―― その場合は`gw.ConnectBackend(...)`を呼べばよく、`onFirstConnect`を呼ぶ必要はない。`onFirstConnect`が実際に呼ばれるのは「`gwH.ptr`がまだnil」の間だけであり、これは`connectBackends`が呼び出し元にreturnする前の、極めて短いウィンドウに限られる。

- [ ] **Step 1: 既存4テストを、supervisor goroutineがテスト終了後にリークしないよう修正する（テストのみの変更、まず実施）**

`internal/cli/server_internal_test.go`の`TestConnectBackends_TimesOutHungBackend`・`TestConnectBackends_ResourceListFailureKeepsBackendTools`・`TestConnectBackends_PromptListFailureKeepsBackendTools`・`TestConnectBackends_LogsSuccessfulConnect`は、いずれも`connectBackends(context.Background(), ...)`のように**キャンセルされない**contextを渡している。Task 2で`connectBackends`が「supervisor goroutineをspawnして`backendConnectTimeout`だけ待つ」実装に変わると、`context.Background()`を渡したテストは、接続に失敗し続けるbackendのsupervisor goroutine（"hung"ケースなら`sleep 30`の再spawnも）をテストバイナリの終了までリークさせてしまう。

各テストを次のパターンに直す（`ctx, cancel`を`connectBackends`呼び出しの直前で作り、`defer cancel()`を**他のdeferより後に**書いて、LIFO順でcancel()が最初に実行されるようにする ―― こうすることで、backendを閉じる他のdeferが動く前にsupervisorのctxが先にキャンセルされ、`Session.Wait()`解除後の再接続試行そのものを防げる）:

`TestConnectBackends_TimesOutHungBackend`（76-106行目付近）を以下に置き換え:

```go
func TestConnectBackends_TimesOutHungBackend(t *testing.T) {
	orig := backendConnectTimeout
	backendConnectTimeout = 100 * time.Millisecond
	t.Cleanup(func() { backendConnectTimeout = orig })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	configs := []config.BackendConfig{
		{Name: "hung", Transport: "stdio", Command: []string{"sleep", "30"}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stop the "hung" backend's supervisor from retrying forever after this test returns

	done := make(chan struct{})
	var conn connected
	go func() {
		conn = connectBackends(ctx, logger, configs, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("connectBackends did not return within 10s of backendConnectTimeout elapsing")
	}
	if len(conn.backends) != 0 {
		t.Fatalf("backends = %v, want none (the hung backend should be excluded)", conn.backends)
	}
}
```

`TestConnectBackends_ResourceListFailureKeepsBackendTools`（161-196行目付近）を以下に置き換え:

```go
func TestConnectBackends_ResourceListFailureKeepsBackendTools(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})

	realHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil)
	backendHTTP := httptest.NewServer(denyMethodHandler(realHandler, "resources/list", "resources/templates/list"))
	defer backendHTTP.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	configs := []config.BackendConfig{
		{Name: "fake", Transport: "http", URL: backendHTTP.URL},
	}

	ctx, cancel := context.WithCancel(context.Background())
	conn := connectBackends(ctx, logger, configs, nil)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()
	defer cancel() // registered last -> runs first, before the backend-close loop above

	if _, ok := conn.backends["fake"]; !ok {
		t.Fatalf("backends = %v, want backend \"fake\" to still be connected despite its resources/list failing", conn.backends)
	}
	if len(conn.toolEntries) != 1 || len(conn.toolEntries[0].Items) != 1 || conn.toolEntries[0].Items[0].Name != "ping" {
		t.Fatalf("toolEntries = %+v, want one entry with tool \"ping\"", conn.toolEntries)
	}
	if len(conn.resourceEntries) != 1 || len(conn.resourceEntries[0].Items) != 0 {
		t.Fatalf("resourceEntries = %+v, want one entry with no items (a resources/list failure is treated as an empty list, not dropped)", conn.resourceEntries)
	}
	if len(conn.resourceTemplateEntries) != 1 || len(conn.resourceTemplateEntries[0].Items) != 0 {
		t.Fatalf("resourceTemplateEntries = %+v, want one entry with no items", conn.resourceTemplateEntries)
	}
}
```

`TestConnectBackends_PromptListFailureKeepsBackendTools`（202-234行目付近）を同じパターンで置き換え（`ctx, cancel := context.WithCancel(context.Background())`を`connectBackends`呼び出しの前に足し、`defer cancel()`を backend-close loop の後に登録):

```go
func TestConnectBackends_PromptListFailureKeepsBackendTools(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})

	realHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil)
	backendHTTP := httptest.NewServer(denyMethodHandler(realHandler, "prompts/list"))
	defer backendHTTP.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	configs := []config.BackendConfig{
		{Name: "fake", Transport: "http", URL: backendHTTP.URL},
	}

	ctx, cancel := context.WithCancel(context.Background())
	conn := connectBackends(ctx, logger, configs, nil)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()
	defer cancel()

	if _, ok := conn.backends["fake"]; !ok {
		t.Fatalf("backends = %v, want backend \"fake\" to still be connected despite its prompts/list failing", conn.backends)
	}
	if len(conn.toolEntries) != 1 || len(conn.toolEntries[0].Items) != 1 || conn.toolEntries[0].Items[0].Name != "ping" {
		t.Fatalf("toolEntries = %+v, want one entry with tool \"ping\"", conn.toolEntries)
	}
	if len(conn.promptEntries) != 1 || len(conn.promptEntries[0].Items) != 0 {
		t.Fatalf("promptEntries = %+v, want one entry with no items (a prompts/list failure is treated as an empty list, not dropped)", conn.promptEntries)
	}
}
```

`TestConnectBackends_LogsSuccessfulConnect`（236-273行目付近）を同じパターンで置き換え:

```go
func TestConnectBackends_LogsSuccessfulConnect(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	configs := []config.BackendConfig{{Name: "fake", Transport: "http", URL: backendHTTP.URL}}

	ctx, cancel := context.WithCancel(context.Background())
	conn := connectBackends(ctx, logger, configs, nil)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()
	defer cancel()

	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Msg       string `json:"msg"`
			Backend   string `json:"backend"`
			Transport string `json:"transport"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "backend connected" {
			found = true
			if rec.Backend != "fake" || rec.Transport != "http" {
				t.Fatalf("backend connected log = %+v, want backend=fake transport=http", rec)
			}
		}
	}
	if !found {
		t.Fatalf("log output = %q, want a \"backend connected\" entry", buf.String())
	}
}
```

- [ ] **Step 2: 既存4テストを実行して、まだ通ることを確認（この時点ではconnectBackendsは未変更）**

Run: `go test ./internal/cli/... -run TestConnectBackends -v -race`
Expected: PASS（コード変更前なので、テストのcontext変更だけでは壊れない）

- [ ] **Step 3: 新規の失敗するテストを書く（reconnect / late-join / ctx-cancel-stops-retry）**

`internal/cli/server_internal_test.go`の`TestConnectBackends_LogsSuccessfulConnect`の直後に追加。`freeAddr`ヘルパーも同じ場所に追加する（`server_test.go`側の`freePort`とは別package `cli` 内なので独立に必要）:

```go
// freeAddr reserves an OS-assigned TCP port, releases it immediately, and
// returns its address -- for tests that need to know an address in advance
// (before anything is listening there) and bind to it later.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing probe listener: %v", err)
	}
	return addr
}

// TestSuperviseBackend_ReconnectsAfterDisconnect checks the full
// disconnect -> item removal -> automatic reconnect -> item restoration
// cycle, driven through connectBackends + a real *gateway.Server, exactly
// as buildGateway wires them together.
func TestSuperviseBackend_ReconnectsAfterDisconnect(t *testing.T) {
	origMin, origMax := backendBackoffMin, backendBackoffMax
	backendBackoffMin, backendBackoffMax = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { backendBackoffMin, backendBackoffMax = origMin, origMax })

	backendHTTP := newFakeBackendHTTP("ping")
	defer backendHTTP.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gwH := &gwHolder{}
	conn := connectBackends(ctx, logger, []config.BackendConfig{{Name: "fake", Transport: "http", URL: backendHTTP.URL}}, gwH)
	firstConn, ok := conn.backends["fake"]
	if !ok {
		t.Fatalf("backends = %v, want backend \"fake\" connected", conn.backends)
	}

	toolTable := router.Resolve(conn.toolEntries, gateway.ToolNameOf, gateway.ToolRename, nil)
	srv := gateway.New(logger, conn.backends, gateway.Tables{Tools: toolTable},
		gateway.Entries{Tools: conn.toolEntries}, gateway.Overrides{}, nil)
	gwH.ptr.Store(srv)

	toolNames := func() []string {
		gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
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
		return names
	}

	if got := toolNames(); len(got) != 1 || got[0] != "ping" {
		t.Fatalf("initial tools = %v, want [ping]", got)
	}

	// Client-initiated Close() is a terminal disconnect (unlike a server-side
	// connection reset, which go-sdk's streamable transport would silently
	// auto-heal on its own -- see this plan's Global Constraints), so this
	// reliably fires Session.Wait() inside superviseBackend.
	if err := firstConn.Close(); err != nil {
		t.Fatalf("closing backend connection: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(toolNames()) != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := toolNames(); len(got) != 0 {
		t.Fatalf("tools after disconnect = %v, want [] (superviseBackend must clear them)", got)
	}

	// The fake backend server is still up -- superviseBackend's retry loop
	// (backoff shrunk above) should reconnect automatically.
	deadline = time.Now().Add(2 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = toolNames()
		if len(got) == 1 && got[0] == "ping" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != 1 || got[0] != "ping" {
		t.Fatalf("tools after automatic reconnect = %v, want [ping]", got)
	}
	if srv.Backends()["fake"] == firstConn {
		t.Fatalf("Backends()[\"fake\"] still points at the closed connection, want a fresh one from the reconnect")
	}
}

// TestSuperviseBackend_LateConnectJoinsViaConnectBackend checks that a
// backend which failed to connect within connectBackends' collection
// window (backendConnectTimeout) joins the gateway automatically via
// ConnectBackend once it eventually succeeds -- the "backend that failed
// to connect at mcprt startup" case from this plan's spec.
func TestSuperviseBackend_LateConnectJoinsViaConnectBackend(t *testing.T) {
	origTimeout := backendConnectTimeout
	backendConnectTimeout = 100 * time.Millisecond
	t.Cleanup(func() { backendConnectTimeout = origTimeout })
	origMin, origMax := backendBackoffMin, backendBackoffMax
	backendBackoffMin, backendBackoffMax = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { backendBackoffMin, backendBackoffMax = origMin, origMax })

	addr := freeAddr(t) // nothing listens here yet

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gwH := &gwHolder{}
	conn := connectBackends(ctx, logger, []config.BackendConfig{{Name: "late", Transport: "http", URL: "http://" + addr}}, gwH)
	if _, ok := conn.backends["late"]; ok {
		t.Fatalf("backends = %v, want \"late\" excluded (nothing was listening within backendConnectTimeout)", conn.backends)
	}

	srv := gateway.New(logger, conn.backends, gateway.Tables{}, gateway.Entries{}, gateway.Overrides{}, nil)
	gwH.ptr.Store(srv) // matches buildGateway: gwH.ptr is populated right after gateway.New

	// Now start listening on the SAME address the config already points at.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listening on %s: %v", addr, err)
	}
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "late-tool", Description: "late-tool"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	lateHTTP := &http.Server{Handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil)}
	go func() { _ = lateHTTP.Serve(ln) }()
	defer func() { _ = lateHTTP.Close() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && srv.Backend("late") == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.Backend("late") == nil {
		t.Fatal("srv.Backend(\"late\") = nil after starting the backend, want it registered via superviseBackend's retry")
	}
}

// TestSuperviseBackend_StopsRetryingWhenContextCancelled checks that
// cancelling ctx makes superviseBackend's retry loop return promptly,
// instead of continuing to retry forever -- this is what lets a superseded
// hot-reload generation's supervisors wind down (see this plan's header
// note on genCtx scoping).
func TestSuperviseBackend_StopsRetryingWhenContextCancelled(t *testing.T) {
	origMin, origMax := backendBackoffMin, backendBackoffMax
	backendBackoffMin, backendBackoffMax = 50*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { backendBackoffMin, backendBackoffMax = origMin, origMax })

	addr := freeAddr(t) // nothing ever listens here -- every attempt fails
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		superviseBackend(ctx, logger, config.BackendConfig{Name: "unreachable", Transport: "http", URL: "http://" + addr}, nil, func(*connectResult) {})
		close(done)
	}()

	time.Sleep(20 * time.Millisecond) // let it start its first (failing) attempt
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("superviseBackend did not return within 2s of ctx cancellation")
	}
}
```

`internal/cli/server_internal_test.go`のimportに`"net"`と`"github.com/wtnb75/mcprt/internal/router"`を追加する必要がある（`gateway`は既にimport済み）。

- [ ] **Step 4: 新規テストを実行して失敗を確認**

Run: `go test ./internal/cli/... -run 'TestSuperviseBackend' -v`
Expected: FAIL（`superviseBackend`/`backendBackoffMin`/`backendBackoffMax`/`connectResult` undefined）

- [ ] **Step 5: `connectResult`・`connectAndList`・`backendBackoffMin`/`Max`・`superviseBackend`を実装し、`connectBackends`を書き換える**

`internal/cli/server.go`の`connectBackends`関数（現在の398-504行目付近、`connected`型定義の直後から`wg.Wait()`で終わる実装全体）を、以下で丸ごと置き換える。`connected`型定義自体（386-396行目）はそのまま残す:

```go
// backendBackoffMin/Max bound superviseBackend's exponential retry delay.
// A var so tests can shrink both.
var (
	backendBackoffMin = 1 * time.Second
	backendBackoffMax = 60 * time.Second
)

// connectResult is connectAndList's successful result: the live backend
// plus its four freshly-listed item sets, as raw slices (not yet wrapped
// in router.Entry -- that wrapping only makes sense once the caller knows
// whether it's building the very first Entries for gateway.New, or calling
// gateway.Server.ConnectBackend, which does its own wrapping via
// upsertEntry, see internal/gateway/reconcile.go).
type connectResult struct {
	backend           *backend.Backend
	tools             []*mcp.Tool
	resources         []*mcp.Resource
	resourceTemplates []*mcp.ResourceTemplate
	prompts           []*mcp.Prompt
}

// connectAndList performs one connection attempt: Connect, then ListTools
// (whose failure aborts the attempt -- the freshly-opened connection is
// closed and the error returned, so superviseBackend's caller retries),
// then ListResources/ListResourceTemplates/ListPrompts (each a soft
// failure: logged and treated as an empty list, since many non-Go-SDK MCP
// servers answer these with a "method not found" error when they don't
// implement that capability at all -- this must not take down an
// otherwise-working tools-only backend). Bounded to backendConnectTimeout
// so one hung backend can't block superviseBackend's retry loop
// indefinitely.
func connectAndList(ctx context.Context, logger *slog.Logger, bc config.BackendConfig, cb backend.ChangeCallbacks) (*connectResult, error) {
	ctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
	defer cancel()

	b, err := backend.Connect(ctx, bc, cb)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	logger.Info("backend connected", "backend", bc.Name, "transport", bc.Transport)
	tools, err := b.ListTools(ctx)
	if err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("list tools: %w", err)
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
	return &connectResult{
		backend:           b,
		tools:             tools,
		resources:         resources,
		resourceTemplates: resourceTemplates,
		prompts:           prompts,
	}, nil
}

// superviseBackend owns backendName's whole connection lifecycle for as
// long as ctx stays alive: connect (retrying with exponential backoff on
// failure, forever -- there is no give-up while ctx is alive), report the
// result, wait for disconnect, clear the backend's items, and reconnect.
//
// ctx is always the generation's own genCtx here (buildGateway, the only
// caller of connectBackends, is itself always called with a generation's
// genCtx -- see this plan's header note). Because genCtx is
// generation-scoped rather than process-scoped, a superseded generation's
// supervisors wind down on their own once that generation's connections
// actually close (a real disconnect, or scheduleDrain's eventual
// force-close after reloadDrainTimeout): Session.Wait() returns, the next
// connectAndList attempt fails immediately against the already-cancelled
// ctx, and the retry-wait select's <-ctx.Done() branch returns. No special
// generation-teardown code is needed for that to happen.
//
// onFirstConnect is called on any successful connect (first-ever or a
// reconnect) for as long as gwH.ptr is still nil -- i.e. before the
// generation's own buildGateway call has finished constructing its
// *gateway.Server. Once gwH.ptr is populated, every successful connect
// (first-ever included) goes through gw.ConnectBackend instead, so a
// backend that connects for the first time after connectBackends' own
// collection window has already closed still gets registered (see this
// plan's design note on this exact race).
//
// gwH may be nil (some callers -- and this plan's own tests -- exercise
// connectBackends/superviseBackend without a gwHolder at all, when they
// don't care about gateway reconcile); every gwH access below is guarded
// accordingly.
func superviseBackend(ctx context.Context, logger *slog.Logger, bc config.BackendConfig, gwH *gwHolder, onFirstConnect func(*connectResult)) {
	cb := backend.ChangeCallbacks{}
	if gwH != nil {
		cb = backend.ChangeCallbacks{
			OnToolsChanged:     toolsChangedCallback(ctx, logger, bc.Name, gwH),
			OnResourcesChanged: resourcesChangedCallback(ctx, logger, bc.Name, gwH),
			OnPromptsChanged:   promptsChangedCallback(ctx, logger, bc.Name, gwH),
		}
	}

	backoff := backendBackoffMin
	for {
		c, err := connectAndList(ctx, logger, bc, cb)
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

		var gw *gateway.Server
		if gwH != nil {
			gw = gwH.ptr.Load()
		}
		if gw != nil {
			gw.ConnectBackend(bc.Name, c.backend, bc.Prefix, c.tools, c.resources, c.resourceTemplates, c.prompts)
		} else {
			onFirstConnect(c)
		}

		if err := c.backend.Session.Wait(); err != nil {
			logger.Warn("backend disconnected", "backend", bc.Name, "error", err)
		} else {
			logger.Info("backend disconnected", "backend", bc.Name)
		}
		if gwH != nil {
			if gw := gwH.ptr.Load(); gw != nil {
				gw.UpdateTools(bc.Name, nil)
				gw.UpdateResources(bc.Name, nil, nil)
				gw.UpdatePrompts(bc.Name, nil)
			}
		}
		// loop: reconnect
	}
}

// connectBackends spawns a persistent superviseBackend goroutine for every
// configured backend and waits up to backendConnectTimeout collecting
// whichever ones connect within that window, then returns with exactly
// that set -- a backend that fails to connect, fails to list tools, or
// doesn't finish within backendConnectTimeout is excluded from this
// generation's founding set (best-effort, matches the pre-reconnect
// behavior), but its supervisor keeps retrying in the background and
// joins later via gateway.Server.ConnectBackend once ctx's gwHolder is
// populated (see buildGateway). A backend that fails to list resources,
// resource templates, or prompts is kept with its tools intact and
// treated as having none of that kind (see connectAndList).
func connectBackends(ctx context.Context, logger *slog.Logger, configs []config.BackendConfig, gwH *gwHolder) connected {
	resultCh := make(chan *connectResult, len(configs))
	for _, bc := range configs {
		go superviseBackend(ctx, logger, bc, gwH, func(c *connectResult) { resultCh <- c })
	}

	result := connected{backends: make(map[string]*backend.Backend, len(configs))}
	deadline := time.After(backendConnectTimeout)
	for i := 0; i < len(configs); i++ {
		select {
		case c := <-resultCh:
			result.backends[c.backend.Name] = c.backend
			result.toolEntries = append(result.toolEntries, router.Entry[*mcp.Tool]{
				BackendName: c.backend.Name, Prefix: c.backend.Prefix, Items: c.tools,
			})
			result.resourceEntries = append(result.resourceEntries, router.Entry[*mcp.Resource]{
				BackendName: c.backend.Name, Items: c.resources,
			})
			result.resourceTemplateEntries = append(result.resourceTemplateEntries, router.Entry[*mcp.ResourceTemplate]{
				BackendName: c.backend.Name, Items: c.resourceTemplates,
			})
			result.promptEntries = append(result.promptEntries, router.Entry[*mcp.Prompt]{
				BackendName: c.backend.Name, Prefix: c.backend.Prefix, Items: c.prompts,
			})
		case <-deadline:
			return result
		}
	}
	return result
}
```

このplanのTask 1・Task 4（既存のhot-reload実装）で使っている型・関数（`gwHolder`・`connected`・`toolsChangedCallback`等）はそのまま。`backend.Backend`のフィールド`Prefix`は`backend.Connect`が`cfg.Prefix`から設定済みだが、design docの`ConnectBackend`シグネチャに合わせて明示的に`bc.Prefix`/`c.backend.Prefix`を渡している点に注意（同じ値）。

- [ ] **Step 6: 新規テストを実行して成功を確認**

Run: `go test ./internal/cli/... -run 'TestSuperviseBackend' -v -race`
Expected: PASS（全ケース）

- [ ] **Step 7: 既存テストを含むパッケージ全体を再実行**

Run: `go test ./internal/cli/... -run 'TestConnectBackends|TestBuildGateway|TestWatchSIGHUP|TestScheduleDrain' -v -race`
Expected: PASS（全ケース。既存のbuildGateway/watchSIGHUP系テストは`connectBackends`の内部実装が変わっても外部から見た振る舞い ―― `buildGateway`の戻り値・`current`の切り替わり ―― は変えていないので、無修正で通るはず。もし壊れるものがあれば、それは本当のregressionなので直すこと）

- [ ] **Step 8: パッケージ全体のテストと静的チェック**

Run:
```bash
go build ./...
go vet ./internal/cli/...
gofmt -l internal/cli/
go test ./internal/cli/... -race
```
Expected: すべてクリーン

- [ ] **Step 9: `go test -race ./...`で全パッケージがグリーンであることを確認**

Run: `go test -race -count=1 ./...`
Expected: 全パッケージPASS

- [ ] **Step 10: コミット**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go
git commit -m "feat(cli): supervise each backend connection with auto-reconnect"
```

---

## Task 3: e2e — downstream clientから見た切断・自動再接続

**Files:**
- Modify: `internal/cli/server_test.go`（`cli_test`パッケージ、外部から`mcprt server`を起動してdownstream client視点で確認）

**Interfaces:**
- Consumes: Task 2完了後の`connectBackends`/`superviseBackend`の実際の挙動（黒箱、`cli.Execute`経由）。既存の`freePort`/`writeConfig`ヘルパー（`server_test.go`内、変更しない）。

- [ ] **Step 1: 失敗するテストを書く**

`internal/cli/server_test.go`の`TestServerCommand_SIGHUPReloadsConfig`の直後に追加:

```go
// TestServerCommand_BackendReconnectsAfterDisconnect drives the whole
// backend-reconnect feature end-to-end: a downstream client sees a
// backend's tool, the backend goes down, the tool disappears, the backend
// comes back up on the same address, and the tool reappears -- all without
// mcprt itself needing a restart or a config reload.
//
// This uses a fully-stopped-then-restarted backend server (not
// CloseClientConnections) because a mere connection reset is exactly the
// class of transient failure go-sdk's own streamable HTTP client
// transparently retries and heals on its own (see this plan's Global
// Constraints) -- it would never reach superviseBackend's disconnect
// detection at all.
func TestServerCommand_BackendReconnectsAfterDisconnect(t *testing.T) {
	newBackendHandler := func() http.Handler {
		backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
		mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
			func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
				return nil, struct{}{}, nil
			})
		return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil)
	}

	backendAddr := freePort(t)
	backendListener, err := net.Listen("tcp", backendAddr)
	if err != nil {
		t.Fatalf("listening on %s: %v", backendAddr, err)
	}
	backendHTTP := &http.Server{Handler: newBackendHandler()}
	go func() { _ = backendHTTP.Serve(backendListener) }()
	defer func() { _ = backendHTTP.Close() }()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: fake
    transport: http
    url: %q
`, gatewayAddr, "http://"+backendAddr))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	dial := func() *mcp.ClientSession {
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
		var session *mcp.ClientSession
		var connectErr error
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + gatewayAddr}, nil)
			if connectErr == nil {
				return session
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("connecting to gateway: %v", connectErr)
		return nil
	}
	toolNames := func(t *testing.T, s *mcp.ClientSession) []string {
		t.Helper()
		var names []string
		for tool, err := range s.Tools(ctx, nil) {
			if err != nil {
				t.Fatalf("listing tools: %v", err)
			}
			names = append(names, tool.Name)
		}
		sort.Strings(names)
		return names
	}

	session := dial()
	if got := toolNames(t, session); len(got) != 1 || got[0] != "ping" {
		t.Fatalf("initial tools = %v, want [ping]", got)
	}

	// Fully stop the backend: every connection attempt (including the
	// transport's own built-in SSE-level auto-reconnect) fails outright,
	// eventually surfacing as a terminal disconnect that superviseBackend
	// notices via Session.Wait().
	if err := backendHTTP.Close(); err != nil {
		t.Fatalf("stopping backend: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if got := toolNames(t, session); len(got) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := toolNames(t, session); len(got) != 0 {
		t.Fatalf("tools after backend stopped = %v, want [] (backend removed from routing table)", got)
	}

	// Restart a backend listening on the SAME address: superviseBackend's
	// unbounded retry loop (production backoff: 1s..60s, unshrinkable from
	// this external test package) reconnects to it automatically.
	backendListener2, err := net.Listen("tcp", backendAddr)
	if err != nil {
		t.Fatalf("re-listening on %s: %v", backendAddr, err)
	}
	backendHTTP2 := &http.Server{Handler: newBackendHandler()}
	go func() { _ = backendHTTP2.Serve(backendListener2) }()
	defer func() { _ = backendHTTP2.Close() }()

	deadline = time.Now().Add(90 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = toolNames(t, session)
		if len(got) == 1 && got[0] == "ping" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(got) != 1 || got[0] != "ping" {
		t.Fatalf("tools after backend restart = %v, want [ping] (automatic reconnect)", got)
	}

	_ = session.Close()
	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}
```

このテストは`internal/cli/server_test.go`の先頭のimportに`"net"`を追加する必要がある（他は既存のimportで足りる）。production設定の`backendBackoffMax`が60秒なので、このテストは最悪90秒近くかかりうる ―― `reloadDrainTimeout`をこのpackageから縮められなかったhot-reload plan Task 6と同じ制約であり、新しい問題ではない。

- [ ] **Step 2: テストを実行して失敗を確認**

Run: `go test ./internal/cli/... -run TestServerCommand_BackendReconnectsAfterDisconnect -v`
Expected: FAIL（Task 1・2が無ければ`ConnectBackend`/`superviseBackend`が存在せず、そもそもbuildが通らない ―― Task 1・2完了後に実施する前提なので、この時点ではPASSしているはず。もしPASSしなければ、実際の再接続シーケンスに何らかの問題があるので調査すること）

- [ ] **Step 3: 実装は不要（Task 1・2で完結）。テストを実行して成功を確認**

Run: `go test ./internal/cli/... -run TestServerCommand_BackendReconnectsAfterDisconnect -v -race`
Expected: PASS（数秒〜最大90秒程度）

- [ ] **Step 4: パッケージ全体と静的チェック**

Run:
```bash
go build ./...
go vet ./internal/cli/...
gofmt -l internal/cli/
go test -race -count=1 ./...
```
Expected: すべてクリーン

- [ ] **Step 5: コミット**

```bash
git add internal/cli/server_test.go
git commit -m "test(cli): add e2e coverage for backend disconnect/auto-reconnect"
```
