package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"
)

func TestHasArgs(t *testing.T) {
	cases := []struct {
		name string
		args any
		want bool
	}{
		{
			name: "nil args",
			args: nil,
			want: false,
		},
		{
			name: "empty json.RawMessage (typed nil from wire)",
			args: json.RawMessage(nil),
			want: false,
		},
		{
			name: "json.RawMessage containing empty object {}",
			args: json.RawMessage(`{}`),
			want: false,
		},
		{
			name: "json.RawMessage containing empty array []",
			args: json.RawMessage(`[]`),
			want: false,
		},
		{
			name: "non-empty json.RawMessage",
			args: json.RawMessage(`{"user":"alice"}`),
			want: true,
		},
		{
			name: "empty map[string]string",
			args: map[string]string{},
			want: false,
		},
		{
			name: "nil map[string]string (typed nil)",
			args: func() map[string]string { var m map[string]string; return m }(),
			want: false,
		},
		{
			name: "non-empty map[string]string",
			args: map[string]string{"name": "world"},
			want: true,
		},
		{
			name: "empty map[string]any (tool call with empty Arguments)",
			args: map[string]any{},
			want: false,
		},
		{
			name: "non-empty map[string]any",
			args: map[string]any{"user": "alice"},
			want: true,
		},
		{
			name: "other types return true",
			args: "string",
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hasArgs(c.args)
			if got != c.want {
				t.Fatalf("hasArgs(%#v) = %v, want %v", c.args, got, c.want)
			}
		})
	}
}

func TestMaskArguments(t *testing.T) {
	cases := []struct {
		name      string
		v         any
		extraKeys []string
		want      any
	}{
		{
			name: "flat object masks a default-pattern key, keeps others",
			v:    json.RawMessage(`{"api_key":"secret123","name":"alice"}`),
			want: map[string]any{"api_key": "***", "name": "alice"},
		},
		{
			name: "nested object masks at any depth",
			v:    json.RawMessage(`{"config":{"password":"hunter2","name":"y"}}`),
			want: map[string]any{"config": map[string]any{"password": "***", "name": "y"}},
		},
		{
			name: "array of objects masks within each element",
			v:    json.RawMessage(`[{"token":"a"},{"note":"b"}]`),
			want: []any{map[string]any{"token": "***"}, map[string]any{"note": "b"}},
		},
		{
			name: "prompt arguments (map[string]string) are masked the same way",
			v:    map[string]string{"authorization": "Bearer xyz", "topic": "go"},
			want: map[string]any{"authorization": "***", "topic": "go"},
		},
		{
			name:      "extraKeys mask in addition to the defaults",
			v:         json.RawMessage(`{"internal_id":"42","name":"alice"}`),
			extraKeys: []string{"internal_id"},
			want:      map[string]any{"internal_id": "***", "name": "alice"},
		},
		{
			name: "case-insensitive substring matching",
			v:    json.RawMessage(`{"APIKey":"x","Credential_ID":"y","Passwd":"z","access_token":"w"}`),
			want: map[string]any{"APIKey": "***", "Credential_ID": "***", "Passwd": "***", "access_token": "***"},
		},
		{
			name: "scalar RawMessage is returned unchanged",
			v:    json.RawMessage(`"hello"`),
			want: "hello",
		},
		{
			name: "malformed RawMessage falls back to its raw string form",
			v:    json.RawMessage(`not json`),
			want: "not json",
		},
		{
			name: "unsupported type falls back to fmt.Sprintf(\"%v\", v)",
			v:    42,
			want: "42",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := maskArguments(c.v, c.extraKeys)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("maskArguments(%#v, %v) = %#v, want %#v", c.v, c.extraKeys, got, c.want)
			}
		})
	}
}

func TestLogCall_Success(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	sess := &mcp.ServerSession{}
	start := time.Now().Add(-5 * time.Millisecond)

	logCall(context.Background(), logger, "tool", "tool", "mytool", "backend-a", sess,
		json.RawMessage(`{"user":"alice"}`), nil, start, nil, nil)

	rec := decodeLastLogLine(t, buf.String())
	if rec["msg"] != "tool call" {
		t.Fatalf("msg = %v, want %q", rec["msg"], "tool call")
	}
	if rec["level"] != "INFO" {
		t.Fatalf("level = %v, want INFO", rec["level"])
	}
	if rec["backend"] != "backend-a" || rec["tool"] != "mytool" {
		t.Fatalf("backend/tool = %v/%v, want backend-a/mytool", rec["backend"], rec["tool"])
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Fatalf("log line %v missing duration_ms", rec)
	}
	if _, ok := rec["client_name"]; ok {
		t.Fatalf("log line %v has client_name, want it omitted (zero-value session has no InitializeParams)", rec)
	}
	if _, ok := rec["remote_addr"]; ok {
		t.Fatalf("log line %v has remote_addr, want it omitted (no value in context)", rec)
	}
	args, ok := rec["arguments"].(map[string]any)
	if !ok || args["user"] != "alice" {
		t.Fatalf("arguments = %v, want map with user=alice", rec["arguments"])
	}
}

func TestLogCall_Failure(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	sess := &mcp.ServerSession{}

	logCall(context.Background(), logger, "tool", "tool", "mytool", "backend-a", sess,
		nil, nil, time.Now(), errors.New("boom"), nil)

	rec := decodeLastLogLine(t, buf.String())
	if rec["msg"] != "tool call failed" {
		t.Fatalf("msg = %v, want %q", rec["msg"], "tool call failed")
	}
	if rec["level"] != "ERROR" {
		t.Fatalf("level = %v, want ERROR", rec["level"])
	}
	if rec["error"] != "boom" {
		t.Fatalf("error = %v, want boom", rec["error"])
	}
	if _, ok := rec["arguments"]; ok {
		t.Fatalf("log line %v has arguments, want it omitted (nil args)", rec)
	}
}

func TestLogCall_ToolCallNoArguments(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	sess := &mcp.ServerSession{}

	// Tool call with no Arguments set (typed-nil json.RawMessage on wire)
	logCall(context.Background(), logger, "tool", "tool", "mytool", "backend-a", sess,
		json.RawMessage(nil), nil, time.Now(), nil, nil)

	rec := decodeLastLogLine(t, buf.String())
	if rec["msg"] != "tool call" {
		t.Fatalf("msg = %v, want %q", rec["msg"], "tool call")
	}
	if _, ok := rec["arguments"]; ok {
		t.Fatalf("log line %v has arguments field, want it omitted for typed-nil json.RawMessage", rec)
	}
}

func TestLogCall_PromptCallNoArguments(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	sess := &mcp.ServerSession{}

	// Prompt call with no Arguments (empty map[string]string)
	logCall(context.Background(), logger, "prompt", "prompt", "myprompt", "backend-a", sess,
		map[string]string{}, nil, time.Now(), nil, nil)

	rec := decodeLastLogLine(t, buf.String())
	if rec["msg"] != "prompt call" {
		t.Fatalf("msg = %v, want %q", rec["msg"], "prompt call")
	}
	if _, ok := rec["arguments"]; ok {
		t.Fatalf("log line %v has arguments field, want it omitted for empty map[string]string", rec)
	}
}

func TestLogCall_RemoteAddrFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	sess := &mcp.ServerSession{}
	ctx := context.WithValue(context.Background(), remoteAddrKey{}, "127.0.0.1:5555")

	logCall(ctx, logger, "resource", "uri", "file:///a", "backend-a", sess, nil, nil, time.Now(), nil, nil)

	rec := decodeLastLogLine(t, buf.String())
	if rec["remote_addr"] != "127.0.0.1:5555" {
		t.Fatalf("remote_addr = %v, want 127.0.0.1:5555", rec["remote_addr"])
	}
	if rec["uri"] != "file:///a" {
		t.Fatalf("uri = %v, want file:///a", rec["uri"])
	}
}

func TestRemoteAddrMiddleware(t *testing.T) {
	var gotAddr string
	var gotOK bool
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAddr, gotOK = remoteAddrFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	rec := httptest.NewRecorder()

	remoteAddrMiddleware(downstream).ServeHTTP(rec, req)

	if !gotOK || gotAddr != "192.0.2.1:1234" {
		t.Fatalf("remoteAddrFromContext = (%q, %v), want (192.0.2.1:1234, true)", gotAddr, gotOK)
	}

	// Without the middleware, the value isn't there.
	gotAddr, gotOK = "", false
	downstream.ServeHTTP(rec, req)
	if gotOK {
		t.Fatalf("remoteAddrFromContext on an unwrapped request = (%q, true), want ok=false", gotAddr)
	}
}

// decodeLastLogLine decodes the last non-empty line of a slog JSON handler's
// output into a generic map, for asserting on individual fields.
func decodeLastLogLine(t *testing.T, out string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatalf("decoding log line %q: %v", lines[len(lines)-1], err)
	}
	return rec
}

func TestLogCall_IncludesTraceIDAndSpanIDWhenSpanValid(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("TraceIDFromHex: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("SpanIDFromHex: %v", err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logCall(ctx, logger, "tool", "tool", "x", "backend-a", &mcp.ServerSession{}, nil, nil, time.Now(), nil, nil)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if rec["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace_id = %v, want 4bf92f3577b34da6a3ce929d0e0e4736", rec["trace_id"])
	}
	if rec["span_id"] != "00f067aa0ba902b7" {
		t.Fatalf("span_id = %v, want 00f067aa0ba902b7", rec["span_id"])
	}
}

func TestLogCall_OmitsTraceIDAndSpanIDWhenNoSpan(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	logCall(context.Background(), logger, "tool", "tool", "x", "backend-a", &mcp.ServerSession{}, nil, nil, time.Now(), nil, nil)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if _, ok := rec["trace_id"]; ok {
		t.Fatalf("trace_id present = %v, want omitted for a ctx with no span", rec["trace_id"])
	}
	if _, ok := rec["span_id"]; ok {
		t.Fatalf("span_id present = %v, want omitted for a ctx with no span", rec["span_id"])
	}
}
