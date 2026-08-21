# Audit/Incident Logging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every backend call (tool/resource/resource-template/prompt) and every server-startup milestone emit one structured log line — success or failure — with caller identity and masked arguments, so incidents and audits don't depend on error-only logging.

**Architecture:** A new `internal/gateway/audit.go` centralizes the log shape (`logCall`) and argument masking (`maskArguments`); the four existing `internal/gateway/gateway.go` handlers call it once each, replacing their error-only `logger.Error(...)` calls. `ServeHTTP` gains a small middleware that stashes `r.RemoteAddr` in the request context so it's visible from `logCall` on every subsequent call in that session. `internal/cli/server.go` gains two startup success logs and a `--log-format text|json` flag. `internal/config` gains `logging.mask_keys` to extend the built-in masking patterns.

**Tech Stack:** Go 1.x, `log/slog`, `github.com/modelcontextprotocol/go-sdk` v1.7.0, `gopkg.in/yaml.v3`, standard `testing`.

**Spec:** `docs/superpowers/specs/2026-08-21-audit-logging-design.md`

## Global Constraints

- Every backend call handler logs exactly one line per call, success or failure — `logger.Info` on success, `logger.Error` on failure — never both, never neither.
- Argument masking matches key names case-insensitively by substring against `defaultMaskKeyPatterns = []string{"key", "auth", "pass", "cred", "token"}` plus any `logging.mask_keys` entries from config.
- `resources/read` (exact and template) handlers never log an `arguments` field — resources have no call arguments.
- `remote_addr` only ever appears for HTTP-served sessions, via `ServeHTTP`'s middleware; stdio sessions never carry it, and that is not an error.
- No new external dependencies. `go build ./...`, `go vet ./...`, `gofmt -l .`, and `go test ./...` must all be clean at the end of every task.
- Run `golangci-lint run ./...` at the end of every task if the `golangci-lint` binary is available (`command -v golangci-lint`); skip silently if not.

---

### Task 1: `logging.mask_keys` config field

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.LoggingConfig{MaskKeys []string}` and `config.Config.Logging LoggingConfig`, consumed by Task 6 (`cfg.Logging.MaskKeys` passed into `gateway.New`).

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`, after `TestParse_ValidConfig` (or anywhere top-level; exact position doesn't matter):

```go
func TestParse_LoggingMaskKeys(t *testing.T) {
	data := []byte(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

logging:
  mask_keys: ["internal_id", "session_token"]
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Logging.MaskKeys) != 2 || cfg.Logging.MaskKeys[0] != "internal_id" || cfg.Logging.MaskKeys[1] != "session_token" {
		t.Fatalf("Logging.MaskKeys = %v, want [internal_id session_token]", cfg.Logging.MaskKeys)
	}
}

func TestParse_LoggingMaskKeysDefaultEmpty(t *testing.T) {
	data := []byte(`
backends:
  - name: a
    transport: stdio
    command: ["x"]
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Logging.MaskKeys) != 0 {
		t.Fatalf("Logging.MaskKeys = %v, want empty", cfg.Logging.MaskKeys)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/... -run TestParse_LoggingMaskKeys -v`
Expected: FAIL with `cfg.Logging undefined (type *config.Config has no field or method Logging)`.

- [ ] **Step 3: Add `LoggingConfig` and wire it into `Config`**

In `internal/config/config.go`, add the field to `Config` (after `PromptOverrides`):

```go
type Config struct {
	Listen                    ListenConfig      `yaml:"listen"`
	Backends                  []BackendConfig   `yaml:"backends"`
	Overrides                 map[string]string `yaml:"overrides,omitempty"`
	ResourceOverrides         map[string]string `yaml:"resource_overrides,omitempty"`
	ResourceTemplateOverrides map[string]string `yaml:"resource_template_overrides,omitempty"`
	PromptOverrides           map[string]string `yaml:"prompt_overrides,omitempty"`
	Logging                   LoggingConfig     `yaml:"logging,omitempty"`
}
```

Add the new type below `DockerConfig` (or anywhere at package level after the existing type declarations):

```go
// LoggingConfig controls audit-log behavior beyond what --log-level/--log-format
// (CLI flags) cover.
type LoggingConfig struct {
	// MaskKeys are extra case-insensitive substrings matched against
	// argument key names, in addition to the built-in defaultMaskKeyPatterns
	// ("key", "auth", "pass", "cred", "token") gateway.maskArguments uses.
	MaskKeys []string `yaml:"mask_keys,omitempty"`
}
```

No `validate()` changes are needed: `MaskKeys` is a free-form string list with no constraints.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: PASS (all config tests, including the two new ones and every pre-existing one).

- [ ] **Step 5: Format, vet, lint**

Run: `gofmt -l internal/config/config.go internal/config/config_test.go && go vet ./internal/config/... && (command -v golangci-lint >/dev/null && golangci-lint run ./internal/config/... || true)`
Expected: `gofmt -l` prints nothing; `go vet` and lint report no issues.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: add logging.mask_keys config field"
```

---

### Task 2: `maskArguments` (argument masking)

**Files:**
- Create: `internal/gateway/audit.go`
- Test: `internal/gateway/audit_test.go`

**Interfaces:**
- Consumes: nothing from other tasks (standalone).
- Produces: `maskArguments(v any, extraKeys []string) any` and `defaultMaskKeyPatterns []string`, both unexported (package `gateway`), consumed by Task 3's `logCall`.

- [ ] **Step 1: Write the failing tests**

Create `internal/gateway/audit_test.go`:

```go
package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gateway/... -run TestMaskArguments -v`
Expected: FAIL with `undefined: maskArguments` (the build fails since `audit.go` doesn't exist yet).

- [ ] **Step 3: Implement `maskArguments`**

Create `internal/gateway/audit.go`:

```go
package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gateway/... -run TestMaskArguments -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Format, vet, lint**

Run: `gofmt -l internal/gateway/audit.go internal/gateway/audit_test.go && go vet ./internal/gateway/... && (command -v golangci-lint >/dev/null && golangci-lint run ./internal/gateway/... || true)`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/audit.go internal/gateway/audit_test.go
git commit -m "feat: add key-name based argument masking for audit logging"
```

---

### Task 3: `logCall` + HTTP remote-address context propagation

**Files:**
- Modify: `internal/gateway/audit.go`
- Modify: `internal/gateway/audit_test.go`

**Interfaces:**
- Consumes: `maskArguments` (Task 2).
- Produces: `logCall(ctx context.Context, logger *slog.Logger, kind, nameKey, name, backend string, sess *mcp.ServerSession, args any, maskKeys []string, start time.Time, err error)`, `remoteAddrMiddleware(next http.Handler) http.Handler`, `remoteAddrFromContext(ctx context.Context) (string, bool)` — all consumed by Task 4 (`logCall` from the four handlers) and Task 5 (`remoteAddrMiddleware` from `ServeHTTP`).

- [ ] **Step 1: Write the failing tests**

Append to `internal/gateway/audit_test.go` (add these imports to the existing `import` block: `"bytes"`, `"context"`, `"encoding/json"` is already there, `"errors"`, `"log/slog"`, `"net/http"`, `"net/http/httptest"`, `"strings"`, `"time"`, and `"github.com/modelcontextprotocol/go-sdk/mcp"`):

```go
func TestLogCall_Success(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	sess := &mcp.ServerSession{}
	start := time.Now().Add(-5 * time.Millisecond)

	logCall(context.Background(), logger, "tool", "tool", "mytool", "backend-a", sess,
		json.RawMessage(`{"user":"alice"}`), nil, start, nil)

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
		nil, nil, time.Now(), errors.New("boom"))

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

func TestLogCall_RemoteAddrFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	sess := &mcp.ServerSession{}
	ctx := context.WithValue(context.Background(), remoteAddrKey{}, "127.0.0.1:5555")

	logCall(ctx, logger, "resource", "uri", "file:///a", "backend-a", sess, nil, nil, time.Now(), nil)

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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gateway/... -run 'TestLogCall|TestRemoteAddrMiddleware' -v`
Expected: FAIL with `undefined: logCall` / `undefined: remoteAddrKey` / `undefined: remoteAddrMiddleware` / `undefined: remoteAddrFromContext`.

- [ ] **Step 3: Implement `logCall` and the remote-address helpers**

Append to `internal/gateway/audit.go` (and add `"context"`, `"log/slog"`, `"net/http"`, `"time"`, and `"github.com/modelcontextprotocol/go-sdk/mcp"` to its import block):

```go
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
	if args != nil {
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gateway/... -v`
Expected: PASS, every test in the package (Task 2's `TestMaskArguments` plus this task's four new tests).

- [ ] **Step 5: Format, vet, lint**

Run: `gofmt -l internal/gateway/audit.go internal/gateway/audit_test.go && go vet ./internal/gateway/... && (command -v golangci-lint >/dev/null && golangci-lint run ./internal/gateway/... || true)`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/audit.go internal/gateway/audit_test.go
git commit -m "feat: add logCall and HTTP remote-address propagation for audit logging"
```

---

### Task 4: Wire `logCall` into the four gateway handlers

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/gateway_test.go`
- Modify: `internal/cli/server.go`

**Interfaces:**
- Consumes: `logCall` (Task 3), `config.Config.Logging.MaskKeys` (Task 1).
- Produces: `gateway.New(logger *slog.Logger, backends map[string]*backend.Backend, tables Tables, maskKeys []string) *mcp.Server` (signature change: new 4th parameter), consumed unchanged by Task 5 and Task 6.

This task changes `gateway.New`'s signature, so `internal/cli/server.go` — the only other caller in the repo — gets its one-line compile fix here too (passing `cfg.Logging.MaskKeys`, already available from Task 1), instead of leaving the whole-repo build broken until Task 6. Task 6 only adds log lines around that already-updated call; it does not touch the call's argument list.

- [ ] **Step 1: Update the 9 existing `gateway.New` call sites in `gateway_test.go`**

These calls don't exercise masking, so they all pass `nil` for the new parameter. Each `old_string` below is a full, unique line in `internal/gateway/gateway_test.go` — replace each with its `new_string`:

1. `srv := gateway.New(logger, map[string]*backend.Backend{"backend-dead": conn}, gateway.Tables{Tools: table})`
   → `srv := gateway.New(logger, map[string]*backend.Backend{"backend-dead": conn}, gateway.Tables{Tools: table}, nil)`
2. `srv := gateway.New(logger, map[string]*backend.Backend{"backend-b": connB}, gateway.Tables{Tools: table})`
   → `srv := gateway.New(logger, map[string]*backend.Backend{"backend-b": connB}, gateway.Tables{Tools: table}, nil)`
3. `srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA, "backend-b": connB}, gateway.Tables{Tools: table})`
   → `srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA, "backend-b": connB}, gateway.Tables{Tools: table}, nil)`
4. `srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Resources: table})`
   → `srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Resources: table}, nil)`
5. `srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{ResourceTemplates: table})`
   → `srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{ResourceTemplates: table}, nil)`
6. `srv := gateway.New(logger, map[string]*backend.Backend{"backend-b": connB}, gateway.Tables{Resources: table})`
   → `srv := gateway.New(logger, map[string]*backend.Backend{"backend-b": connB}, gateway.Tables{Resources: table}, nil)`
7. `srv := gateway.New(logger, map[string]*backend.Backend{"backend": conn}, gateway.Tables{Prompts: table})`
   → `srv := gateway.New(logger, map[string]*backend.Backend{"backend": conn}, gateway.Tables{Prompts: table}, nil)`
8. `srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA, "backend-b": connB}, gateway.Tables{Prompts: table})`
   → `srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA, "backend-b": connB}, gateway.Tables{Prompts: table}, nil)`
9. `srv := gateway.New(logger, map[string]*backend.Backend{"backend-dead": conn}, gateway.Tables{Prompts: table})`
   → `srv := gateway.New(logger, map[string]*backend.Backend{"backend-dead": conn}, gateway.Tables{Prompts: table}, nil)`

(These are compile-fixing changes required by Step 3 below, not new test behavior — there's no separate "run to see it fail" for this step; the whole task's tests are verified together in Step 4.)

- [ ] **Step 2: Write two new failing tests for the actual behavior change**

Add to `internal/gateway/gateway_test.go` (add `"bytes"` and `"encoding/json"` to its import block):

```go
// TestGateway_CallLogsSuccessWithMaskedArguments checks the core behavior
// change: a successful tool call now logs one Info line (previously none),
// carrying caller identity and masked arguments.
func TestGateway_CallLogsSuccessWithMaskedArguments(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	backendServer := newFakeBackendServer("backend-a", "secure")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-a", Items: toolsA}}, toolNameOf, toolRename, nil)

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, []string{"secret_value"})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "secure", Arguments: map[string]any{"user": "alice", "secret_value": "hunter2"}})
	if err != nil {
		t.Fatalf("call secure: %v", err)
	}

	rec := findLogLine(t, buf.String(), "tool call")
	if rec["backend"] != "backend-a" || rec["tool"] != "secure" {
		t.Fatalf("backend/tool = %v/%v, want backend-a/secure", rec["backend"], rec["tool"])
	}
	if rec["session_id"] == "" || rec["session_id"] == nil {
		t.Fatalf("session_id = %v, want non-empty", rec["session_id"])
	}
	if rec["client_name"] != "test-client" || rec["client_version"] != "v1" {
		t.Fatalf("client_name/client_version = %v/%v, want test-client/v1", rec["client_name"], rec["client_version"])
	}
	args, ok := rec["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("arguments = %v, want a map", rec["arguments"])
	}
	if args["user"] != "alice" {
		t.Fatalf("arguments.user = %v, want alice (unmasked)", args["user"])
	}
	if args["secret_value"] != "***" {
		t.Fatalf("arguments.secret_value = %v, want *** (masked via config-supplied maskKeys)", args["secret_value"])
	}
}

// TestGateway_CallLogsFailure checks that a failed tool call logs one Error
// line with the failure reason, alongside the same caller-identity fields
// the success path carries.
func TestGateway_CallLogsFailure(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	backendServer := newFakeBackendServer("backend-dead", "boom")
	httpBackend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpBackend.Close()

	ctx := context.Background()
	conn, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-dead", Transport: "http", URL: httpBackend.URL})
	if err != nil {
		t.Fatalf("connect backend-dead: %v", err)
	}
	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-dead tools: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-dead", Items: tools}}, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-dead": conn}, gateway.Tables{Tools: table}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	if err := conn.Close(); err != nil {
		t.Fatalf("close backend-dead: %v", err)
	}

	_, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "boom", Arguments: map[string]any{}})

	rec := findLogLine(t, buf.String(), "tool call failed")
	if rec["level"] != "ERROR" {
		t.Fatalf("level = %v, want ERROR", rec["level"])
	}
	if rec["error"] == "" || rec["error"] == nil {
		t.Fatalf("error field = %v, want non-empty", rec["error"])
	}
}

// findLogLine returns the first JSON log line in out whose "msg" equals
// wantMsg, failing the test if none matches.
func findLogLine(t *testing.T, out, wantMsg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err == nil && rec["msg"] == wantMsg {
			return rec
		}
	}
	t.Fatalf("log output = %q, want a line with msg=%q", out, wantMsg)
	return nil
}
```

Also add `"strings"` to the import block if not already present (it currently isn't in `gateway_test.go`).

- [ ] **Step 3: Run the new tests to verify they fail**

Run: `go test ./internal/gateway/... -run 'TestGateway_CallLogsSuccessWithMaskedArguments|TestGateway_CallLogsFailure' -v`
Expected: FAIL to compile — `gateway.New` still takes 3 arguments, and `findLogLine` matches nothing yet since `logCall` isn't wired into `callHandler`.

- [ ] **Step 4: Wire `logCall` into the four handlers and thread `maskKeys` through**

In `internal/gateway/gateway.go`, make these exact changes:

Change `New`'s signature and body:

```go
func New(logger *slog.Logger, backends map[string]*backend.Backend, tables Tables, maskKeys []string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{Logger: logger})

	if tables.Tools != nil {
		for _, resolved := range tables.Tools.Items {
			registerTool(srv, logger, backends, resolved, maskKeys)
		}
	}
	if tables.Resources != nil {
		for _, resolved := range tables.Resources.Items {
			registerResource(srv, logger, backends, resolved, maskKeys)
		}
	}
	if tables.ResourceTemplates != nil {
		for _, resolved := range tables.ResourceTemplates.Items {
			registerResourceTemplate(srv, logger, backends, resolved, maskKeys)
		}
	}
	if tables.Prompts != nil {
		for _, resolved := range tables.Prompts.Items {
			registerPrompt(srv, logger, backends, resolved, maskKeys)
		}
	}

	return srv
}
```

Change `registerTool`:

```go
func registerTool(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Tool], maskKeys []string) {
	candidates := append([]router.Candidate[*mcp.Tool]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addTool(srv, logger, c.Item, callHandler(logger, maskKeys, b, c.OriginalName)) {
			return
		}
	}
	logger.Error("tool unavailable: every candidate backend had an invalid definition", "tool", resolved.Item.Name)
}
```

Change `registerResource`:

```go
func registerResource(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Resource], maskKeys []string) {
	candidates := append([]router.Candidate[*mcp.Resource]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addResource(srv, logger, c.Item, resourceReadHandler(logger, maskKeys, b, c.OriginalName)) {
			return
		}
	}
	logger.Error("resource unavailable: every candidate backend had an invalid URI", "uri", resolved.Item.URI)
}
```

Change `resourceReadHandler` (adds `start`/`logCall`, `args` is always `nil`):

```go
func resourceReadHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalURI string) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		start := time.Now()
		result, err := b.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: originalURI})
		logCall(ctx, logger, "resource", "uri", originalURI, b.Name, req.Session, nil, maskKeys, start, err)
		return result, err
	}
}
```

Change `registerResourceTemplate`:

```go
func registerResourceTemplate(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.ResourceTemplate], maskKeys []string) {
	candidates := append([]router.Candidate[*mcp.ResourceTemplate]{{
		Item:         resolved.Item,
		BackendName:  resolved.BackendName,
		OriginalName: resolved.OriginalName,
	}}, resolved.Fallbacks...)

	for _, c := range candidates {
		b := backends[c.BackendName]
		if addResourceTemplate(srv, logger, c.Item, resourceTemplateReadHandler(logger, maskKeys, b)) {
			return
		}
	}
	logger.Error("resource template unavailable: every candidate backend had an invalid URI template", "uriTemplate", resolved.Item.URITemplate)
}
```

Change `resourceTemplateReadHandler`:

```go
func resourceTemplateReadHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		start := time.Now()
		result, err := b.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
		logCall(ctx, logger, "resource template", "uri", req.Params.URI, b.Name, req.Session, nil, maskKeys, start, err)
		return result, err
	}
}
```

Change `registerPrompt`:

```go
func registerPrompt(srv *mcp.Server, logger *slog.Logger, backends map[string]*backend.Backend, resolved *router.Resolved[*mcp.Prompt], maskKeys []string) {
	b := backends[resolved.BackendName]
	srv.AddPrompt(resolved.Item, promptGetHandler(logger, maskKeys, b, resolved.OriginalName))
}
```

Change `promptGetHandler`:

```go
func promptGetHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalName string) mcp.PromptHandler {
	return func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		start := time.Now()
		result, err := b.Session.GetPrompt(ctx, &mcp.GetPromptParams{
			Name:      originalName,
			Arguments: req.Params.Arguments,
		})
		logCall(ctx, logger, "prompt", "prompt", originalName, b.Name, req.Session, req.Params.Arguments, maskKeys, start, err)
		return result, err
	}
}
```

Change `callHandler`:

```go
func callHandler(logger *slog.Logger, maskKeys []string, b *backend.Backend, originalName string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		result, err := b.Session.CallTool(ctx, &mcp.CallToolParams{
			Name:      originalName,
			Arguments: req.Params.Arguments,
		})
		logCall(ctx, logger, "tool", "tool", originalName, b.Name, req.Session, req.Params.Arguments, maskKeys, start, err)
		return result, err
	}
}
```

Note the doc comment above `callHandler` ("A failure is returned to the client and logged...") is still accurate but now understates it (success is logged too) — update it to:

```go
// callHandler forwards a tools/call to originalName on backend b, passing
// the raw arguments through unchanged. Every call is logged via logCall,
// success or failure, so a dead or erroring backend — and normal usage — is
// visible to the operator.
```

- [ ] **Step 5: Fix the other `gateway.New` caller in `internal/cli/server.go`**

`gateway.New` now takes a 4th `maskKeys []string` argument, so its one other caller in the repo needs updating too — otherwise the whole build stays broken until Task 6, which would leave this task's `go build ./...` unclean. In `internal/cli/server.go`, change:

```go
	srv := gateway.New(logger, conn.backends, gateway.Tables{
		Tools:             toolTable,
		Resources:         resourceTable,
		ResourceTemplates: resourceTemplateTable,
		Prompts:           promptTable,
	})
```

to:

```go
	srv := gateway.New(logger, conn.backends, gateway.Tables{
		Tools:             toolTable,
		Resources:         resourceTable,
		ResourceTemplates: resourceTemplateTable,
		Prompts:           promptTable,
	}, cfg.Logging.MaskKeys)
```

This is the only change this task makes to `internal/cli`; Task 6 adds the startup log lines around this same call without touching its argument list again.

- [ ] **Step 6: Run all gateway and cli tests to verify they pass**

Run: `go build ./... && go vet ./... && go test ./internal/gateway/... ./internal/cli/... -v`
Expected: PASS — clean build across the whole repo, and every test in both packages green.

- [ ] **Step 7: Format, vet, lint**

Run: `gofmt -l internal/gateway/gateway.go internal/gateway/gateway_test.go internal/cli/server.go && go vet ./... && (command -v golangci-lint >/dev/null && golangci-lint run ./... || true)`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go internal/cli/server.go
git commit -m "feat: log every backend call (success and failure) with masked arguments"
```

---

### Task 5: `ServeHTTP` remote-address middleware

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/gateway_test.go`

**Interfaces:**
- Consumes: `remoteAddrMiddleware` (Task 3), `gateway.New`'s 4-arg signature (Task 4).
- Produces: nothing new consumed by later tasks — `ServeHTTP`'s exported signature is unchanged.

- [ ] **Step 1: Write the failing test**

Add to `internal/gateway/gateway_test.go` (add `"net"` and `"time"` to its import block if not already present — `"time"` isn't currently imported there):

```go
// TestGateway_ServeHTTP_LogsRemoteAddr checks that a call served through the
// real ServeHTTP (unlike the other tests in this file, which wrap srv in a
// bare mcp.NewStreamableHTTPHandler) carries a remote_addr field in its
// audit log line, via ServeHTTP's remoteAddrMiddleware.
func TestGateway_ServeHTTP_LogsRemoteAddr(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	backendServer := newFakeBackendServer("backend-a", "ping")
	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendServer }, nil))
	defer httpA.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	table := router.Resolve([]router.Entry[*mcp.Tool]{{BackendName: "backend-a", Items: toolsA}}, toolNameOf, toolRename, nil)
	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA}, gateway.Tables{Tools: table}, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing probe listener: %v", err)
	}

	gwCtx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- gateway.ServeHTTP(gwCtx, srv, addr) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + addr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}

	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "ping", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call ping: %v", err)
	}
	_ = session.Close()

	cancel()
	if err := <-serveErr; err != nil {
		t.Fatalf("ServeHTTP exited with error: %v", err)
	}

	rec := findLogLine(t, buf.String(), "tool call")
	addrField, ok := rec["remote_addr"].(string)
	if !ok || !strings.HasPrefix(addrField, "127.0.0.1:") {
		t.Fatalf("remote_addr = %v, want a 127.0.0.1:<port> string", rec["remote_addr"])
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/gateway/... -run TestGateway_ServeHTTP_LogsRemoteAddr -v`
Expected: FAIL — the log line's `remote_addr` field is absent (`ok` is false), since `ServeHTTP` doesn't wrap the handler with `remoteAddrMiddleware` yet.

- [ ] **Step 3: Wrap `ServeHTTP`'s handler with the middleware**

In `internal/gateway/gateway.go`, change:

```go
func ServeHTTP(ctx context.Context, srv *mcp.Server, addr string) error {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	httpServer := &http.Server{Addr: addr, Handler: handler}
```

to:

```go
func ServeHTTP(ctx context.Context, srv *mcp.Server, addr string) error {
	handler := remoteAddrMiddleware(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	httpServer := &http.Server{Addr: addr, Handler: handler}
```

- [ ] **Step 4: Run all gateway tests to verify they pass**

Run: `go test ./internal/gateway/... -v`
Expected: PASS, every test in the package.

- [ ] **Step 5: Format, vet, lint**

Run: `gofmt -l internal/gateway/gateway.go internal/gateway/gateway_test.go && go vet ./internal/gateway/... && (command -v golangci-lint >/dev/null && golangci-lint run ./internal/gateway/... || true)`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go
git commit -m "feat: propagate HTTP remote_addr into audit log lines"
```

---

### Task 6: CLI startup success logs

**Files:**
- Modify: `internal/cli/server.go`
- Modify: `internal/cli/server_internal_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `runServer`'s existing `gateway.New(...)` call, already passing `cfg.Logging.MaskKeys` as of Task 4 — this task only adds log lines around it, it does not touch that call's arguments.
- Produces: nothing new consumed by later tasks.

- [ ] **Step 1: Write the failing tests**

Add to `internal/cli/server_internal_test.go` (add `"os"`, `"path/filepath"`, and `"strings"` to its import block — `"bytes"` and `"encoding/json"` are already imported):

```go
func TestConnectBackends_LogsSuccessfulConnect(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	configs := []config.BackendConfig{{Name: "fake", Transport: "http", URL: backendHTTP.URL}}

	conn := connectBackends(context.Background(), logger, configs)
	defer func() {
		for _, b := range conn.backends {
			_ = b.Close()
		}
	}()

	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Msg       string `json:"msg"`
			Backend   string `json:"backend"`
			Transport string `json:"transport"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "backend connected" {
			found = true
			if rec.Backend != "fake" || rec.Transport != "http" {
				t.Fatalf("backend connected log = %+v, want backend=fake transport=http", rec)
			}
		}
	}
	if !found {
		t.Fatalf("log output = %q, want a \"backend connected\" entry", buf.String())
	}
}

// TestRunServer_LogsListening checks runServer logs its listener
// configuration once at startup, before any listener goroutine starts. It
// swaps os.Stdin the same way TestServerCommand_StdioShutdownIsClean
// (server_test.go) does, so ServeStdio blocks on a pipe instead of the real
// stdin; this must not run with t.Parallel() (nor alongside another test
// touching os.Stdin).
func TestRunServer_LogsListening(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stdin pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = origStdin
		_ = w.Close()
		_ = r.Close()
	})

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("listen:\n  stdio: true\n\nbackends: []\n"), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, logger, configPath) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServer exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer did not exit within 5s of context cancellation")
	}

	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Msg   string `json:"msg"`
			Stdio bool   `json:"stdio"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "listening" {
			found = true
			if !rec.Stdio {
				t.Fatalf("listening log stdio = %v, want true", rec.Stdio)
			}
		}
	}
	if !found {
		t.Fatalf("log output = %q, want a \"listening\" entry", buf.String())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/... -run 'TestConnectBackends_LogsSuccessfulConnect|TestRunServer_LogsListening' -v`
Expected: FAIL — no `"backend connected"` or `"listening"` log lines are found yet (the package already builds cleanly at this point; Task 4 already updated `server.go`'s `gateway.New` call).

- [ ] **Step 3: Add the startup logs**

In `internal/cli/server.go`, in `connectBackends`'s goroutine, change:

```go
			b, err := backend.Connect(ctx, bc)
			if err != nil {
				logger.Error("skipping backend: connect failed", "backend", bc.Name, "error", err)
				return
			}
			tools, err := b.ListTools(ctx)
```

to:

```go
			b, err := backend.Connect(ctx, bc)
			if err != nil {
				logger.Error("skipping backend: connect failed", "backend", bc.Name, "error", err)
				return
			}
			logger.Info("backend connected", "backend", bc.Name, "transport", bc.Transport)
			tools, err := b.ListTools(ctx)
```

In `runServer`, change:

```go
	srv := gateway.New(logger, conn.backends, gateway.Tables{
		Tools:             toolTable,
		Resources:         resourceTable,
		ResourceTemplates: resourceTemplateTable,
		Prompts:           promptTable,
	}, cfg.Logging.MaskKeys)

	running := 0
```

to:

```go
	srv := gateway.New(logger, conn.backends, gateway.Tables{
		Tools:             toolTable,
		Resources:         resourceTable,
		ResourceTemplates: resourceTemplateTable,
		Prompts:           promptTable,
	}, cfg.Logging.MaskKeys)

	logger.Info("listening", "stdio", cfg.Listen.Stdio, "http", cfg.Listen.HTTP)

	running := 0
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/... -v`
Expected: PASS, every test in the package.

- [ ] **Step 5: Run the full build**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Document `logging.mask_keys` in the README**

In `README.md`, add a `logging:` block to the "Minimal example" config sample (after the `prompt_overrides:` block) and a short explanatory paragraph. Find:

```
    prompt_overrides:
      code-review: filesystem
```

Replace with:

```
    prompt_overrides:
      code-review: filesystem

    logging:
      mask_keys: ["internal_id"] # extra key-name substrings to mask in the audit log, in addition to the built-in key/auth/pass/cred/token patterns
```

Then, after the existing paragraph that begins "`prompt_overrides` resolves conflicting **prompt** names..." (ends with "...are not relayed."), add a new paragraph:

```
Every backend call (`tools/call`, `resources/read`, `prompts/get`) is logged
one line per call, success or failure, at `info`/`error` level respectively,
with the calling MCP client's name/version, session ID, HTTP remote address
(HTTP sessions only), call duration, and the call's arguments. Any argument
object key matching (case-insensitively, by substring) `key`, `auth`,
`pass`, `cred`, `token`, or an entry in `logging.mask_keys` has its value
replaced with `***` before logging.
```

- [ ] **Step 7: Format, vet, lint**

Run: `gofmt -l internal/cli/server.go internal/cli/server_internal_test.go && go vet ./internal/cli/... && (command -v golangci-lint >/dev/null && golangci-lint run ./internal/cli/... || true)`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go README.md
git commit -m "feat: log successful backend connects and listener startup, wire mask_keys config"
```

---

### Task 7: `--log-format text|json` flag

**Files:**
- Modify: `internal/cli/server.go`
- Modify: `internal/cli/server_internal_test.go`
- Modify: `internal/cli/server_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: nothing from other tasks (independent of Tasks 1–6 besides sharing `server.go`).
- Produces: nothing consumed by later tasks (this is the last task).

- [ ] **Step 1: Write the failing tests**

Add to `internal/cli/server_internal_test.go` (add `"io"` is already imported):

```go
func TestParseLogFormat(t *testing.T) {
	if _, err := parseLogFormat("text"); err != nil {
		t.Fatalf("parseLogFormat(\"text\"): %v", err)
	}
	if _, err := parseLogFormat("json"); err != nil {
		t.Fatalf("parseLogFormat(\"json\"): %v", err)
	}
	if _, err := parseLogFormat("bogus"); err == nil {
		t.Fatal("parseLogFormat(\"bogus\"): expected error, got nil")
	}

	newHandler, err := parseLogFormat("json")
	if err != nil {
		t.Fatalf("parseLogFormat(\"json\"): %v", err)
	}
	var buf bytes.Buffer
	logger := slog.New(newHandler(&buf, nil))
	logger.Info("hello")
	var rec struct {
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil || rec.Msg != "hello" {
		t.Fatalf("json handler output = %q, want a JSON line with msg=hello", buf.String())
	}
}
```

Add to `internal/cli/server_test.go`:

```go
func TestServerCommand_LogFormatInvalid(t *testing.T) {
	configPath := writeConfig(t, "listen:\n  stdio: true\n\nbackends: []\n")
	err := cli.Execute(context.Background(), []string{"server", "--config", configPath, "--log-format", "bogus"})
	if err == nil {
		t.Fatal("Execute: expected error for invalid --log-format, got nil")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/... -run 'TestParseLogFormat|TestServerCommand_LogFormatInvalid' -v`
Expected: FAIL with `undefined: parseLogFormat` and (for the second test) the command succeeding instead of erroring, since `--log-format` isn't a registered flag yet (cobra will actually error on the *unknown flag* itself, which also satisfies "expected error" — the important failure to fix is the missing `parseLogFormat` function, confirmed by the first test failing to compile).

- [ ] **Step 3: Add `parseLogFormat` and the `--log-format` flag**

In `internal/cli/server.go`, add `"io"` to the import block, then change `newServerCmd`:

```go
func newServerCmd() *cobra.Command {
	var configPath string
	var logLevel string
	var logFormat string

	// SilenceUsage/SilenceErrors: don't dump flag usage on a runtime error
	// (bad config, backend failure, etc.), and let cli.Execute's caller
	// (main.go) be the one place that prints the error.
	cmd := &cobra.Command{
		Use:           "server",
		Short:         "run the mcprt gateway server",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			level, err := parseLogLevel(logLevel)
			if err != nil {
				return err
			}
			newHandler, err := parseLogFormat(logFormat)
			if err != nil {
				return err
			}
			logger := slog.New(newHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))
			return runServer(cmd.Context(), logger, configPath)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to the gateway config file (required)")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, error")
	cmd.Flags().StringVar(&logFormat, "log-format", "text", "log format: text or json")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err) // programmer error: "config" flag name must match Flags().StringVar above
	}

	return cmd
}
```

Add `parseLogFormat` next to `parseLogLevel`:

```go
func parseLogFormat(s string) (func(io.Writer, *slog.HandlerOptions) slog.Handler, error) {
	switch s {
	case "text":
		return func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewTextHandler(w, o) }, nil
	case "json":
		return func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewJSONHandler(w, o) }, nil
	default:
		return nil, fmt.Errorf("unknown log format %q (want text or json)", s)
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/... -v`
Expected: PASS, every test in the package.

- [ ] **Step 5: Run the full test suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, every package in the repo.

- [ ] **Step 6: Document `--log-format` in the README**

In `README.md`, find:

```
    mcprt server --config config.yaml [--log-level info]
```

Replace with:

```
    mcprt server --config config.yaml [--log-level info] [--log-format text]
```

- [ ] **Step 7: Format, vet, lint**

Run: `gofmt -l . && go vet ./... && (command -v golangci-lint >/dev/null && golangci-lint run ./... || true)`
Expected: clean across the whole repo.

- [ ] **Step 8: Commit**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go internal/cli/server_test.go README.md
git commit -m "feat: add --log-format text|json flag"
```
