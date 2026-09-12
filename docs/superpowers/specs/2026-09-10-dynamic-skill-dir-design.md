# mcprt: skill_dir によるプロンプトの動的検出・通知 設計

## 背景・目的

`2026-09-08-static-prompts-design.md`で実装した`skill_file`は、config.yamlの`prompts:`配列1エントリにつき1つのSKILL.md風ファイルを紐付け、起動時／SIGHUPリロード時に一度だけ読んで`StaticPromptConfig`のName/Description/Textを補完する機能である。しかし以下の2点で運用上の柔軟性を欠く。

- 1ファイル=1エントリのため、多数のプロンプトを追加するたびにconfig.yamlへエントリを書き足す必要がある
- 起動時／SIGHUPリロード時にしか読まれないため、ファイルを追加・削除・変更してもプロセスを再起動（またはSIGHUP送信）するまで反映されない

本ドキュメントは、`prompts:`エントリに新フィールド`skill_dir`を追加し、指定ディレクトリ直下の`*.md`を自動的に複数プロンプトとして展開すること、およびファイルの追加・削除・変更をfsnotifyで検知し、実行中のgatewayに即座に反映（`notifications/prompts/list_changed`を伴って通知）することを設計する。

## スコープ

含める:
- `StaticPromptConfig`への新フィールド`skill_dir`（既存の`text`/`skill_file`/`name`/`description`/`arguments`とは排他）
- `skill_dir`直下（非再帰）の`*.md`ファイルを、既存の`parseSkillFile`（front matter対応）でそれぞれ1プロンプトとして展開。front matterに`name`が無い場合はファイル名（拡張子抜き）を使う。`arguments`は常に空
- 起動時／SIGHUPリロード時：`skill_dir`を含む全プロンプトソースを一度に展開し、ソース種別を問わず名前重複・テンプレート構文エラーを検出したら起動/リロードを失敗させる（既存の厳格な検証方針を維持）
- ランタイム：`skill_dir`配下のファイル変更をfsnotifyで検知し、デバウンス後にディレクトリを再スキャン、実行中の`*gateway.Server`の静的プロンプト登録をその場で（世代を切り替えずに）追加/更新/削除する
- `prompts:`内の定義順によるプロンプト名衝突の優先度付け（早い者勝ち）。ランタイムの再スキャンで見つかった不正ファイル・名前衝突は、そのファイルだけログ出力してスキップし、他は適用する寛容な扱いとする
- JSON Schema（config.yaml editor補完用）への`skill_dir`追加

含めない（将来拡張、あるいは対象外と判断）:
- サブディレクトリの再帰探索
- SKILL.md標準に無い`arguments`のfront matter拡張
- ディレクトリ間でのプロンプト名の動的な「明け渡し」（あるディレクトリの上位優先ファイルが削除されても、それまでスキップされていた下位優先ディレクトリのファイルを自動的に昇格させることはしない。次にその下位ディレクトリ自身に変更イベントが発生したときに初めて再評価される）
- デバウンス時間の設定可能化（固定300ms）
- `skill_file`の廃止（引き続き単一ファイル運用として提供）

## コンポーネント構成

### `internal/config`

```go
type StaticPromptConfig struct {
    Name        string                 `yaml:"name,omitempty" json:"name,omitempty"`
    Description string                 `yaml:"description,omitempty" json:"description,omitempty"`
    Arguments   []StaticPromptArgument `yaml:"arguments,omitempty" json:"arguments,omitempty"`
    Text        string                 `yaml:"text,omitempty" json:"text,omitempty"`
    SkillFile   string                 `yaml:"skill_file,omitempty" json:"skill_file,omitempty"`
    SkillDir    string                 `yaml:"skill_dir,omitempty" json:"skill_dir,omitempty"`
}
```

`validate`に追加する検証:

```go
func validateStaticPromptSource(p StaticPromptConfig) error {
    if p.SkillDir == "" {
        return nil
    }
    if p.Name != "" || p.Description != "" || p.Text != "" || p.SkillFile != "" || len(p.Arguments) > 0 {
        return fmt.Errorf("prompts: skill_dir %q: name/description/text/skill_file/arguments must be empty when skill_dir is set", p.SkillDir)
    }
    return nil
}
```

`ScanSkillDir`を新設し、起動時の展開とランタイム再スキャンの両方から呼べるようにする。既存の`parseSkillFile`をそのまま再利用する:

```go
// ScanSkillDir lists dir's *.md files (non-recursive, sorted by filename for
// determinism) and parses each via parseSkillFile into a StaticPromptConfig.
// A file whose front matter omits name falls back to the filename minus its
// extension. Arguments is always empty -- SKILL.md's front matter format has
// no field for it. Returns one entry per file in the same order dir.ReadDir
// (sorted) yields, so callers get a stable, reproducible ordering across
// repeated scans of an unchanged directory.
func ScanSkillDir(dir string) ([]StaticPromptConfig, error) {
    entries, err := os.ReadDir(dir)
    if err != nil {
        return nil, fmt.Errorf("skill_dir %q: %w", dir, err)
    }
    var out []StaticPromptConfig
    for _, e := range entries {
        if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
            continue
        }
        path := filepath.Join(dir, e.Name())
        data, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("skill_dir %q: %w", dir, err)
        }
        name, description, body, err := parseSkillFile(data)
        if err != nil {
            return nil, fmt.Errorf("skill_dir %q: file %q: %w", dir, e.Name(), err)
        }
        if name == "" {
            name = strings.TrimSuffix(e.Name(), ".md")
        }
        out = append(out, StaticPromptConfig{Name: name, Description: description, Text: body})
    }
    return out, nil
}
```

`expandSkillFiles`（呼び出し元は`Parse`のまま）を拡張し、`skill_dir`エントリを`ScanSkillDir`の結果で置き換える形で、最終的な`cfg.Prompts`が「展開済みの実体プロンプトのフラットな列」になるようにする。この時点でエラー（ディレクトリ不在、front matter不正）が出れば`Parse`自体が失敗する。

その後の`validateStaticPrompts`（名前重複・テンプレート構文チェック）は、展開後の列全体に対して従来通り一度だけ走る。ソースが`skill_dir`由来かどうかを区別しない。

各展開済みエントリには、後段（gateway層）で優先度判定に使うため、元の`prompts:`配列インデックス（`skill_dir`が複数ファイルに展開された場合は全て同じインデックス）を持たせる。`StaticPromptConfig`自体は公開APIなので汚さず、`expandSkillFiles`が返す並行スライス、または`buildStaticPrompts`に渡す前段の内部型で持ち回る。

`export`/`import`は従来通り対象外（backendの接続情報のみが対象）。

### `internal/gateway`

`StaticPrompt`に、由来を示すフィールドを追加する:

```go
type StaticPrompt struct {
    Prompt   *mcp.Prompt
    Template *template.Template
}
```

`Server`に以下を追加する（`mu`で保護）:

```go
promptOwner map[string]int          // 現在登録されているプロンプト名 -> 所有エントリのインデックス（優先度、小さいほど高い）
dirEntries  map[int][]*StaticPrompt // skill_dirエントリのインデックス -> 現在そのディレクトリから登録中のプロンプト一覧
```

起動時（`New`）は、`prompts:`の定義順インデックスをそのまま優先度として、fixedエントリ・`skill_dir`展開結果の両方を`promptOwner`に登録しつつ`AddPrompt`する。既存の`staticPromptNames map[string]bool`は`promptOwner`に統合し、置き換える（`updatePromptsLocked`の3箇所のガード— 登録スキップ・除去スキップ・衝突チェック — は`s.staticPromptNames[name]`から`_, ok := s.promptOwner[name]; ok`に読み替える）。

`reconcile.go`に`UpdateDirPrompts`を追加する（`UpdatePrompts`と対称の構造）:

```go
// UpdateDirPrompts applies entryIndex's freshly rescanned skill_dir contents
// to the live server: names entryIndex no longer produces are removed (if it
// still owns them), names it newly produces or changed are (re)registered
// unless a strictly higher-priority entry (smaller index) already owns that
// name. AddPrompt/RemovePrompts on s.mcp trigger go-sdk's own
// notifications/prompts/list_changed to every downstream client -- there is
// no separate notification step here.
func (s *Server) UpdateDirPrompts(entryIndex int, fresh []*StaticPrompt) {
    s.mu.Lock()
    defer s.mu.Unlock()

    old := s.dirEntries[entryIndex]
    freshByName := make(map[string]*StaticPrompt, len(fresh))
    for _, sp := range fresh {
        freshByName[sp.Prompt.Name] = sp
    }

    // Names this entry no longer produces: release ownership and remove,
    // but only if this entry still owns them (a higher-priority entry may
    // have taken over -- Add below never re-assigns ownership away from a
    // higher-priority owner, so this entry can only ever own names nothing
    // else claimed).
    for _, sp := range old {
        name := sp.Prompt.Name
        if _, ok := freshByName[name]; ok {
            continue
        }
        if s.promptOwner[name] == entryIndex {
            delete(s.promptOwner, name)
            s.mcp.RemovePrompts(name)
        }
    }

    oldByName := make(map[string]*StaticPrompt, len(old))
    for _, sp := range old {
        oldByName[sp.Prompt.Name] = sp
    }
    for name, sp := range freshByName {
        if owner, ok := s.promptOwner[name]; ok && owner != entryIndex {
            s.logger.Warn("skill_dir: prompt name claimed by a higher-priority source, skipping",
                "name", name, "entry", entryIndex, "owner_entry", owner)
            continue
        }
        if prev, ok := oldByName[name]; ok && reflect.DeepEqual(prev, sp) {
            continue // unchanged -- skip the redundant AddPrompt/notification
        }
        s.mcp.AddPrompt(sp.Prompt, staticPromptHandler(s.logger, s.maskKeys, sp))
        s.promptOwner[name] = entryIndex
    }
    s.dirEntries[entryIndex] = fresh
}
```

`promptOwner[name] == entryIndex`かつ`owner != entryIndex`の判定だけでは「優先度が高い方が勝つ」という定義順ルールを起動時と完全に一致させられない（ランタイムでは早い者勝ち＝先に`AddPrompt`を呼んだ方が勝つ、インデックスの大小は「初回衝突ログの分かりやすさ」のためだけに使う）。この非対称性はスコープの「ディレクトリ間でのプロンプト名の動的な明け渡しをしない」という制限と表裏一体であり、起動時の厳格な全体検証（重複があれば起動失敗）で通常は起きない状況にのみ関係する。

### fsnotifyウォッチャー（`internal/cli/server.go`）

```go
// watchSkillDir runs until ctx is cancelled, debouncing fsnotify events on
// dir by skillDirDebounce before rescanning and applying the result to gw via
// UpdateDirPrompts. ctx is the same per-generation context backend
// supervisors use, so a SIGHUP-triggered generation swap stops this goroutine
// (via genCancel) the same way it stops backend reconnect loops -- the new
// generation's buildGateway starts a fresh watcher after its own initial
// synchronous scan.
func watchSkillDir(ctx context.Context, logger *slog.Logger, dir string, entryIndex int, gw *gateway.Server) {
    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        logger.Error("skill_dir: watcher setup failed, changes to this directory won't be picked up until next reload", "dir", dir, "error", err)
        return
    }
    defer watcher.Close()
    if err := watcher.Add(dir); err != nil {
        logger.Error("skill_dir: watch failed, changes to this directory won't be picked up until next reload", "dir", dir, "error", err)
        return
    }

    var debounce *time.Timer
    rescan := func() {
        prompts, err := config.ScanSkillDir(dir)
        if err != nil {
            logger.Warn("skill_dir: rescan failed, keeping previous state", "dir", dir, "error", err)
            return
        }
        sps, err := buildStaticPrompts(prompts) // skips/logs per-file errors internally, see below
        gw.UpdateDirPrompts(entryIndex, sps)
    }
    for {
        select {
        case <-ctx.Done():
            return
        case _, ok := <-watcher.Events:
            if !ok {
                return
            }
            if debounce == nil {
                debounce = time.AfterFunc(skillDirDebounce, rescan)
            } else {
                debounce.Reset(skillDirDebounce)
            }
        case err, ok := <-watcher.Errors:
            if !ok {
                return
            }
            logger.Warn("skill_dir: watcher error", "dir", dir, "error", err)
        }
    }
}
```

`buildGateway`は、`cfg.Prompts`のうち`skill_dir`由来のエントリごとに`go watchSkillDir(genCtx, logger, dir, entryIndex, srv)`を起動する（`genCtx`は`watchSIGHUP`が世代ごとに払い出す既存のコンテキスト）。

個別ファイルのパースエラーをそのファイルだけスキップする責務は、`config.ScanSkillDir`ではなく（起動時の厳格な全体検証と共有するため一箇所は失敗させる必要がある）、ランタイム経路専用の薄いラッパーが担う。`internal/cli`に、`ScanSkillDir`相当をもう一段緩く呼ぶ関数を置く:

```go
// scanSkillDirLenient is ScanSkillDir's runtime counterpart: a single bad
// file logs a warning and is excluded, instead of failing the whole scan --
// appropriate for a live rescan (see design doc's error-handling table),
// unlike the strict one-bad-file-fails-everything behavior config.Parse
// needs at startup.
func scanSkillDirLenient(logger *slog.Logger, dir string) ([]config.StaticPromptConfig, error)
```

これは`os.ReadDir`＋ファイルごとに`config`パッケージの（内部的な）1ファイルパース処理を呼ぶ形になるため、`config`パッケージ側に1ファイル単位の関数（`ParseSkillFile`のエクスポート版、または`ScanSkillDirFunc`にファイル単位のエラーコールバックを渡せる形）を用意し、`ScanSkillDir`（厳格版）と`scanSkillDirLenient`（寛容版）の両方がそれを共有する。

## データフロー

**起動シーケンス（差分のみ）**
1. `config.Load`が`prompts:`をパース。`expandSkillFiles`が`skill_file`を展開しつつ、`skill_dir`エントリを`ScanSkillDir`（厳格版）でその場に展開する
2. 展開後の全プロンプト（ソース問わず）に対し`validateStaticPrompts`が名前重複・テンプレート構文を検査。エラーなら起動/リロード失敗
3. `buildGateway`が`buildStaticPrompts`で`[]*gateway.StaticPrompt`に変換し、各エントリの元インデックスを保持したまま`gateway.New`に渡す
4. `gateway.New`が`promptOwner`/`dirEntries`を初期化し全件`AddPrompt`
5. `buildGateway`が`skill_dir`エントリごとに`watchSkillDir`をgenCtx付きで起動

**ランタイム（ファイル変更検知）**
1. fsnotifyイベント受信 → 300msデバウンス
2. `scanSkillDirLenient`で再スキャン。個別ファイルの不正はログ＋スキップ、他は適用
3. `gw.UpdateDirPrompts(entryIndex, ...)`が差分を計算し`AddPrompt`/`RemovePrompts`を呼ぶ
4. go-sdkが自動的に`notifications/prompts/list_changed`を送信。世代切り替えを伴わないため、stdio/HTTPどちらの既存クライアントセッションにも即時反映される

**SIGHUPリロード**
- 従来通り新しい世代（新しい`*gateway.Server`・全backend再接続）を作る。`skill_dir`もこの中で初回スキャンからやり直す。旧世代の`watchSkillDir`は`genCancel`で停止する

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| `skill_dir`と`text`/`skill_file`/`name`/`description`/`arguments`の同時指定 | `validate`がエラー、起動/リロード失敗 |
| 起動/SIGHUP時、`skill_dir`が存在しない・読めない | `ScanSkillDir`がエラーを返し、起動/リロード失敗 |
| 起動/SIGHUP時、展開後の全プロンプト間で名前重複（ソース問わず） | `validateStaticPrompts`がエラー、起動/リロード失敗 |
| ランタイム再スキャンで個別ファイルのfront matterパースエラー | `scanSkillDirLenient`がそのファイルだけWARNログでスキップ、ディレクトリの他ファイルは適用 |
| ランタイム再スキャンで同一ディレクトリ内の名前重複 | ファイル名の昇順で早い方を採用、後者はWARNログでスキップ（`scanSkillDirLenient`内で処理） |
| ランタイムで他エントリと名前衝突（`promptOwner`に既存） | `UpdateDirPrompts`がWARNログでスキップし続ける（相手の名前が空くまで） |
| ランタイムでウォッチャー自体が失敗（`fsnotify.NewWatcher`/`Add`/`watcher.Errors`） | ERRORログを出しそのエントリの監視を停止。既存登録済みプロンプトはそのまま維持（`UpdateDirPrompts`は呼ばれず何も変わらない） |
| 内容に変化がない再スキャン結果（chmodのみ等） | `UpdateDirPrompts`内の`reflect.DeepEqual`チェックで何もしない |

## ロギング

既存方針を継続（`log/slog`）。新規イベントは`skill_dir`をキーにdir/entry/nameを添えてWARN/ERRORで出す。監査ログ（`logCall`）への変更はない（`staticPromptHandler`自体は変わらない）。

## テスト方針

- `internal/config`:
  - `ScanSkillDir`のテーブル駆動テスト（front matter有無、name省略時のファイル名フォールバック、`.md`以外の除外、サブディレクトリ非対象、ファイル名昇順での安定した並び）
  - `skill_dir`と他フィールド併用時の`validate`エラー
  - `skill_dir`のディレクトリ不在エラーが`Parse`を失敗させること
  - 展開後の全プロンプト間の名前重複（`skill_dir`同士、`skill_dir`とfixedエントリ）が`validateStaticPrompts`で検出されること
- `internal/gateway`:
  - `UpdateDirPrompts`の単体テスト — 追加/削除/変更の差分適用、優先度に基づく衝突スキップ、内容不変時に`AddPrompt`が呼ばれないこと
- `internal/cli`:
  - `scanSkillDirLenient`が個別ファイルの不正をスキップし他を返すこと
  - `watchSkillDir`の統合テスト — `t.TempDir()`にファイルを作成/変更/削除し、短いポーリングで`gw`側の状態（`prompts/list`相当）が反映されることを確認
  - `ctx`キャンセルで`watchSkillDir`のgoroutineが終了すること（リークしないこと）
- `go test ./...`で完結。新規依存`github.com/fsnotify/fsnotify`を`go.mod`に追加

## 運用上の注意（README追記予定）

- `skill_dir`はディレクトリ直下の`*.md`のみを対象とし、サブディレクトリは無視される
- ファイル変更の反映は概ね300ms程度の遅延を伴う（デバウンス）
- 複数の`skill_dir`（または`skill_dir`とfixedエントリ）が同じプロンプト名を生成する場合、`prompts:`内での定義順が早い方が勝つ。ただし一度ランタイムで両者が動き出した後にどちらのファイルが「先に」変更されたかによって、この優先度が厳密に守られない稀なケースがある（スコープ節参照）

## 将来拡張（本ドキュメントのスコープ外）

- サブディレクトリの再帰探索
- ディレクトリ間でのプロンプト名の動的な明け渡し（完全な優先度整合性）
- デバウンス時間の設定可能化
- `completion/complete`でのskill_dir由来プロンプト引数のオートコンプリート（現状`arguments`が常に空のため対象外）
