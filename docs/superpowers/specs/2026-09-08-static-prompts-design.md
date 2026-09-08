# mcprt: config直書きの static prompt 設計

## 背景・目的

`2026-08-20-mcprt-prompts-design.md`で実装した`prompts/*`中継は、backendが持つpromptをそのまま右から左へ転送するだけで、mcprt自身がpromptの内容を持つことはない。しかしmcprtは複数backendを1つのgatewayに集約する立場にあり、個々のbackendは互いの存在を知り得ない。「filesystemで読んだ内容をgithubにPRとして出す」のような、backendをまたぐ使い方の手順はbackend側では書けず、集約点であるmcprt自身にしか書けない。

本ドキュメントは、config.yamlに直接prompt定義（名前・説明・引数・テンプレートテキスト）を書き、mcprtがbackendを介さず自分でその`prompts/get`に応答する機能（以下static prompt）を設計する。

## スコープ

含める:
- config.yamlの新規トップレベルキー`prompts:`でのprompt定義（名前、説明、引数リスト、テンプレートテキスト）
- 引数はGoの`text/template`構文（`{{.name}}`）でテキストに埋め込む。呼び出し時の`arguments`（`map[string]string`）がそのままテンプレートのドット変数になる
- `required: true`の引数が呼び出し時に欠けていたら`prompts/get`をエラーにする
- config側のprompt名がbackend提供のprompt名と衝突した場合、常にconfig側を優先する（backend側は登録されない）
- 起動時／SIGHUPリロード時のバリデーション（名前重複、テンプレート構文エラー）

含めない（将来拡張、あるいは対象外と判断):
- 複数メッセージ（role別の会話履歴）の表現 -- 常に単一の`role: user`テキストメッセージ1件を返す
- `{{.name}}`以外のテンプレート構文（mustache風`{{name}}`など）
- config側prompt名の文字種バリデーション -- backend由来のprompt名も検証していないのと一貫させる。Claude Code側で`/mcp__<server>__<prompt>`のプロンプト名部分は許可文字（英数字・`-`・`_`）外が`_`に置換される、という運用上の注意はREADMEに記載するに留める
- `prompt_overrides`との統合 -- static promptは名前が常に勝つ独立ルールとし、`prompt_overrides`（backend間の衝突解決）とは別系統のまま扱う
- `notifications/prompts/list_changed`・`completion/complete` -- 既存のスコープ外方針を継続

## コンポーネント構成（変更点）

### `internal/config`

```go
// StaticPromptConfig defines a prompt mcprt serves directly, without
// forwarding prompts/get to any backend. See internal/gateway.StaticPrompt
// for the runtime representation built from this at gateway construction
// time (buildGateway parses Text as a template there; Validate below only
// checks it parses, it doesn't keep the *template.Template around).
type StaticPromptConfig struct {
    Name        string                 `yaml:"name"`
    Description string                 `yaml:"description,omitempty"`
    Arguments   []StaticPromptArgument `yaml:"arguments,omitempty"`
    Text        string                 `yaml:"text"`
}

type StaticPromptArgument struct {
    Name        string `yaml:"name"`
    Description string `yaml:"description,omitempty"`
    Required    bool   `yaml:"required,omitempty"`
}
```

`Config`に`Prompts []StaticPromptConfig `yaml:"prompts,omitempty"``を追加する（既存の`PromptOverrides map[string]string `yaml:"prompt_overrides,omitempty"``とは別キー。前者はbackend間の名前解決、後者はconfig定義そのもののprompt。混同を避けるため意図的に分ける）。

`Validate`に検証を追加する（`validateTimeouts`と同じ「エラーを返して起動を止める」方針）:

```go
func validateStaticPrompts(prompts []StaticPromptConfig) error {
    seen := make(map[string]bool, len(prompts))
    for _, p := range prompts {
        if p.Name == "" {
            return errors.New("prompts: name is required")
        }
        if seen[p.Name] {
            return fmt.Errorf("prompts %q: duplicate name", p.Name)
        }
        seen[p.Name] = true
        if p.Text == "" {
            return fmt.Errorf("prompts %q: text is required", p.Name)
        }
        if _, err := template.New(p.Name).Parse(p.Text); err != nil {
            return fmt.Errorf("prompts %q: parse text template: %w", p.Name, err)
        }
        argSeen := make(map[string]bool, len(p.Arguments))
        for _, a := range p.Arguments {
            if a.Name == "" {
                return fmt.Errorf("prompts %q: argument name is required", p.Name)
            }
            if argSeen[a.Name] {
                return fmt.Errorf("prompts %q: duplicate argument %q", p.Name, a.Name)
            }
            argSeen[a.Name] = true
        }
    }
    return nil
}
```

`mcprt validate`は`config.Load`（内部で`Validate`を呼ぶ）しか呼ばずbackendに接続しないため、テンプレート構文エラーはここで検出できる必要がある -- gateway層で初めて`text/template.Parse`するのでは`mcprt validate`が素通ししてしまう。この検証は`text/template.Parse`を通すだけで、実際に使う`*template.Template`は保持しない（保持・実行するのはgateway層の役目、後述）。

`export`/`import`（`internal/cli/export.go`/`import.go`、mcprt configとVS Code風`mcp.json`の相互変換）は、backendの接続情報のみを対象にしており、prompt定義は元々関与していない（`PromptOverrides`ですら変換対象外）。static promptもこの2コマンドの対象外のまま変更不要。

### `internal/gateway`

```go
// StaticPrompt is a prompt mcprt serves directly from config, without
// forwarding prompts/get to any backend. Unlike a backend-sourced prompt, it
// always wins a name collision (see New and updatePromptsLocked).
type StaticPrompt struct {
    Prompt   *mcp.Prompt
    Template *template.Template
}

// NewStaticPrompt parses text as a Go text/template and pairs it with the
// mcp.Prompt it renders for. The caller (internal/cli's buildGateway) does
// this conversion once per (re)build, from config.StaticPromptConfig -- a
// bad template here fails the same startup/SIGHUP-reload path a bad backend
// config would. config.Validate already rejects an unparseable Text before
// buildGateway ever runs, so this Parse should not fail in practice; it is
// not skipped, since NewStaticPrompt has no way to assume Validate ran.
func NewStaticPrompt(name, description string, args []*mcp.PromptArgument, text string) (*StaticPrompt, error) {
    tmpl, err := template.New(name).Parse(text)
    if err != nil {
        return nil, fmt.Errorf("prompt %q: parse text template: %w", name, err)
    }
    return &StaticPrompt{
        Prompt: &mcp.Prompt{Name: name, Description: description, Arguments: args},
        Template: tmpl,
    }, nil
}
```

`NewConfig`に`StaticPrompts []*StaticPrompt`を追加する。`Server`に`staticPromptNames map[string]bool`を追加する（`promptOverrides`と同じ並びに置く）。この場は`New`で一度だけ埋め、以後書き換えない（config自体が生きている間は不変）。他の8つのreconcile用フィールドと違い書き込みが起きないので、`mu`保護は必須ではないが、読み出しは既に`mu`配下にある`updatePromptsLocked`からのみ行うため、同じ場所に置いて構わない。

`New`は、backend由来promptの登録ループの直前でstatic prompt名との衝突を弾き、ループの後でstatic prompt自身を登録する:

```go
staticPromptNames := make(map[string]bool, len(cfg.StaticPrompts))
for _, sp := range cfg.StaticPrompts {
    staticPromptNames[sp.Prompt.Name] = true
}
s := &Server{
    // ...既存フィールド...
    staticPromptNames: staticPromptNames,
}

if cfg.Tables.Prompts != nil {
    for _, resolved := range cfg.Tables.Prompts.Items {
        if staticPromptNames[resolved.Item.Name] {
            logger.Warn("prompt shadowed by static config prompt", "prompt", resolved.Item.Name, "backend", resolved.BackendName)
            continue
        }
        registerPrompt(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys)
    }
}
for _, sp := range cfg.StaticPrompts {
    mcpSrv.AddPrompt(sp.Prompt, staticPromptHandler(cfg.Logger, cfg.MaskKeys, sp))
}
```

`updatePromptsLocked`（`reconcile.go`、backendの`list_changed`で再実行される）の登録ループにも同じ衝突チェックを足す。こちらは再接続のたびに繰り返し走るため、`New`と違い衝突のたびにWARNログは出さない（起動時に一度分かれば十分で、再接続のたびに同じWARNが繰り返し出るのはノイズになる）:

```go
for name, resolved := range newTable.Items {
    if s.staticPromptNames[name] {
        continue // static promptが常に勝つ。backend側はここで登録しない
    }
    old, ok := s.promptTable.Items[name]
    if !touchedBy(resolved, old, ok, backendName) {
        continue
    }
    // ...既存の処理...
}
```

除去ループ（`for name := range s.promptTable.Items { if _, ok := newTable.Items[name]; !ok { s.mcp.RemovePrompts(name) } }`）は変更不要 -- static prompt名は`promptEntries`/`promptTable`（backend由来のみを追跡する）に一度も入らないため、このループの対象になりようがない。つまりstatic promptは`New`で一度`AddPrompt`されたら、以後のreconcileで触られることも消されることもない。

ハンドラ本体は既存の`promptGetHandler`と対称の構造にする（`startCallSpan`/`logCall`など監査・トレーシングの扱いを揃える。`mcp.backend`のような属性値・ログの`backend`欄には実backendがいないので`"(static)"`という固定文字列を使う）:

```go
func staticPromptHandler(logger *slog.Logger, maskKeys []string, sp *StaticPrompt) mcp.PromptHandler {
    return func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
        start := time.Now()
        ctx, span := startCallSpan(ctx, req.Extra, "prompts/get",
            attribute.String("mcp.backend", "(static)"),
            attribute.String("mcp.prompt.name", sp.Prompt.Name))
        defer span.End()

        result, err := renderStaticPrompt(sp, req.Params.Arguments)
        recordOutcome(span, err)
        logCall(ctx, logger, "prompt", "prompt", sp.Prompt.Name, "(static)", req.Session, req.Params.Arguments, maskKeys, start, err, nil)
        return result, err
    }
}

func renderStaticPrompt(sp *StaticPrompt, args map[string]string) (*mcp.GetPromptResult, error) {
    for _, a := range sp.Prompt.Arguments {
        if a.Required {
            if _, ok := args[a.Name]; !ok {
                return nil, fmt.Errorf("prompt %q: missing required argument %q", sp.Prompt.Name, a.Name)
            }
        }
    }
    var buf strings.Builder
    if err := sp.Template.Execute(&buf, args); err != nil {
        return nil, fmt.Errorf("prompt %q: render: %w", sp.Prompt.Name, err)
    }
    return &mcp.GetPromptResult{
        Description: sp.Prompt.Description,
        Messages: []*mcp.PromptMessage{
            {Role: "user", Content: &mcp.TextContent{Text: buf.String()}},
        },
    }, nil
}
```

`text/template`は`.`がmap[string]stringのとき`.field`をmapのキー参照として扱うので、`req.Params.Arguments`をそのまま`Execute`に渡せる。宣言されていない（`arguments:`に無い）キーをテンプレートが参照した場合や、非必須引数が渡されなかった場合は、Goの`text/template`のデフォルト挙動どおり空文字列として展開される（エラーにしない）。

`internal/router`は変更しない。static promptはbackendを持たないため`router.Resolve`／`prompt_overrides`の対象にはならず、衝突解決は上記のstatic-name優先チェックだけで完結する。

### `internal/cli/server.go`

`buildGateway`に、config.Prompts -> []*gateway.StaticPromptへの変換ステップを足す:

```go
func buildStaticPrompts(prompts []config.StaticPromptConfig) ([]*gateway.StaticPrompt, error) {
    out := make([]*gateway.StaticPrompt, 0, len(prompts))
    for _, p := range prompts {
        args := make([]*mcp.PromptArgument, 0, len(p.Arguments))
        for _, a := range p.Arguments {
            args = append(args, &mcp.PromptArgument{Name: a.Name, Description: a.Description, Required: a.Required})
        }
        sp, err := gateway.NewStaticPrompt(p.Name, p.Description, args, p.Text)
        if err != nil {
            return nil, err
        }
        out = append(out, sp)
    }
    return out, nil
}
```

`buildGateway`はこれを呼び、エラーなら（他のbackend接続失敗などと同じく）即座に返す。成功したら`gateway.NewConfig{ ..., StaticPrompts: staticPrompts }`に積む。`buildGateway`は起動時と、SIGHUPリロード時（新しい`cfg`で丸ごと作り直す既存の設計）の両方から同じ経路で呼ばれるため、static promptもtimeoutやbackend設定と同様、設定変更はSIGHUPで反映される。

## データフロー

**起動シーケンス（差分のみ）**
1. `config.Load`が`prompts:`をパースし、`Validate`で名前重複・テンプレート構文エラーを検出（検出したら起動しない）
2. `buildGateway`が`cfg.Prompts`を`buildStaticPrompts`で`[]*gateway.StaticPrompt`に変換（ここでも`text/template.Parse`するが、1で通っていれば失敗しない）
3. `gateway.New`が、backend由来prompt登録ループでstatic prompt名との衝突を弾きつつ、static prompt自身を`AddPrompt`で登録

**リクエスト処理（`prompts/get`、static prompt宛）**
1. clientから`prompts/get`（static prompt名、`Arguments`はオプション）を受信
2. `required: true`の引数が`Arguments`に無ければエラーを返す
3. `Arguments`をコンテキストに`Template.Execute`し、単一の`role: user`テキストメッセージとして返す
4. 監査ログ（`backend="(static)"`）とトレーシングは既存のbackend中継pathと同じ形式で出す

**リクエスト処理（backendのlist_changed後）**
- `updatePromptsLocked`が再解決した`newTable`から、static prompt名と衝突するbackend prompt登録をスキップし続ける。static prompt自体はこの経路で一切触れない（`New`で登録されたまま）。

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| `prompts:`内で名前が空、または重複 | `config.Validate`がエラーを返し起動/リロードを止める |
| `text`が空、またはテンプレート構文エラー | 同上 |
| `arguments:`内で名前が空、または重複 | 同上 |
| config側prompt名がbackend提供のprompt名と衝突 | backend側を登録せずWARNログ（起動時のみ）。config側が常に勝つ |
| 呼び出し時に`required: true`の引数が欠けている | `prompts/get`がエラーを返す。監査ログにも記録される |
| テンプレートに未宣言のキーを参照 / 非必須引数が未指定 | エラーにせず空文字列として展開される（`text/template`のデフォルト挙動） |
| backendが同名promptを新たに`list_changed`で報告 | 引き続きstatic prompt側が勝ち、backend側は登録されない（ログは出さない。起動時と違い再接続のたびに繰り返すとノイズになるため） |

## ロギング

既存方針を継続（`log/slog`、`--log-level`、監査ログは`internal/gateway/audit.go`の`logCall`を再利用）。static prompt呼び出しの監査ログは`backend`欄に`"(static)"`という固定文字列を使う点だけが既存のbackend中継pathとの違い。

## テスト方針

- `internal/config`: `prompts:`のYAMLパース、名前重複エラー、`arguments`重複エラー、テンプレート構文エラーのテーブル駆動テスト
- `internal/gateway`:
  - static promptが`prompts/list`に現れ、`prompts/get`が引数をテンプレートに正しく埋め込むこと
  - `required: true`の引数を渡さないとエラーになること
  - backendが同名promptを持つケースでstatic prompt側が勝つこと（`New`時点、および疑似backendの`list_changed`後の両方）
  - 監査ログ・トレーシングが`backend="(static)"`で記録されること
- `internal/cli`: `buildGateway`が`cfg.Prompts`のテンプレート構文エラーを伝播すること（`mcprt server`起動失敗）、`mcprt validate`が同じエラーを検出すること
- `go test ./...`で完結、外部サービス依存なし

## 運用上の注意（README追記）

Claude Codeは`prompts/list`のpromptを`/mcp__<server>__<prompt>`というスラッシュコマンドとして公開する。サーバー名（`claude mcp add`で付ける名前）は英数字・ハイフン・アンダースコアのみに制限されるが、prompt名側は許可文字以外が`_`に置換される（拒否ではない）。このためstatic promptの`name`に`.`やスペースなど`[A-Za-z0-9_-]`以外の文字を使うと、Claude Code側で他のprompt名と意図せず衝突しうる。README（config例の近く）に、`prompts:`の`name`は`[A-Za-z0-9_-]`に収めることを推奨する注記を足す。これはmcprt固有の制約ではなくClaude Code側の挙動なので、mcprt自体に文字種バリデーションは追加しない。

## 将来拡張（本ドキュメントのスコープ外）

- 複数メッセージ（会話履歴風）のstatic prompt表現
- `prompt_overrides`とstatic promptの優先順位を設定可能にする（現状は常にconfig優先で固定）
- `completion/complete`でのstatic prompt引数のオートコンプリート
