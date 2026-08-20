# mcprt: prompts/* 中継 設計

## 背景・目的

v1設計（`2026-08-19-mcprt-gateway-design.md`）はtoolの中継のみをスコープとし、`resources/*`, `prompts/*` の中継は将来拡張として明示的に除外した。本ドキュメントはそのうちpromptの中継（`prompts/list` / `prompts/get`）を対象に設計する。resourceは引き続き対象外。

SDK（`github.com/modelcontextprotocol/go-sdk` v1.7.0）を調査した結果、`Prompt`/`AddPrompt`/`ListPrompts`/`GetPrompt`はtool側の`Tool`/`AddTool`/`ListTools`/`CallTool`と構造的に対称であることを確認した:

- `Prompt.Name`（`Tool.Name`と同様、名前で一意）
- `Server.AddPrompt(p *Prompt, h PromptHandler)` / `PromptHandler func(context.Context, *GetPromptRequest) (*GetPromptResult, error)`
- `ClientSession.Prompts(ctx, params) iter.Seq2[*Prompt, error]`（ページネーション付き取得。`ClientSession.Tools`と同型）
- `ClientSession.GetPrompt(ctx, *GetPromptParams) (*GetPromptResult, error)`（`GetPromptParams.Name` + `Arguments map[string]string`）

ただし`AddTool`と異なり`AddPrompt`はスキーマ検証を行わずpanicしない（`Prompt`にJSON Schemaに相当するフィールドがないため）。この非対称点は設計に反映する（後述）。

## スコープ

含める:
- backendの`prompts/list`結果をマージし、記載順＋overridesで衝突解決してクライアントに公開
- `prompts/get`をprompt名に応じて対応するbackendへ中継
- 衝突解決アルゴリズム（prefix適用→記載順→overrides→隠れたエントリのWARNログ）をtool/prompt間で共有する汎用実装への`internal/router`のリファクタ

含めない（将来拡張）:
- `resources/*`の中継
- `notifications/prompts/list_changed`購読による動的更新
- `completion/complete`（prompt引数のオートコンプリート）の中継
- backendへの自動再接続、gateway自体の認証（v1から引き続き対象外）

## 全体アーキテクチャ

tool側と同じ全体構造を踏襲する。起動時に全backendへ並行接続し、`ListTools`と`ListPrompts`を並行実行、それぞれ独立した`router.Table`（`Table[*mcp.Tool]`と`Table[*mcp.Prompt]`）を構築してからgatewayのサーバを起動する。v1同様、実行中この2つのテーブルは不変。

```
                    ┌─────────────────────────┐
  client(s) ──────▶ │  Gateway Server (SDK製)  │
 (stdio/HTTP)       │  - tools/list, tools/call │
                    │  - prompts/list, prompts/get│
                    └───────────┬─────────────┘
                                │ Tool用テーブル / Prompt用テーブルをそれぞれ引く
                                ▼
                    ┌─────────────────────────┐
                    │   Router (generics)       │
                    │  Table[*mcp.Tool]          │
                    │  Table[*mcp.Prompt]        │
                    └───────────┬─────────────┘
                                │
              ┌─────────────────┼─────────────────┐
              ▼                 ▼                 ▼
        Backend Client    Backend Client     Backend Client
```

## コンポーネント構成（変更点）

### `internal/router`

`Resolve`を型パラメータ化し、tool/promptで衝突解決ロジックを共有する。

```go
type Entry[T any] struct {
    BackendName string
    Prefix      string
    Items       []T
}

type Candidate[T any] struct {
    Item         T
    BackendName  string
    OriginalName string
}

type Resolved[T any] struct {
    Item         T
    BackendName  string
    OriginalName string
    // Fallbacks holds the other backends' definitions for this same exposed
    // name, in priority order. Kept generic for symmetry with tools, even
    // though the prompt gateway path does not currently need it (AddPrompt
    // cannot fail the way AddTool can).
    Fallbacks []Candidate[T]
}

type Conflict struct { // 変更なし
    ExposedName string
    Winner      string
    Losers      []string
}

type Table[T any] struct {
    Items     map[string]*Resolved[T]
    Conflicts []Conflict
}

// Resolve merges entries into a single routing table, exactly as v1's
// Resolve did for tools. nameOf extracts an item's original (un-prefixed)
// name; rename returns a copy of an item with its Name field set to
// exposedName (mirrors v1's exposedTool helper).
func Resolve[T any](entries []Entry[T], nameOf func(T) string, rename func(T, string) T, overrides map[string]string) *Table[T]
```

`*mcp.Tool`と`*mcp.Prompt`はどちらも`Name`フィールドを持つが共通interfaceはないため、名前抽出・改名は呼び出し側が渡す関数（`nameOf`/`rename`）で行う。呼び出し側（`internal/cli/server.go`）は以下のようにtool/promptそれぞれのヘルパーを渡す:

```go
toolNameOf := func(t *mcp.Tool) string { return t.Name }
toolRename := func(t *mcp.Tool, name string) *mcp.Tool { c := *t; c.Name = name; return &c }

promptNameOf := func(p *mcp.Prompt) string { return p.Name }
promptRename := func(p *mcp.Prompt, name string) *mcp.Prompt { c := *p; c.Name = name; return &c }
```

### `internal/backend`

`ListTools`と対称な`ListPrompts`を追加する。

```go
// ListPrompts fetches the backend's full prompt list, following pagination.
func (b *Backend) ListPrompts(ctx context.Context) ([]*mcp.Prompt, error) {
    var prompts []*mcp.Prompt
    for p, err := range b.Session.Prompts(ctx, nil) {
        if err != nil {
            return nil, fmt.Errorf("backend %q: list prompts: %w", b.Name, err)
        }
        prompts = append(prompts, p)
    }
    return prompts, nil
}
```

### `internal/gateway`

`registerPrompt`と`promptGetHandler`を追加する。`AddPrompt`はスキーマ検証をせずpanicしないため、tool側の`addTool`が持つpanic-recoveryとフォールバックループは不要。winner候補をそのまま1回登録する。

```go
func registerPrompt(srv *mcp.Server, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Prompt]) {
    b := backends[resolved.BackendName]
    srv.AddPrompt(resolved.Item, promptGetHandler(logger, b, resolved.OriginalName))
}

func promptGetHandler(logger *slog.Logger, b *backend.Backend, originalName string) mcp.PromptHandler {
    return func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
        result, err := b.Session.GetPrompt(ctx, &mcp.GetPromptParams{
            Name:      originalName,
            Arguments: req.Params.Arguments,
        })
        if err != nil {
            logger.Error("backend call failed", "backend", b.Name, "prompt", originalName, "error", err)
        }
        return result, err
    }
}
```

`New`はtool用・prompt用それぞれの`*router.Table[*mcp.Tool]` / `*router.Table[*mcp.Prompt]`を別引数として受け取り（例: `toolTable`, `promptTable`）、それぞれの`.Items`を`registerTool`/`registerPrompt`でループ登録する。

### `internal/config`

prompt名の衝突解決用に`prompt_overrides: map[string]string`を新設する（`overrides`とは別キー。tool名とprompt名は別の名前空間のため、同じキーを共用すると「このキーがtoolとpromptのどちらを指すか」が設定ファイル上で曖昧になるのを避ける）。構造・バリデーション（参照先backendの存在チェック等）は`overrides`と同じ。

`prefix`は既存のbackend単位の1フィールドを、tool・prompt両方の公開名に引き続き適用する（backendごとに別々のprefixを持たせる要件は現状ない）。

```yaml
backends:
  - name: linter
    transport: stdio
    command: ["mcp-server-linter"]

  - name: linter-strict
    transport: stdio
    command: ["mcp-server-linter", "--strict"]

overrides:
  search: filesystem-archive       # tool名の衝突解決

prompt_overrides:
  code-review: linter-strict       # prompt名の衝突解決
```

### CLI起動シーケンス（`internal/cli/server.go`）

各backend接続後の並行フェッチに`ListPrompts`を追加する。`ListTools`と`ListPrompts`は同じbackend接続に対して並行実行してよい（互いに独立）。取得結果はそれぞれ独立に`router.Resolve`へ渡し、`gateway.New`にtool用・prompt用の2つのTableを渡す。

## データフロー

**起動シーケンス**（v1からの差分のみ）
1. 全backend接続後、`ListTools`と`ListPrompts`を並行実行
2. tool一覧を`router.Resolve`（tool用ヘルパー）でTool用Tableに、prompt一覧を`router.Resolve`（prompt用ヘルパー）でPrompt用Tableにそれぞれ解決（隠れたtool/promptのWARNログもそれぞれ独立に出す）
3. 両Tableを`gateway.New`へ渡し、`tools/*`と`prompts/*`の両ハンドラを構成したSDKの`Server`を起動

**リクエスト処理（`prompts/get`）**
1. clientから`prompts/get`（prompt名Xを含む、`Arguments`はオプション）を受信
2. Prompt用routerでXを引き、対応するbackendの`Client`と本来のprompt名を取得
3. そのbackendの`Client.GetPrompt`を`Arguments`をそのまま渡して呼び出す
4. 結果（成功／エラー）をそのままclientへ返す

**リクエスト処理（`prompts/list`）**
- 起動時に確定した静的なPrompt用Tableをそのまま返す（backendへの問い合わせは発生しない。SDKの`AddPrompt`で登録済みのため、実装上は`Server`が自動的に処理する）

## エラーハンドリング

tool用の表と同じパターンをpromptに適用する。

| ケース | 挙動 |
|---|---|
| 起動時にbackend接続失敗 | ERRORログを出してそのbackendを除外（tool/prompt両方から）、他は起動続行 |
| prompt名の衝突（`prompt_overrides`未指定） | 記載順で先勝ち、負けた側はWARNログを出して`prompts/list`から隠す |
| `prompt_overrides`が存在しないbackend名を指す等の設定ミス | 起動時バリデーションでエラーにして起動を止める |
| 実行中に`prompts/get`がbackendからエラーを返す | そのままclientへエラーを転送 |
| 実行中にbackendが落ちる／接続が切れる | v1同様、自動再接続はしない。以後そのbackend宛のprompt呼び出しはエラーを返し、ログに記録する |
| clientが存在しないprompt名を呼ぶ | 標準的なMCPの「not found」エラーを返す |

## ロギング

v1から変更なし（`log/slog`、`--log-level`フラグ、人間可読テキストハンドラでstderr）。

## テスト方針

- `internal/router`: 汎用化後の`Resolve[T]`をtool/prompt双方のfake入力で駆動するテーブル駆動テストに拡張。衝突解決・overrides・prefix・隠蔽ログのロジックはtool/prompt間で共有されるため、型パラメータを変えたテストケースの追加で足りる（アルゴリズム自体の再テストは不要）
- `internal/backend`: 既存の`newFakeMCPHandler`パターンにpromptの定義を混ぜて`ListPrompts`を検証
- `internal/gateway`: 既存のhttptest/stdio fake backendパターンに、意図的にprompt名を重複させたbackendを追加し、`prompts/list`/`prompts/get`が期待通り動くか検証
- `go test ./...`で完結、外部サービス依存なし

## 将来拡張（本ドキュメントのスコープ外）

- `resources/*`の中継（今回汎用化した`router.Resolve[T]`をそのまま利用できる想定。ただしresourceはURIキーでありtoolやpromptと異なり単純な「名前」ではないため、prefix/衝突解決の意味付けは別途検討が必要）
- `notifications/prompts/list_changed`・`notifications/tools/list_changed`購読による動的なルーティングテーブル再構築
- `completion/complete`の中継
