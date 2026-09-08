package gateway

import (
	"context"
	"log/slog"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SubscriptionRegistry tracks, per resource URI, which downstream
// ServerSessions currently want notifications/resources/updated for it --
// and, implicitly, whether mcprt itself currently has an active upstream
// subscription with the owning backend (reference-counted: mcprt
// subscribes upstream once, on the first downstream subscriber, and
// unsubscribes once the last one leaves). Subscribe/Unsubscribe/
// SessionClosed/BackendReconnected never touch a *backend.Backend
// directly -- their bool/slice return values tell the caller
// (gateway.go's subscribeHandler/unsubscribeHandler/
// startSessionCloseWatcherOnce, and internal/cli's superviseBackend)
// exactly when an upstream Subscribe/Unsubscribe call is needed, keeping
// this type free of backend I/O and easy to test in isolation -- the same
// design CallRouter and ProgressRegistry already use. Relay is the one
// exception: it does perform I/O, but only to the downstream side (via
// *mcp.Server, not *backend.Backend), matching ProgressRegistry.Relay's
// own precedent of calling the downstream side directly rather than
// routing back through a caller.
type SubscriptionRegistry struct {
	mu   sync.Mutex
	subs map[string]*subscription // keyed by URI -- exposed and original are the same string for resources (prefix is never applied to resource URIs), so one key serves both purposes
}

// subscription is one URI's current subscribers plus the backend that owns
// it. backendName/originalURI are set once, when the URI's first
// subscriber arrives (Subscribe), and never change while any subscriber
// remains -- they describe which backend mcprt's own upstream Subscribe
// call went to, so a later Unsubscribe/Relay/BackendReconnected can
// address the same backend without re-resolving resourceTable.
type subscription struct {
	backendName string
	originalURI string
	sessions    map[*mcp.ServerSession]bool
}

// subscriptionToClose is one URI whose last downstream subscriber just
// left (SessionClosed) or that needs re-subscribing after a backend
// reconnect (BackendReconnected) -- both callers need exactly
// backendName/originalURI to issue the matching upstream
// Subscribe/Unsubscribe call.
type subscriptionToClose struct {
	BackendName string
	OriginalURI string
}

// NewSubscriptionRegistry returns an empty registry, ready to use.
func NewSubscriptionRegistry() *SubscriptionRegistry {
	return &SubscriptionRegistry{subs: make(map[string]*subscription)}
}

// Subscribe records session's interest in uri (backendName/originalURI
// describe the owning backend, resolved the same way resourceReadHandler
// resolves them). Calling Subscribe again for a session/uri pair that's
// already recorded is a no-op (idempotent on the map, not double-counted).
// Reports needUpstream=true exactly when this is uri's first subscriber --
// the caller must then issue the backend's actual resources/subscribe
// itself; SubscriptionRegistry has no *backend.Backend to call it with.
func (r *SubscriptionRegistry) Subscribe(session *mcp.ServerSession, uri, backendName, originalURI string) (needUpstream bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	sub, ok := r.subs[uri]
	if !ok {
		sub = &subscription{backendName: backendName, originalURI: originalURI, sessions: make(map[*mcp.ServerSession]bool)}
		r.subs[uri] = sub
	}
	needUpstream = len(sub.sessions) == 0
	sub.sessions[session] = true
	return needUpstream
}

// Unsubscribe removes session's interest in uri. Reports
// needUpstreamUnsubscribe=true exactly when session was uri's last
// subscriber -- the caller must then issue the backend's actual
// resources/unsubscribe itself. Unsubscribing a uri/session pair that was
// never subscribed (or already removed) is a no-op, reporting false.
func (r *SubscriptionRegistry) Unsubscribe(session *mcp.ServerSession, uri string) (needUpstreamUnsubscribe bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	sub, ok := r.subs[uri]
	if !ok {
		return false
	}
	delete(sub.sessions, session)
	if len(sub.sessions) == 0 {
		delete(r.subs, uri)
		return true
	}
	return false
}

// SessionClosed removes every subscription session held, across every URI
// -- called once, when session's Wait() returns (see gateway.go's
// startSessionCloseWatcherOnce), to clean up a downstream client that
// disconnected without ever calling resources/unsubscribe. Reports the
// URIs that lost their last subscriber as a result, each needing an
// upstream Unsubscribe.
func (r *SubscriptionRegistry) SessionClosed(session *mcp.ServerSession) []subscriptionToClose {
	r.mu.Lock()
	defer r.mu.Unlock()

	var closed []subscriptionToClose
	for uri, sub := range r.subs {
		if !sub.sessions[session] {
			continue
		}
		delete(sub.sessions, session)
		if len(sub.sessions) == 0 {
			closed = append(closed, subscriptionToClose{BackendName: sub.backendName, OriginalURI: sub.originalURI})
			delete(r.subs, uri)
		}
	}
	return closed
}

// BackendReconnected reports every URI currently subscribed against
// backendName, regardless of how many downstream sessions still want it --
// for the reconnect path (see superviseBackend's wiring in
// internal/cli/server.go) to re-issue Subscribe on the backend's fresh
// *backend.Backend. An existing subscription must survive a backend
// disconnect/reconnect cycle exactly like tools/resources/prompts
// list-changed handlers already do; a fresh backend connection has no
// memory of subscriptions the previous connection held.
func (r *SubscriptionRegistry) BackendReconnected(backendName string) []subscriptionToClose {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []subscriptionToClose
	for _, sub := range r.subs {
		if sub.backendName == backendName {
			out = append(out, subscriptionToClose{BackendName: sub.backendName, OriginalURI: sub.originalURI})
		}
	}
	return out
}

// Relay notifies mcpSrv's downstream subscribers that originalURI on
// backendName changed, but only if originalURI is currently a tracked
// upstream subscription owned by backendName -- guarding against a
// notification for a URI mcprt never subscribed to, or (like
// ProgressRegistry.Relay's backend-mismatch guard) one a DIFFERENT
// backend is sending under a URI string that collides with another
// backend's own subscription.
//
// Unlike ProgressRegistry.Relay (which owns a direct *mcp.ServerSession
// per entry and calls NotifyProgress on it), Relay does not iterate this
// registry's own session set to deliver the notification: go-sdk's
// *mcp.ServerSession has no exported per-resource notify method (unlike
// NotifyProgress), and go-sdk's *mcp.Server already tracks, internally,
// per URI, exactly which downstream sessions called resources/subscribe
// for it (populated as a side effect of subscribeHandler/
// unsubscribeHandler succeeding) -- including protocol-version-aware
// delivery mcprt has no way to replicate from outside the SDK. Relay
// therefore delegates the actual fan-out to mcpSrv.ResourceUpdated, which
// already does this correctly; re-implementing it here would either
// duplicate that internal bookkeeping or silently drop the
// protocol-version handling go-sdk's own delivery path relies on.
func (r *SubscriptionRegistry) Relay(ctx context.Context, mcpSrv *mcp.Server, logger *slog.Logger, backendName, originalURI string) {
	r.mu.Lock()
	sub, ok := r.subs[originalURI]
	r.mu.Unlock()
	if !ok || sub.backendName != backendName {
		return
	}

	if err := mcpSrv.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: originalURI}); err != nil {
		logger.Warn("resource update relay failed", "uri", originalURI, "backend", backendName, "error", err)
	}
}
