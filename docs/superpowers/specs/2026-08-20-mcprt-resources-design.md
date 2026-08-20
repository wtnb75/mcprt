# mcprt: resources/* 中継 設計

## 背景・目的

v1設計（`2026-08-19-mcprt-gateway-design.md`）はtoolの中継のみをスコープとし、`resources/*`, `prompts/*` の中継は将来拡張として明示的に除外した。promptの中継は別ドキュメント（`2026-08-20-mcprt-prompts-design.md`、未確定・保留中）で設計済み。本ドキュメントはresourceの中継（`resources/list` / `resources/templates/list` / `resources/read`）を対象に設計する。

SDK（`github.com/modelcontextprotocol/go-sdk` v1.7.0）を調査した結果、resourceはtool/promptと以下の点で構造的に異なることを確認した:

- **キーが名前ではなくURI**。`Resource.URI`は絶対URIである必要があり、`Server.AddResource`はURIが不正（`url.Parse`失敗）だとpanicする。この点は`AddTool`（スキーマ不正でpanic）と同じ性質で、`AddPrompt`（panicしない）とは異なる
- `resources/templates/list`という別の一覧がある（`ResourceTemplate.URITemplate`というRFC6570テンプレート文字列がキー）。`AddResourceTemplate`もテンプレート構文が不正だとpanicする
- `resources/read`の実装（`Server.readResource`）はgateway自身の`Server`が、登録済みのexact resourceを最初に試し、マッチしなければ登録済みのresource templateとのパターンマッチを試みる、という探索をSDK内部で行う。このため:
  - exact resourceのread handlerは常に同じ固定URIをbackendへ転送すればよい
  - resource templateのread handlerは、呼び出しごとに異なる実際のURI（`req.Params.URI`、テンプレートにマッチした具体URI）をそのままbackendへ転送する必要がある
- `resources/subscribe` / `resources/unsubscribe` / `notifications/resources/updated` という状態を持つプッシュ型の購読機構がある。tool/promptには存在しない

## スコープ

含める:
- backendの`resources/list`結果をマージし、記載順＋overridesで衝突解決してクライアントに公開
- backendの`resources/templates/list`結果を同様にマージして公開
- `resources/read`をURI（exact resourceまたはresource templateへのマッチ）に応じて対応するbackendへ中継
- 衝突解決には prompts設計で汎用化した`router.Resolve[T any]`をそのまま使う（router自体への変更は不要）

含めない（将来拡張）:
- `resources/subscribe` / `resources/unsubscribe` / `notifications/resources/updated`の中継。状態を持つプッシュ型の仕組みで、セッションごとの購読状態管理が必要になり、v1が一貫して除外してきた「動的な仕組み」の範疇に入るため
- `notifications/resources/list_changed`購読による動的なルーティングテーブル再構築
- URIの衝突を避けるための仮想scheme等によるbackendごとの名前空間分離（後述の「prefix非適用」を参照）

## 設計判断: URIへのprefix非適用

tool/promptのbackend単位`prefix`は、resourceおよびresource templateのURIには適用しない。

理由: URIはすでに`scheme://host/path`という構造でbackend固有の名前空間を内包しており、tool名（`search`のような汎用的な短い文字列）に比べて元々衝突しにくい。文字列連結でprefixを付与すると`gh__file:///data/x`のような無効なURIを生み出し、クライアントがそのURIを他の文脈（例えばブラウザやファイルシステム）で解釈しようとした場合に破綻する。衝突を仮想schemeで完全に回避する設計も検討したが、`resources/list`と`resources/read`双方でURIの往復変換が必要になり実装が複雑化するため、YAGNIの観点から見送る。

実際にURIが衝突した場合（複数backendが同一URIを返す）は、記載順＋`resource_overrides`（後述）で解決する。tool/promptと同じ衝突解決アルゴリズムを、prefixを空文字列にして適用するだけで済む。

## 全体アーキテクチャ

tool/prompt設計と同じ全体構造を踏襲する。起動時に全backendへ並行接続し、`ListTools`/`ListPrompts`に加えて`ListResources`/`ListResourceTemplates`を並行実行、4つの独立した`router.Table[T]`を構築してからgatewayのサーバを起動する。実行中この4つのテーブルは不変。

```
                    ┌──────────────────────────────┐
  client(s) ──────▶ │  Gateway Server (SDK製)        │
 (stdio/HTTP)       │  - tools/*, prompts/*           │
                    │  - resources/list, /templates/list, /read │
                    └───────────┬──────────────────┘
                                │ 4種類のTable[T]をそれぞれ引く
                                ▼
                    ┌──────────────────────────────┐
                    │   Router (generics, prompts設計で汎用化済み) │
                    │  Table[*mcp.Tool] / Table[*mcp.Prompt]       │
                    │  Table[*mcp.Resource] / Table[*mcp.ResourceTemplate] │
                    └───────────┬──────────────────┘
                                │
              ┌─────────────────┼─────────────────┐
              ▼                 ▼                 ▼
        Backend Client    Backend Client     Backend Client
```

## コンポーネント構成（変更点）

### `internal/router`

変更なし。prompts設計で導入した`Resolve[T any]`をそのまま使う。呼び出し側がresource用のヘルパーを渡す:

```go
resourceNameOf := func(r *mcp.Resource) string { return r.URI }
resourceRename := func(r *mcp.Resource, name string) *mcp.Resource { c := *r; c.URI = name; return &c }

templateNameOf := func(t *mcp.ResourceTemplate) string { return t.URITemplate }
templateRename := func(t *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate { c := *t; c.URITemplate = name; return &c }
```

resource用・resource template用の`router.Entry[T]`を構築する際は、prefixが適用されないよう`Prefix: ""`を常に渡す（backend設定の`prefix`フィールドは無視する。tool/promptのEntry構築では引き続き`cfg.Prefix`を使う）。

### `internal/backend`

`ListTools`/`ListPrompts`と対称な`ListResources`・`ListResourceTemplates`を追加する。

```go
func (b *Backend) ListResources(ctx context.Context) ([]*mcp.Resource, error) {
    var resources []*mcp.Resource
    for r, err := range b.Session.Resources(ctx, nil) {
        if err != nil {
            return nil, fmt.Errorf("backend %q: list resources: %w", b.Name, err)
        }
        resources = append(resources, r)
    }
    return resources, nil
}

func (b *Backend) ListResourceTemplates(ctx context.Context) ([]*mcp.ResourceTemplate, error) {
    var templates []*mcp.ResourceTemplate
    for t, err := range b.Session.ResourceTemplates(ctx, nil) {
        if err != nil {
            return nil, fmt.Errorf("backend %q: list resource templates: %w", b.Name, err)
        }
        templates = append(templates, t)
    }
    return templates, nil
}
```

### `internal/gateway`

`AddResource`/`AddResourceTemplate`はURI（テンプレート）が不正だとpanicするため、`addTool`と同じpanic-recovery＋フォールバックのパターンを再利用する。

```go
// registerResource は registerTool と同型：Fallbacks込みの候補を順に試し、
// 不正なURIでpanicしたら次点backendの定義にフォールバックする。
func registerResource(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Resource]) {
    candidates := append([]router.Candidate[*mcp.Resource]{{
        Item: resolved.Item, BackendName: resolved.BackendName, OriginalName: resolved.OriginalName,
    }}, resolved.Fallbacks...)
    for _, c := range candidates {
        b := backends[c.BackendName]
        if addResource(srv, logger, c.Item, resourceReadHandler(logger, b, c.OriginalName)) {
            return
        }
    }
    logger.Error("resource unavailable: every candidate backend had an invalid URI", "uri", resolved.Item.URI)
}

// resourceReadHandler は固定URI（登録時に確定した本来のURI）をbackendへ転送する。
func resourceReadHandler(logger *slog.Logger, b *backend.Backend, originalURI string) mcp.ResourceHandler {
    return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
        result, err := b.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: originalURI})
        if err != nil {
            logger.Error("backend call failed", "backend", b.Name, "uri", originalURI, "error", err)
        }
        return result, err
    }
}

// addResource は addTool と同型：panic recoveryで登録可否を返す。
func addResource(srv *mcp.Server, logger *slog.Logger, r *mcp.Resource, h mcp.ResourceHandler) (ok bool) {
    defer func() {
        if rec := recover(); rec != nil {
            logger.Error("invalid resource definition", "uri", r.URI, "error", rec)
            ok = false
        }
    }()
    srv.AddResource(r, h)
    return true
}
```

`registerResourceTemplate`/`addResourceTemplate`は上記と同型（`srv.AddResourceTemplate`を使う）だが、read handlerだけが異なる:

```go
// resourceTemplateReadHandler は client が指定した実際のURI（テンプレートにマッチした
// 具体URI）をそのままbackendへ転送する。exact resourceと違い固定URIを使わない —
// gatewayのServer自身がexact resourceで見つからない場合にテンプレートとマッチングして
// このhandlerを呼ぶため、呼び出しごとにURIが変わる。
func resourceTemplateReadHandler(logger *slog.Logger, b *backend.Backend) mcp.ResourceHandler {
    return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
        result, err := b.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
        if err != nil {
            logger.Error("backend call failed", "backend", b.Name, "uri", req.Params.URI, "error", err)
        }
        return result, err
    }
}
```

`New`のシグネチャは、tool/prompt/resource/resource templateの4つの独立したテーブルを受け取る形に整理する:

```go
type Tables struct {
    Tools             *router.Table[*mcp.Tool]
    Prompts           *router.Table[*mcp.Prompt]
    Resources         *router.Table[*mcp.Resource]
    ResourceTemplates *router.Table[*mcp.ResourceTemplate]
}

func New(logger *slog.Logger, backends map[string]*backend.Backend, tables Tables) *mcp.Server
```

### `internal/config`

`resource_overrides`・`resource_template_overrides`をそれぞれ新設する（`prompt_overrides`と同じ理由：名前空間ごとに別キーとし、設定ファイル上の曖昧さを避ける）。キーはURI（もしくはURIテンプレート）そのもの、値はbackend名。構造・バリデーション（参照先backendの存在チェック等）は既存の`overrides`と同じ。

`prefix`フィールド自体はconfig構造体としては残すが、resource/resource template routing entryの構築では使わない（前述の設計判断）。

```yaml
resource_overrides:
  "file:///data/README.md": filesystem-primary

resource_template_overrides:
  "file:///data/{path}": filesystem-primary
```

### `internal/cli/server.go`

各backend接続後の並行フェッチに`ListResources`・`ListResourceTemplates`を追加する（`ListTools`・`ListPrompts`と合わせて4つを並行実行してよい。互いに独立）。それぞれ独立に`router.Resolve`へ渡し、`gateway.Tables`を組み立てて`gateway.New`に渡す。

## データフロー

**起動シーケンス**（v1・prompts設計からの差分）
1. 全backend接続後、`ListTools`/`ListPrompts`/`ListResources`/`ListResourceTemplates`を並行実行
2. それぞれ独立に`router.Resolve`で解決（resource/resource templateはprefix空文字列で解決。隠れたエントリのWARNログもそれぞれ独立に出す）
3. 4つのTableを`gateway.Tables`にまとめ`gateway.New`へ渡し、`tools/*`・`prompts/*`・`resources/*`の全ハンドラを構成したSDKの`Server`を起動

**リクエスト処理（`resources/read`、exact resource）**
1. clientから`resources/read`（URI Xを含む）を受信
2. gateway自身の`Server`が登録済みresourceの中からXに一致するものを見つけ、対応する`resourceReadHandler`を呼ぶ
3. handlerは登録時に固定した本来のURI（＝X。prefix非適用のためXと同一）で対応するbackendの`Client.ReadResource`を呼び出す
4. 結果（成功／エラー）をそのままclientへ返す

**リクエスト処理（`resources/read`、resource templateマッチ）**
1. clientから`resources/read`（具体URI Xを含む）を受信
2. gateway自身の`Server`がexact resourceでは見つからず、登録済みresource templateのいずれかにXがマッチすることを検出し、そのテンプレートの`resourceTemplateReadHandler`を呼ぶ
3. handlerはリクエストに含まれる実際のURI（X）をそのまま、そのテンプレートが属するbackendの`Client.ReadResource`へ転送する
4. 結果をそのままclientへ返す

**リクエスト処理（`resources/list` / `resources/templates/list`）**
- 起動時に確定した静的なTableをそのまま返す（backendへの問い合わせは発生しない。`AddResource`/`AddResourceTemplate`で登録済みのため、実装上はgatewayの`Server`が自動的に処理する）

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| 起動時にbackend接続失敗 | ERRORログを出してそのbackendを除外（tool/prompt/resource/resource template全て）、他は起動続行 |
| URI（テンプレート）の衝突（overrides未指定） | 記載順で先勝ち、負けた側はWARNログを出して一覧から隠す |
| backendが返したURI/URIテンプレートが不正（`AddResource`/`AddResourceTemplate`がpanic） | tool同様、次点候補（`Fallbacks`）へフォールバック。全滅ならERRORログを出してそのURIは未公開 |
| `resource_overrides`/`resource_template_overrides`が存在しないbackend名を指す等の設定ミス | 起動時バリデーションでエラーにして起動を止める |
| 実行中に`resources/read`がbackendからエラーを返す | そのままclientへエラーを転送 |
| 実行中にbackendが落ちる／接続が切れる | v1同様、自動再接続はしない。以後そのbackend宛の呼び出しはエラーを返し、ログに記録する |
| clientが存在しない/マッチしないURIを読もうとする | gatewayの`Server`が自動的に`ResourceNotFoundError`を返す（追加実装不要） |

## ロギング

v1・prompts設計から変更なし（`log/slog`、`--log-level`フラグ、人間可読テキストハンドラでstderr）。

## テスト方針

- `internal/router`: 既存の汎用`Resolve[T]`テーブル駆動テストに、`*mcp.Resource`（URIキー、prefix空文字列）と`*mcp.ResourceTemplate`（URITemplateキー）のケースを追加
- `internal/backend`: 既存のfakeサーバーパターンにresource/resource templateの定義を混ぜて`ListResources`・`ListResourceTemplates`を検証
- `internal/gateway`: 既存のhttptest/stdio fake backendパターンに、意図的にURIを重複させたbackendと、resource templateを提供するbackendを追加し、`resources/list`・`resources/templates/list`・`resources/read`（exact一致・template一致の両方）が期待通り動くか検証
- `go test ./...`で完結、外部サービス依存なし

## 将来拡張（本ドキュメントのスコープ外）

- `resources/subscribe` / `resources/unsubscribe` / `notifications/resources/updated`の中継。gatewayが各backendのセッションに対して購読を仲介し、client-backend間のセッション対応表を持つ必要がある
- `notifications/resources/list_changed`・`notifications/tools/list_changed`・`notifications/prompts/list_changed`購読による動的なルーティングテーブル再構築
- URI衝突が実運用で頻発するようなら、仮想schemeによるbackendごとの名前空間分離を再検討する
