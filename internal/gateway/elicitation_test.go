package gateway_test

import (
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/gateway"
)

func TestElicitationRouter_RouteWithZeroInFlightErrors(t *testing.T) {
	r := gateway.NewElicitationRouter()
	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route with zero in-flight calls: got nil error, want an error")
	}
}

func TestElicitationRouter_RouteWithOneInFlightReturnsSession(t *testing.T) {
	r := gateway.NewElicitationRouter()
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

func TestElicitationRouter_RouteWithMultipleInFlightErrors(t *testing.T) {
	r := gateway.NewElicitationRouter()
	leave1 := r.Enter("backend-a", &mcp.ServerSession{})
	defer leave1()
	leave2 := r.Enter("backend-a", &mcp.ServerSession{})
	defer leave2()

	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route with two in-flight calls: got nil error, want an error (ambiguous)")
	}
}

// TestElicitationRouter_SameSessionTwiceIsStillAmbiguous checks that Route
// counts in-flight CALLS, not distinct sessions: two concurrent tools/call
// from the very same downstream session against the same backend must
// still refuse to route, since MCP's elicitation/create carries no
// per-call correlation -- mcprt genuinely cannot tell which of the two
// calls the elicitation belongs to, even though routing it to "the" session
// would happen to reach the right client.
func TestElicitationRouter_SameSessionTwiceIsStillAmbiguous(t *testing.T) {
	r := gateway.NewElicitationRouter()
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

func TestElicitationRouter_DifferentBackendsAreIndependent(t *testing.T) {
	r := gateway.NewElicitationRouter()
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

// TestElicitationRouter_ConcurrentEnterRouteLeave exercises Enter, Route,
// and leave from many goroutines at once -- go test -race must find
// nothing. It does not assert on Route's outcome mid-stress (the in-flight
// count is nondeterministic while goroutines are still entering/leaving),
// only that the router is race-free and left in a correct empty state
// afterward.
func TestElicitationRouter_ConcurrentEnterRouteLeave(t *testing.T) {
	r := gateway.NewElicitationRouter()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leave := r.Enter("backend-a", &mcp.ServerSession{})
			_, _ = r.Route("backend-a")
			leave()
		}()
	}
	wg.Wait()

	if _, err := r.Route("backend-a"); err == nil {
		t.Fatal("Route after all goroutines left: got nil error, want an error (zero in-flight)")
	}
}
