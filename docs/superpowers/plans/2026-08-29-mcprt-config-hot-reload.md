# mcprt: config ホットリロード（graceful restart） Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `mcprt server`（HTTPリスナー使用時）が`SIGHUP`を受信したら、configファイルを再読み込みし、無停止（graceful restart）で新規HTTP接続に反映する。

**Architecture:** `internal/cli/server.go`の起動時ロジック（backend接続→ルーティングテーブル解決→`gateway.New`）を`buildGateway`として切り出し、起動時とSIGHUP時の両方から同じ関数を呼ぶ。`atomic.Pointer[gateway.Server]`（`current`）を新規接続の向き先として持ち、`internal/gateway.ServeHTTP`は固定の`*mcp.Server`ではなく`func() *mcp.Server`を受け取るようにして、`current.Load().MCP()`を返すようにする。`go-sdk`の`StreamableHTTPHandler`は新規セッション作成時にしか`getServer`を呼ばない（既存セッションはセッションIDでルックアップされ、確立時に紐付いた`*mcp.Server`のまま動き続ける）ため、`current`を差し替えるだけで「新規接続だけ新configに乗る」が実現する。旧世代は`genCancel()`で即座にバックグラウンド処理を止め、`reloadDrainTimeout`（5分、テストでは短縮）後に旧backend接続を強制`Close()`する。

**Tech Stack:** Go, `github.com/modelcontextprotocol/go-sdk/mcp` v1.7.0, 標準ライブラリ`os/signal`/`syscall`。

**Spec:** `docs/superpowers/specs/2026-08-25-mcprt-config-hot-reload-design.md`

## Global Constraints

- スコープはHTTPリスナーのみ。stdioリスナーへの反映は行わない（`SIGHUP`受信時、stdioのみの構成なら「hot-reloadはHTTP専用」とログを出すだけで何もしない）。
- config差分反映は「1バイトでも変わったら丸ごと作り直して差し替え」方式。個々の設定項目の差分反映は行わない。
- `reloadDrainTimeout`はYAML設定に露出しない。`backendConnectTimeout`と同じくハードコードされたpackage変数（テスト用に上書き可能な`var`）とする。
- 本プランは、まだ未実装の「backend切断検知・自動再接続」設計（`2026-08-25-mcprt-backend-reconnect-design.md`、`superviseBackend`等）には依存しない。`buildGateway`は現行の`connectBackends`（一度接続に失敗したbackendはその世代では除外、リトライなし）をそのまま呼ぶ。将来`superviseBackend`が実装されても、`buildGateway`のシグネチャ・呼び出し方は変わらない想定。
- `go test -race ./...`がグリーンであること。外部サービス依存なし（シグナル送信はプロセス内から`syscall.Kill(os.Getpid(), syscall.SIGHUP)`等で完結させる）。

---

## Task 1: `gateway.Server.Backends()` アクセサ

**Files:**
- Modify: `internal/gateway/gateway.go:86-89`（既存の`Backend(name string)`の直後に追加）
- Test: `internal/gateway/gateway_test.go`

**Interfaces:**
- Produces: `func (s *Server) Backends() map[string]*backend.Backend` — 接続中の全backendを名前付きで返す。Task 5の`scheduleDrain`が、超過世代の全backend接続を列挙して強制`Close()`するために使う。

- [ ] **Step 1: 失敗するテストを書く**

`internal/gateway/gateway_test.go`の`TestGateway_CallOnDeadBackendReturnsError`の直前あたりに追加（同ファイルの`newFakeBackendServer`ヘルパーを再利用）:

```go
// TestGateway_Backends checks that Backends returns every backend the
// Server was constructed with, keyed by name -- scheduleDrain (see
// internal/cli/server.go) relies on this to force-close a superseded
// generation's connections.
func TestGateway_Backends(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServerA := newFakeBackendServer("backend-a", "ping")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServerA }, nil))
	defer httpA.Close()
	connA, err := backend.Connect(context.Background(), config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL}, backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	backendServerB := newFakeBackendServer("backend-b", "pong")
	httpB := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServerB }, nil))
	defer httpB.Close()
	connB, err := backend.Connect(context.Background(), config.BackendConfig{Name: "backend-b", Transport: "http", URL: httpB.URL}, backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("connect backend-b: %v", err)
	}
	defer func() { _ = connB.Close() }()

	want := map[string]*backend.Backend{"backend-a": connA, "backend-b": connB}
	srv := gateway.New(logger, want, gateway.Tables{}, gateway.Entries{}, gateway.Overrides{}, nil)

	got := srv.Backends()
	if len(got) != len(want) {
		t.Fatalf("Backends() returned %d entries, want %d", len(got), len(want))
	}
	for name, b := range want {
		if got[name] != b {
			t.Fatalf("Backends()[%q] = %v, want %v", name, got[name], b)
		}
	}
}
```

- [ ] **Step 2: テストが失敗することを確認する**

Run: `go test ./internal/gateway/... -run TestGateway_Backends -v`
Expected: FAIL（`srv.Backends undefined`のコンパイルエラー）

- [ ] **Step 3: `Backends()`を実装する**

`internal/gateway/gateway.go:89`（`Backend`メソッドの直後）に追加:

```go
// Backends returns every currently connected backend, keyed by name, for
// scheduleDrain (see internal/cli/server.go) to force-close a superseded
// generation's connections after its drain timeout.
func (s *Server) Backends() map[string]*backend.Backend { return s.backends }
```

- [ ] **Step 4: テストが通ることを確認する**

Run: `go test ./internal/gateway/... -run TestGateway_Backends -v`
Expected: PASS

- [ ] **Step 5: コミット**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go
git commit -m "feat(gateway): add Server.Backends accessor for hot-reload drain"
```

---

## Task 2: `gateway.ServeHTTP`のシグネチャを`func() *mcp.Server`ベースに変更

**Files:**
- Modify: `internal/gateway/gateway.go:366-390`
- Modify: `internal/gateway/gateway_internal_test.go:37`（既存呼び出しの更新）
- Modify: `internal/gateway/gateway_test.go:882`（既存呼び出しの更新）
- Modify: `internal/cli/server.go:181`（既存呼び出しの更新。Task 4で改めて`current.Load()`版に書き換えるが、このTaskの時点では最小変更としてクロージャで包む）
- Test: `internal/gateway/gateway_internal_test.go`（新規テスト追加）

**Interfaces:**
- Consumes: なし（このTaskは独立）
- Produces: `func ServeHTTP(ctx context.Context, getServer func() *mcp.Server, addr string) error` — Task 4がこれを`current.Load().MCP()`を返すクロージャで呼び出す。

- [ ] **Step 1: 失敗するテストを書く**

`internal/gateway/gateway_internal_test.go`の末尾に追加（`getServer`が呼ばれるたびに異なる`*mcp.Server`を返せることを確認する）:

```go
// TestServeHTTP_GetServerCalledPerNewSession checks that ServeHTTP consults
// getServer again for each brand-new session (not just once at startup),
// and that swapping what getServer returns mid-run steers subsequent new
// sessions to the new value -- the mechanism config hot-reload depends on
// (see internal/cli/server.go's buildGateway/watchSIGHUP).
func TestServeHTTP_GetServerCalledPerNewSession(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srvA := mcp.NewServer(&mcp.Implementation{Name: "gen-a", Version: "v1"}, &mcp.ServerOptions{Logger: logger})
	mcp.AddTool(srvA, &mcp.Tool{Name: "from-a", Description: "from a"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	srvB := mcp.NewServer(&mcp.Implementation{Name: "gen-b", Version: "v1"}, &mcp.ServerOptions{Logger: logger})
	mcp.AddTool(srvB, &mcp.Tool{Name: "from-b", Description: "from b"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})

	var current atomic.Pointer[mcp.Server]
	current.Store(srvA)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing probe listener: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- ServeHTTP(ctx, func() *mcp.Server { return current.Load() }, addr) }()
	t.Cleanup(func() {
		cancel()
		<-serveErr
	})

	dial := func() *mcp.ClientSession {
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
		var session *mcp.ClientSession
		var connectErr error
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			session, connectErr = client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: "http://" + addr}, nil)
			if connectErr == nil {
				return session
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("connecting to test server: %v", connectErr)
		return nil
	}
	toolNames := func(t *testing.T, s *mcp.ClientSession) []string {
		t.Helper()
		var names []string
		for tool, err := range s.Tools(context.Background(), nil) {
			if err != nil {
				t.Fatalf("listing tools: %v", err)
			}
			names = append(names, tool.Name)
		}
		return names
	}

	sessionA := dial()
	defer func() { _ = sessionA.Close() }()
	if got := toolNames(t, sessionA); len(got) != 1 || got[0] != "from-a" {
		t.Fatalf("sessionA tools = %v, want [from-a]", got)
	}

	current.Store(srvB)

	sessionB := dial()
	defer func() { _ = sessionB.Close() }()
	if got := toolNames(t, sessionB); len(got) != 1 || got[0] != "from-b" {
		t.Fatalf("sessionB tools = %v, want [from-b]", got)
	}

	// sessionA, established before the swap, must keep working against
	// srvA -- this is the "existing sessions aren't disturbed" half of the
	// hot-reload guarantee.
	if got := toolNames(t, sessionA); len(got) != 1 || got[0] != "from-a" {
		t.Fatalf("sessionA tools after swap = %v, want [from-a] (must stay on its original server)", got)
	}
}
```

このテストは`atomic`と`net`パッケージを使うので、`internal/gateway/gateway_internal_test.go`の`import`に`"sync/atomic"`を追加する（`"net"`は既にimport済み）。

- [ ] **Step 2: テストが失敗することを確認する**

Run: `go test ./internal/gateway/... -run TestServeHTTP_GetServerCalledPerNewSession -v`
Expected: FAIL（`ServeHTTP(ctx, func() *mcp.Server {...}, addr)`の型が現行`ServeHTTP(ctx, *mcp.Server, addr)`と合わずコンパイルエラー）

- [ ] **Step 3: `ServeHTTP`のシグネチャを変更する**

`internal/gateway/gateway.go:366-390`を置き換える:

```go
// ServeHTTP runs a Streamable HTTP server listening on addr, until ctx is
// cancelled. getServer is called once per brand-new session (the
// go-sdk's StreamableHTTPHandler looks up an existing session by its
// Mcp-Session-Id header instead of calling getServer again) -- not a fixed
// value -- so that a config hot-reload (see internal/cli/server.go's
// buildGateway/watchSIGHUP) can swap in a freshly-built *gateway.Server for
// new sessions without disturbing sessions already bound to the previous
// one.
func ServeHTTP(ctx context.Context, getServer func() *mcp.Server, addr string) error {
	handler := remoteAddrMiddleware(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return getServer() }, nil))
	httpServer := &http.Server{Addr: addr, Handler: handler}

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(sctx); err != nil {
			_ = httpServer.Close() // force-close whatever Shutdown couldn't drain in time
			return fmt.Errorf("graceful shutdown timed out: %w", err)
		}
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
```

- [ ] **Step 4: 既存呼び出し2箇所をクロージャで包むよう更新する**

`internal/gateway/gateway_internal_test.go:37`:
```go
go func() { serveErr <- ServeHTTP(ctx, func() *mcp.Server { return srv }, addr) }()
```

`internal/gateway/gateway_test.go:882`:
```go
go func() { serveErr <- gateway.ServeHTTP(gwCtx, func() *mcp.Server { return srv.MCP() }, addr) }()
```

`internal/cli/server.go:181`（このTaskでは最小変更。Task 4で`current.Load()`版に置き換える）:
```go
go func() { errCh <- gateway.ServeHTTP(ctx, func() *mcp.Server { return srv.MCP() }, cfg.Listen.HTTP) }()
```

- [ ] **Step 5: テストが通ることを確認する**

Run: `go test ./internal/gateway/... ./internal/cli/... -v -run 'ServeHTTP|TestServerCommand'`
Expected: PASS（既存テストも含めて全てグリーン）

- [ ] **Step 6: コミット**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_internal_test.go internal/gateway/gateway_test.go internal/cli/server.go
git commit -m "feat(gateway): make ServeHTTP take a getServer func for hot-reload"
```

---

## Task 3: `internal/cli/server.go`: `buildGateway`の切り出し

**Files:**
- Modify: `internal/cli/server.go:98-206`（`runServer`本体からの切り出し）
- Test: `internal/cli/server_internal_test.go`

**Interfaces:**
- Consumes: 既存の`connectBackends`, `gwHolder`, `gateway.New`, `router.Resolve`（すべて変更なし）
- Produces: `func buildGateway(ctx context.Context, logger *slog.Logger, cfg *config.Config) (*gateway.Server, error)` — Task 4の`runServer`が起動時に、Task 5の`watchSIGHUP`がSIGHUPごとに呼ぶ。

- [ ] **Step 1: 失敗するテストを書く**

`internal/cli/server_internal_test.go`に追加（同ファイルの`TestConnectBackends_TimesOutHungBackend`の後ろあたり）:

```go
// TestBuildGateway_NoListenerConfigured checks that buildGateway itself
// validates a listener is configured, independent of runServer's own
// earlier check -- watchSIGHUP (Task 5) relies on buildGateway to reject a
// reloaded config with no listener before swapping anything in.
func TestBuildGateway_NoListenerConfigured(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	if _, err := buildGateway(context.Background(), logger, cfg); err == nil {
		t.Fatal("buildGateway: expected error when no listener is configured, got nil")
	}
}

// TestBuildGateway_Success checks that buildGateway connects to every
// configured backend and returns a *gateway.Server exposing their tools --
// the same construction runServer used to do inline, now reusable for a
// SIGHUP-triggered reload.
func TestBuildGateway_Success(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	cfg := &config.Config{
		Listen: config.ListenConfig{HTTP: "127.0.0.1:0"},
		Backends: []config.BackendConfig{
			{Name: "fake", Transport: "http", URL: backendHTTP.URL},
		},
	}

	srv, err := buildGateway(context.Background(), logger, cfg)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}
	if len(srv.Backends()) != 1 {
		t.Fatalf("Backends() = %v, want 1 entry", srv.Backends())
	}
	if srv.Backend("fake") == nil {
		t.Fatal(`Backend("fake") = nil, want the connected backend`)
	}
}
```

`server_internal_test.go`の既存importに`"bytes"`, `"context"`, `"encoding/json"`, `"io"`, `"log/slog"`, `"net/http"`, `"net/http/httptest"`, `"os"`, `"path/filepath"`, `"strings"`, `"testing"`, `"time"`, `"github.com/modelcontextprotocol/go-sdk/mcp"`, `"github.com/wtnb75/mcprt/internal/config"`が既に含まれている。このStepで書くテストが使う識別子はすべてこれらでカバーされており、新規追加が必要なimportはない。

- [ ] **Step 2: テストが失敗することを確認する**

Run: `go test ./internal/cli/... -run TestBuildGateway -v`
Expected: FAIL（`buildGateway`未定義のコンパイルエラー）

- [ ] **Step 3: `runServer`から`buildGateway`を切り出す**

`internal/cli/server.go:98-206`の`runServer`全体を、以下に置き換える（`buildGateway`を新規追加し、`runServer`は現時点では最小限の変更 — `current`/`genCtx`/`watchSIGHUP`の配線はTask 4・5で行う。この時点では"呼び出し元がbuildGatewayを呼ぶだけ"の等価な形にする）:

```go
func runServer(ctx context.Context, logger *slog.Logger, configPath string) error {
	shutdownTelemetry, err := telemetry.Setup(ctx)
	if err != nil {
		return fmt.Errorf("configuring tracing: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
		defer cancel()
		if err := shutdownTelemetry(sctx); err != nil {
			logger.Error("tracer shutdown failed", "error", err)
		}
	}()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// A child context we can cancel ourselves: if one listener fails while
	// another is still healthy, cancelling here tells the healthy one to
	// shut down too instead of leaving runServer blocked waiting on it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	srv, err := buildGateway(ctx, logger, cfg)
	if err != nil {
		return err
	}
	defer func() {
		for _, b := range srv.Backends() {
			_ = b.Close()
		}
	}()

	logger.Info("listening", "stdio", cfg.Listen.Stdio, "http", cfg.Listen.HTTP)

	running := 0
	errCh := make(chan error, 2)
	if cfg.Listen.Stdio {
		running++
		go func() { errCh <- gateway.ServeStdio(ctx, srv.MCP()) }()
	}
	if cfg.Listen.HTTP != "" {
		running++
		go func() { errCh <- gateway.ServeHTTP(ctx, func() *mcp.Server { return srv.MCP() }, cfg.Listen.HTTP) }()
	}

	// Log each listener's outcome as it arrives, so a listener that fails
	// while another is still healthy is reported immediately. A cancelled
	// context is how a clean shutdown reaches ServeStdio, so it isn't a
	// failure. cancel() on a real failure stops the other listener too,
	// instead of waiting on it indefinitely.
	var firstErr error
	for i := 0; i < running; i++ {
		err := <-errCh
		if err == nil {
			continue
		}
		if errors.Is(err, context.Canceled) {
			logger.Debug("listener stopped due to shutdown", "error", err)
			continue
		}
		logger.Error("listener stopped with error", "error", err)
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	return firstErr
}

// buildGateway connects to every configured backend (see connectBackends)
// and builds a fresh *gateway.Server from scratch. Called once at startup,
// and (from Task 5's watchSIGHUP) once per SIGHUP-triggered reload with a
// freshly-loaded cfg -- the two call sites are otherwise identical, which is
// the whole point of the graceful-restart design: nothing about
// config-derived state is patched piecemeal, it's all rebuilt the same way
// every time.
func buildGateway(ctx context.Context, logger *slog.Logger, cfg *config.Config) (*gateway.Server, error) {
	if !cfg.Listen.Stdio && cfg.Listen.HTTP == "" {
		return nil, errors.New("no listener configured: enable listen.stdio or set listen.http")
	}

	var gwH gwHolder
	conn := connectBackends(ctx, logger, cfg.Backends, &gwH)

	toolTable := router.Resolve(conn.toolEntries, gateway.ToolNameOf, gateway.ToolRename, cfg.Overrides)
	for _, c := range toolTable.Conflicts {
		logger.Warn("tool name conflict", "tool", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	resourceTable := router.Resolve(conn.resourceEntries, gateway.ResourceNameOf, gateway.ResourceRename, cfg.ResourceOverrides)
	for _, c := range resourceTable.Conflicts {
		logger.Warn("resource URI conflict", "uri", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	resourceTemplateTable := router.Resolve(conn.resourceTemplateEntries, gateway.ResourceTemplateNameOf, gateway.ResourceTemplateRename, cfg.ResourceTemplateOverrides)
	for _, c := range resourceTemplateTable.Conflicts {
		logger.Warn("resource template URI conflict", "uriTemplate", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	promptTable := router.Resolve(conn.promptEntries, gateway.PromptNameOf, gateway.PromptRename, cfg.PromptOverrides)
	for _, c := range promptTable.Conflicts {
		logger.Warn("prompt name conflict", "prompt", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

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

	return srv, nil
}
```

- [ ] **Step 4: 新規テストと既存テストが通ることを確認する**

Run: `go test ./internal/cli/... -v`
Expected: PASS（`TestBuildGateway_*`含め、既存の`TestServerCommand_*`もすべてグリーン。この時点ではまだSIGHUP関連の機能はなく、単なる関数切り出しなので既存テストの挙動は変わらない）

- [ ] **Step 5: コミット**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go
git commit -m "refactor(cli): extract buildGateway from runServer"
```

---

## Task 4: `runServer`を`atomic.Pointer[gateway.Server]`（`current`）と世代`ctx`（`genCtx`/`genCancel`）を使う形に配線する

**Files:**
- Modify: `internal/cli/server.go`（Task 3で切り出した`runServer`本体）

**Interfaces:**
- Consumes: Task 3の`buildGateway`
- Produces: `runServer`内のローカル変数`current *atomic.Pointer[gateway.Server]`と`genCancel context.CancelFunc` — Task 5の`watchSIGHUP`がこれらを引数として受け取る。

- [ ] **Step 1: `runServer`を書き換える**

Task 3で書いた`runServer`本体（`buildGateway`関数はそのまま）を、以下に置き換える:

```go
func runServer(ctx context.Context, logger *slog.Logger, configPath string) error {
	shutdownTelemetry, err := telemetry.Setup(ctx)
	if err != nil {
		return fmt.Errorf("configuring tracing: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
		defer cancel()
		if err := shutdownTelemetry(sctx); err != nil {
			logger.Error("tracer shutdown failed", "error", err)
		}
	}()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// A child context we can cancel ourselves: if one listener fails while
	// another is still healthy, cancelling here tells the healthy one to
	// shut down too instead of leaving runServer blocked waiting on it. It
	// also bounds every generation's genCtx below, so process shutdown
	// cancels all of them regardless of hot-reload state.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// genCtx/genCancel scope generation 0's backend connections and
	// list_changed callbacks (see buildGateway) separately from ctx itself,
	// so a later SIGHUP-triggered reload can supersede this generation (via
	// watchSIGHUP's scheduleDrain, Task 5) without tearing down the
	// listeners themselves.
	genCtx, genCancel := context.WithCancel(ctx)
	srv, err := buildGateway(genCtx, logger, cfg)
	if err != nil {
		genCancel()
		return err
	}

	// current is where new HTTP connections get routed (see
	// gateway.ServeHTTP below): generation 0 until a SIGHUP-triggered
	// reload (Task 5's watchSIGHUP) swaps it.
	current := new(atomic.Pointer[gateway.Server])
	current.Store(srv)
	defer func() {
		for _, b := range current.Load().Backends() {
			_ = b.Close()
		}
	}()

	logger.Info("listening", "stdio", cfg.Listen.Stdio, "http", cfg.Listen.HTTP)

	running := 0
	errCh := make(chan error, 2)
	if cfg.Listen.Stdio {
		running++
		// Unlike the HTTP listener, ServeStdio is pinned to generation 0's
		// srv for its whole lifetime -- stdio hot-reload is out of scope
		// (see this plan's Global Constraints and watchSIGHUP below).
		go func() { errCh <- gateway.ServeStdio(ctx, srv.MCP()) }()
	}
	if cfg.Listen.HTTP != "" {
		running++
		go func() { errCh <- gateway.ServeHTTP(ctx, func() *mcp.Server { return current.Load().MCP() }, cfg.Listen.HTTP) }()
		go watchSIGHUP(ctx, logger, configPath, current, genCancel)
	}

	// Log each listener's outcome as it arrives, so a listener that fails
	// while another is still healthy is reported immediately. A cancelled
	// context is how a clean shutdown reaches ServeStdio, so it isn't a
	// failure. cancel() on a real failure stops the other listener too,
	// instead of waiting on it indefinitely.
	var firstErr error
	for i := 0; i < running; i++ {
		err := <-errCh
		if err == nil {
			continue
		}
		if errors.Is(err, context.Canceled) {
			logger.Debug("listener stopped due to shutdown", "error", err)
			continue
		}
		logger.Error("listener stopped with error", "error", err)
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	return firstErr
}
```

`watchSIGHUP`はTask 5でまだ存在しないため、この時点ではコンパイルが通らない。Task 5のStep 1（`watchSIGHUP`のスタブ的実装）と合わせて初めてビルドが通る想定 — このTaskの区切りとしては、次のStepでTask 5の最小限のスタブを仮置きしてビルド・テストを通し、Task 5本体で完成させる。

- [ ] **Step 2: `watchSIGHUP`の最小スタブを一時的に追加する**

`internal/cli/server.go`の末尾（`connectBackends`関数の後）に、ビルドを通すためのプレースホルダを追加する（Task 5のStep 3で本実装に置き換える）:

```go
// watchSIGHUP is implemented in full by Task 5; this stub only exists so
// Task 4's wiring compiles and its existing-test regression check can run
// before Task 5 adds SIGHUP handling itself.
func watchSIGHUP(ctx context.Context, logger *slog.Logger, configPath string, current *atomic.Pointer[gateway.Server], initialGenCancel context.CancelFunc) {
	<-ctx.Done()
}
```

- [ ] **Step 3: 既存の回帰テストがすべて通ることを確認する**

このTaskはリファクタリングのみ（新機能なし）なので、既存テストが無改造で通ることが受け入れ条件。

Run: `go test ./internal/cli/... ./internal/gateway/... -race -v`
Expected: PASS（`TestServerCommand_ServesAggregatedTools`, `TestServerCommand_ServesAggregatedResources`, `TestServerCommand_PrefixNotAppliedToResources`, `TestServerCommand_ServesAggregatedPrompts`, `TestServerCommand_PrefixAppliedToPrompts`, `TestServerCommand_StdioShutdownIsClean`, `TestServerCommand_ListenerFailureCancelsOthers`, `TestServerCommand_NoListenerConfigured`, `TestServerCommand_LogFormatInvalid`, `TestServerCommand_PropagatesToolsListChanged`, `TestServerCommand_PropagatesResourcesListChanged`, `TestServerCommand_PropagatesPromptsListChanged`, `TestServerCommand_ToolsListChanged_ReListFailureKeepsPreviousList`含め全てグリーン）

- [ ] **Step 4: コミット**

```bash
git add internal/cli/server.go
git commit -m "refactor(cli): route new HTTP connections through an atomic current pointer"
```

---

## Task 5: `watchSIGHUP`と`scheduleDrain`の本実装

**Files:**
- Modify: `internal/cli/server.go`（Task 4のスタブを置き換え、`reloadDrainTimeout`変数を追加、importに`os`/`os/signal`/`syscall`を追加）
- Test: `internal/cli/server_internal_test.go`

**Interfaces:**
- Consumes: Task 3の`buildGateway`, Task 1の`gateway.Server.Backends()`
- Produces: `var reloadDrainTimeout time.Duration`（テストが短縮するためのpackage変数）。`watchSIGHUP`と`scheduleDrain`自体は`internal/cli`パッケージ内部関数のまま(exportしない)。

- [ ] **Step 1: 失敗するテストを書く**

`internal/cli/server_internal_test.go`に追加。3つのテストケースを書く: (a) 正常なreload、(b) HTTPリスナーなしのconfigへのreloadは無視、(c) drainタイムアウト後に旧backendが強制クローズされる。

```go
// newFakeBackendHTTP starts an httptest server exposing a single tool named
// toolName, for buildGateway/watchSIGHUP tests that need a real backend to
// connect to. The caller must Close() the returned server.
func newFakeBackendHTTP(toolName string) *httptest.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: toolName, Description: toolName},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	return httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
}

// TestWatchSIGHUP_ReloadsConfig checks that a SIGHUP delivered to the test
// process makes watchSIGHUP rebuild the gateway from the (rewritten) config
// file and swap current to the new generation.
func TestWatchSIGHUP_ReloadsConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendA := newFakeBackendHTTP("from-a")
	defer backendA.Close()
	backendB := newFakeBackendHTTP("from-b")
	defer backendB.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeCfg := func(url string) {
		t.Helper()
		content := fmt.Sprintf("listen:\n  http: \"127.0.0.1:0\"\nbackends:\n  - name: b\n    transport: http\n    url: %q\n", url)
		if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
			t.Fatalf("writing config: %v", err)
		}
	}
	writeCfg(backendA.URL)

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	genCtx, genCancel := context.WithCancel(ctx)
	srv, err := buildGateway(genCtx, logger, cfg)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}
	current := new(atomic.Pointer[gateway.Server])
	current.Store(srv)
	// buildGateway's connected backend keeps its standalone SSE stream open
	// on a context detached from genCtx (see the go-sdk's streamable client:
	// Connect deliberately detaches so the stream survives the connect-time
	// context expiring) -- cancelling genCtx alone never closes it. Without
	// this defer, backendA/backendB.Close() above would block for real,
	// waiting on a connection nothing ever closes; TestBuildGateway_Success
	// (Task 3) established this same pattern for the same reason.
	defer func() {
		for _, b := range current.Load().Backends() {
			_ = b.Close()
		}
	}()

	// Ensure SIGHUP's process-wide disposition is no longer "default"
	// (terminate) before watchSIGHUP's own signal.Notify has necessarily
	// run inside the goroutine below -- otherwise the syscall.Kill a few
	// lines down could race a still-unregistered handler and kill this
	// whole test binary instead of being delivered as a normal signal. Any
	// number of signal.Notify registrations for the same signal all
	// receive it, so this doesn't interfere with watchSIGHUP's own handling.
	sighupRegistered := make(chan os.Signal, 1)
	signal.Notify(sighupRegistered, syscall.SIGHUP)
	defer signal.Stop(sighupRegistered)

	done := make(chan struct{})
	go func() {
		watchSIGHUP(ctx, logger, configPath, current, genCancel)
		close(done)
	}()

	writeCfg(backendB.URL)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if current.Load() != srv {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	newSrv := current.Load()
	if newSrv == srv {
		t.Fatal("current was not swapped after SIGHUP")
	}
	if genCtx.Err() == nil {
		t.Fatal("generation 0's genCtx should be cancelled once superseded")
	}

	cancel()
	<-done
}

// syncBuffer is bytes.Buffer plus a mutex around every access, for tests
// where one goroutine writes log output (via slog, which issues one Write
// per formatted line) while the test goroutine concurrently polls it -- a
// bare bytes.Buffer is not safe for that and -race catches it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestWatchSIGHUP_IgnoresReloadWithoutHTTPListener checks that a SIGHUP
// whose newly-loaded config has no HTTP listener leaves current untouched
// and logs a warning, instead of swapping in a *gateway.Server nothing can
// reach (hot-reload is HTTP-only, see this plan's Global Constraints).
func TestWatchSIGHUP_IgnoresReloadWithoutHTTPListener(t *testing.T) {
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	backendA := newFakeBackendHTTP("from-a")
	defer backendA.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := fmt.Sprintf("listen:\n  http: \"127.0.0.1:0\"\nbackends:\n  - name: b\n    transport: http\n    url: %q\n", backendA.URL)
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	genCtx, genCancel := context.WithCancel(ctx)
	srv, err := buildGateway(genCtx, logger, cfg)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}
	current := new(atomic.Pointer[gateway.Server])
	current.Store(srv)
	defer func() {
		for _, b := range current.Load().Backends() {
			_ = b.Close()
		}
	}()

	sighupRegistered := make(chan os.Signal, 1)
	signal.Notify(sighupRegistered, syscall.SIGHUP)
	defer signal.Stop(sighupRegistered)

	done := make(chan struct{})
	go func() {
		watchSIGHUP(ctx, logger, configPath, current, genCancel)
		close(done)
	}()

	// Rewrite to a stdio-only config (no listen.http): buildGateway itself
	// would accept this (stdio is a valid listener), but watchSIGHUP must
	// refuse it as a hot-reload target.
	if err := os.WriteFile(configPath, []byte("listen:\n  stdio: true\nbackends: []\n"), 0o600); err != nil {
		t.Fatalf("rewriting config: %v", err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logBuf.String(), "hot-reload is only supported for HTTP listeners") {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logBuf.String(), "hot-reload is only supported for HTTP listeners") {
		t.Fatalf("log output = %q, want it to mention HTTP-only hot-reload", logBuf.String())
	}
	if current.Load() != srv {
		t.Fatal("current should not have been swapped")
	}

	cancel()
	<-done
}

// TestScheduleDrain_ForceClosesBackendsAfterTimeout checks that
// scheduleDrain cancels the superseded generation's context immediately,
// but only force-closes its backend connections once reloadDrainTimeout
// has elapsed.
func TestScheduleDrain_ForceClosesBackendsAfterTimeout(t *testing.T) {
	origTimeout := reloadDrainTimeout
	reloadDrainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { reloadDrainTimeout = origTimeout })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendA := newFakeBackendHTTP("from-a")
	defer backendA.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := fmt.Sprintf("listen:\n  http: \"127.0.0.1:0\"\nbackends:\n  - name: b\n    transport: http\n    url: %q\n", backendA.URL)
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	genCtx, genCancel := context.WithCancel(context.Background())
	srv, err := buildGateway(genCtx, logger, cfg)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}
	b := srv.Backend("b")
	if b == nil {
		t.Fatal(`Backend("b") = nil`)
	}

	scheduleDrain(logger, srv, genCancel)

	if genCtx.Err() == nil {
		t.Fatal("scheduleDrain should cancel genCtx immediately")
	}
	// Immediately after scheduleDrain returns, the backend must still be
	// usable (drain timeout hasn't elapsed yet).
	if _, err := b.ListTools(context.Background()); err != nil {
		t.Fatalf("backend should still be open immediately after scheduleDrain: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, lastErr = b.ListTools(context.Background()); lastErr != nil {
			return // force-closed as expected
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("backend was not force-closed within 2s of reloadDrainTimeout elapsing")
}
```

このテストコードを動かすには、`internal/cli/server_internal_test.go`の既存importに以下を追加する必要がある: `"fmt"`, `"os/signal"`, `"sync"`, `"sync/atomic"`, `"syscall"`, `"github.com/wtnb75/mcprt/internal/gateway"`。（`"bytes"`, `"net/http/httptest"`, `"os"`, `"path/filepath"`, `"strings"`は既存importに含まれているため追加不要。）

- [ ] **Step 2: テストが失敗することを確認する**

Run: `go test ./internal/cli/... -run 'TestWatchSIGHUP|TestScheduleDrain' -v`
Expected: FAIL（`reloadDrainTimeout`未定義、`scheduleDrain`未定義のコンパイルエラー。`watchSIGHUP`はTask 4のスタブがあるためコンパイルは通るがロジックが動かず`TestWatchSIGHUP_*`はタイムアウトでFAIL）

- [ ] **Step 3: `watchSIGHUP`本実装と`scheduleDrain`を実装する**

`internal/cli/server.go`のimportに`"os"`, `"os/signal"`, `"syscall"`を追加する。

`backendConnectTimeout`の宣言の直後（`internal/cli/server.go`冒頭のvarブロック）に追加:

```go
// reloadDrainTimeout bounds how long a superseded generation's backend
// connections are kept alive after a hot-reload swap, so sessions still
// bound to it can finish naturally. A var so tests can shrink it.
var reloadDrainTimeout = 5 * time.Minute
```

Task 4で追加したスタブの`watchSIGHUP`を、以下に置き換える。同じ場所に`scheduleDrain`も追加する:

```go
// watchSIGHUP blocks until ctx is cancelled, rebuilding the gateway (via
// buildGateway) and swapping current on every SIGHUP. initialGenCancel is
// the cancel func for the generation runServer already built before
// spawning this loop -- watchSIGHUP takes ownership of it so that
// generation 0 is cancelled on its first supersession exactly like every
// later one; without this, the very first reload would leak generation 0's
// connectBackends-spawned goroutines' resources forever. Only spawned by
// runServer when cfg.Listen.HTTP != "" -- hot-reload only makes sense for
// HTTP (see this plan's Global Constraints).
func watchSIGHUP(ctx context.Context, logger *slog.Logger, configPath string, current *atomic.Pointer[gateway.Server], initialGenCancel context.CancelFunc) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)

	genCancel := initialGenCancel // the currently-live generation's cancel func

	for {
		select {
		case <-ctx.Done():
			return
		case <-sighup:
			cfg, err := config.Load(configPath)
			if err != nil {
				logger.Error("config reload failed, keeping current config", "error", err)
				continue
			}
			// buildGateway itself validates that SOME listener is
			// configured; this check is specifically about whether
			// hot-reload can do anything USEFUL with the new config, which
			// is a stricter, hot-reload-specific condition on top of that.
			if cfg.Listen.HTTP == "" {
				logger.Warn("SIGHUP received, but hot-reload is only supported for HTTP listeners; ignoring")
				continue
			}
			if cfg.Listen.Stdio {
				logger.Warn("SIGHUP received: only the HTTP listener will see the new config; the existing stdio session (if any) keeps running under the old one")
			}

			genCtx, newGenCancel := context.WithCancel(ctx)
			newSrv, err := buildGateway(genCtx, logger, cfg)
			if err != nil {
				logger.Error("config reload failed, keeping current config", "error", err)
				newGenCancel()
				continue
			}

			oldSrv := current.Swap(newSrv)
			logger.Info("config reloaded")

			scheduleDrain(logger, oldSrv, genCancel) // supersede and drain the generation this reload replaced
			genCancel = newGenCancel                 // track the new generation for the NEXT reload (or process shutdown, which cancels ctx and makes newGenCancel a no-op)
		}
	}
}

// scheduleDrain cancels the just-superseded generation's long-lived ctx
// (stopping its backends' list_changed callback contexts) and, after
// reloadDrainTimeout, force-closes every one of its backend connections --
// so a session still bound to oldSrv gets a normal "backend disconnected"
// error from that point on, instead of the old generation's connections
// staying open indefinitely.
func scheduleDrain(logger *slog.Logger, oldSrv *gateway.Server, oldGenCancel context.CancelFunc) {
	oldGenCancel()
	time.AfterFunc(reloadDrainTimeout, func() {
		for name, b := range oldSrv.Backends() {
			if err := b.Close(); err != nil {
				logger.Warn("closing superseded backend connection", "backend", name, "error", err)
			}
		}
	})
}
```

- [ ] **Step 4: テストが通ることを確認する**

Run: `go test ./internal/cli/... -race -run 'TestWatchSIGHUP|TestScheduleDrain|TestBuildGateway' -v`
Expected: PASS

- [ ] **Step 5: パッケージ全体の回帰確認**

Run: `go test ./... -race`
Expected: PASS（全パッケージ）

- [ ] **Step 6: コミット**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go
git commit -m "feat(cli): reload gateway config on SIGHUP via graceful restart"
```

---

## Task 6: e2eテスト — `mcprt server`起動→設定変更→自プロセスへSIGHUP→新規クライアントが新config、既存セッションは旧backendのまま

**Files:**
- Modify: `internal/cli/server_test.go`

**Interfaces:**
- Consumes: `cli.Execute`（既存、変更なし）, Task 5の`watchSIGHUP`（`runServer`経由で間接的に）
- Produces: なし（最終ユーザー視点の受け入れテスト）

- [ ] **Step 1: テストを書く**

`internal/cli/server_test.go`に追加（既存の`freePort`/`writeConfig`ヘルパーを再利用）:

```go
// TestServerCommand_SIGHUPReloadsConfig is this feature's end-to-end
// acceptance test: mcprt server started via cli.Execute, in HTTP mode; the
// config file is rewritten to point at a second backend; SIGHUP is sent to
// the test process itself. A brand-new client session must see the new
// backend's tools, while a session that connected before the reload keeps
// working against the old backend until reloadDrainTimeout elapses, after
// which its calls start failing.
func TestServerCommand_SIGHUPReloadsConfig(t *testing.T) {
	backendA := mcp.NewServer(&mcp.Implementation{Name: "backend-a", Version: "v1"}, nil)
	mcp.AddTool(backendA, &mcp.Tool{Name: "from-a", Description: "from a"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendA }, nil))
	defer httpA.Close()

	backendB := mcp.NewServer(&mcp.Implementation{Name: "backend-b", Version: "v1"}, nil)
	mcp.AddTool(backendB, &mcp.Tool{Name: "from-b", Description: "from b"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	httpB := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendB }, nil))
	defer httpB.Close()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: b
    transport: http
    url: %q
`, gatewayAddr, httpA.URL))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()
	t.Cleanup(func() {
		cancel()
		<-execErr
	})

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

	oldSession := dial()
	defer func() { _ = oldSession.Close() }()
	if got := toolNames(t, oldSession); len(got) != 1 || got[0] != "from-a" {
		t.Fatalf("oldSession tools = %v, want [from-a]", got)
	}

	// Point the config at backend B and reload.
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: b
    transport: http
    url: %q
`, gatewayAddr, httpB.URL)), 0o600); err != nil {
		t.Fatalf("rewriting config: %v", err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP: %v", err)
	}

	// A brand-new session must see backend B's tools once the reload has
	// taken effect (poll: reload is asynchronous).
	deadline := time.Now().Add(5 * time.Second)
	var newSession *mcp.ClientSession
	var got []string
	for time.Now().Before(deadline) {
		newSession = dial()
		got = toolNames(t, newSession)
		if len(got) == 1 && got[0] == "from-b" {
			break
		}
		_ = newSession.Close()
		time.Sleep(100 * time.Millisecond)
	}
	if len(got) != 1 || got[0] != "from-b" {
		t.Fatalf("newSession tools = %v, want [from-b] after reload", got)
	}
	defer func() { _ = newSession.Close() }()

	// The pre-reload session must still work against backend A: it was
	// never disturbed by the swap.
	if got := toolNames(t, oldSession); len(got) != 1 || got[0] != "from-a" {
		t.Fatalf("oldSession tools after reload = %v, want [from-a] (must stay on its original backend)", got)
	}
}
```

`reloadDrainTimeout`は`internal/cli`パッケージの非公開変数なので、外部テストパッケージ（`server_test.go`は`package cli_test`）からは直接書き換えられない。このテストはdrainタイムアウト経過後の強制クローズまでは検証しない（それはTask 5の`TestScheduleDrain_ForceClosesBackendsAfterTimeout`が内部テストとしてカバー済み）。

`internal/cli/server_test.go`の既存importに、このテストが新規に使う`"syscall"`を追加する（`"context"`, `"fmt"`, `"net/http"`, `"net/http/httptest"`, `"os"`, `"sort"`, `"time"`, `"github.com/modelcontextprotocol/go-sdk/mcp"`, `"github.com/wtnb75/mcprt/internal/cli"`は既存importに含まれている）。

- [ ] **Step 2: テストが失敗することを確認する**

Run: `go test ./internal/cli/... -run TestServerCommand_SIGHUPReloadsConfig -v`
Expected: この時点ではTask 1〜5が完了していれば実装自体は揃っているはずなので、コンパイルは通り、まずは実際に動かして確認する（Task 5が正しく実装されていればこの時点でPASSしうる。念のためこのStepで一度実行し、赤か緑かを確認するのが目的）。

- [ ] **Step 3: （Step 2でFAILした場合のみ）実装を修正する**

Task 5の`watchSIGHUP`/`scheduleDrain`実装、またはTask 4の`runServer`配線に戻って原因を特定し修正する。

- [ ] **Step 4: テストが通ることを確認する**

Run: `go test ./internal/cli/... -race -v -run TestServerCommand`
Expected: PASS（新規の`TestServerCommand_SIGHUPReloadsConfig`含め、既存の`TestServerCommand_*`もすべてグリーン）

- [ ] **Step 5: パッケージ全体・レース検出込みの最終回帰確認**

Run: `go test ./... -race`
Expected: PASS

- [ ] **Step 6: コミット**

```bash
git add internal/cli/server_test.go
git commit -m "test(cli): add e2e coverage for SIGHUP config hot-reload"
```

---

## 完了条件（Definition of Done）

- [ ] `go build ./...`が通る
- [ ] `go vet ./...`が通る
- [ ] `go test ./... -race`が全てグリーン
- [ ] `docs/superpowers/specs/2026-08-25-mcprt-config-hot-reload-design.md`のスコープに記載された全項目（`SIGHUP`トリガー、`backends[]`全反映、`overrides`系全反映、HTTP新規接続への反映、旧世代drain）がTask 1〜6でカバーされている
- [ ] stdioリスナーへの非対応（ログのみ）がTask 5の`TestWatchSIGHUP_IgnoresReloadWithoutHTTPListener`でカバーされている
