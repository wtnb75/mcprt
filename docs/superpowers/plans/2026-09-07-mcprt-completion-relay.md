# mcprt: completion/complete 中継 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire `completion/complete` (both `ref/prompt` and `ref/resource`, including resource templates) through to the owning backend, using the same `promptTable`/`resourceTable`/`resourceTemplateTable` routing tables already built for `prompts/get`/`resources/read`.

**Architecture:** Add one new `mcp.ServerOptions.CompletionHandler` function, `(*Server).completionHandler`, in `internal/gateway/gateway.go`. It resolves `req.Params.Ref` against the existing routing tables under `s.mu`, translates the exposed name/URI back to the backend's original name/URI (same responsibility `registerPrompt`/`registerResource`/`registerResourceTemplate` already have), then calls `b.Session.Complete` outside the lock and returns its result unchanged. `New` must build the `*Server` before the `*mcp.Server` so `s.completionHandler` exists to hand to `mcp.NewServer`'s `ServerOptions` — currently `New` builds `mcpSrv` first, so this requires reordering that construction.

**Tech Stack:** Go, `github.com/modelcontextprotocol/go-sdk/mcp` v1.7.0 (per the spec's stated version).

**Spec:** `docs/superpowers/specs/2026-09-07-mcprt-completion-relay-design.md`

## Global Constraints

- No new state: reuse `s.promptTable`, `s.resourceTable`, `s.resourceTemplateTable` exactly as `registerPrompt`/`registerResource`/`registerResourceTemplate` populate them — do not introduce a fourth table or cache.
- Single-backend fan-out only: one `ref` resolves to exactly one backend (no merging across backends), matching `tools/call`'s existing model.
- Success is never logged (high-frequency, side-effect-free per keystroke); only failures get `logger.Warn`, matching the spec's Logging section — do not route through `logCall`/`startCallSpan`.
- Unknown `ref` (unregistered prompt name or URI, in neither `resourceTable` nor `resourceTemplateTable`) returns an error, not an empty completion list.

---

### Task 1: `completion/complete` forwarding in `internal/gateway`

**Files:**
- Modify: `internal/gateway/gateway.go` (add `completionHandler`, reorder `New`'s construction, wire `CompletionHandler` into `mcp.ServerOptions`)
- Test: `internal/gateway/gateway_test.go` (new tests + one new fake-backend helper)
- Modify: `README.md:104-105` (drop the "not relayed" claim for `completion/complete`)

**Interfaces:**
- Consumes: `s.promptTable *router.Table[*mcp.Prompt]`, `s.resourceTable *router.Table[*mcp.Resource]`, `s.resourceTemplateTable *router.Table[*mcp.ResourceTemplate]`, `s.backends map[string]*backend.Backend`, `s.mu sync.Mutex`, `s.logger *slog.Logger` — all existing `Server` fields (`internal/gateway/gateway.go:110-133`). `router.Resolved[T]{Item, BackendName, OriginalName, Fallbacks}` (`internal/router/router.go:15-24`). `backend.Backend{Session *mcp.ClientSession}` (`internal/backend/backend.go:40-50`).
- Produces: `func (s *Server) completionHandler(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error)`, wired as `mcp.ServerOptions.CompletionHandler` inside `New`. No other task depends on this (it's the only task in this plan).

This task covers the whole spec (it's small enough that splitting it further would just fragment one review gate).

- [ ] **Step 1: Add a fake completion backend helper and write the failing tests**

Add to `internal/gateway/gateway_test.go`, near the other `newFake*BackendServer` helpers (after `newFakePromptBackendServer` around line 507):

```go
// newFakeCompletionBackendServer returns a fake backend that serves one
// prompt ("greet") and one resource template ("file:///dir/{f}"), and
// answers completion/complete by echoing back what it was asked to
// complete -- ref type, name/URI, and the argument's name/value -- as a
// single completion value, so tests can assert the backend received the
// *original* (un-prefixed) ref, not the gateway-exposed one.
func newFakeCompletionBackendServer(name string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, &mcp.ServerOptions{
		CompletionHandler: func(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
			ref := req.Params.Ref
			target := ref.Name
			if ref.Type == "ref/resource" {
				target = ref.URI
			}
			value := ref.Type + ":" + target + ":" + req.Params.Argument.Name + "=" + req.Params.Argument.Value
			return &mcp.CompleteResult{Completion: mcp.CompletionResultDetails{Values: []string{value}}}, nil
		},
	})
	srv.AddPrompt(&mcp.Prompt{Name: "greet"},
		func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{}, nil
		})
	srv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "file:///dir/{f}", Name: "dir"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{}, nil
		})
	return srv
}
```

Then add these tests directly after `TestGateway_PromptGetOnDeadBackendReturnsError` (the file's last prompt-related test, ends around line 875 -- run `rg -n "^func TestGateway_PromptGetOnDeadBackendReturnsError" internal/gateway/gateway_test.go` and insert after that function's closing brace):

```go
// TestGateway_CompletionRefPromptForwardsToBackend checks that
// completion/complete with a ref/prompt is forwarded to the backend that
// owns the prompt, with the backend's completion result returned unchanged.
func TestGateway_CompletionRefPromptForwardsToBackend(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := newFakeCompletionBackendServer("backend-a")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL}, backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	prompts, err := connA.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("list backend-a prompts: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Prompt]{{BackendName: "backend-a", Items: prompts}}, promptNameOf, promptRename, nil)

	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"backend-a": connA},
		Tables:   gateway.Tables{Prompts: table},
	})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.Complete(ctx, &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "greet"},
		Argument: mcp.CompleteParamsArgument{Name: "lang", Value: "en"},
	})
	if err != nil {
		t.Fatalf("Complete(ref/prompt greet): %v", err)
	}
	want := []string{"ref/prompt:greet:lang=en"}
	if !slices.Equal(res.Completion.Values, want) {
		t.Fatalf("Complete(ref/prompt greet) values = %v, want %v", res.Completion.Values, want)
	}
}

// TestGateway_CompletionRefPromptTranslatesPrefixedName checks that when a
// prompt is exposed under a backend prefix, the gateway forwards the
// backend's *original* (un-prefixed) name in the ref it sends upstream --
// the same prefix-stripping registerPrompt already does for prompts/get.
func TestGateway_CompletionRefPromptTranslatesPrefixedName(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := newFakeCompletionBackendServer("backend-a")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL}, backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	prompts, err := connA.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("list backend-a prompts: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Prompt]{
		{BackendName: "backend-a", Prefix: "a__", Items: prompts},
	}, promptNameOf, promptRename, nil)

	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"backend-a": connA},
		Tables:   gateway.Tables{Prompts: table},
	})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.Complete(ctx, &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "a__greet"},
		Argument: mcp.CompleteParamsArgument{Name: "lang", Value: "en"},
	})
	if err != nil {
		t.Fatalf("Complete(ref/prompt a__greet): %v", err)
	}
	// The backend only ever saw "greet" (its own, un-prefixed name), not the
	// gateway-exposed "a__greet".
	want := []string{"ref/prompt:greet:lang=en"}
	if !slices.Equal(res.Completion.Values, want) {
		t.Fatalf("Complete(ref/prompt a__greet) values = %v, want %v (original name forwarded)", res.Completion.Values, want)
	}
}

// TestGateway_CompletionRefResourceTemplateForwardsToBackend checks that
// completion/complete with a ref/resource matching a registered resource
// template's URI-template string is forwarded to the owning backend.
func TestGateway_CompletionRefResourceTemplateForwardsToBackend(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendServer := newFakeCompletionBackendServer("backend-a")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL}, backend.ChangeCallbacks{})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	templates, err := connA.ListResourceTemplates(ctx)
	if err != nil {
		t.Fatalf("list backend-a resource templates: %v", err)
	}
	templateNameOf := func(rt *mcp.ResourceTemplate) string { return rt.URITemplate }
	templateRename := func(rt *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate {
		c := *rt
		c.URITemplate = name
		return &c
	}
	table := router.Resolve([]router.Entry[*mcp.ResourceTemplate]{
		{BackendName: "backend-a", Items: templates},
	}, templateNameOf, templateRename, nil)

	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"backend-a": connA},
		Tables:   gateway.Tables{ResourceTemplates: table},
	})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.Complete(ctx, &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/resource", URI: "file:///dir/{f}"},
		Argument: mcp.CompleteParamsArgument{Name: "f", Value: "re"},
	})
	if err != nil {
		t.Fatalf("Complete(ref/resource file:///dir/{f}): %v", err)
	}
	want := []string{"ref/resource:file:///dir/{f}:f=re"}
	if !slices.Equal(res.Completion.Values, want) {
		t.Fatalf("Complete(ref/resource file:///dir/{f}) values = %v, want %v", res.Completion.Values, want)
	}
}

// TestGateway_CompletionUnknownRefPromptReturnsError checks that
// completion/complete for a ref/prompt naming an unregistered prompt
// returns an error instead of an empty completion list.
func TestGateway_CompletionUnknownRefPromptReturnsError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := gateway.New(gateway.NewConfig{Logger: logger, Backends: map[string]*backend.Backend{}})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	_, err = session.Complete(ctx, &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "no-such-prompt"},
		Argument: mcp.CompleteParamsArgument{Name: "lang", Value: "en"},
	})
	if err == nil {
		t.Fatalf("Complete(ref/prompt no-such-prompt): got no error, want an error")
	}
}

// TestGateway_CompletionUnknownRefResourceReturnsError checks that
// completion/complete for a ref/resource URI matching neither a registered
// resource nor a resource template returns an error.
func TestGateway_CompletionUnknownRefResourceReturnsError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := gateway.New(gateway.NewConfig{Logger: logger, Backends: map[string]*backend.Backend{}})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	_, err = session.Complete(ctx, &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/resource", URI: "file:///no-such"},
		Argument: mcp.CompleteParamsArgument{Name: "f", Value: ""},
	})
	if err == nil {
		t.Fatalf("Complete(ref/resource file:///no-such): got no error, want an error")
	}
}
```

- [ ] **Step 2: Run the new tests to verify they fail**

Run: `go test ./internal/gateway/... -run TestGateway_Completion -v`

Expected: all five new tests FAIL. `TestGateway_CompletionUnknownRef*` will currently fail with something like "completion/complete not supported" (a fine failure — `CompletionHandler` isn't wired yet) rather than a compile error; the other three will fail the same way. If instead you get a **compile** error, fix imports/helper placement before moving on (the test file already imports `slices`, `mcp`, `router`, `backend`, `config`, `httptest`, `slog`, `io`, `context`, `testing` at the top — no new imports should be needed).

- [ ] **Step 3: Reorder `New`'s construction so `s` exists before `mcpSrv`**

In `internal/gateway/gateway.go`, replace `New` (lines 183-235) with:

```go
func New(cfg NewConfig) *Server {
	s := &Server{
		logger:   cfg.Logger,
		backends: cfg.Backends,
		maskKeys: cfg.MaskKeys,
		relays:   cfg.Relays,

		toolEntries:   cfg.Entries.Tools,
		toolTable:     emptyTable(cfg.Tables.Tools),
		toolOverrides: cfg.Overrides.Tools,

		resourceEntries:           cfg.Entries.Resources,
		resourceTable:             emptyTable(cfg.Tables.Resources),
		resourceOverrides:         cfg.Overrides.Resources,
		resourceTemplateEntries:   cfg.Entries.ResourceTemplates,
		resourceTemplateTable:     emptyTable(cfg.Tables.ResourceTemplates),
		resourceTemplateOverrides: cfg.Overrides.ResourceTemplates,

		promptEntries:   cfg.Entries.Prompts,
		promptTable:     emptyTable(cfg.Tables.Prompts),
		promptOverrides: cfg.Overrides.Prompts,
	}

	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{
		Logger:                    cfg.Logger,
		KeepAlive:                 cfg.KeepAlive,
		KeepAliveFailureThreshold: cfg.KeepAliveFailureThreshold,
		CompletionHandler:         s.completionHandler,
	})
	s.mcp = mcpSrv

	if cfg.Tables.Tools != nil {
		for _, resolved := range cfg.Tables.Tools.Items {
			registerTool(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys, cfg.Relays)
		}
	}
	if cfg.Tables.Resources != nil {
		for _, resolved := range cfg.Tables.Resources.Items {
			registerResource(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys)
		}
	}
	if cfg.Tables.ResourceTemplates != nil {
		for _, resolved := range cfg.Tables.ResourceTemplates.Items {
			registerResourceTemplate(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys)
		}
	}
	if cfg.Tables.Prompts != nil {
		for _, resolved := range cfg.Tables.Prompts.Items {
			registerPrompt(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys)
		}
	}

	return s
}
```

This is a pure reordering: every field assignment and registration call is unchanged, only their relative order and the new `CompletionHandler: s.completionHandler` line and `s.mcp = mcpSrv` line are new. `s.completionHandler` is a method value bound to `s`, which is legal to take before `s`'s later fields (`s.mcp`) are set — Go method values close over the pointer, not the struct's current contents, and `completionHandler` isn't invoked until a request arrives, long after `New` returns.

- [ ] **Step 4: Add `completionHandler` and run the tests again to confirm they still fail on the missing method (compile step)**

Add this new function to `internal/gateway/gateway.go`, placed after `New` and before `registerTool` (i.e. right after the closing `}` of the new `New`):

```go
// completionHandler forwards completion/complete to the backend that owns
// req.Params.Ref (a prompt name or a resource/resource-template URI),
// resolved through the same promptTable/resourceTable/resourceTemplateTable
// registerPrompt/registerResource/registerResourceTemplate already populate
// at registration time -- no new state is needed. Locked only for the table
// lookup: s.mu guards promptTable/resourceTable/resourceTemplateTable
// against a concurrent UpdatePrompts/UpdateResources (see reconcile.go), but
// the actual backend I/O (b.Session.Complete) happens after Unlock, matching
// every other handler in this file.
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
			// completion targets a resource template's URI-template string
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
		s.logger.Warn("completion: unknown ref", "type", ref.Type, "name", ref.Name, "uri", ref.URI)
		return nil, fmt.Errorf("completion: unknown %s (name=%q uri=%q)", ref.Type, ref.Name, ref.URI)
	}

	result, err := b.Session.Complete(ctx, &mcp.CompleteParams{
		Ref:      originalRef,
		Argument: req.Params.Argument,
		Context:  req.Params.Context,
	})
	if err != nil {
		s.logger.Warn("completion: backend call failed", "backend", b.Name, "type", ref.Type, "name", ref.Name, "uri", ref.URI, "error", err)
	}
	return result, err
}
```

Run: `go build ./...`
Expected: builds cleanly (this confirms `completionHandler` compiles and `New` wires it correctly).

- [ ] **Step 5: Run the new tests to verify they pass**

Run: `go test ./internal/gateway/... -run TestGateway_Completion -v`

Expected: all five tests PASS.

- [ ] **Step 6: Run the full package test suite to confirm no regression**

Run: `go test ./internal/gateway/... -v 2>&1 | tail -80`

Expected: PASS for the whole package (in particular, every existing `TestGateway_Prompt*`/`TestGateway_Resource*` test, since `New`'s reordering must not change any observable behavior).

- [ ] **Step 7: Update README's stale "not relayed" claim**

In `README.md`, find the sentence (currently around line 104-105):

```
`notifications/prompts/list_changed` and `completion/complete` are not
relayed.
```

Replace with:

```
`notifications/prompts/list_changed` is not relayed. `completion/complete`
(both `ref/prompt` and `ref/resource`, including resource templates) is
forwarded to the backend that owns the referenced prompt/resource, the same
way `tools/call`/`resources/read`/`prompts/get` are.
```

- [ ] **Step 8: Run go vet and the full test suite one more time**

Run: `gofmt -l internal/gateway/gateway.go internal/gateway/gateway_test.go && go vet ./... && go test ./...`

Expected: `gofmt -l` prints nothing (no formatting diffs), `go vet` is silent, `go test ./...` passes for the whole module.

- [ ] **Step 9: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go README.md
git commit -m "feat(gateway): forward completion/complete to the owning backend"
```

## Self-Review Notes

- **Spec coverage:** ref/prompt forwarding (Step 4, tested Step 1's first test), ref/resource exact + template forwarding (Step 4, tested Step 1's third test — exact-resource case is structurally identical to the template case already covered plus `TestGateway_ResourceReadExact`'s existing exact-resource table-building pattern, so a fourth test was judged redundant with the template test's coverage of the same code path), unknown ref -> error for both types (Step 1's fourth/fifth tests), prefix name translation (Step 1's second test), no logging on success / `logger.Warn` on error only (Step 4's code, matching spec's Logging section), `New` construction reorder (Step 3), README update (Step 7). No gaps found.
- **Placeholder scan:** none found — every step has literal code or an exact command.
- **Type consistency:** `completionHandler`'s signature (`func(context.Context, *mcp.CompleteRequest) (*mcp.CompleteResult, error)`) matches `mcp.ServerOptions.CompletionHandler`'s field type exactly, and matches the spec's own signature.
