package gateway_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/gateway"
)

func TestSubscriptionRegistry_SubscribeFirstSubscriberNeedsUpstream(t *testing.T) {
	r := gateway.NewSubscriptionRegistry()
	sessionA := &mcp.ServerSession{}
	sessionB := &mcp.ServerSession{}

	if need := r.Subscribe(sessionA, "file:///a", "backend-a", "file:///a"); !need {
		t.Fatal("first Subscribe for a URI: needUpstream = false, want true")
	}
	if need := r.Subscribe(sessionB, "file:///a", "backend-a", "file:///a"); need {
		t.Fatal("second Subscribe for the same URI: needUpstream = true, want false")
	}
	// Same session subscribing again to the same URI is idempotent, not a
	// third distinct subscriber.
	if need := r.Subscribe(sessionA, "file:///a", "backend-a", "file:///a"); need {
		t.Fatal("re-Subscribe by an already-subscribed session: needUpstream = true, want false")
	}
}

func TestSubscriptionRegistry_UnsubscribeLastSubscriberNeedsUpstreamUnsubscribe(t *testing.T) {
	r := gateway.NewSubscriptionRegistry()
	sessionA := &mcp.ServerSession{}
	sessionB := &mcp.ServerSession{}
	r.Subscribe(sessionA, "file:///a", "backend-a", "file:///a")
	r.Subscribe(sessionB, "file:///a", "backend-a", "file:///a")

	if need := r.Unsubscribe(sessionA, "file:///a"); need {
		t.Fatal("Unsubscribe with another subscriber remaining: needUpstreamUnsubscribe = true, want false")
	}
	if need := r.Unsubscribe(sessionB, "file:///a"); !need {
		t.Fatal("Unsubscribe of the last subscriber: needUpstreamUnsubscribe = false, want true")
	}
	// Unsubscribing again (already gone) is a no-op, not an error/panic.
	if need := r.Unsubscribe(sessionB, "file:///a"); need {
		t.Fatal("Unsubscribe of an already-removed session: needUpstreamUnsubscribe = true, want false")
	}
	// A URI nobody ever subscribed to is also a no-op.
	if need := r.Unsubscribe(sessionA, "file:///never"); need {
		t.Fatal("Unsubscribe of an unknown URI: needUpstreamUnsubscribe = true, want false")
	}
}

func TestSubscriptionRegistry_SessionClosedReturnsURIsThatLostLastSubscriber(t *testing.T) {
	r := gateway.NewSubscriptionRegistry()
	sessionA := &mcp.ServerSession{}
	sessionB := &mcp.ServerSession{}
	r.Subscribe(sessionA, "file:///a", "backend-a", "file:///a")
	r.Subscribe(sessionA, "file:///b", "backend-b", "file:///b")
	r.Subscribe(sessionB, "file:///a", "backend-a", "file:///a") // sessionA is not the last subscriber of file:///a

	closed := r.SessionClosed(sessionA)
	if len(closed) != 1 {
		t.Fatalf("SessionClosed(sessionA) returned %d entries, want 1 (only file:///b lost its last subscriber)", len(closed))
	}
	if closed[0].OriginalURI != "file:///b" || closed[0].BackendName != "backend-b" {
		t.Fatalf("SessionClosed(sessionA)[0] = %+v, want {BackendName: backend-b, OriginalURI: file:///b}", closed[0])
	}

	// sessionA already fully removed: a second call must not report anything
	// again.
	if closed := r.SessionClosed(sessionA); len(closed) != 0 {
		t.Fatalf("second SessionClosed(sessionA) = %+v, want empty (already cleaned up)", closed)
	}

	closedB := r.SessionClosed(sessionB)
	if len(closedB) != 1 || closedB[0].OriginalURI != "file:///a" || closedB[0].BackendName != "backend-a" {
		t.Fatalf("SessionClosed(sessionB) = %+v, want [{backend-a file:///a}] (sessionB was file:///a's last remaining subscriber)", closedB)
	}
}

func TestSubscriptionRegistry_BackendReconnectedReturnsOnlyThatBackendsURIs(t *testing.T) {
	r := gateway.NewSubscriptionRegistry()
	sessionA := &mcp.ServerSession{}
	r.Subscribe(sessionA, "file:///a", "backend-a", "file:///a")
	r.Subscribe(sessionA, "file:///b", "backend-b", "file:///b")
	r.Subscribe(sessionA, "file:///a2", "backend-a", "file:///a2")

	got := r.BackendReconnected("backend-a")
	if len(got) != 2 {
		t.Fatalf("BackendReconnected(backend-a) returned %d entries, want 2", len(got))
	}
	var uris []string
	for _, c := range got {
		if c.BackendName != "backend-a" {
			t.Fatalf("BackendReconnected(backend-a) entry %+v has BackendName != backend-a", c)
		}
		uris = append(uris, c.OriginalURI)
	}
	sort.Strings(uris)
	if !slices.Equal(uris, []string{"file:///a", "file:///a2"}) {
		t.Fatalf("BackendReconnected(backend-a) URIs = %v, want [file:///a file:///a2]", uris)
	}

	if got := r.BackendReconnected("backend-c"); len(got) != 0 {
		t.Fatalf("BackendReconnected(backend-c) = %+v, want empty (no subscriptions for that backend)", got)
	}
}

// TestSubscriptionRegistry_ConcurrentSubscribeUnsubscribeSessionClosed
// exercises Subscribe/Unsubscribe/SessionClosed/BackendReconnected from
// many goroutines at once -- go test -race must find nothing.
func TestSubscriptionRegistry_ConcurrentSubscribeUnsubscribeSessionClosed(t *testing.T) {
	r := gateway.NewSubscriptionRegistry()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := &mcp.ServerSession{}
			uri := fmt.Sprintf("file:///%d", i%5)
			r.Subscribe(session, uri, "backend-a", uri)
			r.Unsubscribe(session, uri)
			r.SessionClosed(session)
			r.BackendReconnected("backend-a")
		}(i)
	}
	wg.Wait()

	// The registry must still be usable afterward.
	session := &mcp.ServerSession{}
	if need := r.Subscribe(session, "file:///final", "backend-a", "file:///final"); !need {
		t.Fatal("Subscribe after concurrent stress: needUpstream = false, want true (registry should be empty by now)")
	}
}

// newSubscribableServer returns a bare *mcp.Server with SubscribeHandler/
// UnsubscribeHandler wired to always succeed (this exercises
// SubscriptionRegistry.Relay in isolation, not gateway.Server's own
// resourceTable-based validation -- see gateway_test.go for that), plus a
// connected client session (for the test to call Subscribe on directly)
// and a channel that receives every notifications/resources/updated URI
// the client's ResourceUpdatedHandler sees.
func newSubscribableServer(t *testing.T) (srv *mcp.Server, clientSession *mcp.ClientSession, resourceUpdatedCh <-chan string, cleanup func()) {
	t.Helper()

	srv = mcp.NewServer(&mcp.Implementation{Name: "probe", Version: "v1"}, &mcp.ServerOptions{
		SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))

	ch := make(chan string, 128)
	client := mcp.NewClient(&mcp.Implementation{Name: "probe-client", Version: "v1"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			ch <- req.Params.URI
		},
	})
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: httpSrv.URL}, nil)
	if err != nil {
		t.Fatalf("connect probe client: %v", err)
	}

	return srv, cs, ch, func() {
		_ = cs.Close()
		httpSrv.Close()
	}
}

func TestSubscriptionRegistry_RelayNotifiesSubscribedDownstream(t *testing.T) {
	srv, clientSession, resourceUpdatedCh, cleanup := newSubscribableServer(t)
	defer cleanup()
	ctx := context.Background()

	if err := clientSession.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("client Subscribe: %v", err)
	}

	r := gateway.NewSubscriptionRegistry()
	r.Subscribe(&mcp.ServerSession{}, "file:///a", "backend-a", "file:///a")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r.Relay(ctx, srv, logger, "backend-a", "file:///a")

	select {
	case uri := <-resourceUpdatedCh:
		if uri != "file:///a" {
			t.Fatalf("resource updated URI = %q, want file:///a", uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("downstream ResourceUpdatedHandler did not fire within 5s of Relay")
	}
}

func TestSubscriptionRegistry_RelayIgnoresUnknownURI(t *testing.T) {
	srv, clientSession, resourceUpdatedCh, cleanup := newSubscribableServer(t)
	defer cleanup()
	ctx := context.Background()
	if err := clientSession.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("client Subscribe: %v", err)
	}

	r := gateway.NewSubscriptionRegistry()
	// No r.Subscribe call at all: the registry never learned about
	// file:///a, even though the SDK-side client did subscribe to it.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r.Relay(ctx, srv, logger, "backend-a", "file:///a")

	select {
	case uri := <-resourceUpdatedCh:
		t.Fatalf("received resource update for URI %q, want none relayed (registry never tracked it)", uri)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSubscriptionRegistry_RelayIgnoresBackendMismatch proves that a
// notification claiming a URI the registry has on record as owned by a
// DIFFERENT backend is silently dropped from delivery -- the counterpart
// of TestProgressRegistry_RelayDropsBackendMismatch for this registry --
// and that the anomaly is logged via EventResourceUpdateBackendMismatch.
func TestSubscriptionRegistry_RelayIgnoresBackendMismatch(t *testing.T) {
	srv, clientSession, resourceUpdatedCh, cleanup := newSubscribableServer(t)
	defer cleanup()
	ctx := context.Background()
	if err := clientSession.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("client Subscribe: %v", err)
	}

	r := gateway.NewSubscriptionRegistry()
	r.Subscribe(&mcp.ServerSession{}, "file:///a", "backend-a", "file:///a")

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	r.Relay(ctx, srv, logger, "backend-b", "file:///a")

	select {
	case uri := <-resourceUpdatedCh:
		t.Fatalf("received resource update for URI %q from mismatched backend, want none relayed", uri)
	case <-time.After(200 * time.Millisecond):
	}

	if !bytes.Contains(logBuf.Bytes(), []byte(gateway.EventResourceUpdateBackendMismatch)) {
		t.Fatalf("log output = %q, want it to contain event %q", logBuf.String(), gateway.EventResourceUpdateBackendMismatch)
	}
}
