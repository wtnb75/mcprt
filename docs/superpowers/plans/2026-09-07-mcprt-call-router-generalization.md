# CallRouter Generalization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rename `ElicitationRouter` to `CallRouter` (and `gateway.Relays.Elicit` to `gateway.Relays.Calls`) as a pure, behavior-preserving refactor, so a later change can wire `sampling/createMessage` and `roots/list` through the same in-flight-call-tracking component instead of each reimplementing it.

**Architecture:** Mechanical rename across three layers that must move together: (1) the router type itself and its unit tests in `internal/gateway`, (2) `gateway.Relays`/`callHandler` in `internal/gateway/gateway.go` plus the integration tests that construct a `Relays{Elicit: ...}`, (3) the CLI wiring in `internal/cli/server.go` plus its tests. No logic changes anywhere — every existing elicitation test must pass unmodified in behavior (only identifier names change).

**Tech Stack:** Go, `go test`, `golangci-lint`, `gofmt`, `go vet` (via `task lint` / `task test`).

**Spec:** `docs/superpowers/specs/2026-09-07-mcprt-call-router-generalization-design.md`

## Global Constraints

- Pure refactor: the existing elicitation relay's observable behavior (routing rules, error conditions, logged events) must not change. Only type/field/file names change.
- `relays.Elicit != nil` becomes `relays.Calls != nil` — same nil-means-disabled semantics, just renamed (per the design doc's concrete `callHandler` code sample, not the abbreviated scope bullet that implied removing the check — the code sample is authoritative).
- Do not touch `elicitTimeout`, `OnElicit`, `ElicitParams`/`ElicitResult`, `EventElicitation*`, or the `config.Duration` field literally named `Elicit` in `internal/config` / `internal/cli/timeouts_internal_test.go` — those are unrelated to the router rename (elicitation-the-feature keeps its name; only the shared *router* is renamed).
- Do not edit historical spec/plan docs under `docs/superpowers/specs/` or `docs/superpowers/plans/` that mention `ElicitationRouter` (e.g. `2026-08-25-mcprt-elicitation-relay-design.md`) — they are dated records of past decisions, not live docs.
- Use `git mv` for the file renames so history is preserved.

---

### Task 1: Rename `ElicitationRouter` → `CallRouter` and update `internal/gateway/gateway.go`

**Files:**
- Rename (via `git mv`): `internal/gateway/elicitation.go` → `internal/gateway/call_router.go`
- Rename (via `git mv`): `internal/gateway/elicitation_test.go` → `internal/gateway/call_router_test.go`
- Modify: `internal/gateway/gateway.go` (the `Relays` struct, `NewConfig`'s doc comment, and `callHandler`)
- Modify: `internal/gateway/gateway_test.go` (the two elicitation integration tests that construct a router directly)

**Interfaces:**
- Produces: `gateway.CallRouter` (renamed from `gateway.ElicitationRouter`), `gateway.NewCallRouter() *CallRouter`, `(*CallRouter).Enter(backendName string, session *mcp.ServerSession) (leave func())`, `(*CallRouter).Route(backendName string) (*mcp.ServerSession, error)` — identical signatures to the old `ElicitationRouter` methods.
- Produces: `gateway.Relays.Calls *CallRouter` (renamed from `Relays.Elicit`).

- [ ] **Step 1: Rename the router file and its test file with git mv**

```bash
git mv internal/gateway/elicitation.go internal/gateway/call_router.go
git mv internal/gateway/elicitation_test.go internal/gateway/call_router_test.go
```

- [ ] **Step 2: Rewrite `internal/gateway/call_router.go`'s content**

Replace the entire file content with:

```go
package gateway

import (
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CallRouter tracks, per backend, which downstream ServerSessions
// currently have a tools/call in flight against that backend -- so that a
// backend-initiated request with no built-in call correlation (MCP defines
// several: elicitation/create, sampling/createMessage, roots/list) can be
// routed to the right downstream session when exactly one call is in
// flight, and refused otherwise. Shared by every such feature rather than
// each tracking its own copy of the same in-flight-call state.
type CallRouter struct {
	mu    sync.Mutex
	calls map[string]*backendCalls // keyed by backend name, created lazily
}

// backendCalls tracks one backend's in-flight tools/call sessions, keyed by
// a per-backendCalls monotonic counter rather than stored in a plain slice:
// a slice index shifts under concurrent removal (leave), so identifying an
// entry by a stable key that Enter hands out and leave deletes by is what
// makes concurrent Enter/leave safe without any index-bookkeeping.
type backendCalls struct {
	mu   sync.Mutex
	next uint64
	live map[uint64]*mcp.ServerSession
}

// NewCallRouter returns an empty router, ready to use.
func NewCallRouter() *CallRouter {
	return &CallRouter{calls: make(map[string]*backendCalls)}
}

// Enter records one in-flight tools/call for backendName, owned by
// session -- the same session may Enter more than once, for two concurrent
// calls from the same downstream client to the same backend, and each
// counts as a separate in-flight call for Route's purposes. The caller
// must call the returned leave func exactly once (via defer) when the call
// returns, success or failure.
func (r *CallRouter) Enter(backendName string, session *mcp.ServerSession) (leave func()) {
	r.mu.Lock()
	bc, ok := r.calls[backendName]
	if !ok {
		bc = &backendCalls{live: make(map[uint64]*mcp.ServerSession)}
		r.calls[backendName] = bc
	}
	r.mu.Unlock()

	bc.mu.Lock()
	id := bc.next
	bc.next++
	bc.live[id] = session
	bc.mu.Unlock()

	return func() {
		bc.mu.Lock()
		delete(bc.live, id)
		bc.mu.Unlock()
	}
}

// Route reports the single downstream session to forward a backend-initiated
// request to, for the given backend. It returns an error -- and forwards
// nothing -- unless exactly one tools/call is currently in flight for
// backendName: zero in-flight calls means there's nothing to correlate to
// (the request arrived too late, or the backend is misbehaving); more than
// one means mcprt cannot tell which call it belongs to (MCP defines several
// such requests -- elicitation/create, sampling/createMessage, roots/list --
// that carry no per-call correlation token), and guessing wrong would route
// a backend's question to an unrelated client -- even when every in-flight
// call happens to belong to the same session, the count alone decides,
// never the sessions' identity.
//
// This "exactly one" invariant is honest about in-flight calls Enter still
// knows about, but call cancellation can make that count go stale faster
// than the backend's own processing does: go-sdk's outgoing-call bookkeeping
// retires a tools/call the instant its context is done, without waiting for
// the backend to actually stop working (see jsonrpc2.AsyncCall.Await --
// on <-ctx.Done() it returns ctx.Err() immediately, it does not block on the
// backend's eventual response). callHandler's `defer leave()` runs right
// after CallTool returns, so a downstream client A that cancels or
// disconnects mid-call frees A's slot at that instant even though backend B
// may still be running A's call and can still emit such a request for it
// moments later. If, at that exact moment, some unrelated client C happens
// to be the only other call in flight to B, Route will hand B's question --
// meant for A's now-abandoned call -- to C instead, because Route has no
// way to tell "B is still working on a call whose slot was freed early"
// from "B is idle." Closing this gap -- for example, by keeping a
// cancelled call's slot counted toward ambiguity for a short grace window
// after cancellation, instead of dropping it the instant CallTool returns
// -- is a deliberate future decision, not attempted here.
func (r *CallRouter) Route(backendName string) (*mcp.ServerSession, error) {
	r.mu.Lock()
	bc, ok := r.calls[backendName]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("elicitation: no in-flight tools/call for backend %q", backendName)
	}

	bc.mu.Lock()
	defer bc.mu.Unlock()
	switch len(bc.live) {
	case 0:
		return nil, fmt.Errorf("elicitation: no in-flight tools/call for backend %q", backendName)
	case 1:
		for _, s := range bc.live {
			return s, nil
		}
	}
	return nil, fmt.Errorf("elicitation: %d concurrent tools/call in flight for backend %q, cannot disambiguate", len(bc.live), backendName)
}
```

(The `elicitation:` error-message prefix is intentionally left unchanged — this is a pure rename, and no test or caller depends on it, but changing wording that isn't required by the design doc risks an unintended behavior diff.)

- [ ] **Step 3: Rewrite `internal/gateway/call_router_test.go`'s content**

Replace the entire file content with:

```go
package gateway_test

import (
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/gateway"
)

func TestCallRouter_RouteWithZeroInFlightErrors(t *testing.T) {
	r := gateway.NewCallRouter()
	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route with zero in-flight calls: got nil error, want an error")
	}
}

func TestCallRouter_RouteWithOneInFlightReturnsSession(t *testing.T) {
	r := gateway.NewCallRouter()
	session := &mcp.ServerSession{}
	leave := r.Enter("backend-a", session)
	defer leave()

	got, err := r.Route("backend-a")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got != session {
		t.Fatalf("Route returned %v, want %v", got, session)
	}
}

func TestCallRouter_RouteWithMultipleInFlightErrors(t *testing.T) {
	r := gateway.NewCallRouter()
	leave1 := r.Enter("backend-a", &mcp.ServerSession{})
	defer leave1()
	leave2 := r.Enter("backend-a", &mcp.ServerSession{})
	defer leave2()

	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route with two in-flight calls: got nil error, want an error (ambiguous)")
	}
}

// TestCallRouter_SameSessionTwiceIsStillAmbiguous checks that Route
// counts in-flight CALLS, not distinct sessions: two concurrent tools/call
// from the very same downstream session against the same backend must
// still refuse to route, since MCP's elicitation/create carries no
// per-call correlation -- mcprt genuinely cannot tell which of the two
// calls the elicitation belongs to, even though routing it to "the" session
// would happen to reach the right client.
func TestCallRouter_SameSessionTwiceIsStillAmbiguous(t *testing.T) {
	r := gateway.NewCallRouter()
	session := &mcp.ServerSession{}
	leave1 := r.Enter("backend-a", session)
	leave2 := r.Enter("backend-a", session)

	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route with two in-flight calls from the same session: got nil error, want an error")
	}

	leave1()
	got, err := r.Route("backend-a")
	if err != nil {
		t.Fatalf("Route after one leave: %v", err)
	}
	if got != session {
		t.Fatalf("Route after one leave = %v, want %v", got, session)
	}

	leave2()
	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route after both leaves: got nil error, want an error (zero in-flight)")
	}
}

func TestCallRouter_DifferentBackendsAreIndependent(t *testing.T) {
	r := gateway.NewCallRouter()
	sessionA := &mcp.ServerSession{}
	leaveA := r.Enter("backend-a", sessionA)
	defer leaveA()

	if _, err := r.Route("backend-b"); err == nil {
		t.Fatal("Route(backend-b) with zero in-flight calls for backend-b: got nil error, want an error")
	}
	got, err := r.Route("backend-a")
	if err != nil {
		t.Fatalf("Route(backend-a): %v", err)
	}
	if got != sessionA {
		t.Fatalf("Route(backend-a) = %v, want %v", got, sessionA)
	}
}

// TestCallRouter_ConcurrentEnterRouteLeave exercises Enter, Route,
// and leave from many goroutines at once -- go test -race must find
// nothing. It does not assert on Route's outcome mid-stress (the in-flight
// count is nondeterministic while goroutines are still entering/leaving),
// only that the router is race-free and left in a correct empty state
// afterward.
func TestCallRouter_ConcurrentEnterRouteLeave(t *testing.T) {
	r := gateway.NewCallRouter()
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			leave := r.Enter("backend-a", &mcp.ServerSession{})
			_, _ = r.Route("backend-a")
			leave()
		})
	}
	wg.Wait()

	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route after all goroutines left: got nil error, want an error (zero in-flight)")
	}
}
```

- [ ] **Step 4: Update `internal/gateway/gateway.go`'s `Relays` struct**

Find:

```go
// Relays bundles the optional cross-call correlation services a gateway
// can wire in. A nil field means that feature is disabled, matching the
// existing nil-means-disabled convention each of *ProgressRegistry and
// *ElicitationRouter already had as standalone parameters.
type Relays struct {
	Progress *ProgressRegistry
	Elicit   *ElicitationRouter
}
```

Replace with:

```go
// Relays bundles the optional cross-call correlation services a gateway
// can wire in. A nil field means that feature is disabled, matching the
// existing nil-means-disabled convention each of *ProgressRegistry and
// *CallRouter already had as standalone parameters.
type Relays struct {
	Progress *ProgressRegistry
	Calls    *CallRouter
}
```

- [ ] **Step 5: Update `internal/gateway/gateway.go`'s `NewConfig` doc comment**

Find:

```go
// nil MaskKeys means no extra masking, and a
// nil Relays.Progress/Relays.Elicit means that relay feature is disabled.
```

Replace with:

```go
// nil MaskKeys means no extra masking, and a
// nil Relays.Progress/Relays.Calls means that relay feature is disabled.
```

- [ ] **Step 6: Update `internal/gateway/gateway.go`'s `callHandler` doc comment and body**

Find:

```go
// relays.Elicit is non-nil, it records this call as in-flight against b for
// the whole duration of the backend call, so a backend's elicitation/create
// (relayed via relays.Elicit.Route, wired through backend.ChangeCallbacks.
// OnElicit) can be routed back to req.Session when -- and only when --
// this is the sole tools/call in flight against b.
```

Replace with:

```go
// relays.Calls is non-nil, it records this call as in-flight against b for
// the whole duration of the backend call, so a backend's elicitation/create
// (relayed via relays.Calls.Route, wired through backend.ChangeCallbacks.
// OnElicit) can be routed back to req.Session when -- and only when --
// this is the sole tools/call in flight against b.
```

Then find:

```go
		if relays.Elicit != nil {
			leave := relays.Elicit.Enter(b.Name, req.Session)
			defer leave()
		}
```

Replace with:

```go
		if relays.Calls != nil {
			leave := relays.Calls.Enter(b.Name, req.Session)
			defer leave()
		}
```

- [ ] **Step 7: Update `internal/gateway/gateway_test.go`'s two elicitation integration tests**

Three mechanical find/replace passes over the whole file (each string occurs exactly where described; replace every occurrence):

1. Replace `gateway.NewElicitationRouter()` with `gateway.NewCallRouter()` (2 occurrences, in `TestGateway_CallHandlerRoutesElicitationToSession`-style tests around what are currently lines 1647 and 1731).
2. Replace `gateway.Relays{Elicit: elicitRouter}` with `gateway.Relays{Calls: callRouter}` (2 occurrences, currently around lines 1680 and 1762).
3. Replace remaining occurrences of the identifier `elicitRouter` with `callRouter` (the `elicitRouter := ...` declarations and the `elicitRouter.Route("backend-a")` calls inside each `OnElicit` closure — 4 occurrences total after step 2 has already consumed the two `Relays{Elicit: elicitRouter}` occurrences).

- [ ] **Step 8: Verify the package compiles and its tests pass**

Run: `go build ./internal/gateway/... && go test -race -cover ./internal/gateway/...`
Expected: PASS, no compile errors, no leftover references to `ElicitationRouter`/`NewElicitationRouter`/`Relays.Elicit`.

Also run: `rg -n "ElicitationRouter|relays\.Elicit|Relays\{Elicit" internal/gateway/`
Expected: no matches.

- [ ] **Step 9: Commit**

```bash
git add internal/gateway/call_router.go internal/gateway/call_router_test.go internal/gateway/gateway.go internal/gateway/gateway_test.go
git status
git commit -m "refactor(gateway): rename ElicitationRouter to CallRouter"
```

(Note: `git mv` in Step 1 already staged the renames; `git add` on the new paths plus the modified files is a no-op for the rename itself but stages the content edits.)

---

### Task 2: Rewire `internal/cli` to use `gateway.Relays.Calls` / `gateway.CallRouter`

**Files:**
- Modify: `internal/cli/server.go`
- Modify: `internal/cli/server_internal_test.go`
- Modify: `internal/cli/server_test.go`

**Interfaces:**
- Consumes: `gateway.CallRouter`, `gateway.NewCallRouter()`, `gateway.Relays.Calls` from Task 1.

- [ ] **Step 1: Update `internal/cli/server.go`'s `buildGateway` relay construction**

Find:

```go
	gwH.relays = gateway.Relays{
		Progress: gateway.NewProgressRegistry(),
		Elicit:   gateway.NewElicitationRouter(),
	}
```

Replace with:

```go
	gwH.relays = gateway.Relays{
		Progress: gateway.NewProgressRegistry(),
		Calls:    gateway.NewCallRouter(),
	}
```

- [ ] **Step 2: Update `internal/cli/server.go`'s `gwHolder` doc comment**

Find:

```go
// nil *gateway.ProgressRegistry/*gateway.ElicitationRouter everywhere else.
```

Replace with:

```go
// nil *gateway.ProgressRegistry/*gateway.CallRouter everywhere else.
```

- [ ] **Step 3: Update `internal/cli/server.go`'s `OnElicit` wiring**

Find:

```go
		if gwH.relays.Elicit != nil {
			cb.OnElicit = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				session, err := gwH.relays.Elicit.Route(bc.Name)
```

Replace with:

```go
		if gwH.relays.Calls != nil {
			cb.OnElicit = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				session, err := gwH.relays.Calls.Route(bc.Name)
```

- [ ] **Step 4: Update `internal/cli/server_test.go`'s doc comment**

Find:

```go
// (superviseBackend's OnElicit, ElicitationRouter.Route, elicitTimeout).
```

Replace with:

```go
// (superviseBackend's OnElicit, CallRouter.Route, elicitTimeout).
```

- [ ] **Step 5: Update `internal/cli/server_internal_test.go`'s doc comments and test body**

Find:

```go
// It wires the real production path (connectBackendsWaitable, i.e.
// superviseBackends/superviseBackend, with a real *gateway.ElicitationRouter
// in gwH.relays.Elicit) against a real backend whose "ask" tool calls
```

Replace with:

```go
// It wires the real production path (connectBackendsWaitable, i.e.
// superviseBackends/superviseBackend, with a real *gateway.CallRouter
// in gwH.relays.Calls) against a real backend whose "ask" tool calls
```

Find:

```go
// gwH.relays.Elicit -- exactly what gateway.callHandler would do around a
```

Replace with:

```go
// gwH.relays.Calls -- exactly what gateway.callHandler would do around a
```

Find:

```go
	// *mcp.ServerSession to Enter into gwH.relays.Elicit -- ElicitationRouter's
```

Replace with:

```go
	// *mcp.ServerSession to Enter into gwH.relays.Calls -- CallRouter's
```

Find:

```go
	gwH := &gwHolder{relays: gateway.Relays{Elicit: gateway.NewElicitationRouter()}}
```

Replace with:

```go
	gwH := &gwHolder{relays: gateway.Relays{Calls: gateway.NewCallRouter()}}
```

Find:

```go
	leave := gwH.relays.Elicit.Enter("fake", capturedSession)
```

Replace with:

```go
	leave := gwH.relays.Calls.Enter("fake", capturedSession)
```

- [ ] **Step 6: Verify the package compiles and its tests pass**

Run: `go build ./internal/cli/... && go test -race -cover ./internal/cli/...`
Expected: PASS, no compile errors.

Also run: `rg -n "ElicitationRouter|relays\.Elicit|Relays\{Elicit" internal/cli/`
Expected: no matches.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go internal/cli/server_test.go
git commit -m "refactor(cli): wire gateway.Relays.Calls instead of Relays.Elicit"
```

---

### Task 3: Full-repo verification and PR

**Files:** none (verification only)

- [ ] **Step 1: Confirm no stray references remain anywhere in Go source**

Run: `rg -n "ElicitationRouter" -g '*.go'`
Expected: no matches.

- [ ] **Step 2: Run the full test suite**

Run: `task test` (equivalent to `go test -cover ./...`)
Expected: PASS, all packages, no failures or new skips.

- [ ] **Step 3: Run lint**

Run: `task lint` (equivalent to `gofmt -l .`, `go vet ./...`, `golangci-lint run ./...`)
Expected: no output from `gofmt -l .` (nothing to reformat), no errors from `go vet` or `golangci-lint`.

- [ ] **Step 4: Review the full diff**

Run: `git diff main --stat` and `git log --oneline main..HEAD`
Expected: only the files listed in Tasks 1-2 changed, two commits, both renames show as renames (not delete+add) in `git status`/`git diff --stat`.

- [ ] **Step 5: Push and open the PR**

```bash
git push -u origin HEAD
gh pr create --title "refactor(gateway): generalize ElicitationRouter into CallRouter" --body "$(cat <<'EOF'
## Summary
- Rename `ElicitationRouter` to `CallRouter` (and `gateway.Relays.Elicit` to `Relays.Calls`) as a pure, behavior-preserving refactor, per docs/superpowers/specs/2026-09-07-mcprt-call-router-generalization-design.md.
- Lays the groundwork for routing `sampling/createMessage` and `roots/list` through the same shared in-flight-call tracker in follow-up changes (out of scope here).

## Test plan
- [x] `go test -race -cover ./internal/gateway/...`
- [x] `go test -race -cover ./internal/cli/...`
- [x] `task test`
- [x] `task lint`
EOF
)"
```

Report the PR URL back to the user.
