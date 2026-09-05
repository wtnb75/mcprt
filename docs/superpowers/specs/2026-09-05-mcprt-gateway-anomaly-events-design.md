# mcprt: gateway異常/状態変化イベントの記録 設計

## 背景・目的

`docs/superpowers/plans/2026-09-04-mcprt-progress-relay.md`・`docs/superpowers/plans/2026-09-05-mcprt-elicitation-relay.md`の実装・レビューを経て、mcprtの監査ログ体制を横断的に調査した結果（このドキュメントに先立つ調査セッション）、以下が判明した：

`internal/gateway/audit.go`の`logCall`は`tools/call`・`resources/read`・`resources/templates/read`・`prompts/get`の4操作のみを構造化audit行として記録する、手作業で配線された仕組みである。この4操作の**外側**で起きる、運用上・セキュリティ上注目すべき事象——

- backend間のtoken取り違え（`progress relay`が、間違ったbackendから来た通知を検知して破棄するケース）
- elicitation中継のルーティング拒否・タイムアウト・失敗
- `list_changed`再解決の**成功**（現状、失敗時のWarnしか存在せず、backendのtool/resource/prompt構成が変わった事実そのものが記録されない）
- 複数backend間のtool/resource/prompt名前・URI衝突（あるbackendの定義が別backendの定義を隠す）

——は、いずれも`logger.Warn`/`logger.Info`で個別に記録されてはいるものの、①ログ集約基盤で「監査上注目すべきイベント」として一括抽出する手段がなく、②`list_changed`の成功時のように**そもそも記録されていない**箇所もある。

本ドキュメントは、これらのイベントを一貫した形式で記録するための軽量な仕組み（`internal/gateway.LogEvent`）を定義し、既存の該当箇所をこれに移行し、欠けている成功パスのログを追加する設計を定める。

**`logCall`自体は変更しない**——`tools/call`等4操作の既存audit行は対象外。また、標準的な運用ログ（backend接続/切断、SIGHUP config reload、リスナー起動等）を全て`LogEvent`化することも意図しない——後述のスコープを参照。

## スコープ

含める:
- `internal/gateway`に`LogEvent(ctx, logger, level, event string, args ...any)`を新設する。
- 以下4カテゴリの既存ログ呼び出しを`LogEvent`経由に置き換える：
  1. progress relay: backend不一致による通知破棄（`internal/gateway/progress.go`）
  2. elicitation relay: ルーティング拒否・downstreamタイムアウト・downstream失敗（`internal/cli/server.go`の`cb.OnElicit`）
  3. tool/resource/prompt名前・URI衝突（`internal/gateway/reconcile.go`の`logNewConflicts`、および`internal/cli/server.go`の`buildGateway`内の初回衝突ログ）
- 以下の欠けている成功パスのログを`LogEvent`で新規追加する：
  4. `list_changed`再解決成功（`internal/cli/server.go`の`toolsChangedCallback`・`resourcesChangedCallback`・`promptsChangedCallback`）

含めない（対象外）:
- `logCall`（`tools/call`等4操作のaudit行）自体の変更。
- 通常の運用ログ（backend接続/切断/再接続、SIGHUP config reload、リスナー起動/停止、tracer shutdown等）の`LogEvent`化。これらは「何が起きたか」であって「注目すべき異常/構成変化」ではなく、既存の直接`logger.X`呼び出しのままで適切と判断する。
- `LogEvent`の引数へのマスキング適用。本ドキュメント対象のイベントが運ぶ値（backend名・tool/resource名・エラー文字列・件数）はユーザー入力の引数ではないため、`maskArguments`のような機構は不要。
- trace_id/span_idの相関付け。`LogEvent`は将来のcontext対応ログハンドラのために`ctx context.Context`を受け取るが、本ドキュメントの時点ではtrace抽出ロジックを実装しない。
- 別途検討中の`mcprt call`スタンドアロンコマンドの監査ログ（`docs/superpowers/specs/2026-09-05-mcprt-cli-call-audit-log-design.md`で扱う、別の問題）。

## 全体アーキテクチャ

```
internal/gateway/events.go（新規）
┌────────────────────────────────────────────────────────┐
│ func LogEvent(ctx, logger, level, event string, args...) │
│     └─ logger.Log(ctx, level, "gateway event",             │
│                    "event", event, args...)                 │
└────────────────────────────────────────────────────────────┘
        ▲                    ▲                    ▲
        │                    │                    │
internal/gateway/     internal/gateway/    internal/cli/server.go
  progress.go            reconcile.go        (buildGateway,
  (backend不一致)      (logNewConflicts:      list_changed成功,
                         name_conflict)        elicitation拒否/
                                                タイムアウト/失敗)
```

`LogEvent`は`internal/gateway`に置き、exportして`internal/cli`からも呼べるようにする（既存の`gateway.ToolNameOf`等と同じ、gatewayパッケージが提供する共有ユーティリティのパターンを踏襲）。

## コンポーネント構成

### `internal/gateway/events.go`（新規）

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

### 1. progress relay: backend不一致（`internal/gateway/progress.go`）

Before（既存、`Relay`メソッド内）:
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
（`Relay`は既に`ctx context.Context`を引数に持つため、そのまま渡せる。同一パッケージ内なので`LogEvent`は無修飾で呼べる。）

### 2. elicitation relay: 拒否・タイムアウト・失敗（`internal/cli/server.go`の`cb.OnElicit`）

Before（既存）:
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
					if errors.Is(err, context.DeadlineExceeded) {
						logger.Warn("elicitation: downstream did not respond within timeout", "backend", bc.Name, "error", err)
					} else {
						logger.Warn("elicitation: downstream request failed", "backend", bc.Name, "error", err)
					}
				}
				return res, err
			}
```
After（`internal/cli`からは`gateway.LogEvent`とexport名で呼ぶ）:
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
					if errors.Is(err, context.DeadlineExceeded) {
						gateway.LogEvent(ctx, logger, slog.LevelWarn, "elicitation_timeout", "backend", bc.Name, "error", err)
					} else {
						gateway.LogEvent(ctx, logger, slog.LevelWarn, "elicitation_failed", "backend", bc.Name, "error", err)
					}
				}
				return res, err
			}
```
（元のctxが期限切れした後の`session.Elicit`呼び出しなので、`LogEvent`には`ectx`ではなく元の`ctx`を渡す——`ectx`は既にタイムアウトしている可能性があり、`Log`呼び出し自体をブロックさせたくない。）

### 3. 名前・URI衝突

`internal/gateway/reconcile.go`の`logNewConflicts`を変更（`msg`パラメータを`kind`に、`field`をログのキー名ではなく値の一部として使うよう統一——4種類のフィールドキー名（`tool`/`uri`/`uriTemplate`/`prompt`）がバラバラだったのを`kind`+`name`に揃えることで、`LogEvent`の狙い通り横断的に集約しやすくする）：

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
（`logNewConflicts`の呼び出し元である`updateToolsLocked`等は今日ctxを持たない——`Server.UpdateTools(backendName, items)`のシグネチャにctxがなく、これを拡張するのは本ドキュメントのスコープ外。追跡すべき単一のリクエストに紐づくイベントではない（reconcile全体の結果）ため、`context.Background()`で十分と判断する。）

呼び出し元（`reconcile.go`の`updateToolsLocked`・`updateResourcesLocked`×2・`updatePromptsLocked`）は`msg`引数を削除し`kind`のみ渡すよう変更する（例: `logNewConflicts(s.logger, "tool", s.toolTable.Conflicts, newTable.Conflicts)`）。

`internal/cli/server.go`の`buildGateway`内、初回衝突ログも同様に統一する：

Before:
```go
	toolTable := router.Resolve(conn.toolEntries, gateway.ToolNameOf, gateway.ToolRename, cfg.Overrides)
	for _, c := range toolTable.Conflicts {
		logger.Warn("tool name conflict", "tool", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}
	// ...resource/resourceTemplate/promptも同様に3箇所
```
After:
```go
	toolTable := router.Resolve(conn.toolEntries, gateway.ToolNameOf, gateway.ToolRename, cfg.Overrides)
	for _, c := range toolTable.Conflicts {
		gateway.LogEvent(ctx, logger, slog.LevelWarn, "name_conflict", "kind", "tool", "name", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}
	// ...resource/resourceTemplate/promptも同様（kindをそれぞれ"resource"/"resourceTemplate"/"prompt"に）
```
（`buildGateway`は`ctx context.Context`を引数に持つので、そのまま渡せる。）

### 4. `list_changed`再解決成功（新規追加、`internal/cli/server.go`）

`toolsChangedCallback`（`resourcesChangedCallback`・`promptsChangedCallback`も同様のパターン）:

Before:
```go
		tools, err := b.ListTools(lctx)
		if err != nil {
			logger.Warn("list_changed: re-list failed, keeping previous list", "backend", backendName, "kind", "tools", "error", err)
			return
		}
		gw.UpdateTools(backendName, tools)
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
```
（失敗パスの既存Warnログはそのまま——本ドキュメントは"成功パスに何もない"というギャップだけを埋める。`resourcesChangedCallback`は`resources`と`resource templates`両方の件数を1つの`LogEvent`呼び出しにまとめる: `"kind", "resources", "resource_count", len(resources), "template_count", len(templates)`。`promptsChangedCallback`は`"kind", "prompts", "count", len(prompts)`。）

## データフロー

各カテゴリのイベントは、既存のコードパス（progressのRelay、elicitationのOnElicit、reconcileのUpdate*、list_changedのコールバック、buildGatewayの初回衝突チェック）が実行された**結果として**発火する——`LogEvent`はロジックに割り込まず、既存の`logger.Warn`/`logger.Info`呼び出しを置き換える（またはロジック変更なしに1行追加する）だけ。呼び出し順序・エラーハンドリング・戻り値への影響は一切ない。

## エラーハンドリング

`LogEvent`自体はエラーを返さない（`slog.Logger.Log`はエラーを返さない設計のため）。呼び出し元の既存のエラーハンドリング（`return nil, err`等）は変更しない——ログ呼び出しをどう書き換えても、呼び出し元の制御フローには影響しない。

## テスト方針

- `internal/gateway/events_test.go`（新規）: `LogEvent`が指定した`level`・`"gateway event"`という固定msg・`"event"`フィールド・追加の`args`を正しく出力することを、`slog.NewJSONHandler`で確認する単体テスト。
- 各カテゴリの移行箇所については、**新規のログアサーションテストは必須としない**——既存のprogress中継・elicitation中継・list_changed・conflictそれぞれのテストが、ロジックの振る舞い（通知が正しく中継される/されない、reconcileが正しく反映される等）を既にカバーしている。`logger.Warn`呼び出しを`LogEvent`に置き換えても、これらの既存テストが引き続き全てPASSすることが「ロジックを壊していない」ことの検証になる。
- ただし`list_changed`再解決成功のログは**新規追加**のログ行なので、少なくとも1つ（例えば`TestServerCommand_PropagatesToolsListChanged`のような既存e2eテストに、成功時に`"event", "list_changed_reconciled"`を含む行が出力されることを確認するアサーションを追加する）で、追加したログが実際に出力されることを検証する。
- `go build ./... && go vet ./... && go test ./... -race`がモジュール全体でエラーなく通ることを最終的な受け入れ基準とする。

## 将来拡張（本ドキュメントのスコープ外）

- `LogEvent`にtrace_id/span_id相関を追加する（`ctx`は既に受け取っているため、`audit.go`の`logCall`が使っているのと同じ`trace.SpanContextFromContext(ctx)`パターンをそのまま適用できる）。
- ログ集約基盤側で`event`フィールドをキーにした監視・アラートルールを設定する運用面の話（本ドキュメントはコード側の下地を作るところまで）。
- `mcprt call`等スタンドアロンCLIコマンドの監査ログは別ドキュメント（`2026-09-05-mcprt-cli-call-audit-log-design.md`）で扱う。
