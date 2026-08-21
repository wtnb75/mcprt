# mcprt: 監査・障害調査用ログ拡充 設計

## 背景・目的

現状の`internal/gateway/gateway.go`のハンドラ群（`callHandler`/`resourceReadHandler`/`resourceTemplateReadHandler`/`promptGetHandler`）は、backend呼び出しが**失敗した時だけ**`logger.Error("backend call failed", ...)`を出す。成功した呼び出しは一切ログに残らない。`internal/cli/server.go`の`connectBackends`/`runServer`も同様に、失敗系（接続失敗・List失敗・名前衝突）のみログを出し、起動成功・backend接続成功はログに残らない。

これは「異常系のデバッグ用ログ」としては機能するが、以下2つの目的には不十分:

- **障害調査**: 問題発生前後に何が呼ばれていたか（成功していた呼び出しも含めて）を事後に再構成できない。
- **セキュリティ監査**: 誰が・いつ・何を呼んだかの証跡が残らない。呼び出し元の識別情報（MCPクライアント名/バージョン、セッションID、HTTP接続元IP）も一切記録されない。

本ドキュメントは、既存の`log/slog`ベースのロギングを拡張し、成功・失敗を問わず1呼び出し1行の構造化ログを残す設計を定義する。

SDK（`github.com/modelcontextprotocol/go-sdk` v1.7.0）を調査した結果、以下を確認済み:

- 各ハンドラが受け取る`req *mcp.CallToolRequest`等（`= mcp.ServerRequest[*mcp.CallToolParamsRaw]`等のエイリアス）は`Session *mcp.ServerSession`フィールドを持ち、`req.Session.InitializeParams().ClientInfo`（`Name`/`Version`）と`req.Session.ID()`が追加配線なしで取得できる（`mcp/shared.go`, `mcp/server.go`）。
- HTTPのRemoteAddrは、`mcp.NewStreamableHTTPHandler`に渡す`http.Handler`をラップし、`r.Context()`に`context.WithValue`で値を注入すれば、セッション確立時の`req.Context()`がセッションの基底contextとして使われるため、以後の全呼び出しのハンドラ`ctx`からも取得できる（`mcp/streamable.go`のセッション確立コード、および`internal/xcontext.Detach`が値だけを残しcancelを切り離す実装であることを確認済み）。
- `tools/call`の`Arguments`は`json.RawMessage`、`prompts/get`の`Arguments`は`map[string]string`（`mcp/protocol.go`）。マスキング関数はこの2形を両方扱えるようにする。

## スコープ

含める:
- `internal/gateway`の4ハンドラで、成功・失敗を問わず1呼び出し1行の監査ログを出す
- 呼び出し元識別情報（MCPクライアント名/バージョン、セッションID、HTTP接続元IP）の記録
- ツール/プロンプト引数のキー名ベースマスキング（デフォルトパターン＋config.yamlでの追加指定）
- 起動時の成功系ログ（backend接続成功、サーバlisten開始）
- `--log-format text|json`フラグの追加

含めない（将来拡張）:
- resource URIのマスキング（URIにクエリパラメータ等で機微情報が載るケースは今回は対象外。必要になれば別途検討）
- ログのファイル出力・ローテーション（現状どおりstderrのみ。ファイル出力はsystemd/docker等の外部仕組みに委ねる）
- 値ベースのマスキング（正規表現によるクレジットカード番号検出等の高度なDLP的機能）
- HTTPアクセスログ（TCP接続単位の記録。今回はMCPセッション単位の`remote_addr`記録に留める）
- 監査ログの改ざん防止・署名（syslog転送やWORMストレージ等の運用要件は本ドキュメントの対象外）

## 全体アーキテクチャ

```
                    ┌─────────────────────────────┐
  client(s) ──────▶ │   gateway handlers            │
 (stdio/HTTP)       │   callHandler / resourceRead   │
                    │   Handler / resourceTemplate   │
                    │   ReadHandler / promptGetHandler│
                    └───────────────┬─────────────────┘
                                    │ 呼び出し前後で
                                    ▼
                    ┌─────────────────────────────┐
                    │  logCall (internal/gateway/    │
                    │  audit.go)                     │
                    │  - backend/tool名               │
                    │  - session_id/client_name/      │
                    │    client_version（req.Session）│
                    │  - remote_addr（ctxから、HTTPのみ)│
                    │  - duration_ms                  │
                    │  - arguments（maskArguments適用）│
                    │  - 成功: Info / 失敗: Error       │
                    └─────────────────────────────┘

  HTTPのみ: ServeHTTP が http.Handler を remoteAddr注入
  middlewareでラップ → r.Context() 経由でセッションの
  基底contextに値が乗り、以後の全呼び出しから参照可能
```

## コンポーネント構成

### `internal/gateway/audit.go`（新規）

```go
// logCall logs one backend call's outcome — success or failure — in a
// consistent shape, so investigating an incident doesn't require treating
// the success and error paths as separate log formats.
// kind labels the log message ("tool"/"resource"/"resource template"/"prompt");
// nameKey is the field name for name ("tool"/"uri"/"prompt" — resource and
// resource template both use "uri").
func logCall(ctx context.Context, logger *slog.Logger, kind, nameKey, name, backend string, sess *mcp.ServerSession, args any, maskKeys []string, start time.Time, err error) {
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
    if args != nil {
        attrs = append(attrs, "arguments", maskArguments(args, maskKeys))
    }
    if err != nil {
        logger.Error(kind+" call failed", append(attrs, "error", err)...)
        return
    }
    logger.Info(kind+" call", attrs...)
}

// maskArguments returns a copy of v with any object key matching (case-
// insensitively, by substring) one of the default patterns or extraKeys
// replaced with "***". v is either json.RawMessage (tool arguments) or
// map[string]string (prompt arguments); both are normalized to a walkable
// any tree first.
func maskArguments(v any, extraKeys []string) any

// defaultMaskKeyPatterns are matched case-insensitively as substrings
// against argument key names: covers apikey/api_key/access_key/private_key
// (key), authorization (auth), password/passwd (pass), credential (cred),
// token.
var defaultMaskKeyPatterns = []string{"key", "auth", "pass", "cred", "token"}
```

HTTP RemoteAddr伝播用に、同ファイルに以下を追加:

```go
type remoteAddrKey struct{}

func remoteAddrMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), remoteAddrKey{}, r.RemoteAddr)))
    })
}

func remoteAddrFromContext(ctx context.Context) (string, bool) {
    addr, ok := ctx.Value(remoteAddrKey{}).(string)
    return addr, ok
}
```

### `internal/gateway/gateway.go`（既存4ハンドラの変更）

各ハンドラの末尾、現在の`if err != nil { logger.Error(...) }`ブロックを`logCall(...)`呼び出し1行に置き換える。例（`callHandler`）:

```go
func callHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalName string) mcp.ToolHandler {
    return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
        start := time.Now()
        result, err := b.Session.CallTool(ctx, &mcp.CallToolParams{
            Name:      originalName,
            Arguments: req.Params.Arguments,
        })
        logCall(ctx, logger, "tool", "tool", originalName, b.Name, req.Session, req.Params.Arguments, maskKeys, start, err)
        return result, err
    }
}
```

`resourceReadHandler`/`resourceTemplateReadHandler`は引数を持たない（URI呼び出しのみ）ので`args`は`nil`を渡し、ログに`arguments`フィールドは出ない。

`New`/`registerTool`等のシグネチャに`maskKeys []string`を通す（`gateway.New`の引数に追加）。

### `internal/gateway/gateway.go`（`ServeHTTP`の変更）

```go
func ServeHTTP(ctx context.Context, srv *mcp.Server, addr string) error {
    handler := remoteAddrMiddleware(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
    ...
}
```

### `internal/cli/server.go`（起動時ログ）

- `connectBackends`のgoroutine内、`backend.Connect`成功直後に`logger.Info("backend connected", "backend", bc.Name, "transport", bc.Transport)`を追加。
- `runServer`で各リスナーをgoroutine起動する直前に`logger.Info("listening", "stdio", cfg.Listen.Stdio, "http", cfg.Listen.HTTP)`を追加。
- `gateway.New(...)`呼び出しに`cfg.Logging.MaskKeys`（後述の設定）を渡す。

### `internal/cli/server.go`（`--log-format`フラグ）

```go
var logFormat string
cmd.Flags().StringVar(&logFormat, "log-format", "text", "log format: text or json")
...
func parseLogFormat(s string) (func(io.Writer, *slog.HandlerOptions) slog.Handler, error) {
    switch s {
    case "text":
        return func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewTextHandler(w, o) }, nil
    case "json":
        return func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewJSONHandler(w, o) }, nil
    default:
        return nil, fmt.Errorf("unknown log format %q (want text or json)", s)
    }
}
```

`parseLogLevel`と同じ位置・同じ形のバリデーション関数として追加する。

### `internal/config/config.go`（設定拡張）

```go
type Config struct {
    Listen                    ListenConfig      `yaml:"listen"`
    Backends                  []BackendConfig   `yaml:"backends"`
    Overrides                 map[string]string `yaml:"overrides,omitempty"`
    ResourceOverrides         map[string]string `yaml:"resource_overrides,omitempty"`
    ResourceTemplateOverrides map[string]string `yaml:"resource_template_overrides,omitempty"`
    PromptOverrides           map[string]string `yaml:"prompt_overrides,omitempty"`
    Logging                   LoggingConfig     `yaml:"logging,omitempty"`
}

// LoggingConfig controls audit-log behavior beyond what --log-level/--log-format
// (CLI flags) cover.
type LoggingConfig struct {
    // MaskKeys are extra case-insensitive substrings matched against
    // argument key names, in addition to the built-in defaultMaskKeyPatterns
    // ("key", "auth", "pass", "cred", "token").
    MaskKeys []string `yaml:"mask_keys,omitempty"`
}
```

`validate(cfg)`に追加のバリデーションは不要（自由な文字列リストのため）。

## データフロー

**tool呼び出しの例（resource/resource template/promptも対称）**

1. clientが`tools/call`を送信
2. SDKが`req *mcp.CallToolRequest`を構築（`req.Session`にセッション情報を含む）し、`callHandler`が返した`mcp.ToolHandler`を呼ぶ
3. ハンドラが`start := time.Now()`を記録し、`b.Session.CallTool(ctx, ...)`でbackendへ転送
4. 呼び出し完了後、成否に関わらず`logCall(ctx, logger, "tool", "tool", originalName, b.Name, req.Session, req.Params.Arguments, maskKeys, start, err)`を1回呼ぶ
5. `logCall`内: `req.Session.ID()`・`req.Session.InitializeParams().ClientInfo`から識別情報を取得、`ctx`から`remote_addr`を取得（HTTPかつmiddlewareが値を注入していれば）、`maskArguments`で引数をマスキングしたうえで、成功なら`Info`・失敗なら`Error`で1行出力
6. ハンドラは`result, err`をそのままSDKに返す（ログ出力は呼び出し結果に影響しない）

**HTTP RemoteAddrの伝播**

1. `ServeHTTP`が`remoteAddrMiddleware`でラップした`http.Handler`をlisten
2. clientからのPOSTリクエスト到達時、middlewareが`r.RemoteAddr`をcontextに注入してから`StreamableHTTPHandler`に委譲
3. セッション未確立（初回POST/初期化）の場合、SDKはこの`req.Context()`を新しいセッションの基底contextとして使う（`xcontext.Detach`でcancelは切り離すがValueは保持）
4. 以後、同一セッションでの全呼び出し（2回目以降のPOST、`sessInfo.transport.ServeHTTP`経由）でも、ハンドラに渡る`ctx`はこの基底contextの子孫であり続けるため`remote_addr`を参照できる
5. stdioの場合はmiddlewareが存在しないため`remote_addr`は常に付与されない（想定どおり）

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| backend呼び出し成功 | `logCall`が`logger.Info`で1行出力（従来は無出力） |
| backend呼び出し失敗 | `logCall`が`logger.Error`で1行出力（従来と同じ内容＋識別情報・duration追加） |
| `req.Session.InitializeParams()`が`nil`（通常は起こらないが防御的に） | `client_name`/`client_version`フィールドを省略し、他のフィールドはそのまま出力 |
| HTTPで`remote_addr`が未設定（stdioの場合、またはmiddlewareを経由しないテストコード等） | `remote_addr`フィールドを省略（エラーにしない） |
| `maskArguments`が未知の型（`json.RawMessage`でも`map[string]string`でもない）を受け取った | 呼び出し側で型を保証しているため通常は起こらない。防御的に`fmt.Sprintf("%v", v)`相当の文字列化にフォールバックする |
| `config.yaml`の`logging.mask_keys`が空 | デフォルトパターンのみ適用（従来どおり動作） |
| `--log-format`に不正な値 | `newServerCmd`のRunE内で即エラーを返す（`parseLogLevel`と同じパターン） |

## テスト方針

- **`internal/gateway/audit_test.go`（新規）**
  - `maskArguments`: フラットなキー一致、ネストしたobject/array、`map[string]string`（prompt引数）のそれぞれで期待通りマスキングされることを表形式でテスト。`extraKeys`との併用も確認。
  - `logCall`: `slog.NewTextHandler`/`NewJSONHandler`を`bytes.Buffer`に向け、成功時は`Info`、失敗時は`Error`で期待フィールド（`backend`/`tool`/`session_id`/`client_name`/`client_version`/`duration_ms`/`arguments`/`error`）が出ることを確認。
- **`internal/gateway/gateway_internal_test.go`（既存拡張）**
  - `callHandler`等が成功時にも1行ログを出すことを検証するケースを追加（既存のエラー時ログ検証パターンを流用）。
- **`internal/gateway`のHTTP統合テスト**
  - `httptest.Server`経由で`ServeHTTP`相当の構成を立て、`remoteAddrMiddleware`経由でログに`remote_addr`が乗ることを確認する統合テストを1つ追加。
- **`internal/config`**
  - `logging.mask_keys`のYAMLパースと、`Config`構造体への反映をテスト。
- **`internal/cli/server_internal_test.go`（既存拡張）**
  - `parseLogFormat`のユニットテスト（`parseLogLevel`と同型）。
  - `connectBackends`成功時に`backend connected`ログが出ることの確認。
- `go test ./...`で完結、外部サービス依存なし。

## 将来拡張（本ドキュメントのスコープ外）

- resource URIの機微情報マスキング
- ログのファイル出力・ローテーション
- 値ベース（正規表現）マスキング
- HTTP接続単位のアクセスログ
- 監査ログの改ざん防止・外部転送（syslog等）
