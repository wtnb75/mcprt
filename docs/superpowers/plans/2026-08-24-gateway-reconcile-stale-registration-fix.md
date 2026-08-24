# Gateway Reconcile Stale-Registration Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix a bug found in the final whole-branch review of the `list_changed` feature (`docs/superpowers/plans/2026-08-24-mcprt-list-changed.md`, Minor finding #3): when a backend's `list_changed` reconcile picks a tool-name winner whose definition is unregisterable (invalid `InputSchema`) and there is no valid fallback, the gateway silently keeps serving the *previous* (now-stale) tool registration for that exposed name instead of removing it.

**Architecture:** `registerTool` (`internal/gateway/gateway.go`) already knows, internally, whether it managed to register anything — it just doesn't tell its caller. Change it to return `(ok bool)`. In `internal/gateway/reconcile.go`'s `UpdateTools`, when `registerTool` for a new-or-changed exposed name returns `false`, explicitly call `s.mcp.RemoveTools(name)` — cleaning up any stale prior registration. `RemoveTools` is confirmed safe to call unconditionally on failure (verified against `go-sdk@v1.7.0`'s `featureSet.remove`, which returns `false`/skips notifying when the name was never registered), so no "was it previously registered" check is needed first.

**Tech Stack:** Go, `github.com/modelcontextprotocol/go-sdk` v1.7.0 (already pinned; no version change).

**Spec:** This plan's "spec" is the finding itself, from the final whole-branch review of `docs/superpowers/plans/2026-08-24-mcprt-list-changed.md`: *"`registerTool` total failure can leave a stale registration on reconcile"* — at startup, every candidate failing means "never registered" (correct); reached via `UpdateTools` after a successful prior registration, it means the *previous* registration stays live, so the gateway keeps advertising a tool definition the new resolution just rejected.

**Out of scope — and why:** The original finding named `registerTool` specifically. Investigating whether the same bug reaches `registerResource`/`registerResourceTemplate` (via `UpdateResources`) shows it structurally cannot: `router.Resolve`'s `rename` step forces every resource/resource-template candidate's `URI`/`URITemplate` field to equal the exposed name itself (`resourceRename`/`resourceTemplateRename` in `internal/gateway/reconcile.go` set `c.URI = name` / `c.URITemplate = name`, where `name` is the exposed name — the map key in `router.Table.Items`). `AddResource`/`AddResourceTemplate`'s panic condition depends only on that URI/URITemplate string. So for a given exposed name, EVERY candidate that could ever be tried (winner or any fallback) carries that same, invariant URI value across every reconcile — if it was a validly-absolute URI before, it stays exactly that same valid URI after, no matter what the backend's raw item content changes to. A resource/resource-template can only ever be "unregisterable" if its exposed name itself was never a valid absolute URI/template — and in that case it was never successfully registered in the first place (at startup or any earlier reconcile), so there is no prior valid registration to go stale. Tools don't have this invariant: `InputSchema` validity is independent of the tool's `Name` field, so the exact same exposed name can flip from a valid to an invalid definition across a reconcile — which is exactly the scenario this plan fixes. `UpdatePrompts`/`registerPrompt` are also out of scope: `registerPrompt`'s own doc comment states it cannot fail (`mcp.Prompt` has no JSON-Schema-bearing field for `AddPrompt` to reject). No SDK version change. No change to `New`'s startup-time behavior.

## Global Constraints

- No new dependencies.
- Comments and log messages in English.
- Verify with `task test` (`go test -cover ./...`) and `task lint` (`gofmt -l .` + `go vet ./...` + `golangci-lint run ./...`), both defined in the repo's `Taskfile.yml`.
- `RemoveTools` is safe to call on a name that was never registered (verified against `go-sdk@v1.7.0/mcp/features.go`'s `featureSet.remove`: it returns `false` and `changeAndNotify` skips notifying downstream when nothing was actually removed) — no defensive "was it previously registered" check is needed before calling it.

---

### Task 1: Return registration success from `registerTool`, and clean up on reconcile failure

**Files:**
- Modify: `internal/gateway/gateway.go:161-175` (`registerTool`)
- Modify: `internal/gateway/reconcile.go` (`UpdateTools`)
- Test: `internal/gateway/reconcile_test.go`

**Interfaces:**
- Consumes: existing `addTool` (`internal/gateway/gateway.go`), which already returns `(ok bool)` — unchanged by this task.
- Produces: `registerTool(...) (ok bool)` — gains a return value; its one existing call site in `New` (`internal/gateway/gateway.go`) keeps compiling unchanged since Go allows discarding a function's return value.

- [ ] **Step 1: Write the failing test**

Append to `internal/gateway/reconcile_test.go`, right after `TestUpdateTools_ConflictFallbackPromotesWhenWinnerRemoved` and before `TestUpdateTools_LogsOnlyNewConflicts`:

```go
// TestUpdateTools_RemovesStaleRegistrationWhenNewWinnerIsInvalid is the
// regression test for the bug found in this feature's final whole-branch
// review: when a backend's list_changed reconcile picks a winner whose
// definition registerTool can't register (invalid schema) and there is no
// valid fallback, the tool must be REMOVED from the gateway, not left
// serving its previous (now-stale) definition.
func TestUpdateTools_RemovesStaleRegistrationWhenNewWinnerIsInvalid(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "search", Description: "v1", InputSchema: toolSchema}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"a": {Name: "a"}},
		gateway.Tables{Tools: table}, gateway.Entries{Tools: entries}, gateway.Overrides{}, nil)

	if got := downstreamToolNames(t, ctx, srv.MCP()); !equalStrings(got, []string{"search"}) {
		t.Fatalf("initial tools = %v, want [search]", got)
	}

	// Backend "a" changes "search" to a definition with no InputSchema --
	// mcp.Server.AddTool panics on that, registerTool's addTool wrapper
	// recovers and reports failure, and there is no other backend to fall
	// back to. The exposed name ("search") is unchanged, so this is a
	// same-name valid-to-invalid transition, not a brand-new invalid name.
	srv.UpdateTools("a", []*mcp.Tool{{Name: "search", Description: "v2, broken"}})

	if got := downstreamToolNames(t, ctx, srv.MCP()); len(got) != 0 {
		t.Fatalf("tools after UpdateTools with an invalid winner and no fallback = %v, want [] (stale registration must be removed, not left serving the old definition)", got)
	}
}
```

(`toolSchema`, `downstreamToolNames`, and `equalStrings` are existing helpers already defined earlier in this file — no new imports needed.)

- [ ] **Step 2: Run the new test and confirm it fails**

Run: `go test ./internal/gateway/... -run TestUpdateTools_RemovesStaleRegistrationWhenNewWinnerIsInvalid -v`
Expected: FAIL — reports `tools after UpdateTools ... = [search], want []`, proving the bug is real and reproduced (the stale, previously-valid "search" registration is still being served).

- [ ] **Step 3: Change `registerTool` to report success**

In `internal/gateway/gateway.go`, change:

```go
func registerTool(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Tool], maskKeys []string) {
	candidates := append([]router.Candidate[*mcp.Tool]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addTool(srv, logger, c.Item, callHandler(logger, maskKeys, b, c.OriginalName)) {
			return
		}
	}
	logger.Error("tool unavailable: every candidate backend had an invalid definition", "tool", resolved.Item.Name)
}
```

to:

```go
// registerTool registers resolved.Item, falling back to the next
// lower-priority backend's definition (if any) when one turns out to be
// unregisterable, so a conflict's winner having a malformed schema doesn't
// need to take a validly-defined loser down with it. It reports whether
// anything was registered, so a reconcile caller (see reconcile.go) can
// clean up a stale prior registration when every candidate fails.
func registerTool(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Tool], maskKeys []string) (ok bool) {
	candidates := append([]router.Candidate[*mcp.Tool]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addTool(srv, logger, c.Item, callHandler(logger, maskKeys, b, c.OriginalName)) {
			return true
		}
	}
	logger.Error("tool unavailable: every candidate backend had an invalid definition", "tool", resolved.Item.Name)
	return false
}
```

`New`'s one call site (`registerTool(mcpSrv, logger, backends, resolved, maskKeys)`, inside the `if tables.Tools != nil { for ... }` loop) needs NO change — Go allows calling a function and discarding its return value, and at startup "every candidate failed" already correctly means "never registered" (there's nothing to clean up, since nothing was registered yet).

- [ ] **Step 4: Clean up a stale registration in `UpdateTools`**

In `internal/gateway/reconcile.go`, change `UpdateTools`'s register loop from:

```go
	for name, resolved := range newTable.Items {
		if old, ok := s.toolTable.Items[name]; !ok || !reflect.DeepEqual(old, resolved) {
			registerTool(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
		}
	}
```

to:

```go
	for name, resolved := range newTable.Items {
		if old, ok := s.toolTable.Items[name]; !ok || !reflect.DeepEqual(old, resolved) {
			if !registerTool(s.mcp, s.logger, s.backends, resolved, s.maskKeys) {
				// Every candidate for name was unregisterable (registerTool
				// already logged this). Without this, a prior successful
				// registration for the same exposed name would keep serving
				// its now-superseded definition. RemoveTools on a name that
				// was never registered is a safe no-op (go-sdk's
				// featureSet.remove returns false and skips notifying).
				s.mcp.RemoveTools(name)
			}
		}
	}
```

- [ ] **Step 5: Run the new test and confirm it passes**

Run: `go test ./internal/gateway/... -run TestUpdateTools_RemovesStaleRegistrationWhenNewWinnerIsInvalid -v`
Expected: PASS.

- [ ] **Step 6: Run the full gateway package test suite plus `-race`**

Run: `go test ./internal/gateway/... -v` then `go test -race ./internal/gateway/...`
Expected: PASS, no regressions in any existing test (in particular `TestGateway_FallsBackWhenWinnerSchemaInvalid` in `gateway_test.go`, which exercises `registerTool` at startup through `New` and must keep passing unchanged since `New`'s call site doesn't inspect the new return value).

- [ ] **Step 7: Run the whole repo's test suite and lint**

Run: `task test`
Expected: PASS, all packages.

Run: `task lint`
Expected: 0 issues.

- [ ] **Step 8: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/reconcile.go internal/gateway/reconcile_test.go
git commit -m "fix(gateway): remove stale tool registration when a reconciled winner is invalid

registerTool now reports whether it actually registered anything;
UpdateTools uses that to explicitly RemoveTools a name whose new winner
(and every fallback) turned out to be unregisterable, instead of silently
leaving the previous, now-stale definition live. Fixes the Minor #3 finding
from the list_changed feature's final whole-branch review. Scoped to tools
only: registerResource/registerResourceTemplate can't reach the same bug,
since router.Resolve's rename step forces every resource/resource-template
candidate's URI to equal the exposed name itself, so a same-name valid-to-
invalid transition is structurally impossible there (see this plan's
header for the full reasoning)."
```

---

## Completion check

```bash
task test
task lint
go test -race ./internal/gateway/...
```
