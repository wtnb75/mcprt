# mcprt: config ホットリロード（graceful restart）設計

## 背景・目的

list_changed機能・backend再接続設計は、backendの一覧変更や接続の生死には対応するが、`mcprt`自身の設定ファイル（`backends[]`の追加/削除/内容変更、`overrides`系、`logging.mask_keys`）を変更するには、プロセスを再起動する必要がある。本ドキュメントでは、`SIGHUP`シグナルを受けてconfigを再読み込みし、無停止でその内容を反映する仕組みを設計する。

## スコープ

含める:
- `SIGHUP`シグナルをトリガーとした、configファイルの再読み込み
- `backends[]`の追加・削除・内容変更（URL/command/prefix等、何であれ）を含む、config全体の反映
- `overrides`/`resource_overrides`/`resource_template_overrides`/`prompt_overrides`/`logging.mask_keys`の反映
- HTTPリスナーへの新規接続に対する反映（graceful restart方式）
- 旧世代のbackend接続の、一定時間後の強制クローズ（drain）

含めない（今回のスコープ外）:
- stdioリスナーへの反映。stdioは1プロセスにつき1本のパイプ・1セッションしかなく、「新規接続」という概念自体が存在しないため、稼働中のstdioセッションを裏で差し替えることは原理的にできない。`SIGHUP`受信時は「stdioはhot-reload非対応」とログを出すのみとする（stdio版mcprtは通常1クライアントに1プロセスの使い方であり、実運用上の制約は小さいと判断）
- `listen.stdio`/`listen.http`自体の変更（リスナーの張り替えはプロセス再起動が必要）
- drainタイムアウト値のYAML設定への露出（既存の同種タイムアウトと同じくハードコードされたpackage変数とする）
- 個々の設定項目（`overrides`のみ、`mask_keys`のみ等）を差分反映する仕組み。configの内容が1バイトでも変われば、新世代を丸ごと作り直して差し替える（後述の理由）

## 設計方針: なぜ「差分反映」ではなく「丸ごと作り直して差し替え」か

`backends[]`の内容変更（特に`prefix`）や`logging.mask_keys`を、既存のbackend接続・登録済みitemに対して部分的に反映しようとすると、2つの構造的な難点にぶつかる:

- `internal/gateway/reconcile.go`の`upsertEntry`/`replaceEntry`は、既存entryの`Items`だけを差し替え、`Prefix`は一度設定されたら更新しない。`prefix`の変更を正しく反映するには、結局そのbackendのentryを丸ごと作り直す必要がある。
- `logging.mask_keys`は、tool登録時（`registerTool`呼び出し時）に`callHandler`等のクロージャへ**値として焼き込まれる**。変更を反映するには、既存backendの全itemを再登録し直す必要がある。

一方、`go-sdk`の`mcp.NewStreamableHTTPHandler`は、**HTTPリクエストごとに`*mcp.Server`を取得するコールバック関数**を受け取る設計になっている。これを利用し、「configが変わったら、起動時と全く同じ手順で新しい`*gateway.Server`一式（backend接続・entries・table・overrides・maskKeys）をまるごと構築し、新規HTTP接続だけをそちらへ向ける」という方式（graceful restart）を採ることで、上記2つの難点を個別に解決する必要がなくなる。既にバインド済みの旧セッションは、そのセッションが終わるまで旧backend接続・旧`*gateway.Server`のまま動き続ける。

## 全体アーキテクチャ

```
   internal/cli/server.go
   ┌───────────────────────────────────────────────┐
   │ runServer                                       │
   │  1. current := new(atomic.Pointer[gateway.Server])│
   │  2. srv, err := buildGateway(ctx, logger, cfg)    │  ← 起動時
   │  3. current.Store(srv)                            │
   │  4. gateway.ServeHTTP(ctx, current.Load, addr)     │  ← func() *mcp.Server
   │  5. gateway.ServeStdio(ctx, srv.MCP())             │  ← 起動時のsrvに固定のまま
   │  6. SIGHUPループをspawn                             │
   └───────────────────────────────────────────────┘
                       │ SIGHUP受信ごと
                       ▼
   ┌───────────────────────────────────────────────┐
   │ 1. cfg, err := config.Load(configPath)             │
   │    失敗 → ログのみ、reload中止                       │
   │ 2. newSrv, err := buildGateway(genCtx, logger, cfg) │  ← 起動時と同じ関数
   │ 3. current.Store(newSrv)  ← 新規HTTP接続はここから    │
   │ 4. oldGenCancel()  ← 旧世代supervisorのリトライを止める│
   │ 5. reloadDrainTimeout後、旧世代の全backend.Close()    │
   └───────────────────────────────────────────────┘
```

## コンポーネント構成

### `internal/gateway/gateway.go`: `ServeHTTP`のシグネチャ変更

```go
// ServeHTTP runs a Streamable HTTP server listening on addr, until ctx is
// cancelled. getServer is called once per incoming request to obtain the
// *mcp.Server to route it to -- not a fixed value -- so that a config
// hot-reload (see internal/cli/server.go) can swap in a freshly-built
// *gateway.Server for new connections without disturbing sessions already
// bound to the previous one.
func ServeHTTP(ctx context.Context, getServer func() *mcp.Server, addr string) error {
	handler := remoteAddrMiddleware(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return getServer() }, nil))
	httpServer := &http.Server{Addr: addr, Handler: handler}
	// ...以下、httpServer.ListenAndServe()/Shutdown周りは変更なし
}
```

（`ServeStdio`は変更しない — スコープ外。呼び出し元は`gateway.ServeStdio(ctx, srv.MCP())`のまま、起動時に一度構築した`srv`に固定される。）

### `internal/cli/server.go`: `buildGateway`（起動時ロジックの切り出し）

現行`runServer`の「`connectBackends`→`router.Resolve`×4→`gateway.New`」の一連を、再利用可能な関数として切り出す:

```go
// buildGateway connects to every configured backend (see connectBackends;
// this reuses the backend-reconnect design's superviseBackend, so a
// backend that fails here keeps retrying in the background exactly as it
// would after a later disconnect) and builds a fresh *gateway.Server from
// scratch. Called once at startup, and once per SIGHUP-triggered reload
// with a freshly-loaded cfg -- the two call sites are otherwise identical,
// which is the whole point of the graceful-restart design (see this
// plan's header): nothing about config-derived state is patched
// piecemeal, it's all rebuilt the same way every time.
func buildGateway(ctx context.Context, logger *slog.Logger, cfg *config.Config) (*gateway.Server, error) {
	if !cfg.Listen.Stdio && cfg.Listen.HTTP == "" {
		return nil, errors.New("no listener configured: enable listen.stdio or set listen.http")
	}

	var gwH gwHolder
	conn := connectBackends(ctx, logger, cfg.Backends, &gwH)

	toolTable := router.Resolve(conn.toolEntries, gateway.ToolNameOf, gateway.ToolRename, cfg.Overrides)
	for _, c := range toolTable.Conflicts {
		logger.Warn("tool name conflict", "tool", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}
	// ...resourceTable/resourceTemplateTable/promptTableも同様（現行runServerと同じ）

	srv := gateway.New(logger, conn.backends, gateway.Tables{
		Tools: toolTable, Resources: resourceTable, ResourceTemplates: resourceTemplateTable, Prompts: promptTable,
	}, gateway.Entries{
		Tools: conn.toolEntries, Resources: conn.resourceEntries, ResourceTemplates: conn.resourceTemplateEntries, Prompts: conn.promptEntries,
	}, gateway.Overrides{
		Tools: cfg.Overrides, Resources: cfg.ResourceOverrides, ResourceTemplates: cfg.ResourceTemplateOverrides, Prompts: cfg.PromptOverrides,
	}, cfg.Logging.MaskKeys)
	gwH.ptr.Store(srv)

	return srv, nil
}
```

### `internal/cli/server.go`: `runServer`と`watchSIGHUP`

```go
// reloadDrainTimeout bounds how long a superseded generation's backend
// connections are kept alive after a hot-reload swap, so sessions still
// bound to it can finish naturally. A var so tests can shrink it.
var reloadDrainTimeout = 5 * time.Minute

// watchSIGHUP blocks until ctx is cancelled, rebuilding the gateway (via
// buildGateway) and swapping current on every SIGHUP. initialGenCancel is
// the cancel func for the generation runServer already built before
// spawning this loop (see below) -- watchSIGHUP takes ownership of it so
// that generation 0 is cancelled on its first supersession exactly like
// every later one; without this, the very first reload would leak
// generation 0's superviseBackend goroutines forever (they'd keep retrying
// under a ctx nothing ever cancels). It is a no-op loop (does nothing but
// log) when cfg.Listen.HTTP == "" -- hot-reload only makes sense for HTTP,
// see this design's scope.
func watchSIGHUP(ctx context.Context, logger *slog.Logger, configPath string, current *atomic.Pointer[gateway.Server], initialGenCancel context.CancelFunc) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)

	genCancel := initialGenCancel // the currently-live generation's cancel func

	for {
		select {
		case <-ctx.Done():
			return
		case <-sighup:
			cfg, err := config.Load(configPath)
			if err != nil {
				logger.Error("config reload failed, keeping current config", "error", err)
				continue
			}
			// buildGateway itself validates that SOME listener is
			// configured; this check is specifically about whether
			// hot-reload can do anything USEFUL with the new config, which
			// is a stricter, hot-reload-specific condition on top of that.
			if cfg.Listen.HTTP == "" {
				logger.Warn("SIGHUP received, but hot-reload is only supported for HTTP listeners; ignoring")
				continue
			}
			if cfg.Listen.Stdio {
				logger.Warn("SIGHUP received: only the HTTP listener will see the new config; the existing stdio session (if any) keeps running under the old one")
			}

			genCtx, newGenCancel := context.WithCancel(ctx)
			newSrv, err := buildGateway(genCtx, logger, cfg)
			if err != nil {
				logger.Error("config reload failed, keeping current config", "error", err)
				newGenCancel()
				continue
			}

			oldSrv := current.Swap(newSrv)
			logger.Info("config reloaded")

			scheduleDrain(logger, oldSrv, genCancel) // supersede and drain the generation this reload replaced
			genCancel = newGenCancel                 // track the new generation for the NEXT reload (or process shutdown, which cancels ctx and makes newGenCancel a no-op)
		}
	}
}

// scheduleDrain cancels the just-superseded generation's long-lived ctx
// (stopping its backends' superviseBackend retry loops) and, after
// reloadDrainTimeout, force-closes every one of its backend connections --
// which also unblocks any surviving superviseBackend's session.Wait(),
// letting those goroutines exit cleanly instead of leaking.
func scheduleDrain(logger *slog.Logger, oldSrv *gateway.Server, oldGenCancel context.CancelFunc) {
	oldGenCancel()
	time.AfterFunc(reloadDrainTimeout, func() {
		for name, b := range oldSrv.Backends() { // Backends() は既存のBackend(name)に加える新しいエクスポートメソッド、または内部専用アクセサ
			if err := b.Close(); err != nil {
				logger.Warn("closing superseded backend connection", "backend", name, "error", err)
			}
		}
	})
}
```

（`scheduleDrain`が全backend一覧を列挙する必要があるため、`gateway.Server`に既存の`Backend(name string) *backend.Backend`に加えて、全件を返すアクセサが必要になる。実装プラン側で具体的な名前・シグネチャを詰める。）

`runServer`は、自身の初回構築でも`genCtx, genCancel := context.WithCancel(ctx)`を作り、`buildGateway(genCtx, ...)`（`ctx`を直接ではなく`genCtx`を渡す）で世代0を構築する。`current := new(atomic.Pointer[gateway.Server])`を用意して`current.Store(...)`し、`gateway.ServeHTTP(ctx, func() *mcp.Server { return current.Load().MCP() }, cfg.Listen.HTTP)`という形でServeHTTPへ渡す。`cfg.Listen.HTTP != ""`のときのみ、`watchSIGHUP(ctx, logger, configPath, current, genCancel)`をgoroutineとしてspawnする（世代0の`genCancel`をそのまま引き渡すことで、`watchSIGHUP`が最初のreload時にそれをキャンセルできるようにする）。

## データフロー

1. `mcprt`が`SIGHUP`を受信する。
2. `config.Load(configPath)`でconfigを再読み込みする。失敗したらログを出しreloadを中止（旧世代のまま継続）。
3. `buildGateway(genCtx, logger, cfg)`で新世代を構築する。これは起動時と全く同じ関数で、新backend群への接続・List・`gateway.New`を行う（一部backendが繋がらなくても、それらは新世代自身の`superviseBackend`がバックグラウンドで再試行し続ける — 既存の再接続設計をそのまま利用）。
4. `current.Swap(newSrv)`で、新規HTTP接続の向き先を新世代に切り替える。
5. 旧世代の`genCancel()`を呼び、旧世代の各backendの`superviseBackend`リトライループを止める（すでに繋がっている接続自体はまだ生きている）。
6. `reloadDrainTimeout`（5分）経過後、旧世代の全backend接続を強制`Close()`する。これにより、まだ旧`*gateway.Server`にバインドされたまま生きているセッションからの呼び出しは、以降「切断済みbackend」と同じエラーになる。

## エラーハンドリング

| ケース | 挙動 |
|---|---|
| `SIGHUP`時、configのパースに失敗 | ログを出し、reload中止。旧世代のままサービス継続 |
| `SIGHUP`時、新configに有効なリスナー設定がない | ログを出し、reload中止 |
| `SIGHUP`時、新世代のbackend接続が一部/全部失敗 | 起動時と同じ扱い。繋がった分だけで新世代として差し替える（全滅でも差し替えは成立する）。繋がらなかった分は新世代自身がバックグラウンドで再試行を続ける |
| stdioのみ構成で`SIGHUP`受信 | 「hot-reloadはHTTPのみ対応」とログを出すのみ、何もしない |
| stdio+http混在構成で`SIGHUP`受信 | HTTPの新規接続だけ新世代に切り替わる。既存のstdio 1セッションは旧世代のまま動き続ける旨をログに明記 |
| drainタイムアウト経過時、旧backend接続がまだ使用中のセッションがある | 強制クローズ。以降その呼び出しは既存の「切断済みbackend」エラーと同じ扱いになる |
| 短時間に`SIGHUP`が連続して届く | 各回が独立して新世代を構築・差し替える。前の`SIGHUP`によるreloadがまだbackend接続中でも、次の`SIGHUP`は独立した新世代構築を並行して開始してよい（`buildGateway`は副作用を`current`への`Swap`まで持たないため、途中で追い越されても問題ない） |

## ロギング

- reload成功: `logger.Info("config reloaded")`
- reload失敗（パースエラー・リスナー未設定）: `logger.Error("config reload failed, keeping current config", "error", ...)`
- stdio非対応: `logger.Warn("SIGHUP received, but hot-reload is only supported for HTTP listeners; ignoring")`
- 旧backend接続の強制クローズ: `logger.Warn("closing superseded backend connection", "backend", ..., "error", ...)`（エラーがあれば）

## テスト方針

- **`internal/gateway`**: `ServeHTTP`のシグネチャ変更に伴う既存テストの更新（`func() *mcp.Server`を渡す形へ）。加えて、`getServer`が呼ばれるたびに異なる`*mcp.Server`を返すケース（新規接続ごとに違うサーバーへ向くこと）を確認する新規テスト。
- **`internal/cli`**: `buildGateway`単体テスト（config読み込み・backend接続失敗時の挙動）。`watchSIGHUP`の統合テスト — fakeプロセスへ`SIGHUP`相当（テスト内では直接関数を叩く、またはプロセス自身に実際にシグナルを送る）を発生させ、(a) 新規接続が新config内容を反映すること、(b) reload前から張られていた既存セッションが、reload後も（drainタイムアウトを迎えるまでは）旧backendのまま呼び出しに応答し続けること、(c) `reloadDrainTimeout`を短くしたテスト用の値で、時間経過後に旧backendへの呼び出しがエラーになることを確認する。
- **`internal/cli`（e2e）**: `mcprt server`をHTTPモードで起動→設定ファイルを書き換え→自プロセスへ`SIGHUP`を送る→新規クライアント接続でtool一覧が新config通りになることを確認する。
- `go test -race ./...`で完結、外部サービス依存なし（シグナル送信はGoの`syscall.Kill(os.Getpid(), syscall.SIGHUP)`等でプロセス内から完結させる）。

## 将来拡張（本ドキュメントのスコープ外）

- stdioリスナーへの反映（現状、原理的に困難）
- drainタイムアウト値のYAML設定への露出
- 個々の設定項目単位での差分反映（現状は「configが1バイトでも変わったら丸ごと作り直す」方式）
