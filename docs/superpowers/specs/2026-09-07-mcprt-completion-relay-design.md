# mcprt: completion/complete 中継 設計

## 背景・目的

MCP仕様は、プロンプトの引数やリソーステンプレートのURI変数を補完候補として提示する`completion/complete`リクエストを定義している。mcprtは現状、`ServerOptions.CompletionHandler`を配線していないため、downstreamクライアントが`completion/complete`を送ると即座に「サポートしない」エラーになる。

`tools/call`・`resources/read`・`prompts/get`は既に個別のbackendへフォワードされているのに対し、`completion/complete`だけが未対応になっている状態を解消する。

## スコープ

含める:
- `ref/prompt`（プロンプト名で指定）・`ref/resource`（URIで指定、リソーステンプレートを含む）両方の`completion/complete`を、該当プロンプト/リソース(テンプレート)を提供しているbackendへフォワードする
- 未知の`ref`（登録されていないプロンプト名・URI）はエラーを返す

含めない（将来拡張）:
- `prefix`/`overrides`適用後の名前と、backend側の元の名前が異なる場合の相関以上の複雑なロジック（既存の`promptGetHandler`等と同じ解決方法をそのまま使う）
- 複数backendにまたがる候補のマージ（`completion/complete`は1つの`ref`に対して1つのbackendが答えるものであり、tools/callと同様に「解決された1つのbackend」へ単純フォワードする設計とする）

## 技術的な制約（設計の前提）

`go-sdk@v1.7.0`を調査した結果:
- `mcp.ServerOptions.CompletionHandler func(context.Context, *CompleteRequest) (*CompleteResult, error)` — サーバー全体で1つ、`completion/complete`受信時に呼ばれる（`tools/call`などと違い、登録された個別アイテムごとのハンドラではない）。
- `mcp.CompleteParams.Ref *CompleteReference{Type, Name, URI}` — `Type == "ref/prompt"`なら`Name`、`Type == "ref/resource"`なら`URI`が実際の対象を指す。**この情報だけで、既存の`promptTable`/`resourceTable`/`resourceTemplateTable`によるルーティングがそのまま使える**（新しい状態は不要）。
- `mcp.ClientSession.Complete(ctx, params *CompleteParams) (*CompleteResult, error)` — mcprtがbackendへ`completion/complete`を送るクライアント側メソッド。

## 全体アーキテクチャ

```
  downstream ──▶ completion/complete ──▶ gateway.Server
                                            │ CompletionHandler
                                            │ (ref.Type/Name/URIでpromptTable/
                                            │  resourceTable/resourceTemplateTable
                                            │  を引き、backendを解決)
                                            ▼
                                       Backend Client
                                       (b.Session.Complete)
```

新規コンポーネントは不要。既存の`Server`が保持する`toolTable`/`resourceTable`/`resourceTemplateTable`/`promptTable`（いずれも`*router.Table[T]`、`Items map[string]*router.Resolved[T]`）をそのまま参照する。

## コンポーネント構成

### `internal/gateway/gateway.go`: `completionHandler`（新規）

```go
// completionHandler forwards completion/complete to the backend that owns
// req.Params.Ref (a prompt name or a resource/resource-template URI),
// resolved through the same tables registerPrompt/registerResource/
// registerResourceTemplate already used at registration time.
func (s *Server) completionHandler(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	ref := req.Params.Ref
	s.mu.Lock()
	var b *backend.Backend
	var originalRef *mcp.CompleteReference
	switch ref.Type {
	case "ref/prompt":
		if resolved, ok := s.promptTable.Items[ref.Name]; ok {
			b = s.backends[resolved.BackendName]
			originalRef = &mcp.CompleteReference{Type: "ref/prompt", Name: resolved.OriginalName}
		}
	case "ref/resource":
		if resolved, ok := s.resourceTable.Items[ref.URI]; ok {
			b = s.backends[resolved.BackendName]
			originalRef = &mcp.CompleteReference{Type: "ref/resource", URI: resolved.OriginalName}
		} else if resolved, ok := s.resourceTemplateTable.Items[ref.URI]; ok {
			// completion targets a resource template's URI template string
			// itself (e.g. "file:///dir/{f}"), matched exactly against
			// registered template strings -- not the same "does a concrete
			// URI match this template" resolution resourceTemplateReadHandler
			// does at read time.
			b = s.backends[resolved.BackendName]
			originalRef = &mcp.CompleteReference{Type: "ref/resource", URI: resolved.OriginalName}
		}
	}
	s.mu.Unlock()

	if b == nil {
		return nil, fmt.Errorf("completion: unknown %s (name=%q uri=%q)", ref.Type, ref.Name, ref.URI)
	}

	return b.Session.Complete(ctx, &mcp.CompleteParams{
		Ref:      originalRef,
		Argument: req.Params.Argument,
		Context:  req.Params.Context,
	})
}
```

`registerPrompt`/`registerResource`/`registerResourceTemplate`と同じ「`prefix`/`overrides`適用後の`ref`から、backendの元の名前へ変換する」責務をここでも担う（`originalRef`の構築）。

### `internal/gateway/gateway.go`: `New`への配線

```go
mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{
	Logger:                    cfg.Logger,
	KeepAlive:                 cfg.KeepAlive,
	KeepAliveFailureThreshold: cfg.KeepAliveFailureThreshold,
	CompletionHandler:         s.completionHandler,
})
```

`s`（`*Server`）を先に構築してから`mcpSrv`を作る必要がある（`New`の現在のコード順序を入れ替える）ため、既存の「まず`mcpSrv`を作り、その後`Server`構造体を組み立てる」順序を見直す小さなリファクタリングを伴う。

## データフロー

1. downstreamが`completion/complete`（`ref.Type`が`ref/prompt`または`ref/resource`）を送信する。
2. `completionHandler`が`ref`から対応する`router.Resolved`エントリを引き、backendと元の名前/URIを得る。
3. 見つからなければエラーを返す。
4. 見つかれば`b.Session.Complete(ctx, ...)`をそのまま呼び、結果（またはエラー）をそのままdownstreamへ返す。

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| `ref.Type`が`ref/prompt`で、該当プロンプト名が未登録 | `completion: unknown ref/prompt "<name>"`エラーを返す |
| `ref.Type`が`ref/resource`で、該当URIが具体リソース・テンプレートいずれにも未登録 | `completion: unknown ref/resource "<uri>"`エラーを返す |
| backendが`completion/complete`自体をサポートしない | `b.Session.Complete`がSDKレベルのエラーを返す → そのままdownstreamへ伝播 |
| backendが切断中 | `b.Session.Complete`がエラーを返す → そのままdownstreamへ伝播（`tools/call`等と同じ扱い、特別なリトライはしない） |

## ロギング

`tools/call`等の監査ログ（`logCall`）と同じ扱いにはしない。`completion/complete`は高頻度（入力補完のたびに呼ばれうる）かつ副作用がないため、成功時はログを残さない。エラー時のみ`logger.Warn`（backend名・ref・エラー）を記録する。

## テスト方針

- **`internal/gateway`**: fakeバックエンドにプロンプト・リソーステンプレートを登録し、`ref/prompt`・`ref/resource`（テンプレート）それぞれで補完候補が正しく返ることを確認。未知の`ref`でエラーになることを確認。`prefix`適用時に、downstream向けの`ref.Name`とbackendへ送る`originalRef.Name`が正しく変換されることを確認（`promptGetHandler`のprefix変換テストと対になるケース）。
- `go test ./...`で完結、外部サービス依存なし。

## 将来拡張（本ドキュメントのスコープ外）

- なし（本機能はそれ自体で完結する）
