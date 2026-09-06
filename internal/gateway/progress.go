package gateway

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ProgressRelayTimeout bounds how long Relay waits for a downstream
// NotifyProgress write to complete, so a downstream client that stopped
// reading its SSE stream can't stall this backend's entire notification
// pipeline (go-sdk dispatches one backend's notifications sequentially) --
// only this one progress relay is affected, not the tools/call itself
// (Relay's caller never returns an error to it). A var so tests can shrink
// it.
var ProgressRelayTimeout = 5 * time.Second

// ProgressRegistry correlates a backend-facing progress token (which mcprt
// generates fresh for every forwarded tools/call that carries one, to
// guarantee uniqueness across concurrent calls to the same backend) with
// the downstream ServerSession and progress token that originated the
// call, so a notifications/progress a backend sends mid-call can be
// relayed back to the right downstream request under its own token.
type ProgressRegistry struct {
	mu      sync.Mutex
	next    atomic.Uint64
	entries map[uint64]*progressEntry
}

// progressEntry is one in-flight forwarded tools/call's correlation state.
// session/originalToken are set once at Register and never mutated;
// count/lastMessage are updated by Relay (called from the backend client's
// notification-handling goroutine) and read by Summary (called by
// callHandler, on a different goroutine, after CallTool returns) -- hence
// their own mutex, separate from the registry's.
type progressEntry struct {
	session       *mcp.ServerSession
	originalToken any
	backendName   string

	mu          sync.Mutex
	count       int
	lastMessage string
}

// NewProgressRegistry returns an empty registry, ready to use.
func NewProgressRegistry() *ProgressRegistry {
	return &ProgressRegistry{entries: make(map[uint64]*progressEntry)}
}

// Register allocates a fresh internal token for one forwarded tools/call,
// remembers session/originalToken/backendName so a later Relay can find its
// way back, and returns the internal token to set on the outgoing
// CallToolParams, plus a cleanup func the caller must defer to remove the
// entry once the call returns (success or error). backendName is the
// backend the call is being forwarded to; Relay requires a matching
// backendName before it will relay a notification into this entry, so one
// backend can't inject a progress notification into another backend's
// in-flight call by guessing or colliding on the small sequential internal
// token space. The returned *progressEntry can be read (via Summary) after
// cleanup to build the audit log line.
func (r *ProgressRegistry) Register(session *mcp.ServerSession, originalToken any, backendName string) (internalToken uint64, entry *progressEntry, cleanup func()) {
	internalToken = r.next.Add(1)
	entry = &progressEntry{session: session, originalToken: originalToken, backendName: backendName}

	r.mu.Lock()
	r.entries[internalToken] = entry
	r.mu.Unlock()

	cleanup = func() {
		r.mu.Lock()
		delete(r.entries, internalToken)
		r.mu.Unlock()
	}
	return internalToken, entry, cleanup
}

// normalizeProgressToken converts a decoded JSON-RPC progress token into the
// uint64 form Register handed out, for comparison against the registry's
// keys. JSON numbers decode into float64 when the target type is `any`
// (the common case here, since the token round-trips through JSON on its
// way to and from the backend); int64/uint64 are accepted too in case a
// caller constructs params in-process without going through JSON. Anything
// else (including a negative number) reports ok=false.
func normalizeProgressToken(t any) (token uint64, ok bool) {
	switch v := t.(type) {
	case uint64:
		return v, true
	case int64:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case float64:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	default:
		return 0, false
	}
}

// Relay looks up params.ProgressToken (expected to be one Register handed
// out) and, if still registered AND registered for backendName (the
// backend this notification actually arrived from), forwards params to the
// matching downstream ServerSession under its original token, and records
// the event in the entry's summary. A token no longer in the registry (the
// call already completed), not decodable as a Register-issued token, or
// registered for a DIFFERENT backend than backendName is silently dropped
// -- the backend-mismatch case is a misbehaving or malicious backend
// echoing (or guessing) a small sequential internal token that currently
// belongs to a different backend's in-flight call, and is treated the same
// as the ordinary "expected race" drop below: no error propagated to the
// caller.
//
// The downstream NotifyProgress write is bounded by ProgressRelayTimeout
// so a stalled downstream client can't block this backend's whole
// notification-dispatch pipeline.
func (r *ProgressRegistry) Relay(ctx context.Context, logger *slog.Logger, backendName string, params *mcp.ProgressNotificationParams) {
	token, ok := normalizeProgressToken(params.ProgressToken)
	if !ok {
		return
	}

	r.mu.Lock()
	entry, found := r.entries[token]
	r.mu.Unlock()
	if !found {
		return
	}
	if entry.backendName != backendName {
		LogEvent(ctx, logger, slog.LevelWarn, EventProgressBackendMismatch,
			"token_backend", entry.backendName, "notification_backend", backendName)
		return
	}

	entry.mu.Lock()
	entry.count++
	if params.Message != "" {
		entry.lastMessage = params.Message
	}
	entry.mu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, ProgressRelayTimeout)
	defer cancel()
	err := entry.session.NotifyProgress(rctx, &mcp.ProgressNotificationParams{
		ProgressToken: entry.originalToken,
		Progress:      params.Progress,
		Total:         params.Total,
		Message:       params.Message,
	})
	if err != nil {
		logger.Warn("progress relay: downstream NotifyProgress failed", "error", err)
	}
}

// Summary reports how many progress events were relayed for entry, and the
// most recent Message (empty if none carried one or none arrived).
func (e *progressEntry) Summary() (count int, lastMessage string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.count, e.lastMessage
}
