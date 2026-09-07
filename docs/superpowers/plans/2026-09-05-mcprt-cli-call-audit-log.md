# mcprt: `mcprt call`の監査ログ Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `internal/gateway/audit.go`の`maskArguments`をexportし、`mcprt call`（`internal/cli/call.go`）が`mcprt server`の`logCall`と同じ精神（backend名・tool名・マスク済み引数・所要時間・成否）の監査ログを1行出すようにする。

**Architecture:** `maskArguments`を`MaskArguments`としてexportし（振る舞いは一切変更しない、単純なリネーム）、`internal/cli/call.go`に新規`logCLICall`ヘルパーを追加、`runCall`の`b.Session.CallTool`呼び出し前後に組み込む。

**Tech Stack:** Go 1.x, 標準の`log/slog`, `go test`（`-race`込み）。

**Spec:** `docs/superpowers/specs/2026-09-05-mcprt-cli-call-audit-log-design.md`

## Global Constraints

- `mcprt server`側の`logCall`・監査ログ体制（`internal/gateway/audit.go`）は`maskArguments`→`MaskArguments`のリネーム以外、一切変更しない。
- `mcprt call`のロジック（tool解決・実際の呼び出し・結果表示・エラーハンドリング）は変更しない——ログの追加のみ。
- `mcprt list`・`mcprt ping`・`mcprt import`・`mcprt export`・`mcprt validate`・`mcprt init`への同様のログ追加は対象外。
- `mcprt call`への`--log-level`/`--log-format`フラグ追加は対象外——既存の`logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))`（デフォルトInfoレベル）をそのまま使う。
- `go.mod`のモジュールパスは`github.com/wtnb75/mcprt`。

---

## ファイル構成

| ファイル | 種別 | 責務 |
|---|---|---|
| `internal/gateway/audit.go` | 変更 | `maskArguments`を`MaskArguments`としてexport |
| `internal/gateway/audit_test.go` | 変更 | `TestMaskArguments`の呼び出し名を更新 |
| `internal/cli/call.go` | 変更 | `logCLICall`ヘルパー新設、`runCall`への組み込み |
| `internal/cli/call_test.go` | 変更 | 監査ログの出力・マスキングを確認するテストを追加 |

---

## Task 1: `MaskArguments`のexportと`mcprt call`への監査ログ組み込み

**Files:**
- Modify: `internal/gateway/audit.go`
- Modify: `internal/gateway/audit_test.go`
- Modify: `internal/cli/call.go`
- Modify: `internal/cli/call_test.go`

**Interfaces:**
- Produces: `gateway.MaskArguments(v any, extraKeys []string) any`（`internal/cli/call.go`が消費する）。

- [ ] **Step 1: `internal/gateway/audit.go`の`maskArguments`をexportする**

`internal/gateway/audit.go`の`maskArguments`関数を変更:

Before:
```go
// maskArguments returns a copy of v with any object key matching (case-
// insensitively, by substring) one of defaultMaskKeyPatterns or extraKeys
// replaced with "***". v is either json.RawMessage (tool arguments) or
// map[string]string (prompt arguments); both are normalized to a walkable
// any tree first. A v of neither type, or malformed JSON, falls back to a
// string representation rather than panicking or dropping the field.
func maskArguments(v any, extraKeys []string) any {
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
After:
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

`internal/gateway/audit.go`の`logCall`内、唯一の呼び出し箇所を変更:

- Before: `attrs = append(attrs, "arguments", maskArguments(args, maskKeys))`
- After: `attrs = append(attrs, "arguments", MaskArguments(args, maskKeys))`

`internal/gateway/audit_test.go`の`TestMaskArguments`内、呼び出し名を変更:

- Before: `got := maskArguments(c.v, c.extraKeys)`
- After: `got := MaskArguments(c.v, c.extraKeys)`

（エラーメッセージ内の`"maskArguments(%#v, %v) = ..."`という文字列はテストの説明文なので、そのままでもよいし`MaskArguments`に合わせて変えてもよい——どちらでも動作に影響しない。）

- [ ] **Step 2: `go test`で`internal/gateway`パッケージを確認する**

Run: `go build ./internal/gateway/... && go vet ./internal/gateway/... && go test ./internal/gateway/... -run TestMaskArguments -race -v`
Expected: `PASS`。

- [ ] **Step 3: `internal/cli/call.go`に`logCLICall`を追加し、`runCall`に組み込む**

`internal/cli/call.go`の`import`に`"time"`を追加する。`printCallJSON`の前あたりに新規関数を追加:

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

`runCall`内、`b.Session.CallTool`呼び出しの直後を変更:

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

（`cfg`・`logger`・`arguments`・`resolved`・`toolName`は全て`runCall`内の既存のローカル変数——新規のフィールド追加やシグネチャ変更は不要。`gateway`パッケージは`call.go`で既にimport済み。）

- [ ] **Step 4: `go build`でコンパイルエラーを確認する**

Run: `go build ./... && go vet ./...`
Expected: エラーなし。

- [ ] **Step 5: `internal/cli/call_test.go`にテストを追加する**

`internal/cli/call_test.go`の末尾に追加。マスク対象キーを含む`--args`で呼び出し、stderr（`root.SetErr`で捕捉）に監査ログ行が出て、値がマスクされていることを確認する:

```go
// TestCallCommand_LogsAuditLineWithMaskedArguments checks that mcprt call
// now emits a "cli tool call" audit-style log line (to stderr, via the
// logger runCall already builds) carrying the backend name, tool name, and
// masked arguments -- mirroring the gateway's own tools/call audit line but
// for the standalone CLI path, which previously logged nothing at all.
func TestCallCommand_LogsAuditLineWithMaskedArguments(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	root := cli.NewRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"call", "--config", configPath, "--args", `{"message":"hi","api_key":"secret-value"}`, "echo"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("call: %v", err)
	}

	logged := errOut.String()
	if !strings.Contains(logged, "cli tool call") {
		t.Fatalf("log output = %q, want a \"cli tool call\" line", logged)
	}
	if !strings.Contains(logged, "backend=backend-a") {
		t.Fatalf("log output = %q, want backend=backend-a", logged)
	}
	if !strings.Contains(logged, "tool=echo") {
		t.Fatalf("log output = %q, want tool=echo", logged)
	}
	if strings.Contains(logged, "secret-value") {
		t.Fatalf("log output = %q, want api_key's value masked, not present in plaintext", logged)
	}
	if !strings.Contains(logged, "***") {
		t.Fatalf("log output = %q, want a masked (***) value present", logged)
	}
}

// TestCallCommand_LogsAuditLineOnFailure checks the failure path: a tool
// call that itself fails with a Go error (not just an IsError result) still
// produces a "cli tool call failed" line with the error. The fake backend's
// "explode" tool handler returns a non-nil error (rather than a result with
// IsError: true, which the existing "boom" tool in newFakeCallBackend uses)
// -- go-sdk's mcp.AddTool translates a handler's returned error into a
// JSON-RPC error response, which surfaces at the CALLER's CallTool as a
// genuine Go error, exactly the path logCLICall's err branch needs to
// exercise. This is a dedicated inline backend (not the shared
// newFakeCallBackend helper), since none of its existing tools return a Go
// error this way and it's simpler to add one tool here than to change a
// helper other tests also rely on.
func TestCallCommand_LogsAuditLineOnFailure(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "backend-a", Version: "v1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "explode", Description: "always fails", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, fmt.Errorf("boom")
		})
	backendA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	root := cli.NewRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"call", "--config", configPath, "explode"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := root.ExecuteContext(ctx); err == nil {
		t.Fatal("call explode: got no error, want one (the tool handler always returns an error)")
	}

	logged := errOut.String()
	if !strings.Contains(logged, "cli tool call failed") {
		t.Fatalf("log output = %q, want a \"cli tool call failed\" line", logged)
	}
	if !strings.Contains(logged, "backend=backend-a") || !strings.Contains(logged, "tool=explode") {
		t.Fatalf("log output = %q, want backend=backend-a and tool=explode", logged)
	}
}
```

（`bytes`・`strings`・`context`・`fmt`・`net/http`・`net/http/httptest`・`github.com/modelcontextprotocol/go-sdk/mcp`が`call_test.go`にまだimportされていなければ追加する——既存の`TestCallCommand_Text`・`newFakeCallBackend`が大半を既にimport済みのはず。`writeConfig`は既存のヘルパー（`server_test.go`で定義、同じ`cli_test`パッケージ内なので参照可能）。）

- [ ] **Step 6: 新規テストを実行する**

Run: `go test ./internal/cli/... -run TestCallCommand_LogsAuditLine -race -v`
Expected: `PASS`（両テスト）。

- [ ] **Step 7: モジュール全体のテストを実行する**

Run: `go build ./... && go vet ./... && go test ./... -race`
Expected: 全パッケージ`ok`。

- [ ] **Step 8: コミット**

```bash
git add internal/gateway/audit.go internal/gateway/audit_test.go \
        internal/cli/call.go internal/cli/call_test.go
git commit -m "feat(cli,gateway): add audit logging to mcprt call, export MaskArguments"
```

---

## Self-Review メモ（実行者向けではなく記録用）

- Spec coverage: specの「含める」2項目（`MaskArguments`のexport、`mcprt call`への監査ログ追加）は全てTask 1でカバー。「含めない」項目（他のCLIコマンドへの拡張、`--log-level`/`--log-format`フラグ、`internal/gateway/events.go`との統合）は触れない。
- タスクを1つにまとめた理由: exportされる`MaskArguments`とそれを使う`logCLICall`は互いに意味を持たない片方だけの変更（exportだけでは何も使われず、`logCLICall`だけでは未exportの関数を参照してコンパイルが通らない）であり、レビュー時に片方だけを承認する余地がないため、2つのSDDタスクに分ける価値がない——1タスクとして扱う。
- 型整合性: `MaskArguments(v any, extraKeys []string) any`のシグネチャは、Step 1でexportした通りStep 3の`logCLICall`から一貫して使われている。
