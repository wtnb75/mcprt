package gateway

import (
	"context"
	"log/slog"
)

// LogEvent records one notable, audit-worthy anomaly or state-change event
// that falls outside the request/response shape logCall covers (no single
// downstream ServerSession or MCP method to attribute it to): a backend
// misbehaving in a way relay code must safely refuse, a backend's tool/
// resource/prompt list successfully reconciling after list_changed, or two
// backends' exposed names colliding. Every call site shares the same
// "gateway event" message and an "event" field naming the specific kind, so
// these lines are greppable/filterable as one group -- distinct from the
// routine operational Info/Warn/Error logging this codebase also does
// (backend connect/disconnect, config reload, listener start/stop, ...),
// which stays as direct logger calls: LogEvent is reserved for events with
// audit value, something an operator investigating an incident or a
// config-hygiene issue would specifically want to search for.
//
// level lets a caller choose the right severity per event (Warn for an
// anomaly/refusal, Info for a routine-but-audit-worthy state change like a
// successful list_changed reconcile) -- LogEvent itself doesn't judge
// severity. ctx is accepted (passed straight to slog.Logger.Log, matching
// that method's own signature) so a future context-aware log handler (e.g.
// one that enriches a line with trace_id from an active span) applies
// automatically without an API change here; no such enrichment happens
// today.
func LogEvent(ctx context.Context, logger *slog.Logger, level slog.Level, event string, args ...any) {
	logger.Log(ctx, level, "gateway event", append([]any{"event", event}, args...)...)
}
