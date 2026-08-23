package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"
)

// defaultMaskKeyPatterns are matched case-insensitively as substrings
// against argument key names: covers apikey/api_key/access_key/private_key
// (key), authorization (auth), password/passwd (pass), credential (cred),
// token.
var defaultMaskKeyPatterns = []string{"key", "auth", "pass", "cred", "token"}

// maskArguments returns a copy of v with any object key matching (case-
// insensitively, by substring) one of defaultMaskKeyPatterns or extraKeys
// replaced with "***". v is either json.RawMessage (tool arguments) or
// map[string]string (prompt arguments); both are normalized to a walkable
// any tree first. A v of neither type, or malformed JSON, falls back to a
// string representation rather than panicking or dropping the field.
func maskArguments(v any, extraKeys []string) any {
	switch t := v.(type) {
	case json.RawMessage:
		var parsed any
		if err := json.Unmarshal(t, &parsed); err != nil {
			return string(t)
		}
		return maskValue(parsed, extraKeys)
	case map[string]string:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = val
		}
		return maskValue(m, extraKeys)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// maskValue walks a JSON-shaped any tree (the output of json.Unmarshal into
// `any`, or the map maskArguments builds for prompt arguments), replacing
// every object value whose key matches shouldMask.
func maskValue(v any, extraKeys []string) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if shouldMask(k, extraKeys) {
				out[k] = "***"
				continue
			}
			out[k] = maskValue(val, extraKeys)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = maskValue(val, extraKeys)
		}
		return out
	default:
		return t
	}
}

// shouldMask reports whether key matches a default or extra mask pattern,
// case-insensitively, by substring.
func shouldMask(key string, extraKeys []string) bool {
	lower := strings.ToLower(key)
	for _, p := range defaultMaskKeyPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	for _, p := range extraKeys {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// hasArgs reports whether args carries meaningful call arguments. args is
// nil for resource reads; a typed-nil json.RawMessage or map[string]string
// — which a plain `args != nil` interface check would not catch — also
// means "no arguments" (e.g. a tool/prompt call that never set Arguments).
func hasArgs(args any) bool {
	switch t := args.(type) {
	case nil:
		return false
	case json.RawMessage:
		if len(t) == 0 {
			return false
		}
		// Check if it's an empty JSON object {} or array []
		trimmed := bytes.TrimSpace(t)
		return len(trimmed) != 2 || (trimmed[0] != '{' && trimmed[0] != '[')
	case map[string]string:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	default:
		return true
	}
}

// logCall logs one backend call's outcome — success or failure — in a
// consistent shape, so investigating an incident doesn't require treating
// the success and error paths as separate log formats.
// kind labels the log message ("tool"/"resource"/"resource template"/"prompt");
// nameKey is the field name for name ("tool"/"uri"/"prompt" — resource and
// resource template both use "uri"). args is nil for resource reads, which
// have no call arguments.
func logCall(ctx context.Context, logger *slog.Logger, kind, nameKey, name, backend string, sess *mcp.ServerSession, args any, maskKeys []string, start time.Time, err error) {
	attrs := []any{
		"backend", backend,
		nameKey, name,
		"session_id", sess.ID(),
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if ip := sess.InitializeParams(); ip != nil && ip.ClientInfo != nil {
		attrs = append(attrs, "client_name", ip.ClientInfo.Name, "client_version", ip.ClientInfo.Version)
	}
	if addr, ok := remoteAddrFromContext(ctx); ok {
		attrs = append(attrs, "remote_addr", addr)
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
	}
	if hasArgs(args) {
		attrs = append(attrs, "arguments", maskArguments(args, maskKeys))
	}
	if err != nil {
		logger.Error(kind+" call failed", append(attrs, "error", err)...)
		return
	}
	logger.Info(kind+" call", attrs...)
}

// remoteAddrKey is the context key ServeHTTP's remoteAddrMiddleware uses to
// carry the client's TCP address to every logCall for that session.
type remoteAddrKey struct{}

// remoteAddrMiddleware stashes r.RemoteAddr in the request context before
// calling next. The MCP SDK reuses the request context that establishes a
// session as that session's base context for every later call on it, so
// this value stays reachable from logCall for the whole session's lifetime,
// not just its first request.
func remoteAddrMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), remoteAddrKey{}, r.RemoteAddr)))
	})
}

// remoteAddrFromContext retrieves the value remoteAddrMiddleware stashed, if
// any. It reports ok=false for stdio sessions (no middleware ever runs) or
// any context that didn't come from an HTTP request through it.
func remoteAddrFromContext(ctx context.Context) (string, bool) {
	addr, ok := ctx.Value(remoteAddrKey{}).(string)
	return addr, ok
}
