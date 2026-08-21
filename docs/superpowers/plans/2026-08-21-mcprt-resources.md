# mcprt: resources/* 中継 実装プラン

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** gatewayが`resources/list` / `resources/templates/list` / `resources/read`をbackendへ中継できるようにする（tool中継と対称な機能）。

**Architecture:** 起動時に全backendへ`ListTools`に加え`ListResources`・`ListResourceTemplates`を実行し、URIをキーとした独立ルーティングテーブルを構築する。tool用の衝突解決ロジック（`internal/router`）を`Resolve[T any]`へ汎用化し、resource/resource templateもそのまま流用する。resource系のURIにはbackend単位の`prefix`を適用しない。

**Tech Stack:** Go 1.25、`github.com/modelcontextprotocol/go-sdk` v1.7.0、標準`net/http`・`log/slog`。

**Spec:** `docs/superpowers/specs/2026-08-20-mcprt-resources-design.md`

## Global Constraints

- resource/resource templateのURIには`prefix`を適用しない（`router.Entry`構築時に`Prefix: ""`固定。tool側は引き続き`cfg.Prefix`を使う）
- `resources/subscribe` / `unsubscribe` / `notifications/resources/updated` / `notifications/*/list_changed`購読は本プランの範囲外（実装しない）
- prompt中継（`2026-08-20-mcprt-prompts-design.md`）は未確定・保留中であり、本プランのスコープ外。`internal/router`の汎用化はresourceのために行うが、prompt固有のコードは一切追加しない
- 衝突解決は「記載順＋overrides」方式を維持し、tool用と同一アルゴリズムをresource/resource templateにも適用する
- `go test ./...`が最終的に外部サービス依存なしで完結すること
- 事前調査で判明した重要な前提差分: 仕様書は「`router.Resolve[T any]`はprompts設計で汎用化済みなので変更不要」としているが、実際のリポジトリ（`internal/router/router.go`）はまだtool専用の非ジェネリック実装であり、prompts設計自体も未確定・保留中で未着手。そのため本プランのTask 1で`Resolve[T any]`への汎用化を行う（prompts設計のドラフトに記載された型シグネチャに合わせるが、prompt固有コードは追加しない）

---

## 事前調査で確定した設計詳細

- `internal/router/router.go`は現在tool専用（`Entry`/`Resolved`/`Candidate`/`Table`が非ジェネリック、`Resolve(entries []Entry, overrides map[string]string) *Table`）。generics化がTask 1の前提作業になる。
- generics化後のシグネチャは、未確定の`2026-08-20-mcprt-prompts-design.md`ドラフトに記載された型と完全に一致させる（将来prompt対応が入っても再設計不要にするため）:
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
      Fallbacks    []Candidate[T]
  }
  type Conflict struct {
      ExposedName string
      Winner      string
      Losers      []string
  }
  type Table[T any] struct {
      Items     map[string]*Resolved[T]
      Conflicts []Conflict
  }
  func Resolve[T any](entries []Entry[T], nameOf func(T) string, rename func(T, string) T, overrides map[string]string) *Table[T]
  ```
- SDK確認済み: `(*mcp.ClientSession).Resources(ctx, params) iter.Seq2[*mcp.Resource, error]`、`.ResourceTemplates(ctx, params) iter.Seq2[*mcp.ResourceTemplate, error]`、`(*mcp.Server).AddResource(r *mcp.Resource, h mcp.ResourceHandler)`（`url.Parse(r.URI)`失敗でpanic）、`AddResourceTemplate(t *mcp.ResourceTemplate, h mcp.ResourceHandler)`（`uritemplate.New(t.URITemplate)`失敗でpanic）。`mcp.Resource{URI, Name, ...}`、`mcp.ResourceTemplate{URITemplate, Name, ...}`。`mcp.ReadResourceResult{Contents []*mcp.ResourceContents}`、`mcp.ResourceContents{URI, Text, MIMEType, ...}`。
- `url.Parse("http://%gg")`は不正URLエラーを返す（`invalid URL escape "%gg"`）。panicテストのfixtureとして使う。

---

### Task 1: `internal/router`をResolve[T any]へ汎用化し、tool経路を移行する

**Files:**
- Modify: `internal/router/router.go`（全面書き換え）
- Modify: `internal/router/router_test.go`（generics対応、resource/resource template向けケース追加）
- Modify: `internal/gateway/gateway.go`（`router.Table` → `router.Table[*mcp.Tool]`等への追随。動作変更なし）
- Modify: `internal/gateway/gateway_test.go`（generics呼び出しへ追随）
- Modify: `internal/cli/server.go`（`router.Resolve`呼び出しをgenerics対応シグネチャへ）

**Interfaces:**
- Produces: `router.Entry[T]` / `router.Candidate[T]` / `router.Resolved[T]` / `router.Conflict` / `router.Table[T]` / `router.Resolve[T any](entries []Entry[T], nameOf func(T) string, rename func(T, string) T, overrides map[string]string) *Table[T]`（上記「事前調査で確定した設計詳細」のシグネチャそのまま）
- Consumes: なし（このタスクが基盤）

- [ ] **Step 1: router_test.goをgenerics対応で書き換える（先にテストを書く）**

`internal/router/router_test.go`を以下の内容で置き換える:

```go
package router_test

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/router"
)

func tool(name string) *mcp.Tool {
	return &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}
}

func toolNameOf(t *mcp.Tool) string { return t.Name }

func toolRename(t *mcp.Tool, name string) *mcp.Tool {
	c := *t
	c.Name = name
	return &c
}

func TestResolve_NoConflicts(t *testing.T) {
	table := router.Resolve([]router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{tool("alpha")}},
		{BackendName: "b", Items: []*mcp.Tool{tool("beta")}},
	}, toolNameOf, toolRename, nil)

	if len(table.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none", table.Conflicts)
	}
	if len(table.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(table.Items))
	}
	if r := table.Items["alpha"]; r == nil || r.BackendName != "a" || r.OriginalName != "alpha" {
		t.Fatalf("Items[alpha] = %+v, unexpected", r)
	}
}

func TestResolve_CollisionFirstListedWins(t *testing.T) {
	table := router.Resolve([]router.Entry[*mcp.Tool]{
		{BackendName: "first", Items: []*mcp.Tool{tool("search")}},
		{BackendName: "second", Items: []*mcp.Tool{tool("search")}},
	}, toolNameOf, toolRename, nil)

	got := table.Items["search"]
	if got == nil || got.BackendName != "first" {
		t.Fatalf("Items[search] = %+v, want backend \"first\"", got)
	}
	if len(table.Conflicts) != 1 || table.Conflicts[0].Winner != "first" || len(table.Conflicts[0].Losers) != 1 || table.Conflicts[0].Losers[0] != "second" {
		t.Fatalf("Conflicts = %+v, want one conflict won by \"first\", hiding \"second\"", table.Conflicts)
	}
	if len(got.Fallbacks) != 1 || got.Fallbacks[0].BackendName != "second" || got.Fallbacks[0].Item.Name != "search" {
		t.Fatalf("Items[search].Fallbacks = %+v, want one fallback candidate from \"second\"", got.Fallbacks)
	}
}

func TestResolve_OverrideWinsOverListOrder(t *testing.T) {
	table := router.Resolve([]router.Entry[*mcp.Tool]{
		{BackendName: "first", Items: []*mcp.Tool{tool("search")}},
		{BackendName: "second", Items: []*mcp.Tool{tool("search")}},
	}, toolNameOf, toolRename, map[string]string{"search": "second"})

	got := table.Items["search"]
	if got == nil || got.BackendName != "second" {
		t.Fatalf("Items[search] = %+v, want backend \"second\" (via override)", got)
	}
	if len(table.Conflicts) != 1 || table.Conflicts[0].Winner != "second" {
		t.Fatalf("Conflicts = %+v, want winner \"second\"", table.Conflicts)
	}
}

func TestResolve_PrefixAppliedBeforeCollisionCheck(t *testing.T) {
	table := router.Resolve([]router.Entry[*mcp.Tool]{
		{BackendName: "a", Prefix: "a__", Items: []*mcp.Tool{tool("search")}},
		{BackendName: "b", Prefix: "b__", Items: []*mcp.Tool{tool("search")}},
	}, toolNameOf, toolRename, nil)

	if len(table.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none (prefixes make names distinct)", table.Conflicts)
	}
	if _, ok := table.Items["a__search"]; !ok {
		t.Fatal("Items[a__search] missing")
	}
	if _, ok := table.Items["b__search"]; !ok {
		t.Fatal("Items[b__search] missing")
	}
}

func TestResolve_IneffectiveOverrideFallsBackToListOrder(t *testing.T) {
	// "third" is a real backend but doesn't produce a "search" tool, so the
	// override can't apply to it; resolution falls back to list order.
	table := router.Resolve([]router.Entry[*mcp.Tool]{
		{BackendName: "first", Items: []*mcp.Tool{tool("search")}},
		{BackendName: "second", Items: []*mcp.Tool{tool("search")}},
	}, toolNameOf, toolRename, map[string]string{"search": "third"})

	got := table.Items["search"]
	if got == nil || got.BackendName != "first" {
		t.Fatalf("Items[search] = %+v, want backend \"first\" (override target has no such tool)", got)
	}
}

func TestResolve_URIKeyedItems(t *testing.T) {
	resource := func(uri string) *mcp.Resource { return &mcp.Resource{URI: uri, Name: uri} }
	nameOf := func(r *mcp.Resource) string { return r.URI }
	rename := func(r *mcp.Resource, name string) *mcp.Resource { c := *r; c.URI = name; return &c }

	table := router.Resolve([]router.Entry[*mcp.Resource]{
		{BackendName: "a", Items: []*mcp.Resource{resource("file:///data/README.md")}},
		{BackendName: "b", Items: []*mcp.Resource{resource("file:///data/README.md")}},
	}, nameOf, rename, map[string]string{"file:///data/README.md": "b"})

	got := table.Items["file:///data/README.md"]
	if got == nil || got.BackendName != "b" {
		t.Fatalf("Items[...] = %+v, want backend \"b\" (via override)", got)
	}
	if got.Item.URI != "file:///data/README.md" {
		t.Fatalf("Item.URI = %q, want unchanged (prefix empty)", got.Item.URI)
	}
}

func TestResolve_URITemplateKeyedItems(t *testing.T) {
	tmpl := func(uriTemplate string) *mcp.ResourceTemplate {
		return &mcp.ResourceTemplate{URITemplate: uriTemplate, Name: uriTemplate}
	}
	nameOf := func(t *mcp.ResourceTemplate) string { return t.URITemplate }
	rename := func(t *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate {
		c := *t
		c.URITemplate = name
		return &c
	}

	table := router.Resolve([]router.Entry[*mcp.ResourceTemplate]{
		{BackendName: "a", Items: []*mcp.ResourceTemplate{tmpl("file:///data/{path}")}},
	}, nameOf, rename, nil)

	got := table.Items["file:///data/{path}"]
	if got == nil || got.BackendName != "a" || got.Item.URITemplate != "file:///data/{path}" {
		t.Fatalf("Items[...] = %+v, unexpected", got)
	}
}
```

- [ ] **Step 2: テストを実行し、コンパイルエラーで失敗することを確認する**

Run: `go test ./internal/router/...`
Expected: FAIL（`router.Entry[*mcp.Tool]`等が未定義でコンパイルエラー）

- [ ] **Step 3: `internal/router/router.go`をgenerics対応で書き換える**

`internal/router/router.go`を以下の内容で置き換える:

```go
package router

// Entry is one backend's list of items (tools, resources, or resource
// templates), tagged with the backend's exposed-name prefix. entries passed
// to Resolve must be ordered by priority: index 0 is the highest priority
// (wins ties absent an override).
type Entry[T any] struct {
	BackendName string
	Prefix      string
	Items       []T
}

// Resolved is a single item exposed by the gateway, mapped back to the
// backend and original (un-prefixed) name that serves it.
type Resolved[T any] struct {
	Item         T
	BackendName  string
	OriginalName string
	// Fallbacks holds the other backends' definitions for this same exposed
	// name (in priority order), for a caller that wants to try the next one
	// if Item turns out to be unregisterable (e.g. a malformed schema or an
	// invalid URI).
	Fallbacks []Candidate[T]
}

// Candidate is one backend's definition that could serve a given exposed
// name.
type Candidate[T any] struct {
	Item         T
	BackendName  string
	OriginalName string
}

// Conflict records that multiple backends produced the same exposed name,
// and which backend's item won.
type Conflict struct {
	ExposedName string
	Winner      string
	Losers      []string
}

// Table is the fully resolved routing table: exposed name -> the
// backend/item that serves it, plus a record of any naming conflicts that
// were resolved along the way.
type Table[T any] struct {
	Items     map[string]*Resolved[T]
	Conflicts []Conflict
}

type candidate[T any] struct {
	backendName  string
	originalName string
	item         T
}

// Resolve merges entries' item lists into a single routing table. nameOf
// returns an item's own (un-prefixed) name; rename returns a copy of an
// item with its name (or URI) set to the given exposed name. overrides maps
// an exposed name to the backend name that must win that name's conflict;
// an override that names a real backend which does not produce an item
// under that exposed name has no effect (resolution falls back to list
// order).
func Resolve[T any](entries []Entry[T], nameOf func(T) string, rename func(T, string) T, overrides map[string]string) *Table[T] {
	candidatesByName := make(map[string][]candidate[T])
	var order []string // first-seen order, for deterministic conflict reporting

	for _, e := range entries {
		for _, item := range e.Items {
			exposedName := e.Prefix + nameOf(item)
			if _, seen := candidatesByName[exposedName]; !seen {
				order = append(order, exposedName)
			}
			candidatesByName[exposedName] = append(candidatesByName[exposedName], candidate[T]{
				backendName:  e.BackendName,
				originalName: nameOf(item),
				item:         item,
			})
		}
	}

	table := &Table[T]{Items: make(map[string]*Resolved[T], len(order))}
	for _, exposedName := range order {
		cands := candidatesByName[exposedName]
		winnerIdx := 0
		if wantBackend, ok := overrides[exposedName]; ok {
			for i, c := range cands {
				if c.backendName == wantBackend {
					winnerIdx = i
					break
				}
			}
		}
		winner := cands[winnerIdx]
		resolved := &Resolved[T]{
			Item:         rename(winner.item, exposedName),
			BackendName:  winner.backendName,
			OriginalName: winner.originalName,
		}
		if len(cands) > 1 {
			conflict := Conflict{ExposedName: exposedName, Winner: winner.backendName}
			for i, c := range cands {
				if i == winnerIdx {
					continue
				}
				conflict.Losers = append(conflict.Losers, c.backendName)
				resolved.Fallbacks = append(resolved.Fallbacks, Candidate[T]{
					Item:         rename(c.item, exposedName),
					BackendName:  c.backendName,
					OriginalName: c.originalName,
				})
			}
			table.Conflicts = append(table.Conflicts, conflict)
		}
		table.Items[exposedName] = resolved
	}
	return table
}
```

- [ ] **Step 4: router単体のテストを実行し、成功を確認する**

Run: `go test ./internal/router/...`
Expected: PASS

- [ ] **Step 5: `internal/gateway/gateway.go`をgenerics呼び出しへ追随させる**

`internal/gateway/gateway.go`の`New`・`registerTool`を以下に置き換える（他の関数・import・`ServeStdio`/`ServeHTTP`/`shutdownTimeout`は変更しない）:

```go
// New builds an mcp.Server that exposes table's resolved tools, forwarding
// each tools/call to the backend that owns it. backends must contain an
// entry for every BackendName referenced in table (the caller builds both
// from the same set of connected backends).
func New(logger *slog.Logger, backends map[string]*backend.Backend, table *router.Table[*mcp.Tool]) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{Logger: logger})

	for _, resolved := range table.Items {
		registerTool(srv, logger, backends, resolved)
	}

	return srv
}

// registerTool registers resolved.Item, falling back to the next
// lower-priority backend's definition (if any) when one turns out to be
// unregisterable, so a conflict's winner having a malformed schema doesn't
// need to take a validly-defined loser down with it.
func registerTool(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Tool]) {
	candidates := append([]router.Candidate[*mcp.Tool]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addTool(srv, logger, c.Item, callHandler(logger, b, c.OriginalName)) {
			return
		}
	}
	logger.Error("tool unavailable: every candidate backend had an invalid definition", "tool", resolved.Item.Name)
}
```

- [ ] **Step 6: `internal/gateway/gateway_test.go`をgenerics呼び出しへ追随させる**

3箇所を置き換える:

1. `router.Resolve([]router.Entry{{BackendName: "backend-dead", Tools: tools}}, nil)` を
   `router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-dead", Items: tools}}, toolNameOf, toolRename, nil)` に置き換える。

2. `TestGateway_FallsBackWhenWinnerSchemaInvalid`内の手組みテーブルを:

```go
	table := &router.Table[*mcp.Tool]{
		Items: map[string]*router.Resolved[*mcp.Tool]{
			"search": {
				Item:         &mcp.Tool{Name: "search"},
				BackendName:  "backend-a",
				OriginalName: "search",
				Fallbacks: []router.Candidate[*mcp.Tool]{{
					Item:         toolsB[0],
					BackendName:  "backend-b",
					OriginalName: "search",
				}},
			},
		},
	}
```

に置き換える。

3. `TestGateway_RoutesByPriorityAndExposesUniqueTools`内の:

```go
	table := router.Resolve([]router.Entry{
		{BackendName: "backend-a", Tools: toolsA},
		{BackendName: "backend-b", Tools: toolsB},
	}, nil)
```

を:

```go
	table := router.Resolve([]router.Entry[*mcp.Tool]{
		{BackendName: "backend-a", Items: toolsA},
		{BackendName: "backend-b", Items: toolsB},
	}, toolNameOf, toolRename, nil)
```

に置き換える。

さらにファイル末尾（`newFakeBackendServer`の直後など）に、テストファイル内で使うヘルパーを追加する:

```go
func toolNameOf(t *mcp.Tool) string { return t.Name }

func toolRename(t *mcp.Tool, name string) *mcp.Tool {
	c := *t
	c.Name = name
	return &c
}
```

- [ ] **Step 7: `internal/cli/server.go`をgenerics呼び出しへ追随させる**

`internal/cli/server.go`の`runServer`内の以下:

```go
	table := router.Resolve(entries, cfg.Overrides)
```

を:

```go
	table := router.Resolve(entries, toolNameOf, toolRename, cfg.Overrides)
```

に置き換える。`connectBackends`内の`router.Entry{BackendName: bc.Name, Prefix: bc.Prefix, Tools: tools}`を`router.Entry[*mcp.Tool]{BackendName: bc.Name, Prefix: bc.Prefix, Items: tools}`に置き換える。`connectBackends`のシグネチャ中の`[]router.Entry`はすべて`[]router.Entry[*mcp.Tool]`に置き換える（`map[string]*backend.Backend, []router.Entry`→`map[string]*backend.Backend, []router.Entry[*mcp.Tool]`、内部の`outcome`構造体の`entry`フィールド型、`entries`変数の型も同様）。

ファイル末尾に以下のヘルパーを追加し、`"github.com/modelcontextprotocol/go-sdk/mcp"`をimportに追加する:

```go
func toolNameOf(t *mcp.Tool) string { return t.Name }

func toolRename(t *mcp.Tool, name string) *mcp.Tool {
	c := *t
	c.Name = name
	return &c
}
```

- [ ] **Step 8: リポジトリ全体のテストを実行し、成功を確認する**

Run: `go build ./... && go test ./...`
Expected: PASS（tool中継の挙動は変わらない）

- [ ] **Step 9: Commit**

```bash
git add internal/router/router.go internal/router/router_test.go internal/gateway/gateway.go internal/gateway/gateway_test.go internal/cli/server.go
git commit -m "refactor: generalize router.Resolve to Resolve[T any]"
```

---

### Task 2: `internal/backend`に`ListResources`・`ListResourceTemplates`を追加する

**Files:**
- Modify: `internal/backend/backend.go`
- Test: `internal/backend/backend_test.go`

**Interfaces:**
- Consumes: `*mcp.ClientSession`の`Resources(ctx, nil) iter.Seq2[*mcp.Resource, error]` / `ResourceTemplates(ctx, nil) iter.Seq2[*mcp.ResourceTemplate, error]`（SDK既存API、`ListTools`が使う`.Tools`と対称）
- Produces: `(b *Backend) ListResources(ctx context.Context) ([]*mcp.Resource, error)`、`(b *Backend) ListResourceTemplates(ctx context.Context) ([]*mcp.ResourceTemplate, error)`

- [ ] **Step 1: 失敗するテストを書く**

`internal/backend/backend_test.go`の`TestConnect_UnknownTransport`の直前に、以下のテストを追加する:

```go
func TestListResourcesAndResourceTemplates(t *testing.T) {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	handler := func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{Text: "stub"}}}, nil
	}
	fakeServer.AddResource(&mcp.Resource{URI: "file:///a", Name: "a"}, handler)
	fakeServer.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "file:///dir/{f}", Name: "dir"}, handler)

	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil))
	defer srv.Close()

	ctx := context.Background()
	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	resources, err := b.ListResources(ctx)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(resources) != 1 || resources[0].URI != "file:///a" {
		t.Fatalf("ListResources = %+v, want one resource with URI file:///a", resources)
	}

	templates, err := b.ListResourceTemplates(ctx)
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	if len(templates) != 1 || templates[0].URITemplate != "file:///dir/{f}" {
		t.Fatalf("ListResourceTemplates = %+v, want one template file:///dir/{f}", templates)
	}
}
```

- [ ] **Step 2: テストを実行し、失敗することを確認する**

Run: `go test ./internal/backend/... -run TestListResourcesAndResourceTemplates -v`
Expected: FAIL（`b.ListResources`未定義でコンパイルエラー）

- [ ] **Step 3: `internal/backend/backend.go`に実装を追加する**

`ListTools`メソッドの直後に以下を追加する:

```go
// ListResources fetches the backend's full resource list, following
// pagination.
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

// ListResourceTemplates fetches the backend's full resource template list,
// following pagination.
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

- [ ] **Step 4: テストを実行し、成功することを確認する**

Run: `go test ./internal/backend/... -v`
Expected: PASS（既存テストも含め全て通る）

- [ ] **Step 5: Commit**

```bash
git add internal/backend/backend.go internal/backend/backend_test.go
git commit -m "feat: add Backend.ListResources and ListResourceTemplates"
```

---

### Task 3: `internal/config`に`resource_overrides`・`resource_template_overrides`を追加する

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.ResourceOverrides map[string]string`（yaml: `resource_overrides`）、`Config.ResourceTemplateOverrides map[string]string`（yaml: `resource_template_overrides`）。既存の`Config.Overrides`（tool用）はそのまま残す。

- [ ] **Step 1: 失敗するテストを書く**

`internal/config/config_test.go`の`TestParse_OverrideReferencesUnknownBackend`の直後に以下を追加する:

```go
func TestParse_ResourceOverrides(t *testing.T) {
	data := []byte(`
backends:
  - name: filesystem-primary
    transport: stdio
    command: ["a"]
  - name: filesystem-secondary
    transport: stdio
    command: ["b"]

resource_overrides:
  "file:///data/README.md": filesystem-primary

resource_template_overrides:
  "file:///data/{path}": filesystem-primary
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ResourceOverrides["file:///data/README.md"] != "filesystem-primary" {
		t.Fatalf("ResourceOverrides[...] = %q, want %q", cfg.ResourceOverrides["file:///data/README.md"], "filesystem-primary")
	}
	if cfg.ResourceTemplateOverrides["file:///data/{path}"] != "filesystem-primary" {
		t.Fatalf("ResourceTemplateOverrides[...] = %q, want %q", cfg.ResourceTemplateOverrides["file:///data/{path}"], "filesystem-primary")
	}
}

func TestParse_ResourceOverrideReferencesUnknownBackend(t *testing.T) {
	data := []byte(`
backends:
  - name: known
    transport: stdio
    command: ["a"]

resource_overrides:
  "file:///data/README.md": nonexistent
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for resource_overrides referencing unknown backend, got nil")
	}
}

func TestParse_ResourceTemplateOverrideReferencesUnknownBackend(t *testing.T) {
	data := []byte(`
backends:
  - name: known
    transport: stdio
    command: ["a"]

resource_template_overrides:
  "file:///data/{path}": nonexistent
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for resource_template_overrides referencing unknown backend, got nil")
	}
}
```

- [ ] **Step 2: テストを実行し、失敗することを確認する**

Run: `go test ./internal/config/... -run TestParse_Resource -v`
Expected: FAIL（`cfg.ResourceOverrides`未定義でコンパイルエラー）

- [ ] **Step 3: `internal/config/config.go`に実装を追加する**

`Config`構造体を以下に置き換える:

```go
// Config is the top-level gateway configuration, loaded from a YAML file.
type Config struct {
	Listen                    ListenConfig      `yaml:"listen"`
	Backends                  []BackendConfig   `yaml:"backends"`
	Overrides                 map[string]string `yaml:"overrides,omitempty"`
	ResourceOverrides         map[string]string `yaml:"resource_overrides,omitempty"`
	ResourceTemplateOverrides map[string]string `yaml:"resource_template_overrides,omitempty"`
}
```

`validate`関数末尾の以下のブロック:

```go
	for toolName, backendName := range cfg.Overrides {
		if !names[backendName] {
			return fmt.Errorf("override %q references unknown backend %q", toolName, backendName)
		}
	}

	return nil
}
```

を以下に置き換える:

```go
	for toolName, backendName := range cfg.Overrides {
		if !names[backendName] {
			return fmt.Errorf("override %q references unknown backend %q", toolName, backendName)
		}
	}
	for uri, backendName := range cfg.ResourceOverrides {
		if !names[backendName] {
			return fmt.Errorf("resource_overrides %q references unknown backend %q", uri, backendName)
		}
	}
	for uriTemplate, backendName := range cfg.ResourceTemplateOverrides {
		if !names[backendName] {
			return fmt.Errorf("resource_template_overrides %q references unknown backend %q", uriTemplate, backendName)
		}
	}

	return nil
}
```

- [ ] **Step 4: テストを実行し、成功することを確認する**

Run: `go test ./internal/config/... -v`
Expected: PASS（既存テストも含め全て通る）

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: add resource_overrides and resource_template_overrides config keys"
```

---

### Task 4: `internal/gateway`にresource/resource template中継を追加し、`Tables`構造体を導入する

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/gateway_test.go`
- Modify: `internal/cli/server.go:97`（`gateway.New`呼び出しをTables型に合わせる最小限の1行修正。resource/resource templateの実配線はTask 5で行う）

**Interfaces:**
- Consumes: Task 1の`router.Table[T]`・`router.Resolved[T]`・`router.Candidate[T]`、Task 2の`backend.Backend`（変更なし、型のみ）
- Produces: `gateway.Tables{Tools *router.Table[*mcp.Tool]; Resources *router.Table[*mcp.Resource]; ResourceTemplates *router.Table[*mcp.ResourceTemplate]}`、`gateway.New(logger *slog.Logger, backends map[string]*backend.Backend, tables Tables) *mcp.Server`（**シグネチャ変更**: 第3引数が`*router.Table[*mcp.Tool]`から`Tables`へ）

- [ ] **Step 1: 失敗するテストを書く**

`internal/gateway/gateway_test.go`の`newFakeBackendServer`と`toolNameOf`/`toolRename`の後（ファイル末尾）に以下を追加する:

```go
func newFakeResourceBackendServer(name string, uris ...string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, nil)
	for _, uri := range uris {
		srv.AddResource(&mcp.Resource{URI: uri, Name: uri},
			func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: name}}}, nil
			})
	}
	return srv
}

func newFakeResourceTemplateBackendServer(name, uriTemplate string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, nil)
	srv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: uriTemplate, Name: uriTemplate},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: req.Params.URI}}}, nil
		})
	return srv
}

// TestGateway_ResourceReadExact checks that resources/read for an exact
// registered URI is forwarded to the owning backend and its result
// returned unchanged.
func TestGateway_ResourceReadExact(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := newFakeResourceBackendServer("backend-a", "file:///a")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	resources, err := connA.ListResources(ctx)
	if err != nil {
		t.Fatalf("list backend-a resources: %v", err)
	}

	resourceNameOf := func(r *mcp.Resource) string { return r.URI }
	resourceRename := func(r *mcp.Resource, name string) *mcp.Resource { c := *r; c.URI = name; return &c }
	table := router.Resolve([]router.Entry[*mcp.Resource]{
		{BackendName: "backend-a", Items: resources},
	}, resourceNameOf, resourceRename, nil)

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Resources: table})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///a"})
	if err != nil {
		t.Fatalf("ReadResource(file:///a): %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].Text != "backend-a" {
		t.Fatalf("ReadResource result = %+v, want text \"backend-a\"", res.Contents)
	}
}

// TestGateway_ResourceTemplateReadForwardsActualURI checks that
// resources/read for a URI matching a registered template forwards the
// client's actual requested URI to the backend (not the template string).
func TestGateway_ResourceTemplateReadForwardsActualURI(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := newFakeResourceTemplateBackendServer("backend-a", "file:///dir/{f}")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	templates, err := connA.ListResourceTemplates(ctx)
	if err != nil {
		t.Fatalf("list backend-a resource templates: %v", err)
	}

	templateNameOf := func(rt *mcp.ResourceTemplate) string { return rt.URITemplate }
	templateRename := func(rt *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate { c := *rt; c.URITemplate = name; return &c }
	table := router.Resolve([]router.Entry[*mcp.ResourceTemplate]{
		{BackendName: "backend-a", Items: templates},
	}, templateNameOf, templateRename, nil)

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{ResourceTemplates: table})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///dir/x"})
	if err != nil {
		t.Fatalf("ReadResource(file:///dir/x): %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].Text != "file:///dir/x" {
		t.Fatalf("ReadResource result = %+v, want text \"file:///dir/x\" (the actual requested URI forwarded to the backend)", res.Contents)
	}
}

// TestGateway_ResourceFallsBackWhenWinnerURIInvalid checks that when the
// conflict winner's resource URI is malformed (AddResource panics on it),
// the gateway falls back to the next candidate rather than dropping the
// resource entirely.
func TestGateway_ResourceFallsBackWhenWinnerURIInvalid(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendBServer := newFakeResourceBackendServer("backend-b", "file:///a")
	httpB := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendBServer }, nil))
	defer httpB.Close()

	ctx := context.Background()
	connB, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-b", Transport: "http", URL: httpB.URL})
	if err != nil {
		t.Fatalf("connect backend-b: %v", err)
	}
	defer func() { _ = connB.Close() }()

	resourcesB, err := connB.ListResources(ctx)
	if err != nil {
		t.Fatalf("list backend-b resources: %v", err)
	}

	// Hand-build a table: the winner ("backend-a") has an invalid URI, which
	// mcp.Server.AddResource panics on; the loser ("backend-b") is a valid
	// fallback candidate.
	table := &router.Table[*mcp.Resource]{
		Items: map[string]*router.Resolved[*mcp.Resource]{
			"resource-key": {
				Item:         &mcp.Resource{URI: "http://%gg", Name: "bad"},
				BackendName:  "backend-a",
				OriginalName: "http://%gg",
				Fallbacks: []router.Candidate[*mcp.Resource]{{
					Item:         resourcesB[0],
					BackendName:  "backend-b",
					OriginalName: "file:///a",
				}},
			},
		},
	}

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-b": connB}, gateway.Tables{Resources: table})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///a"})
	if err != nil {
		t.Fatalf("ReadResource(file:///a): %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].Text != "backend-b" {
		t.Fatalf("ReadResource result = %+v, want text \"backend-b\" (the fallback)", res.Contents)
	}
}
```

既存3テスト（`TestGateway_CallOnDeadBackendReturnsError`、`TestGateway_FallsBackWhenWinnerSchemaInvalid`、`TestGateway_RoutesByPriorityAndExposesUniqueTools`）内の`gateway.New(logger, backends, table)`呼び出しはすべて`gateway.New(logger, backends, gateway.Tables{Tools: table})`に置き換える。

- [ ] **Step 2: テストを実行し、失敗することを確認する**

Run: `go test ./internal/gateway/... -v`
Expected: FAIL（`gateway.Tables`未定義、`gateway.New`の引数個数不一致でコンパイルエラー）

- [ ] **Step 3: `internal/gateway/gateway.go`に実装を追加する**

`New`関数を以下に置き換え、`registerTool`の直後（`callHandler`の前）に新関数群を追加する:

```go
// Tables bundles the independent routing tables the gateway serves: tools,
// resources, and resource templates. They are built once at startup and
// never change while the gateway runs.
type Tables struct {
	Tools             *router.Table[*mcp.Tool]
	Resources         *router.Table[*mcp.Resource]
	ResourceTemplates *router.Table[*mcp.ResourceTemplate]
}

// New builds an mcp.Server that exposes tables' resolved tools/resources,
// forwarding each call to the backend that owns it. backends must contain
// an entry for every BackendName referenced in tables (the caller builds
// both from the same set of connected backends).
func New(logger *slog.Logger, backends map[string]*backend.Backend, tables Tables) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{Logger: logger})

	if tables.Tools != nil {
		for _, resolved := range tables.Tools.Items {
			registerTool(srv, logger, backends, resolved)
		}
	}
	if tables.Resources != nil {
		for _, resolved := range tables.Resources.Items {
			registerResource(srv, logger, backends, resolved)
		}
	}
	if tables.ResourceTemplates != nil {
		for _, resolved := range tables.ResourceTemplates.Items {
			registerResourceTemplate(srv, logger, backends, resolved)
		}
	}

	return srv
}
```

（`registerTool`は変更しない。）以下を`callHandler`の前に追加する:

```go
// registerResource registers resolved.Item, falling back to the next
// lower-priority backend's definition (if any) when one turns out to have
// an invalid URI, so a conflict's winner having a malformed URI doesn't
// need to take a validly-defined loser down with it.
func registerResource(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Resource]) {
	candidates := append([]router.Candidate[*mcp.Resource]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addResource(srv, logger, c.Item, resourceReadHandler(logger, b, c.OriginalName)) {
			return
		}
	}
	logger.Error("resource unavailable: every candidate backend had an invalid URI", "uri", resolved.Item.URI)
}

// resourceReadHandler forwards resources/read to originalURI on backend b.
// originalURI is the fixed URI this exact resource was registered under:
// prefix is never applied to resource URIs, so it equals the exposed URI,
// and every call for this resource reads the same URI.
func resourceReadHandler(logger *slog.Logger, b *backend.Backend, originalURI string) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		result, err := b.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: originalURI})
		if err != nil {
			logger.Error("backend call failed", "backend", b.Name, "uri", originalURI, "error", err)
		}
		return result, err
	}
}

// addResource registers r on srv, recovering from AddResource's panic on an
// invalid or non-absolute URI so that one broken backend resource
// definition can't take down the whole gateway process at startup. It
// reports whether r was registered.
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

// registerResourceTemplate is registerResource's counterpart for resource
// templates: same panic-recovery/fallback structure, but its read handler
// forwards the caller's actual matched URI instead of a fixed one.
func registerResourceTemplate(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.ResourceTemplate]) {
	candidates := append([]router.Candidate[*mcp.ResourceTemplate]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addResourceTemplate(srv, logger, c.Item, resourceTemplateReadHandler(logger, b)) {
			return
		}
	}
	logger.Error("resource template unavailable: every candidate backend had an invalid URI template", "uriTemplate", resolved.Item.URITemplate)
}

// resourceTemplateReadHandler forwards resources/read to the actual URI the
// client requested (req.Params.URI, the concrete URI that matched this
// template) on backend b -- unlike an exact resource, a template serves a
// different URI on every call, so the fixed-URI approach resourceReadHandler
// uses doesn't apply here.
func resourceTemplateReadHandler(logger *slog.Logger, b *backend.Backend) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		result, err := b.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
		if err != nil {
			logger.Error("backend call failed", "backend", b.Name, "uri", req.Params.URI, "error", err)
		}
		return result, err
	}
}

// addResourceTemplate registers t on srv, recovering from
// AddResourceTemplate's panic on an invalid URI template. It reports
// whether t was registered.
func addResourceTemplate(srv *mcp.Server, logger *slog.Logger, t *mcp.ResourceTemplate, h mcp.ResourceHandler) (ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Error("invalid resource template definition", "uriTemplate", t.URITemplate, "error", rec)
			ok = false
		}
	}()
	srv.AddResourceTemplate(t, h)
	return true
}
```

- [ ] **Step 4: `internal/cli/server.go`の`gateway.New`呼び出しを最小限修正する**

`internal/cli/server.go`の`runServer`内、以下の行:

```go
	srv := gateway.New(logger, backends, table)
```

を:

```go
	srv := gateway.New(logger, backends, gateway.Tables{Tools: table})
```

に置き換える（resource/resource templateの取得・配線はTask 5で行うため、ここでは`gateway.New`が新シグネチャでコンパイルが通る状態にするだけでよい）。

- [ ] **Step 5: テストを実行し、成功することを確認する**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go internal/cli/server.go
git commit -m "feat: add resource and resource template relay to the gateway"
```

---

### Task 5: `internal/cli/server.go`でresource/resource templateを起動時に取得し配線する

**Files:**
- Modify: `internal/cli/server.go`
- Modify: `internal/cli/server_internal_test.go`（`connectBackends`のシグネチャ変更に追随）
- Modify: `internal/cli/server_test.go`（resource中継のe2eテストを追加）

**Interfaces:**
- Consumes: `backend.Backend.ListResources`/`ListResourceTemplates`（Task 2）、`config.Config.ResourceOverrides`/`ResourceTemplateOverrides`（Task 3）、`gateway.Tables`（Task 4）
- Produces: `connectBackends`の戻り値を単一の非公開`connected`構造体にまとめる（`backends map[string]*backend.Backend`、`toolEntries []router.Entry[*mcp.Tool]`、`resourceEntries []router.Entry[*mcp.Resource]`、`resourceTemplateEntries []router.Entry[*mcp.ResourceTemplate]`）

- [ ] **Step 1: 失敗するテストを書く（e2e）**

`internal/cli/server_test.go`の`TestServerCommand_ServesAggregatedTools`の直後に以下を追加する:

```go
func TestServerCommand_ServesAggregatedResources(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	backendSrv.AddResource(&mcp.Resource{URI: "file:///a", Name: "a"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "hello"}}}, nil
		})
	backendSrv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "file:///dir/{f}", Name: "dir"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: req.Params.URI}}}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: fake
    transport: http
    url: %q
`, gatewayAddr, backendHTTP.URL))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + gatewayAddr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///a"})
	if err != nil {
		t.Fatalf("ReadResource(file:///a): %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].Text != "hello" {
		t.Fatalf("ReadResource(file:///a) = %+v, want text \"hello\"", res.Contents)
	}

	res, err = session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///dir/x"})
	if err != nil {
		t.Fatalf("ReadResource(file:///dir/x): %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].Text != "file:///dir/x" {
		t.Fatalf("ReadResource(file:///dir/x) = %+v, want text \"file:///dir/x\" (template match forwards actual URI)", res.Contents)
	}
	_ = session.Close()

	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}
```

- [ ] **Step 2: テストを実行し、失敗することを確認する**

Run: `go test ./internal/cli/... -run TestServerCommand_ServesAggregatedResources -v`
Expected: FAIL（backendがresourceを提供していても、gatewayが未配線なので`ResourceNotFoundError`が返る）

- [ ] **Step 3: `internal/cli/server.go`を書き換える**

まず、`"github.com/modelcontextprotocol/go-sdk/mcp"`をimportに追加する。

`runServer`関数を以下に置き換える:

```go
func runServer(ctx context.Context, logger *slog.Logger, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if !cfg.Listen.Stdio && cfg.Listen.HTTP == "" {
		return errors.New("no listener configured: enable listen.stdio or set listen.http")
	}

	// A child context we can cancel ourselves: if one listener fails while
	// another is still healthy, cancelling here tells the healthy one to
	// shut down too instead of leaving runServer blocked waiting on it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn := connectBackends(ctx, logger, cfg.Backends)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()

	toolTable := router.Resolve(conn.toolEntries, toolNameOf, toolRename, cfg.Overrides)
	for _, c := range toolTable.Conflicts {
		logger.Warn("tool name conflict", "tool", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	resourceTable := router.Resolve(conn.resourceEntries, resourceNameOf, resourceRename, cfg.ResourceOverrides)
	for _, c := range resourceTable.Conflicts {
		logger.Warn("resource URI conflict", "uri", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	resourceTemplateTable := router.Resolve(conn.resourceTemplateEntries, resourceTemplateNameOf, resourceTemplateRename, cfg.ResourceTemplateOverrides)
	for _, c := range resourceTemplateTable.Conflicts {
		logger.Warn("resource template URI conflict", "uriTemplate", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	srv := gateway.New(logger, conn.backends, gateway.Tables{
		Tools:             toolTable,
		Resources:         resourceTable,
		ResourceTemplates: resourceTemplateTable,
	})

	running := 0
	errCh := make(chan error, 2)
	if cfg.Listen.Stdio {
		running++
		go func() { errCh <- gateway.ServeStdio(ctx, srv) }()
	}
	if cfg.Listen.HTTP != "" {
		running++
		go func() { errCh <- gateway.ServeHTTP(ctx, srv, cfg.Listen.HTTP) }()
	}

	// Log each listener's outcome as it arrives, so a listener that fails
	// while another is still healthy is reported immediately. A cancelled
	// context is how a clean shutdown reaches ServeStdio, so it isn't a
	// failure. cancel() on a real failure stops the other listener too,
	// instead of waiting on it indefinitely.
	var firstErr error
	for i := 0; i < running; i++ {
		err := <-errCh
		if err == nil {
			continue
		}
		if errors.Is(err, context.Canceled) {
			logger.Debug("listener stopped due to shutdown", "error", err)
			continue
		}
		logger.Error("listener stopped with error", "error", err)
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	return firstErr
}

// toolNameOf and toolRename already exist in this file (added in Task 1) --
// do not redeclare them here.

func resourceNameOf(r *mcp.Resource) string { return r.URI }

func resourceRename(r *mcp.Resource, name string) *mcp.Resource {
	c := *r
	c.URI = name
	return &c
}

func resourceTemplateNameOf(t *mcp.ResourceTemplate) string { return t.URITemplate }

func resourceTemplateRename(t *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate {
	c := *t
	c.URITemplate = name
	return &c
}
```

`connected`型と`connectBackends`関数を以下に置き換える:

```go
// connected is the outcome of connectBackends: the live backend
// connections, plus each kind of item list gathered from them, ready to
// pass to router.Resolve. Entries preserve configs' order, since
// router.Resolve treats that order as priority (index 0 = highest).
type connected struct {
	backends                map[string]*backend.Backend
	toolEntries              []router.Entry[*mcp.Tool]
	resourceEntries          []router.Entry[*mcp.Resource]
	resourceTemplateEntries  []router.Entry[*mcp.ResourceTemplate]
}
```

The field alignment above may not exactly match `gofmt`'s column widths; run `gofmt -w internal/cli/server.go` immediately after writing this (before running any tests) so the file matches what `task lint` expects. The same applies to every other code block in this plan: treat the shown spacing as approximate and let `gofmt -w` be the source of truth.

```go

// connectBackends connects to every configured backend concurrently and
// lists its tools, resources, and resource templates. A backend that fails
// to connect, fails any of those listings, or exceeds backendConnectTimeout
// is logged and excluded (best-effort); it does not fail or stall the whole
// startup.
func connectBackends(ctx context.Context, logger *slog.Logger, configs []config.BackendConfig) connected {
	type outcome struct {
		backend               *backend.Backend
		toolEntry             router.Entry[*mcp.Tool]
		resourceEntry         router.Entry[*mcp.Resource]
		resourceTemplateEntry router.Entry[*mcp.ResourceTemplate]
	}
	outcomes := make([]*outcome, len(configs))

	var wg sync.WaitGroup
	for i, bc := range configs {
		wg.Add(1)
		go func(i int, bc config.BackendConfig) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, backendConnectTimeout)
			defer cancel()

			b, err := backend.Connect(ctx, bc)
			if err != nil {
				logger.Error("skipping backend: connect failed", "backend", bc.Name, "error", err)
				return
			}
			tools, err := b.ListTools(ctx)
			if err != nil {
				logger.Error("skipping backend: list tools failed", "backend", bc.Name, "error", err)
				_ = b.Close()
				return
			}
			resources, err := b.ListResources(ctx)
			if err != nil {
				logger.Error("skipping backend: list resources failed", "backend", bc.Name, "error", err)
				_ = b.Close()
				return
			}
			resourceTemplates, err := b.ListResourceTemplates(ctx)
			if err != nil {
				logger.Error("skipping backend: list resource templates failed", "backend", bc.Name, "error", err)
				_ = b.Close()
				return
			}
			outcomes[i] = &outcome{
				backend: b,
				toolEntry: router.Entry[*mcp.Tool]{
					BackendName: bc.Name, Prefix: bc.Prefix, Items: tools,
				},
				// resource/resource template entries never carry a prefix:
				// URIs already encode a backend-specific namespace, and
				// string-concatenating a prefix onto one would produce an
				// invalid URI.
				resourceEntry: router.Entry[*mcp.Resource]{
					BackendName: bc.Name, Items: resources,
				},
				resourceTemplateEntry: router.Entry[*mcp.ResourceTemplate]{
					BackendName: bc.Name, Items: resourceTemplates,
				},
			}
		}(i, bc)
	}
	wg.Wait()

	result := connected{backends: make(map[string]*backend.Backend, len(configs))}
	for _, o := range outcomes {
		if o == nil {
			continue
		}
		result.backends[o.toolEntry.BackendName] = o.backend
		result.toolEntries = append(result.toolEntries, o.toolEntry)
		result.resourceEntries = append(result.resourceEntries, o.resourceEntry)
		result.resourceTemplateEntries = append(result.resourceTemplateEntries, o.resourceTemplateEntry)
	}
	return result
}
```

- [ ] **Step 4: `internal/cli/server_internal_test.go`を更新する**

`TestConnectBackends_TimesOutHungBackend`内の:

```go
	done := make(chan struct{})
	var backends map[string]*backend.Backend
	go func() {
		backends, _ = connectBackends(context.Background(), logger, configs)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("connectBackends did not return within 10s of backendConnectTimeout elapsing")
	}
	if len(backends) != 0 {
		t.Fatalf("backends = %v, want none (the hung backend should be excluded)", backends)
	}
```

を:

```go
	done := make(chan struct{})
	var conn connected
	go func() {
		conn = connectBackends(context.Background(), logger, configs)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("connectBackends did not return within 10s of backendConnectTimeout elapsing")
	}
	if len(conn.backends) != 0 {
		t.Fatalf("backends = %v, want none (the hung backend should be excluded)", conn.backends)
	}
```

に置き換える。この置き換えによって`"github.com/wtnb75/mcprt/internal/backend"`importが未使用になる場合は、import文からも削除する。

- [ ] **Step 5: テストを実行し、成功することを確認する**

Run: `go build ./... && go test ./...`
Expected: PASS（`TestServerCommand_ServesAggregatedResources`含め全て通る）

- [ ] **Step 6: `task lint`を実行し、gofmt/vet/golangci-lintが通ることを確認する**

Run: `task lint`
Expected: 差分・エラーなし（`connected`構造体フィールドのアラインメントなど、gofmtが自動整形するものはこのタイミングで整える）

- [ ] **Step 7: Commit**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go internal/cli/server_test.go
git commit -m "feat: wire resource and resource template relay into the server command"
```

---

### Task 6: ドキュメント更新と最終確認

**Files:**
- Modify: `README.md`
- Modify: `config-example.yaml`

**Interfaces:**
- Consumes: なし（ドキュメントのみ）

- [ ] **Step 1: `README.md`を更新する**

冒頭の説明文:

```markdown
mcprt aggregates multiple MCP servers (local stdio subprocesses and remote
HTTP servers) behind a single MCP gateway endpoint.
```

を:

```markdown
mcprt aggregates multiple MCP servers (local stdio subprocesses and remote
HTTP servers) behind a single MCP gateway endpoint, relaying `tools/*` and
`resources/*` calls to whichever backend serves them.
```

に置き換える。

設定例の`overrides:`ブロックの直後（`When ssh is set...`の前）に以下を挿入する:

```markdown
    resource_overrides:
      "file:///data/README.md": filesystem

    resource_template_overrides:
      "file:///data/{path}": filesystem

`overrides` resolves conflicting **tool** names (after each backend's
`prefix` is applied). `resource_overrides` and `resource_template_overrides`
resolve conflicting resource URIs and URI templates the same way, but
`prefix` is never applied to resources: a URI already carries a
backend-specific namespace (`scheme://host/path`), and string-concatenating
a prefix onto one would produce an invalid URI. `resources/subscribe` and
`notifications/resources/updated` are not relayed.
```

- [ ] **Step 2: `config-example.yaml`を更新する**

ファイル末尾に以下を追加する:

```yaml

resource_overrides:
  "file:///example/data.txt": example
```

- [ ] **Step 3: 最終確認**

Run: `task build && task test && task lint`
Expected: 全て成功（ビルド成功、全テストPASS、lintエラーなし）

- [ ] **Step 4: Commit**

```bash
git add README.md config-example.yaml
git commit -m "docs: document resource_overrides and resource_template_overrides"
```

---

## 完了条件

- `go test ./...`が外部サービス依存なしで全てPASSする
- `task lint`（`gofmt -l .` / `go vet ./...` / `golangci-lint run ./...`）がエラーなしで完了する
- `resources/list` / `resources/templates/list` / `resources/read`（exact一致・template一致の両方）がgateway経由でbackendへ正しく中継される（`internal/gateway`・`internal/cli`のe2eテストで確認済み）
- resource/resource templateのURI衝突が記載順＋`resource_overrides`/`resource_template_overrides`で解決される（`internal/router`のテーブル駆動テストで確認済み）
- resource/resource templateのURIに`prefix`が適用されない（`connectBackends`が`Prefix`を設定しないことで保証）
