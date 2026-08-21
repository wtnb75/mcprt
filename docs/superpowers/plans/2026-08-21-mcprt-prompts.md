# mcprt prompts/* relay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Relay MCP `prompts/list` and `prompts/get` through the mcprt gateway, merging each backend's prompts into one routing table with the same prefix/list-order/overrides conflict resolution already used for tools.

**Architecture:** Same shape as the existing tools and resources relays: at startup, connect to every backend, fetch its prompts alongside its tools/resources/resource templates, resolve them into a `router.Table[*mcp.Prompt]` via the already-generic `router.Resolve[T]`, and register each winning prompt on the gateway's `mcp.Server` with a handler that forwards `prompts/get` to the owning backend.

**Tech Stack:** Go 1.25, `github.com/modelcontextprotocol/go-sdk` v1.7.0, existing `internal/router`/`internal/backend`/`internal/gateway`/`internal/config`/`internal/cli` packages.

**Spec:** `docs/superpowers/specs/2026-08-20-mcprt-prompts-design.md`

## Important: spec vs. current codebase

The spec was written assuming `internal/router.Resolve` was still tools-only and needed generalizing (its "含める" list names the router refactor as in-scope). **That refactor already happened** as part of a separate, already-shipped resources/* relay feature: `router.Entry[T]`, `Resolved[T]`, `Candidate[T]`, `Table[T]`, and `Resolve[T any](entries []Entry[T], nameOf func(T) string, rename func(T, string) T, overrides map[string]string) *Table[T]` all exist today exactly as the spec describes them (`internal/router/router.go`), already exercised by three concrete types (`*mcp.Tool`, `*mcp.Resource`, `*mcp.ResourceTemplate`). **No task in this plan touches `internal/router`** — prompts just becomes a fourth type parameter for the same generic function. This plan's tasks are correspondingly narrower than the spec's own "含める" list: only backend/gateway/config/cli wiring remains.

The same already-shipped feature also revealed and fixed a regression worth carrying forward here (see Global Constraint 5 below): treating a non-tools listing call (`ListResources`/`ListResourceTemplates`) as fatal to a backend's connection silently drops tools-only backends built with non-Go-SDK MCP servers, which answer an unimplemented method with a JSON-RPC error instead of an empty list. This plan applies the same non-fatal treatment to `ListPrompts` from the start, rather than waiting for a review to catch it again.

## Global Constraints

- `prompt_overrides` is a **separate** config key from `overrides` (tool names and prompt names are different namespaces; conflating them would make a shared key ambiguous about which kind of conflict it resolves). Exact key name: `prompt_overrides`, YAML tag `prompt_overrides,omitempty`, type `map[string]string`.
- Backend `prefix` **is** applied to prompt names, exactly like tool names (unlike resource/resource-template URIs, which never get a prefix). Do not special-case prompts the way resources are special-cased.
- `mcp.Server.AddPrompt` performs no schema validation and never panics (confirmed in `go-sdk@v1.7.0/mcp/server.go:236-242` — `Prompt` has no JSON-Schema-bearing field for `AddPrompt` to reject). `registerPrompt` therefore does **not** need `addTool`/`addResource`'s panic-recovery-and-fallback-to-next-candidate loop: resolve the winner and register it directly, once.
- `prompts/list` is served statically from the table built at startup (identical to how `tools/list` and `resources/list` already work) — no backend round-trip at request time, and this plan adds no new request-time list handler.
- A backend's `ListPrompts` failure must be **non-fatal**: log a warning and treat the prompt list as empty, keeping the backend and its tools/resources connected. Only `Connect` and `ListTools` failures exclude a backend entirely. This mirrors the already-fixed treatment of `ListResources`/`ListResourceTemplates` in `internal/cli/server.go`'s `connectBackends`, and exists for the same reason: a tools-only backend built with a non-Go-SDK MCP server commonly answers an unadvertised capability's list method with a JSON-RPC "method not found" error rather than an empty list.
- `internal/cli/export.go` and `internal/cli/init.go` must stay in sync with new config keys as they're added (`export.go` warns when dropping a key mcp.json has no equivalent for; `init.go`'s `exampleConfig()` includes every override kind) — this was a gap found and fixed after the fact for `resource_overrides`/`resource_template_overrides`; this plan adds `prompt_overrides` to both from the start.

---

### Task 1: `internal/backend`: add `ListPrompts`

**Files:**
- Modify: `internal/backend/backend.go`
- Test: `internal/backend/backend_test.go`

**Interfaces:**
- Consumes: `b.Session.Prompts(ctx, params) iter.Seq2[*mcp.Prompt, error]` (already exists on `*mcp.ClientSession` in the vendored SDK; same shape as `Session.Tools`/`Session.Resources`)
- Produces: `func (b *Backend) ListPrompts(ctx context.Context) ([]*mcp.Prompt, error)`

- [ ] **Step 1: Write the failing test**

Add to `internal/backend/backend_test.go`, right after `TestListResourcesAndResourceTemplates` (around line 301):

```go
func TestListPrompts(t *testing.T) {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	fakeServer.AddPrompt(&mcp.Prompt{Name: "greet", Description: "say hello"},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{}}, nil
		})

	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil))
	defer srv.Close()

	ctx := context.Background()
	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	prompts, err := b.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts) != 1 || prompts[0].Name != "greet" {
		t.Fatalf("ListPrompts = %+v, want one prompt named \"greet\"", prompts)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/backend/... -run TestListPrompts -v`
Expected: FAIL with `b.ListPrompts undefined (type *backend.Backend has no field or method ListPrompts)`

- [ ] **Step 3: Implement `ListPrompts`**

In `internal/backend/backend.go`, add immediately after `ListResourceTemplates` (after its closing `}`, before `// Close closes the connection to the backend.`):

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

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/backend/... -run TestListPrompts -v`
Expected: PASS

- [ ] **Step 5: Run the package suite**

Run: `go test ./internal/backend/...`
Expected: all PASS, pristine output

- [ ] **Step 6: Commit**

```bash
git add internal/backend/backend.go internal/backend/backend_test.go
git commit -m "feat: add Backend.ListPrompts"
```

---

### Task 2: `internal/config`: add `prompt_overrides`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.PromptOverrides map[string]string` (yaml `prompt_overrides,omitempty`), validated the same way as `Overrides`/`ResourceOverrides`/`ResourceTemplateOverrides` (every referenced backend name must exist)

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`, right after `TestParse_ResourceOverrideReferencesUnknownBackend` (the function ending around line 360):

```go

func TestParse_PromptOverrides(t *testing.T) {
	data := []byte(`
backends:
  - name: linter
    transport: stdio
    command: ["a"]
  - name: linter-strict
    transport: stdio
    command: ["b"]

prompt_overrides:
  code-review: linter-strict
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.PromptOverrides["code-review"] != "linter-strict" {
		t.Fatalf("PromptOverrides[...] = %q, want %q", cfg.PromptOverrides["code-review"], "linter-strict")
	}
}

func TestParse_PromptOverrideReferencesUnknownBackend(t *testing.T) {
	data := []byte(`
backends:
  - name: known
    transport: stdio
    command: ["a"]

prompt_overrides:
  code-review: nonexistent
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for prompt_overrides referencing unknown backend, got nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/... -run TestParse_PromptOverrides -v`
Expected: FAIL (`cfg.PromptOverrides undefined` — compile error)

- [ ] **Step 3: Add the field and its validation**

In `internal/config/config.go`, add `PromptOverrides` to the `Config` struct, right after `ResourceTemplateOverrides`:

```go
type Config struct {
	Listen                    ListenConfig      `yaml:"listen"`
	Backends                  []BackendConfig   `yaml:"backends"`
	Overrides                 map[string]string `yaml:"overrides,omitempty"`
	ResourceOverrides         map[string]string `yaml:"resource_overrides,omitempty"`
	ResourceTemplateOverrides map[string]string `yaml:"resource_template_overrides,omitempty"`
	PromptOverrides           map[string]string `yaml:"prompt_overrides,omitempty"`
}
```

In `validate`, add a loop right after the `ResourceTemplateOverrides` loop (before the final `return nil`):

```go
	for promptName, backendName := range cfg.PromptOverrides {
		if !names[backendName] {
			return fmt.Errorf("prompt_overrides %q references unknown backend %q", promptName, backendName)
		}
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: all PASS, including the two new tests

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: add prompt_overrides config key"
```

---

### Task 3: `internal/gateway`: prompt relay

**Files:**
- Modify: `internal/gateway/gateway.go`
- Test: `internal/gateway/gateway_test.go`

**Interfaces:**
- Consumes: `backend.Backend.ListPrompts` (Task 1, only indirectly — this task's tests build `router.Entry[*mcp.Prompt]`/`router.Table[*mcp.Prompt]` directly, same as `TestGateway_RoutesByPriorityAndExposesUniqueTools` does for tools); `router.Resolve[T]`, `router.Table[T]`, `router.Resolved[T]`, `router.Candidate[T]` (already exist, generic)
- Produces: `Tables.Prompts *router.Table[*mcp.Prompt]` (new field on the existing `Tables` struct); `New` registers `tables.Prompts.Items` the same way it already loops `tables.Tools.Items`/`tables.Resources.Items`

- [ ] **Step 1: Write the failing tests**

Add to `internal/gateway/gateway_test.go`. First, two small test helpers, right after `newFakeResourceTemplateBackendServer` (around line 255):

```go

func newFakePromptBackendServer(name string, promptNames ...string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, nil)
	for _, promptName := range promptNames {
		srv.AddPrompt(&mcp.Prompt{Name: promptName, Description: "fake prompt " + promptName},
			func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{
					Role:    "user",
					Content: &mcp.TextContent{Text: name + ":" + req.Params.Name},
				}}}, nil
			})
	}
	return srv
}

func promptNameOf(p *mcp.Prompt) string { return p.Name }

func promptRename(p *mcp.Prompt, name string) *mcp.Prompt {
	c := *p
	c.Name = name
	return &c
}
```

Then the test cases, appended at the end of the file:

```go

// TestGateway_PromptGetForwardsArgumentsAndResult checks that prompts/get
// for a registered prompt is forwarded to the owning backend with the
// caller's arguments, and the backend's result is returned unchanged.
func TestGateway_PromptGetForwardsArgumentsAndResult(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	backendServer.AddPrompt(&mcp.Prompt{Name: "greet"},
		func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: "hello " + req.Params.Arguments["name"]},
			}}}, nil
		})
	httpBackend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpBackend.Close()

	ctx := context.Background()
	conn, err := backend.Connect(ctx, config.BackendConfig{Name: "backend", Transport: "http", URL: httpBackend.URL})
	if err != nil {
		t.Fatalf("connect backend: %v", err)
	}
	defer func() { _ = conn.Close() }()
	prompts, err := conn.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("list backend prompts: %v", err)
	}

	table := router.Resolve([]router.Entry[*mcp.Prompt]{{BackendName: "backend", Items: prompts}}, promptNameOf, promptRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"backend": conn}, gateway.Tables{Prompts: table})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet", Arguments: map[string]string{"name": "world"}})
	if err != nil {
		t.Fatalf("GetPrompt(greet): %v", err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("GetPrompt(greet) messages = %+v, want 1", res.Messages)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "hello world" {
		t.Fatalf("GetPrompt(greet) content = %+v, want text \"hello world\"", res.Messages[0].Content)
	}
}

// TestGateway_PromptRoutesByPriorityAndExposesUniquePrompts checks
// conflict resolution and prompts/list for two backends that share one
// prompt name and each also expose a unique one.
func TestGateway_PromptRoutesByPriorityAndExposesUniquePrompts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendAServer := newFakePromptBackendServer("backend-a", "review", "unique_a")
	backendBServer := newFakePromptBackendServer("backend-b", "review", "unique_b")

	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendAServer }, nil))
	defer httpA.Close()
	httpB := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendBServer }, nil))
	defer httpB.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()
	connB, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-b", Transport: "http", URL: httpB.URL})
	if err != nil {
		t.Fatalf("connect backend-b: %v", err)
	}
	defer func() { _ = connB.Close() }()

	promptsA, err := connA.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("list backend-a prompts: %v", err)
	}
	promptsB, err := connB.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("list backend-b prompts: %v", err)
	}

	table := router.Resolve([]router.Entry[*mcp.Prompt]{
		{BackendName: "backend-a", Items: promptsA},
		{BackendName: "backend-b", Items: promptsB},
	}, promptNameOf, promptRename, nil)

	if len(table.Conflicts) != 1 || table.Conflicts[0].ExposedName != "review" || table.Conflicts[0].Winner != "backend-a" {
		t.Fatalf("unexpected conflicts: %+v", table.Conflicts)
	}

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA, "backend-b": connB}, gateway.Tables{Prompts: table})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var names []string
	for prompt, err := range session.Prompts(ctx, nil) {
		if err != nil {
			t.Fatalf("list gateway prompts: %v", err)
		}
		names = append(names, prompt.Name)
	}
	sort.Strings(names)
	want := []string{"review", "unique_a", "unique_b"}
	if !slices.Equal(names, want) {
		t.Fatalf("prompts/list = %v, want %v", names, want)
	}

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "review"})
	if err != nil {
		t.Fatalf("GetPrompt(review): %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "backend-a:review" {
		t.Fatalf("GetPrompt(review) content = %+v, want text \"backend-a:review\" (the conflict winner)", res.Messages[0].Content)
	}

	res, err = session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "unique_b"})
	if err != nil {
		t.Fatalf("GetPrompt(unique_b): %v", err)
	}
	text, ok = res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "backend-b:unique_b" {
		t.Fatalf("GetPrompt(unique_b) content = %+v, want text \"backend-b:unique_b\"", res.Messages[0].Content)
	}
}

// TestGateway_PromptGetOnDeadBackendReturnsError checks that a prompts/get
// call to a backend that is no longer reachable surfaces the error to the
// client, the same way TestGateway_CallOnDeadBackendReturnsError already
// checks for tools/call.
func TestGateway_PromptGetOnDeadBackendReturnsError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := newFakePromptBackendServer("backend-dead", "boom")
	httpBackend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpBackend.Close()

	ctx := context.Background()
	conn, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-dead", Transport: "http", URL: httpBackend.URL})
	if err != nil {
		t.Fatalf("connect backend-dead: %v", err)
	}
	prompts, err := conn.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("list backend-dead prompts: %v", err)
	}

	table := router.Resolve([]router.Entry[*mcp.Prompt]{{BackendName: "backend-dead", Items: prompts}}, promptNameOf, promptRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-dead": conn}, gateway.Tables{Prompts: table})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	if err := conn.Close(); err != nil {
		t.Fatalf("close backend-dead: %v", err)
	}

	_, err = session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "boom"})
	if err == nil {
		t.Fatal("GetPrompt(boom) on dead backend: got no error, want one")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gateway/... -run TestGateway_Prompt -v`
Expected: FAIL to compile (`gateway.Tables{Prompts: table}` — `Prompts` field does not exist; `mcp.Prompt`/`AddPrompt`/`GetPromptParams` etc. already exist in the vendored SDK, so only the gateway-side plumbing is missing)

- [ ] **Step 3: Implement the prompt relay**

In `internal/gateway/gateway.go`, add `Prompts` to the `Tables` struct:

```go
type Tables struct {
	Tools             *router.Table[*mcp.Tool]
	Resources         *router.Table[*mcp.Resource]
	ResourceTemplates *router.Table[*mcp.ResourceTemplate]
	Prompts           *router.Table[*mcp.Prompt]
}
```

In `New`, add a loop registering prompts, right after the `ResourceTemplates` block:

```go
	if tables.Prompts != nil {
		for _, resolved := range tables.Prompts.Items {
			registerPrompt(srv, logger, backends, resolved)
		}
	}
```

Add `registerPrompt` and `promptGetHandler` — place them after `addResourceTemplate` and before `callHandler`, so the tool/resource/resourceTemplate/prompt sections stay in registration order:

```go

// registerPrompt registers resolved.Item on srv. Unlike registerTool and
// registerResource, there is no panic-recovery/fallback loop here:
// mcp.Server.AddPrompt performs no schema validation and cannot panic (a
// Prompt has no JSON-Schema-bearing field for it to reject), so the winner
// is always registerable.
func registerPrompt(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Prompt]) {
	b := backends[resolved.BackendName]
	srv.AddPrompt(resolved.Item, promptGetHandler(logger, b, resolved.OriginalName))
}

// promptGetHandler forwards prompts/get to originalName on backend b,
// passing the caller's arguments through unchanged.
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

Update the doc comment on `New` (currently "New builds an mcp.Server that exposes tables' resolved tools/resources...") to mention prompts too:

```go
// New builds an mcp.Server that exposes tables' resolved
// tools/resources/prompts, forwarding each call to the backend that owns
// it. backends must contain an entry for every BackendName referenced in
// tables (the caller builds both from the same set of connected backends).
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gateway/... -v`
Expected: all PASS, including the four new/modified tests, pristine output

- [ ] **Step 5: Run `task lint`**

Run: `task lint`
Expected: no diff/errors (gofmt/vet/golangci-lint clean)

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go
git commit -m "feat: add prompt relay to the gateway"
```

---

### Task 4: `internal/cli/server.go`: wire prompts into startup

**Files:**
- Modify: `internal/cli/server.go`
- Modify: `internal/cli/server_internal_test.go`
- Modify: `internal/cli/server_test.go`

**Interfaces:**
- Consumes: `backend.Backend.ListPrompts` (Task 1), `config.Config.PromptOverrides` (Task 2), `gateway.Tables.Prompts` (Task 3)
- Produces: `connected.promptEntries []router.Entry[*mcp.Prompt]` (new field on the existing `connected` struct); `promptNameOf`/`promptRename` helpers in `internal/cli/server.go` (package-level, alongside the existing `toolNameOf`/`resourceNameOf`/etc.)

- [ ] **Step 1: Write the failing e2e tests**

Add to `internal/cli/server_test.go`, right after `TestServerCommand_PrefixNotAppliedToResources` (before `TestServerCommand_StdioShutdownIsClean`):

```go

func TestServerCommand_ServesAggregatedPrompts(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	backendSrv.AddPrompt(&mcp.Prompt{Name: "greet", Description: "say hello"},
		func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: "hello " + req.Params.Arguments["name"]},
			}}}, nil
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

	var names []string
	for prompt, err := range session.Prompts(ctx, nil) {
		if err != nil {
			t.Fatalf("listing prompts: %v", err)
		}
		names = append(names, prompt.Name)
	}
	if len(names) != 1 || names[0] != "greet" {
		t.Fatalf("prompts/list = %v, want [greet]", names)
	}

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet", Arguments: map[string]string{"name": "world"}})
	if err != nil {
		t.Fatalf("GetPrompt(greet): %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "hello world" {
		t.Fatalf("GetPrompt(greet) content = %+v, want text \"hello world\"", res.Messages[0].Content)
	}
	_ = session.Close()

	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}

// TestServerCommand_PrefixAppliedToPrompts checks the mirror-image
// invariant of TestServerCommand_PrefixNotAppliedToResources: a backend's
// `prefix` DOES apply to prompt names, exactly like tool names, because
// prompt names live in the same kind of flat namespace tool names do
// (unlike resource URIs).
func TestServerCommand_PrefixAppliedToPrompts(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	backendSrv.AddPrompt(&mcp.Prompt{Name: "greet"},
		func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: "hello"},
			}}}, nil
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
    prefix: "gh__"
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

	var names []string
	for prompt, err := range session.Prompts(ctx, nil) {
		if err != nil {
			t.Fatalf("listing prompts: %v", err)
		}
		names = append(names, prompt.Name)
	}
	if len(names) != 1 || names[0] != "gh__greet" {
		t.Fatalf("prompts/list = %v, want [gh__greet] (prefix must be applied to prompt names)", names)
	}

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "gh__greet"})
	if err != nil {
		t.Fatalf("GetPrompt(gh__greet): %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "hello" {
		t.Fatalf("GetPrompt(gh__greet) content = %+v, want text \"hello\"", res.Messages[0].Content)
	}
	_ = session.Close()

	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}
```

Add to `internal/cli/server_internal_test.go`, right after `TestConnectBackends_ResourceListFailureKeepsBackendTools` (reusing the existing `denyMethodHandler` helper defined earlier in the same file):

```go

// TestConnectBackends_PromptListFailureKeepsBackendTools is
// TestConnectBackends_ResourceListFailureKeepsBackendTools's counterpart for
// prompts/list: a backend whose prompts/list errors out must still be
// connected, with its tools intact, instead of being dropped entirely.
func TestConnectBackends_PromptListFailureKeepsBackendTools(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})

	realHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil)
	backendHTTP := httptest.NewServer(denyMethodHandler(realHandler, "prompts/list"))
	defer backendHTTP.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	configs := []config.BackendConfig{
		{Name: "fake", Transport: "http", URL: backendHTTP.URL},
	}

	conn := connectBackends(context.Background(), logger, configs)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()

	if _, ok := conn.backends["fake"]; !ok {
		t.Fatalf("backends = %v, want backend \"fake\" to still be connected despite its prompts/list failing", conn.backends)
	}
	if len(conn.toolEntries) != 1 || len(conn.toolEntries[0].Items) != 1 || conn.toolEntries[0].Items[0].Name != "ping" {
		t.Fatalf("toolEntries = %+v, want one entry with tool \"ping\"", conn.toolEntries)
	}
	if len(conn.promptEntries) != 1 || len(conn.promptEntries[0].Items) != 0 {
		t.Fatalf("promptEntries = %+v, want one entry with no items (a prompts/list failure is treated as an empty list, not dropped)", conn.promptEntries)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/... -run 'TestServerCommand_ServesAggregatedPrompts|TestServerCommand_PrefixAppliedToPrompts|TestConnectBackends_PromptListFailureKeepsBackendTools' -v`
Expected: FAIL to compile (`conn.promptEntries undefined`)

- [ ] **Step 3: Wire prompts into `connectBackends` and `runServer`**

In `internal/cli/server.go`, update the timeout doc comment to mention prompts:

```go
// backendConnectTimeout bounds how long connectBackends waits on any single
// backend's Connect plus its
// ListTools/ListResources/ListResourceTemplates/ListPrompts calls, so one
// hung backend can't stall the whole gateway's startup (see
// connectBackends). A var so tests can shrink it.
var backendConnectTimeout = 30 * time.Second
```

In `runServer`, add a prompt table resolve-and-log block right after the `resourceTemplateTable` block, and add `Prompts: promptTable` to the `gateway.Tables{...}` literal:

```go
	resourceTemplateTable := router.Resolve(conn.resourceTemplateEntries, resourceTemplateNameOf, resourceTemplateRename, cfg.ResourceTemplateOverrides)
	for _, c := range resourceTemplateTable.Conflicts {
		logger.Warn("resource template URI conflict", "uriTemplate", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	promptTable := router.Resolve(conn.promptEntries, promptNameOf, promptRename, cfg.PromptOverrides)
	for _, c := range promptTable.Conflicts {
		logger.Warn("prompt name conflict", "prompt", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	srv := gateway.New(logger, conn.backends, gateway.Tables{
		Tools:             toolTable,
		Resources:         resourceTable,
		ResourceTemplates: resourceTemplateTable,
		Prompts:           promptTable,
	})
```

Add `promptEntries` to the `connected` struct:

```go
type connected struct {
	backends                map[string]*backend.Backend
	toolEntries             []router.Entry[*mcp.Tool]
	resourceEntries         []router.Entry[*mcp.Resource]
	resourceTemplateEntries []router.Entry[*mcp.ResourceTemplate]
	promptEntries           []router.Entry[*mcp.Prompt]
}
```

Update `connectBackends`'s doc comment and body. The full function becomes:

```go
// connectBackends connects to every configured backend concurrently and
// lists its tools, resources, resource templates, and prompts. A backend
// that fails to connect, fails to list tools, or exceeds
// backendConnectTimeout is logged and excluded entirely (best-effort); it
// does not fail or stall the whole startup. A backend that fails to list
// resources, resource templates, or prompts is kept with its tools intact
// and treated as having none of that kind: many non-Go-SDK MCP servers
// answer resources/list, resources/templates/list, or prompts/list with a
// JSON-RPC "method not found" error when they don't implement that
// capability at all, rather than an empty list, and that must not take down
// an otherwise-working tools-only backend.
func connectBackends(ctx context.Context, logger *slog.Logger, configs []config.BackendConfig) connected {
	type outcome struct {
		backend               *backend.Backend
		toolEntry             router.Entry[*mcp.Tool]
		resourceEntry         router.Entry[*mcp.Resource]
		resourceTemplateEntry router.Entry[*mcp.ResourceTemplate]
		promptEntry           router.Entry[*mcp.Prompt]
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
				logger.Warn("backend lists no resources", "backend", bc.Name, "error", err)
				resources = nil
			}
			resourceTemplates, err := b.ListResourceTemplates(ctx)
			if err != nil {
				logger.Warn("backend lists no resource templates", "backend", bc.Name, "error", err)
				resourceTemplates = nil
			}
			prompts, err := b.ListPrompts(ctx)
			if err != nil {
				logger.Warn("backend lists no prompts", "backend", bc.Name, "error", err)
				prompts = nil
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
				// prompt entries DO carry a prefix, like tools: prompt
				// names are a flat namespace, not a URI.
				promptEntry: router.Entry[*mcp.Prompt]{
					BackendName: bc.Name, Prefix: bc.Prefix, Items: prompts,
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
		result.promptEntries = append(result.promptEntries, o.promptEntry)
	}
	return result
}
```

Add `promptNameOf`/`promptRename` at the end of the file, after `resourceTemplateRename`:

```go

func promptNameOf(p *mcp.Prompt) string { return p.Name }

func promptRename(p *mcp.Prompt, name string) *mcp.Prompt {
	c := *p
	c.Name = name
	return &c
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./...`
Expected: PASS, including all new tests, no compile errors in `internal/cli/list.go`/`internal/cli/call.go` (they call `connectBackends` too, but only read `conn.backends`/`conn.toolEntries`, both still present and unchanged in shape — adding a field to `connected` does not break them)

- [ ] **Step 5: Run `task lint`**

Run: `task lint`
Expected: no diff/errors

- [ ] **Step 6: Commit**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go internal/cli/server_test.go
git commit -m "feat: wire prompt relay into the server command"
```

---

### Task 5: `internal/cli/export.go` and `internal/cli/init.go`: `prompt_overrides` parity

**Files:**
- Modify: `internal/cli/export.go`
- Modify: `internal/cli/init.go`
- Test: `internal/cli/export_test.go`
- Test: `internal/cli/init_test.go`

**Interfaces:**
- Consumes: `config.Config.PromptOverrides` (Task 2)

- [ ] **Step 1: Write the failing tests**

Add to `internal/cli/export_test.go`, right after `TestExportCommand_ResourceOverridesWarnedAndDropped`:

```go

func TestExportCommand_PromptOverridesWarnedAndDropped(t *testing.T) {
	configPath := writeExportConfig(t, `
backends:
  - name: srv
    transport: http
    url: https://example.com/mcp
prompt_overrides:
  code-review: srv
`)
	outPath := filepath.Join(t.TempDir(), "mcp.json")

	var errOut bytes.Buffer
	root := cli.NewRootCmd()
	root.SetErr(&errOut)
	root.SetArgs([]string{"export", "--config", configPath, outPath})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if !strings.Contains(errOut.String(), "prompt_overrides") {
		t.Fatalf("stderr = %q, want a warning about prompt_overrides being dropped", errOut.String())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated mcp.json: %v", err)
	}
	if strings.Contains(string(data), "prompt_overrides") {
		t.Fatalf("generated mcp.json = %s, must not contain a prompt_overrides field", data)
	}
}
```

Update `TestInitCommand_WritesParsableConfig` in `internal/cli/init_test.go` to also assert the new key is present, adding this check after the existing `ResourceTemplateOverrides` check:

```go
	if len(cfg.PromptOverrides) == 0 {
		t.Fatalf("generated config has no prompt_overrides example:\n%s", data)
	}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/... -run 'TestExportCommand_PromptOverridesWarnedAndDropped|TestInitCommand_WritesParsableConfig' -v`
Expected: FAIL — export test fails because no warning is emitted; init test fails because `cfg.PromptOverrides` is empty

- [ ] **Step 3: Add the warning and the example key**

In `internal/cli/export.go`, add a warning right after the existing `ResourceTemplateOverrides` warning:

```go
	if len(src.ResourceTemplateOverrides) > 0 {
		warn("resource_template_overrides: dropped (mcp.json has no equivalent for resolving conflicting URI templates)")
	}
	if len(src.PromptOverrides) > 0 {
		warn("prompt_overrides: dropped (mcp.json has no equivalent for resolving conflicting prompt names)")
	}
```

In `internal/cli/init.go`, add a `PromptOverrides` entry to `exampleConfig()`, right after `ResourceTemplateOverrides`:

```go
		ResourceTemplateOverrides: map[string]string{
			"file:///example/{path}": "example-stdio",
		},
		PromptOverrides: map[string]string{
			"example_prompt": "example-stdio",
		},
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/... -v`
Expected: all PASS, pristine output

- [ ] **Step 5: Run `task lint`**

Run: `task lint`
Expected: no diff/errors

- [ ] **Step 6: Commit**

```bash
git add internal/cli/export.go internal/cli/init.go internal/cli/export_test.go internal/cli/init_test.go
git commit -m "feat: warn on dropped prompt_overrides in export, add to init template"
```

---

### Task 6: Documentation and final verification

**Files:**
- Modify: `README.md`
- Modify: `config-example.yaml`

**Interfaces:**
- Consumes: none (documentation only)

- [ ] **Step 1: Update `README.md`**

Replace the intro paragraph:

```markdown
mcprt aggregates multiple MCP servers (local stdio subprocesses and remote
HTTP servers) behind a single MCP gateway endpoint, relaying `tools/*` and
`resources/*` calls to whichever backend serves them.
```

with:

```markdown
mcprt aggregates multiple MCP servers (local stdio subprocesses and remote
HTTP servers) behind a single MCP gateway endpoint, relaying `tools/*`,
`resources/*`, and `prompts/*` calls to whichever backend serves them.
```

In the config example block, add a `prompt_overrides` entry right after the existing `resource_template_overrides` entry:

```markdown
    resource_template_overrides:
      "file:///data/{path}": filesystem

    prompt_overrides:
      code-review: filesystem
```

Extend the explanatory paragraph right after that block (currently ending "`resources/subscribe` and `notifications/resources/updated` are not relayed.") by appending:

```markdown
`prompt_overrides` resolves conflicting **prompt** names, the same way
`overrides` resolves tool names — including `prefix` being applied to
prompt names before conflict resolution, exactly like tool names (unlike
resource/resource-template URIs, which never get a prefix).
`notifications/prompts/list_changed` and `completion/complete` are not
relayed.
```

- [ ] **Step 2: Update `config-example.yaml`**

Append to the end of the file:

```yaml

prompt_overrides:
  example_prompt: example
```

- [ ] **Step 3: Final verification**

Run: `task build && task test && task lint`
Expected: all succeed (build succeeds, all tests PASS, no lint errors)

- [ ] **Step 4: Commit**

```bash
git add README.md config-example.yaml
git commit -m "docs: document prompt_overrides"
```

---

## 完了条件

- `go test ./...` が外部サービス依存なしで全て PASS する
- `task lint`（`gofmt -l .` / `go vet ./...` / `golangci-lint run ./...`）がエラーなしで完了する
- `prompts/list` / `prompts/get` が gateway 経由で backend へ正しく中継される（`internal/gateway`・`internal/cli` の e2e テストで確認済み）
- prompt 名の衝突が記載順＋`prompt_overrides` で解決される（`internal/gateway` のテーブル駆動テストで確認済み）
- backend 単位の `prefix` が prompt 名にも適用される（tool と同じ挙動、`internal/cli` の e2e テストで確認済み）
- `ListPrompts` の失敗は該当 backend の tools/resources を道連れにしない（`internal/cli` の回帰テストで確認済み）
