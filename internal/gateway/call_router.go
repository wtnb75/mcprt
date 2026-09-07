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
