# mcprt: `mcprt call`の監査ログ 設計

## 背景・目的

mcprtの監査ログ体制を横断的に調査した結果（このドキュメントに先立つ調査セッション）、`internal/gateway/audit.go`の`logCall`（`tools/call`等のaudit行）は`mcprt server`経由の呼び出ししかカバーしておらず、**スタンドアロンCLIコマンドの`mcprt call`は一切監査されない**ことが判明した。

`internal/cli/call.go`の`runCall`は、config解決済みのbackendに対して`b.Session.CallTool(ctx, &mcp.CallToolParams{Name: resolved.OriginalName, Arguments: arguments})`を直接呼び出し、結果を標準出力に印字するだけで、`logCall`のような構造化ログを一切経由しない。オペレーターが手動で任意のtoolを任意の引数（`--args`で秘匥情報を含みうるJSON）で呼び出せる正規の手段であるにもかかわらず、「何をいつどのbackendに対して呼んだか」が全く記録に残らない——これは`mcprt server`側に監査ログが存在するという前提を静かに迂回できる穴である。

本ドキュメントは、`mcprt call`にmcprt server側の`logCall`と同じ精神（backend名・tool名・マスク済み引数・所要時間・成否）を持つ監査ログを追加する設計を定める。

## スコープ

含める:
- `internal/gateway/audit.go`の引数マスキング関数`maskArguments`を`MaskArguments`としてexportし、`internal/cli`から再利用できるようにする。
- `internal/cli/call.go`の`runCall`に、`b.Session.CallTool`呼び出しの前後を計測し、成功/失敗を1行のログとして記録する処理を追加する。

含めない（対象外）:
- `mcprt list`・`mcprt ping`・`mcprt import`・`mcprt export`・`mcprt validate`・`mcprt init`への同様のログ追加。`list`/`ping`は読み取り専用の診断コマンドであり監査の優先度が低く、`import`/`export`/`validate`/`init`はbackendへの呼び出しを一切行わない（ローカルファイル操作のみ）ため性質が異なる——将来、必要になれば別ドキュメントで扱う。
- `mcprt call`への`--log-level`/`--log-format`フラグの追加（`mcprt server`は持つが`call`にはない）。`runCall`は現状デフォルトのテキストハンドラ・Infoレベルで`slog.Logger`を既に持っており、本ドキュメントで追加するログ行はInfo/Errorレベルなのでデフォルト設定のままで出力される。フラグ拡張は別の関心事とし、対象外とする。
- `internal/gateway/events.go`（`docs/superpowers/specs/2026-09-05-mcprt-gateway-anomaly-events-design.md`で扱う、別の問題）との統合。本ドキュメントの対象は「`tools/call`と同種の呼び出しがどこでも監査されるようにする」ことであり、"注目すべき異常"を集約する`LogEvent`とは目的が異なる——`runCall`のログは既存の`logCall`と同様、成功/失敗ごとに1行を記録する形を踏襲する。

## 全体アーキテクチャ

```
internal/gateway/audit.go
┌─────────────────────────────────────────┐
│ func MaskArguments(v any, extraKeys       │  (旧 maskArguments を export)
│                     []string) any          │
└─────────────────────────────────────────┘
                    ▲
                    │ 再利用
                    │
internal/cli/call.go
┌─────────────────────────────────────────────────┐
│ func runCall(...) error {                          │
│     ...                                              │
│     start := time.Now()                               │
│     result, err := b.Session.CallTool(ctx, ...)         │
│     logCLICall(logger, resolved.BackendName, toolName,    │
│                 arguments, cfg.Logging.MaskKeys, start, err)│
│     ...                                                      │
│ }                                                              │
│                                                                  │
│ func logCLICall(logger, backend, tool string, arguments any,     │
│                  maskKeys []string, start time.Time, err error)   │
└───────────────────────────────────────────────────────────────────┘
```

## コンポーネント構成

### `internal/gateway/audit.go`: `MaskArguments`のexport

現在の`maskArguments`（引数マスキングのエントリポイント。`maskValue`・`shouldMask`は内部実装で、これらはexportしない）をexportする:

```go
// MaskArguments returns a copy of v with any object key matching (case-
// insensitively, by substring) one of defaultMaskKeyPatterns or extraKeys
// replaced with "***". v is either json.RawMessage (tool arguments) or
// map[string]string (prompt arguments); both are normalized to a walkable
// any tree first. A v of neither type, or malformed JSON, falls back to a
// string representation rather than panicking or dropping the field.
//
// Exported so internal/cli's standalone commands (mcprt call) can apply the
// exact same masking rules mcprt server's own audit log (logCall, below)
// uses -- two independently-maintained masking implementations would risk
// silently drifting apart on which key patterns get redacted.
func MaskArguments(v any, extraKeys []string) any {
	switch t := v.(type) {
	case json.RawMessage:
		var parsed any
		if err := json.Unmarshal(t, &parsed); err != nil {
			return string(t)
		}
		return maskValue(parsed, extraKeys)
	case map[string]string:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = val
		}
		return maskValue(m, extraKeys)
	default:
		return fmt.Sprintf("%v", v)
	}
}
```

`logCall`内の呼び出し（`maskArguments(args, maskKeys)`）を`MaskArguments(args, maskKeys)`に変更する。`maskValue`・`shouldMask`・`defaultMaskKeyPatterns`はunexportedのまま。

### `internal/cli/call.go`: `logCLICall`（新規）と`runCall`への組み込み

```go
// logCLICall logs one mcprt-call invocation's outcome in the same spirit as
// the gateway's own tools/call audit line (internal/gateway/audit.go's
// logCall) -- backend, tool, masked arguments, duration, success/failure --
// but without the server-side identity fields (session_id, client_name/
// version, remote_addr, trace/span id) that don't exist for a one-shot CLI
// invocation with no downstream MCP session. Uses "cli " message prefixes
// (distinct from logCall's "tool call"/"tool call failed") so a log
// pipeline that also ingests mcprt server's audit log can tell a
// human-operator-invoked call apart from one the gateway relayed.
func logCLICall(logger *slog.Logger, backendName, tool string, arguments any, maskKeys []string, start time.Time, err error) {
	attrs := []any{
		"backend", backendName,
		"tool", tool,
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if arguments != nil {
		attrs = append(attrs, "arguments", gateway.MaskArguments(arguments, maskKeys))
	}
	if err != nil {
		logger.Error("cli tool call failed", append(attrs, "error", err)...)
		return
	}
	logger.Info("cli tool call", attrs...)
}
```

`runCall`（`internal/cli/call.go`）の変更箇所:

Before:
```go
	result, err := b.Session.CallTool(ctx, &mcp.CallToolParams{Name: resolved.OriginalName, Arguments: arguments})
	if err != nil {
		return fmt.Errorf("calling tool %q: %w", toolName, err)
	}
```
After:
```go
	start := time.Now()
	result, err := b.Session.CallTool(ctx, &mcp.CallToolParams{Name: resolved.OriginalName, Arguments: arguments})
	logCLICall(logger, resolved.BackendName, toolName, arguments, cfg.Logging.MaskKeys, start, err)
	if err != nil {
		return fmt.Errorf("calling tool %q: %w", toolName, err)
	}
```
（`"time"`パッケージのimportを追加する。`arguments`は既存の`var arguments any`——`--args`未指定なら`nil`のまま、指定されていれば`json.RawMessage(argsJSON)`——をそのまま渡す。`cfg.Logging.MaskKeys`は`mcprt server`が使っているのと同じconfigフィールドで、`mcprt call`も同じ`config.Load(configPath)`で読み込み済みのため新規のフィールド追加は不要。）

`result.IsError`（tool自体が「成功はしたがエラー結果を返した」ケース）は`b.Session.CallTool`自体のGoエラー値としては現れないため、`logCLICall`の`err`引数には影響しない——これは`logCall`が`tools/call`のGoエラーだけを見て`IsError`を見ていないのと同じ扱いで、一貫性がある（`result.IsError`はtool自身の実行結果であり、mcprt側の呼び出し失敗ではない）。

## データフロー

1. `mcprt call`が`--args`で渡されたJSON引数を解決する（既存ロジック、変更なし）。
2. `start := time.Now()`で計測開始。
3. `b.Session.CallTool`を呼ぶ（既存ロジック、変更なし）。
4. `logCLICall`を呼び、`gateway.MaskArguments`でマスクした引数・backend名・tool名・所要時間・成否を1行ログ出力する。
5. 既存通りエラーハンドリング・結果表示（`printCallJSON`/`printCallText`）に続く。

## エラーハンドリング

`logCLICall`はエラーを返さない（`slog.Logger`のログ出力自体は失敗しても呼び出し元に伝播させない、既存の`logCall`と同じ設計）。`runCall`の既存のエラー処理（`calling tool %q`のwrapエラー、`result.IsError`時のエラー返却）は変更しない。

## テスト方針

- `internal/cli/call_test.go`に、`root.SetErr(&errOut)`（既存の`export_test.go`/`import_test.go`が使っているパターン）でstderrを捕捉し、`mcprt call`実行後に以下を確認するテストを追加する:
  - 成功時: `errOut`に`"cli tool call"`というmsgを含む行があり、backend名・tool名が正しく出力されている。
  - `--args`にマスク対象キー（例: `api_key`）を含めて呼んだ場合、その値が`errOut`に平文で出現せず、代わりに`***`が出現する（`internal/gateway.MaskArguments`が正しく適用されていることの確認）。
  - `boom`のようにtool呼び出し自体がGoエラーを返すケースで、`"cli tool call failed"`というmsgと`error`フィールドが出力される。
- `internal/gateway/audit_test.go`の既存の`TestMaskArguments`はそのまま（`maskArguments`→`MaskArguments`のリネームに伴い、テスト内の呼び出し名だけ更新する）。
- `go build ./... && go vet ./... && go test ./... -race`がモジュール全体でエラーなく通ることを最終的な受け入れ基準とする。

## 将来拡張（本ドキュメントのスコープ外）

- `mcprt list`・`mcprt import`等、他のスタンドアロンコマンドへの同様のログ追加。
- `mcprt call`への`--log-level`/`--log-format`フラグ追加（`mcprt server`との一貫性）。
- `internal/gateway/events.go`の`LogEvent`（別ドキュメント）との将来的な統合可能性の検討。
