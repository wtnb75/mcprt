package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/wtnb75/mcprt/internal/gateway"
)

// TestLogEvent_WritesEventFieldAndLevel checks that LogEvent produces the
// fixed "gateway event" message, an "event" field carrying the caller's
// event name, the requested level, and any additional args passed through.
func TestLogEvent_WritesEventFieldAndLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	gateway.LogEvent(context.Background(), logger, slog.LevelWarn, "name_conflict", "kind", "tool", "name", "search")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decoding log line %q: %v", buf.String(), err)
	}
	if rec["msg"] != "gateway event" {
		t.Fatalf("msg = %v, want \"gateway event\"", rec["msg"])
	}
	if rec["level"] != "WARN" {
		t.Fatalf("level = %v, want WARN", rec["level"])
	}
	if rec["event"] != "name_conflict" {
		t.Fatalf("event = %v, want name_conflict", rec["event"])
	}
	if rec["kind"] != "tool" || rec["name"] != "search" {
		t.Fatalf("kind/name = %v/%v, want tool/search", rec["kind"], rec["name"])
	}
}

// TestLogEvent_InfoLevel checks that a caller-chosen Info level is honored
// (LogEvent doesn't hardcode a severity), matching the distinction between
// an anomaly (Warn) and a routine-but-audit-worthy state change (Info) the
// design relies on.
func TestLogEvent_InfoLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	gateway.LogEvent(context.Background(), logger, slog.LevelInfo, "list_changed_reconciled", "backend", "fake", "kind", "tools", "count", 3)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decoding log line %q: %v", buf.String(), err)
	}
	if rec["level"] != "INFO" {
		t.Fatalf("level = %v, want INFO", rec["level"])
	}
	if rec["event"] != "list_changed_reconciled" {
		t.Fatalf("event = %v, want list_changed_reconciled", rec["event"])
	}
	if rec["backend"] != "fake" || rec["kind"] != "tools" {
		t.Fatalf("backend/kind = %v/%v, want fake/tools", rec["backend"], rec["kind"])
	}
	if rec["count"] != float64(3) { // JSON numbers decode as float64
		t.Fatalf("count = %v, want 3", rec["count"])
	}
}
