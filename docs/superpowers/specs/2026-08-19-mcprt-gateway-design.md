# mcprt: MCP Gateway v1 設計

## 背景・目的

複数のMCPサーバ（ローカルのstdioサブプロセス／リモートのHTTPサーバ）を1つのバックエンドとして束ね、単一のMCPエンドポイントとしてクライアントに見せるゲートウェイを作る。

- 各backendは既存の小さいMCPサーバをそのまま使う（gateway自身はtoolを自前実装しない）
- backend間でtool名が衝突した場合は、設定ファイルの記載順（＋明示的なoverrides）で解決する
- v1はスコープを絞り、後述の「将来拡張」は含めない

## スコープ (v1)

含める:
- backendのtool一覧をマージし、記載順＋overridesで衝突解決してクライアントに公開
- `tools/call` をtool名に応じて対応するbackendへ中継
- stdio / HTTPの両方でbackendに接続でき、両方でクライアントを受け付けられる

含めない（将来拡張。詳細は末尾）:
- 実行中の動的なtool一覧更新（`list_changed`対応）
- `resources/*`, `prompts/*` の中継
- `sampling/createMessage` の逆プロキシ
- backendへの自動再接続
- gateway自体の認証

## 全体アーキテクチャ

```
                    ┌─────────────────────────┐
  client(s) ──────▶ │  Gateway Server (SDK製)  │
 (stdio/HTTP)       │  - tools/list ハンドラ    │
                    │  - tools/call ハンドラ(汎用)│
                    └───────────┬─────────────┘
                                │ ルーティングテーブルを引く
                                ▼
                    ┌─────────────────────────┐
                    │   Router                 │
                    │  map[toolName]*Backend   │
                    └───────────┬─────────────┘
                                │
              ┌─────────────────┼─────────────────┐
              ▼                 ▼                 ▼
        Backend Client    Backend Client     Backend Client
        (stdio subprocess) (HTTP remote)      (HTTP remote)
```

gatewayは1プロセス。起動時に全backendへ接続してtool一覧を取得し、ルーティングテーブルを構築してからクライアント向けのサーバを起動する。v1では動的更新がないため、実行中このテーブルは不変。

クライアント向けの受け口（Server）はstdio・HTTPを設定に応じて有効化する。backend側は「stdioサブプロセス」「HTTPリモート」のどちらも同じ`Client`インターフェースとして扱う。

採用アーキテクチャ: 公式Go SDK (`modelcontextprotocol/go-sdk`, v1.0)の`Client`/`Server`をそのまま利用する。backend1台につき1つの`Client`を持ち、gatewayの`Server`には低レベルの汎用`CallTool`ハンドラを1つだけ登録してrouterに委譲する。生のJSON-RPCフレーミングやリクエストID対応はSDKに任せ、自前実装しない。

## コンポーネント構成

| コンポーネント | 役割 |
|---|---|
| `internal/config` | YAML設定ファイルをパースし、backend一覧・prefix・overridesの構造体に変換。バリデーション（overridesの参照先backendが存在するか、backend名の重複等）もここで行う |
| `internal/backend` | backend1台につき1インスタンス。stdioなら公式SDKでサブプロセスを起動、HTTPならリモートに接続。どちらも同じ`Client`として扱えるようラップする |
| `internal/router` | 全backendの`ListTools`結果を受け取り、prefix適用→記載順／overridesで衝突解決→`map[toolName]resolvedTool{backend, originalName}`を構築。隠れたtoolはWARNログに出す |
| `internal/gateway` | 公式SDKの`Server`をラップ。`tools/list`はrouterのテーブルをそのまま返し、`tools/call`は1個の汎用ハンドラでtool名をrouterに引かせてbackendへ転送 |
| `cmd/mcprt` | エントリポイント（cobraで構成）。v1から`server`サブコマンドとして実装（`mcprt server --config <path> [--log-level <level>]`）。起動シーケンス（全backend並行接続→router構築→server起動）を実行 |

## データフロー

**起動シーケンス**
1. `--config`のYAMLを読み込み、バリデーション（不正な場合は起動せずエラー終了）
2. 全backendへ並行接続（goroutine + WaitGroup）。失敗したbackendはERRORログを出して除外（best-effort、他は起動続行）
3. 接続できた各backendに対して`ListTools`を並行実行
4. 全結果をrouterに渡し、prefix適用→記載順／overridesで衝突解決→ルーティングテーブル確定（隠れたtoolのWARNログもここで出す）
5. 確定したtool一覧でSDKの`Server`を構成し、設定に応じてstdio/HTTPのリスンを開始

**リクエスト処理（`tools/call`）**
1. clientから`tools/call`（tool名Xを含む）を受信
2. routerでXを引き、対応するbackendの`Client`と、backend内での本来のtool名（prefixを剥がした名前）を取得
3. そのbackendの`Client.CallTool`を呼び出す（IDの対応付けはSDK内部に任せる）
4. 結果（成功／エラー）をそのままclientへ返す

**リクエスト処理（`tools/list`）**
- 起動時に確定した静的なルーティングテーブルをそのまま返すだけ（backendへの問い合わせは発生しない）

## 設定ファイル

```yaml
listen:
  stdio: true          # stdioでも待ち受けるか
  http: ":8080"         # 空文字/未指定ならHTTPは無効

backends:
  - name: filesystem
    transport: stdio
    command: ["mcp-server-filesystem", "--root", "/data"]
    env:
      FOO: bar
    # prefix省略時はprefixなし

  - name: github
    transport: http
    url: "http://localhost:9090/mcp"
    headers:
      Authorization: "Bearer ${GITHUB_TOKEN}"   # 環境変数展開
    prefix: "gh__"

overrides:
  search: github    # tool名"search"は常にbackend "github"を採用（記載順より優先）
```

- 優先度は**backendsリストの記載順**（上ほど高優先）のみで決める。数値によるpriority指定は行わない（overridesで個別に上書きできれば十分なため）
- `${VAR}`形式の環境変数展開は`headers`/`env`の値に対して行う
- `overrides`が存在しないbackend名を指している場合は起動時バリデーションでエラーにする

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| 起動時にbackend接続失敗 | ERRORログを出してそのbackendを除外、他は起動続行 |
| tool名の衝突（overrides未指定） | 記載順で先勝ち、負けた側はWARNログを出して`tools/list`から隠す |
| `overrides`が存在しないbackend名を指す等の設定ミス | 起動時バリデーションでエラーにして起動を止める |
| 実行中に`tools/call`がbackendからエラーを返す | そのままclientへエラーを転送 |
| 実行中にbackendが落ちる／接続が切れる | v1では自動再接続はしない。以後そのbackend宛の呼び出しはエラーを返し、ログに記録する。復旧はgateway再起動 |
| clientが存在しないtool名を呼ぶ | 標準的なMCPの「tool not found」エラーを返す |

## ロギング

- `log/slog`を使用
- `--log-level`フラグ（`debug`/`info`/`warn`/`error`、デフォルト`info`）でレベル切り替え
- 出力は人間が読みやすいテキストハンドラでstderrへ（JSON出力等は将来検討）

## CLI

- `spf13/cobra`を使用
- v1から`server`サブコマンドとして実装する: `mcprt server --config <path> [--log-level <level>]`
- 将来`mcprt validate`のような他のサブコマンドを追加しやすい構成にしておく（最初から単一コマンドで作らず、サブコマンド前提の構造にしておく）

## テスト方針

- `internal/router`: fakeのtool一覧を入力にした単体テストで衝突解決・overrides・prefix・隠蔽ログを網羅
- `internal/config`: YAMLフィクスチャを使ったパース・バリデーションの単体テスト（overrides参照先不在、backend名重複などの異常系含む）
- `internal/backend` / `internal/gateway`: `httptest.Server`でダミーのHTTP backendを複数（tool名を意図的に重複させる）立てて実際にgatewayを起動し、公式SDKの`Client`から接続して`tools/list`/`tools/call`が期待通り動くか検証。stdio backendも同様にテスト用の小さいMCPサーバをサブプロセスとして起動して検証
- `go test ./...`で完結、外部サービス依存なし

## 将来拡張（v1スコープ外）

- `notifications/tools/list_changed`購読による動的なルーティングテーブル再構築
- `resources/*`, `prompts/*` の同様の中継（router/gatewayのロジックを汎用化して使い回す想定）
- `sampling/createMessage`の逆プロキシ（backend→client方向のリクエスト転送。この場合はリクエストID/progressTokenの付け替えが必要になる）
- `notifications/progress`, `notifications/message`の透過転送
- backendへの自動再接続
- gateway自体の認証（HTTP公開時）
