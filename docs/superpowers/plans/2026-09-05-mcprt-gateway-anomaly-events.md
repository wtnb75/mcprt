# mcprt: gateway異常/状態変化イベントの記録 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `internal/gateway`に`LogEvent`という軽量な共通ヘルパーを新設し、既存の4種類のログ呼び出し（progress relayのbackend不一致、elicitation relayの拒否/タイムアウト/失敗、tool/resource/prompt名前・URI衝突）をこれに移行し、欠けている`list_changed`再解決成功のログを新規追加する。

**Architecture:** `internal/gateway/events.go`に`LogEvent(ctx, logger, level, event string, args ...any)`を新設し、`"gateway event"`という固定msgと`"event"`フィールドで統一的にログ出力する。`internal/gateway`パッケージ内（progress.go・reconcile.go）と`internal/cli`パッケージ（server.go）の両方から`gateway.LogEvent`として呼べるようexportする。

**Tech Stack:** Go 1.x, 標準の`log/slog`, `go test`（`-race`込み）。

**Spec:** `docs/superpowers/specs/2026-09-05-mcprt-gateway-anomaly-events-design.md`

## Global Constraints

- `internal/gateway/audit.go`の`logCall`（`tools/call`等4操作のaudit行）は一切変更しない。
- 通常の運用ログ（backend接続/切断/再接続、SIGHUP config reload、リスナー起動/停止、tracer shutdown等）は`LogEvent`化しない——既存の直接`logger.X`呼び出しのまま。
- `LogEvent`の引数へのマスキング（`maskArguments`相当）は適用しない——本ドキュメント対象のイベントが運ぶ値はユーザー入力の引数ではない。
- `LogEvent`はtrace_id/span_idの相関付けを行わない（`ctx`は受け取るが、`slog.Logger.Log`への受け渡しにのみ使う）。
- 各移行箇所のロジック（エラーハンドリング・制御フロー・戻り値）は一切変更しない——ログ呼び出しの書き換え・追加のみ。
- `go.mod`のモジュールパスは`github.com/wtnb75/mcprt`。

---

## ファイル構成

| ファイル | 種別 | 責務 |
|---|---|---|
| `internal/gateway/events.go` | 新規 | `LogEvent`ヘルパー |
| `internal/gateway/events_test.go` | 新規 | `LogEvent`の単体テスト |
| `internal/gateway/progress.go` | 変更 | backend不一致ログを`LogEvent`化 |
| `internal/gateway/reconcile.go` | 変更 | `logNewConflicts`のシグネチャ変更・`LogEvent`化、4箇所の呼び出し元を更新 |
| `internal/cli/server.go` | 変更 | `buildGateway`の初回衝突ログを`LogEvent`化、`list_changed`成功ログを新規追加、elicitation関連3箇所のログを`LogEvent`化 |
| `internal/cli/server_internal_test.go` | 変更 | `toolsChangedCallback`が成功時に`list_changed_reconciled`イベントを出力することを確認するテストを追加 |

---

## Task 1: `internal/gateway`: `LogEvent`の新設と`progress.go`・`reconcile.go`の移行

**Files:**
- Create: `internal/gateway/events.go`
- Create: `internal/gateway/events_test.go`
- Modify: `internal/gateway/progress.go`
- Modify: `internal/gateway/reconcile.go`

**Interfaces:**
- Produces: `gateway.LogEvent(ctx context.Context, logger *slog.Logger, level slog.Level, event string, args ...any)`。Task 2の`internal/cli/server.go`がこれをそのまま使う。

- [ ] **Step 1: `internal/gateway/events.go`を作成する**

```go
package gateway

import (
	"context"
	"log/slog"
)

// LogEvent records one notable, audit-worthy anomaly or state-change event
// that falls outside the request/response shape logCall covers (no single
// downstream ServerSession or MCP method to attribute it to): a backend
// misbehaving in a way relay code must safely refuse, a backend's tool/
// resource/prompt list successfully reconciling after list_changed, or two
// backends' exposed names colliding. Every call site shares the same
// "gateway event" message and an "event" field naming the specific kind, so
// these lines are greppable/filterable as one group -- distinct from the
// routine operational Info/Warn/Error logging this codebase also does
// (backend connect/disconnect, config reload, listener start/stop, ...),
// which stays as direct logger calls: LogEvent is reserved for events with
// audit value, something an operator investigating an incident or a
// config-hygiene issue would specifically want to search for.
//
// level lets a caller choose the right severity per event (Warn for an
// anomaly/refusal, Info for a routine-but-audit-worthy state change like a
// successful list_changed reconcile) -- LogEvent itself doesn't judge
// severity. ctx is accepted (passed straight to slog.Logger.Log, matching
// that method's own signature) so a future context-aware log handler (e.g.
// one that enriches a line with trace_id from an active span) applies
// automatically without an API change here; no such enrichment happens
// today.
func LogEvent(ctx context.Context, logger *slog.Logger, level slog.Level, event string, args ...any) {
	logger.Log(ctx, level, "gateway event", append([]any{"event", event}, args...)...)
}
```

- [ ] **Step 2: `internal/gateway/events_test.go`を作成する**

```go
package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/wtnb75/mcprt/internal/gateway"
)

// TestLogEvent_WritesEventFieldAndLevel checks that LogEvent produces the
// fixed "gateway event" message, an "event" field carrying the caller's
// event name, the requested level, and any additional args passed through.
func TestLogEvent_WritesEventFieldAndLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	gateway.LogEvent(context.Background(), logger, slog.LevelWarn, "name_conflict", "kind", "tool", "name", "search")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decoding log line %q: %v", buf.String(), err)
	}
	if rec["msg"] != "gateway event" {
		t.Fatalf("msg = %v, want \"gateway event\"", rec["msg"])
	}
	if rec["level"] != "WARN" {
		t.Fatalf("level = %v, want WARN", rec["level"])
	}
	if rec["event"] != "name_conflict" {
		t.Fatalf("event = %v, want name_conflict", rec["event"])
	}
	if rec["kind"] != "tool" || rec["name"] != "search" {
		t.Fatalf("kind/name = %v/%v, want tool/search", rec["kind"], rec["name"])
	}
}

// TestLogEvent_InfoLevel checks that a caller-chosen Info level is honored
// (LogEvent doesn't hardcode a severity), matching the distinction between
// an anomaly (Warn) and a routine-but-audit-worthy state change (Info) the
// design relies on.
func TestLogEvent_InfoLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	gateway.LogEvent(context.Background(), logger, slog.LevelInfo, "list_changed_reconciled", "backend", "fake", "kind", "tools", "count", 3)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decoding log line %q: %v", buf.String(), err)
	}
	if rec["level"] != "INFO" {
		t.Fatalf("level = %v, want INFO", rec["level"])
	}
	if rec["event"] != "list_changed_reconciled" {
		t.Fatalf("event = %v, want list_changed_reconciled", rec["event"])
	}
	if rec["backend"] != "fake" || rec["kind"] != "tools" {
		t.Fatalf("backend/kind = %v/%v, want fake/tools", rec["backend"], rec["kind"])
	}
	if rec["count"] != float64(3) { // JSON numbers decode as float64
		t.Fatalf("count = %v, want 3", rec["count"])
	}
}
```

- [ ] **Step 3: テストを実行して確認する**

Run: `go test ./internal/gateway/... -run TestLogEvent -race -v`
Expected: `PASS`（両テスト）。

- [ ] **Step 4: `internal/gateway/progress.go`のbackend不一致ログを`LogEvent`化する**

`internal/gateway/progress.go`の`Relay`メソッド内、以下の`logger.Warn`呼び出しを変更:

Before:
```go
	if entry.backendName != backendName {
		logger.Warn("progress relay: dropping notification with backend mismatch",
			"token_backend", entry.backendName, "notification_backend", backendName)
		return
	}
```
After:
```go
	if entry.backendName != backendName {
		LogEvent(ctx, logger, slog.LevelWarn, "progress_backend_mismatch",
			"token_backend", entry.backendName, "notification_backend", backendName)
		return
	}
```

- [ ] **Step 5: `internal/gateway/reconcile.go`の`logNewConflicts`を`LogEvent`化する**

`internal/gateway/reconcile.go`のimportに`"context"`を追加する。`logNewConflicts`関数を変更:

Before:
```go
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
```
After:
```go
// logNewConflicts records, via LogEvent, every conflict in newConflicts
// whose exposed name wasn't already conflicting in oldConflicts -- an
// already-known conflict isn't re-logged, and a conflict that disappears
// isn't logged at all, matching startup's one-shot conflict logging.
// context.Background() is used rather than a caller-supplied ctx: this
// fires from Server.UpdateTools/UpdateResources/UpdatePrompts, whose
// signatures carry no ctx (they reconcile a whole backend's list, not a
// single tracked request), so there is no request-scoped context to thread
// through here without a larger, out-of-scope signature change.
func logNewConflicts(logger *slog.Logger, kind string, oldConflicts, newConflicts []router.Conflict) {
	seen := make(map[string]bool, len(oldConflicts))
	for _, c := range oldConflicts {
		seen[c.ExposedName] = true
	}
	for _, c := range newConflicts {
		if !seen[c.ExposedName] {
			LogEvent(context.Background(), logger, slog.LevelWarn, "name_conflict",
				"kind", kind, "name", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
		}
	}
}
```

`internal/gateway/reconcile.go`の4箇所の呼び出し元を変更（`msg`引数を削除し`kind`だけを渡す）:

- Before: `logNewConflicts(s.logger, "tool name conflict", "tool", s.toolTable.Conflicts, newTable.Conflicts)`
  After: `logNewConflicts(s.logger, "tool", s.toolTable.Conflicts, newTable.Conflicts)`
- Before: `logNewConflicts(s.logger, "resource URI conflict", "uri", s.resourceTable.Conflicts, newResourceTable.Conflicts)`
  After: `logNewConflicts(s.logger, "resource", s.resourceTable.Conflicts, newResourceTable.Conflicts)`
- Before: `logNewConflicts(s.logger, "resource template URI conflict", "uriTemplate", s.resourceTemplateTable.Conflicts, newTemplateTable.Conflicts)`
  After: `logNewConflicts(s.logger, "resourceTemplate", s.resourceTemplateTable.Conflicts, newTemplateTable.Conflicts)`
- Before: `logNewConflicts(s.logger, "prompt name conflict", "prompt", s.promptTable.Conflicts, newTable.Conflicts)`
  After: `logNewConflicts(s.logger, "prompt", s.promptTable.Conflicts, newTable.Conflicts)`

- [ ] **Step 6: `internal/gateway`パッケージ全体のビルド・テストを実行する**

Run: `go build ./internal/gateway/... && go vet ./internal/gateway/... && go test ./internal/gateway/... -race`
Expected: エラーなし、全テストPASS（既存のprogress中継・conflictまわりのテストが、ログ呼び出しの書き換え後も引き続き通ることを確認——ロジックは変えていないので、既存アサーションは全て通るはず）。

- [ ] **Step 7: コミット**

```bash
git add internal/gateway/events.go internal/gateway/events_test.go \
        internal/gateway/progress.go internal/gateway/reconcile.go
git commit -m "feat(gateway): add LogEvent, migrate progress-mismatch and name-conflict logging"
```

---

## Task 2: `internal/cli/server.go`: 衝突ログ・elicitationログの移行と`list_changed`成功ログの新規追加

**Files:**
- Modify: `internal/cli/server.go`
- Modify: `internal/cli/server_internal_test.go`

**Interfaces:**
- Consumes: Task 1の`gateway.LogEvent(ctx, logger, level, event string, args ...any)`。

- [ ] **Step 1: `buildGateway`の初回衝突ログを`LogEvent`化する**

`internal/cli/server.go`の`buildGateway`内、4箇所の`logger.Warn`呼び出しを変更:

Before:
```go
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
```
After:
```go
	toolTable := router.Resolve(conn.toolEntries, gateway.ToolNameOf, gateway.ToolRename, cfg.Overrides)
	for _, c := range toolTable.Conflicts {
		gateway.LogEvent(ctx, logger, slog.LevelWarn, "name_conflict", "kind", "tool", "name", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	resourceTable := router.Resolve(conn.resourceEntries, gateway.ResourceNameOf, gateway.ResourceRename, cfg.ResourceOverrides)
	for _, c := range resourceTable.Conflicts {
		gateway.LogEvent(ctx, logger, slog.LevelWarn, "name_conflict", "kind", "resource", "name", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	resourceTemplateTable := router.Resolve(conn.resourceTemplateEntries, gateway.ResourceTemplateNameOf, gateway.ResourceTemplateRename, cfg.ResourceTemplateOverrides)
	for _, c := range resourceTemplateTable.Conflicts {
		gateway.LogEvent(ctx, logger, slog.LevelWarn, "name_conflict", "kind", "resourceTemplate", "name", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	promptTable := router.Resolve(conn.promptEntries, gateway.PromptNameOf, gateway.PromptRename, cfg.PromptOverrides)
	for _, c := range promptTable.Conflicts {
		gateway.LogEvent(ctx, logger, slog.LevelWarn, "name_conflict", "kind", "prompt", "name", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}
```

（`buildGateway`は既に`ctx context.Context`を引数に持つので、そのまま渡す。`kind`の値はTask 1で`reconcile.go`側に揃えた`"tool"`/`"resource"`/`"resourceTemplate"`/`"prompt"`と一致させる——同じ`name_conflict`イベントが起動時とreconcile時のどちらから来ても同じ`kind`値で集約できるようにするため。）

- [ ] **Step 2: `list_changed`再解決成功のログを新規追加する**

`internal/cli/server.go`の`toolsChangedCallback`・`resourcesChangedCallback`・`promptsChangedCallback`を変更:

Before（`toolsChangedCallback`）:
```go
		tools, err := b.ListTools(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "tools", "error", err)
			return
		}
		gw.UpdateTools(backendName, tools)
	}
}
```
After:
```go
		tools, err := b.ListTools(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "tools", "error", err)
			return
		}
		gw.UpdateTools(backendName, tools)
		gateway.LogEvent(ctx, logger, slog.LevelInfo, "list_changed_reconciled", "backend", backendName, "kind", "tools", "count", len(tools))
	}
}
```

Before（`resourcesChangedCallback`）:
```go
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
```
After:
```go
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
		gateway.LogEvent(ctx, logger, slog.LevelInfo, "list_changed_reconciled", "backend", backendName, "kind", "resources", "resource_count", len(resources), "template_count", len(templates))
	}
}
```

Before（`promptsChangedCallback`）:
```go
		prompts, err := b.ListPrompts(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "prompts", "error", err)
			return
		}
		gw.UpdatePrompts(backendName, prompts)
	}
}
```
After:
```go
		prompts, err := b.ListPrompts(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "prompts", "error", err)
			return
		}
		gw.UpdatePrompts(backendName, prompts)
		gateway.LogEvent(ctx, logger, slog.LevelInfo, "list_changed_reconciled", "backend", backendName, "kind", "prompts", "count", len(prompts))
	}
}
```

- [ ] **Step 3: elicitation関連3箇所のログを`LogEvent`化する**

`internal/cli/server.go`の`superviseBackend`内、`cb.OnElicit`クロージャを変更:

Before:
```go
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
					// Distinguish "nobody answered in time" from every other
					// failure: the latter includes, notably, the SDK's own
					// protocol-version refusal of server-initiated
					// elicitation under MCP 2026-07-28+ (see elicitTimeout's
					// doc comment) -- an operator reading logs should not be
					// misled into thinking a human simply took too long when
					// the real cause could be that, a disconnected client,
					// or anything else session.Elicit can return.
					if errors.Is(err, context.DeadlineExceeded) {
						logger.Warn("elicitation: downstream did not respond within timeout", "backend", bc.Name, "error", err)
					} else {
						logger.Warn("elicitation: downstream request failed", "backend", bc.Name, "error", err)
					}
				}
				return res, err
			}
```
After:
```go
			cb.OnElicit = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				session, err := gwH.relays.Elicit.Route(bc.Name)
				if err != nil {
					gateway.LogEvent(ctx, logger, slog.LevelWarn, "elicitation_routing_refused", "backend", bc.Name, "error", err)
					return nil, err
				}
				ectx, cancel := context.WithTimeout(ctx, elicitTimeout)
				defer cancel()
				res, err := session.Elicit(ectx, req.Params)
				if err != nil {
					// Distinguish "nobody answered in time" from every other
					// failure: the latter includes, notably, the SDK's own
					// protocol-version refusal of server-initiated
					// elicitation under MCP 2026-07-28+ (see elicitTimeout's
					// doc comment) -- an operator reading logs should not be
					// misled into thinking a human simply took too long when
					// the real cause could be that, a disconnected client,
					// or anything else session.Elicit can return.
					if errors.Is(err, context.DeadlineExceeded) {
						gateway.LogEvent(ctx, logger, slog.LevelWarn, "elicitation_timeout", "backend", bc.Name, "error", err)
					} else {
						gateway.LogEvent(ctx, logger, slog.LevelWarn, "elicitation_failed", "backend", bc.Name, "error", err)
					}
				}
				return res, err
			}
```

（`ctx`はクロージャの引数——`ectx`はタイムアウト済みの可能性があるので使わない。）

- [ ] **Step 4: `go build`・`go vet`でコンパイルエラーを確認する**

Run: `go build ./... && go vet ./...`
Expected: エラーなし（`internal/cli/server.go`は既に`log/slog`をimport済みなので、`slog.LevelWarn`/`slog.LevelInfo`の参照に追加importは不要）。

- [ ] **Step 5: `list_changed_reconciled`ログを直接検証するテストを追加する**

`cli.Execute`経由のe2eテスト（`internal/cli/server_test.go`）は、テスト側から`root`コマンドを取得できず（`cli.Execute`が内部で`NewRootCmd()`を構築するため）、サーバーのロガー出力を`SetErr`で捕捉する手段がない。そのため、`toolsChangedCallback`を直接呼び出す内部テストとして`internal/cli/server_internal_test.go`に追加する:

```go
// TestToolsChangedCallback_LogsReconciledEventOnSuccess checks that a
// successful list_changed reconcile now emits a "list_changed_reconciled"
// gateway event (via gateway.LogEvent) -- previously nothing was logged on
// this path at all, only on failure.
func TestToolsChangedCallback_LogsReconciledEventOnSuccess(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	httpBackend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer httpBackend.Close()

	ctx := context.Background()
	conn, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: httpBackend.URL}, backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close() }()
	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	entries := []router.Entry[*mcp.Tool]{{BackendName: "fake", Items: tools}}
	table := router.Resolve(entries, gateway.ToolNameOf, gateway.ToolRename, nil)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"fake": conn},
		Tables:   gateway.Tables{Tools: table},
		Entries:  gateway.Entries{Tools: entries},
	})

	gwH := &gwHolder{}
	gwH.ptr.Store(srv)

	toolsChangedCallback(ctx, logger, "fake", gwH)()

	var rec map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err == nil && r["event"] == "list_changed_reconciled" {
			rec = r
			break
		}
	}
	if rec == nil {
		t.Fatalf("log output = %q, want a line with event=list_changed_reconciled", buf.String())
	}
	if rec["backend"] != "fake" || rec["kind"] != "tools" {
		t.Fatalf("backend/kind = %v/%v, want fake/tools", rec["backend"], rec["kind"])
	}
	if rec["count"] != float64(1) { // JSON numbers decode as float64
		t.Fatalf("count = %v, want 1", rec["count"])
	}
}
```

`internal/cli/server_internal_test.go`は`package cli`（内部テスト）であり、`toolsChangedCallback`・`gwHolder`は既にこのファイルから直接参照できる。`bytes`・`encoding/json`・`strings`が未importなら追加する（`router`・`backend`・`config`・`gateway`・`mcp`・`httptest`・`http`は既にこのファイルでimport済みのはず——`go build`で確認し、不足があれば追加する）。

- [ ] **Step 6: 新規テストを実行する**

Run: `go test ./internal/cli/... -run TestToolsChangedCallback_LogsReconciledEventOnSuccess -race -v`
Expected: `PASS`。

- [ ] **Step 7: モジュール全体のテストを実行する**

Run: `go build ./... && go vet ./... && go test ./... -race`
Expected: 全パッケージ`ok`（既存のconflict・list_changed・elicitationまわりのテストが、ログ呼び出しの書き換え後も引き続き通ることを確認）。

- [ ] **Step 8: コミット**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go
git commit -m "feat(cli): migrate conflict/elicitation logging to gateway.LogEvent, log list_changed success"
```

---

## Self-Review メモ（実行者向けではなく記録用）

- Spec coverage: specの「含める」4カテゴリ（progress不一致・elicitation拒否/タイムアウト/失敗・名前衝突・list_changed成功）は全てTask 1・Task 2でカバー。「含めない」項目（`logCall`自体の変更、通常運用ログの`LogEvent`化、マスキング、trace相関、`mcprt call`監査ログ）はどのタスクでも触れない。
- 型整合性: `LogEvent`のシグネチャ`(ctx, logger, level, event string, args ...any)`はTask 1で定義した通り、Task 2の全呼び出し箇所で一貫して使われている。`kind`の値（`"tool"`/`"resource"`/`"resourceTemplate"`/`"prompt"`）はTask 1（reconcile.go）とTask 2（buildGateway）で完全に一致させた。
- テスト設計の修正: 当初`cli.Execute`経由のe2eテストにアサーションを追加する案を検討したが、`cli.Execute`がテスト側にcobra rootを公開しないためログ出力を捕捉できないことが判明し、`toolsChangedCallback`を直接呼ぶ内部テストに変更した（Task 2 Step 5）。
