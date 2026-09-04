package gateway

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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

	mu          sync.Mutex
	count       int
	lastMessage string
}

// NewProgressRegistry returns an empty registry, ready to use.
func NewProgressRegistry() *ProgressRegistry {
	return &ProgressRegistry{entries: make(map[uint64]*progressEntry)}
}

// Register allocates a fresh internal token for one forwarded tools/call,
// remembers session/originalToken so a later Relay can find its way back,
// and returns the internal token to set on the outgoing CallToolParams,
// plus a cleanup func the caller must defer to remove the entry once the
// call returns (success or error). The returned *progressEntry can be read
// (via Summary) after cleanup to build the audit log line.
func (r *ProgressRegistry) Register(session *mcp.ServerSession, originalToken any) (internalToken uint64, entry *progressEntry, cleanup func()) {
	internalToken = r.next.Add(1)
	entry = &progressEntry{session: session, originalToken: originalToken}

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
// out) and, if still registered, forwards params to the matching
// downstream ServerSession under its original token, and records the event
// in the entry's summary. A token no longer in the registry (the call
// already completed) or not decodable as a Register-issued token is
// silently dropped -- an expected race with a backend's last few in-flight
// notifications, not an error.
func (r *ProgressRegistry) Relay(ctx context.Context, logger *slog.Logger, params *mcp.ProgressNotificationParams) {
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

	entry.mu.Lock()
	entry.count++
	if params.Message != "" {
		entry.lastMessage = params.Message
	}
	entry.mu.Unlock()

	err := entry.session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
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
