package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/cli"
)

// newFakeCallBackend serves two tools: "echo", which echoes its arguments
// back as text content, and "boom", which always returns an error result.
func newFakeCallBackend(name string) *httptest.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "echo", Description: "echoes arguments back", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args, err := json.Marshal(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(args)}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "boom", Description: "always fails", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "boom failed"}}}, nil
		})
	return httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
}

func TestCallCommand_Text(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	root := cli.NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"call", "--config", configPath, "--args", `{"message":"hi"}`, "echo"})

	// A cancellable context, not context.Background(): runCall's
	// connectBackends (gwH == nil, a production-correct choice for a
	// one-shot CLI command whose process exits right after) spawns a
	// persistent superviseBackend goroutine per backend that keeps retrying
	// for as long as ctx stays alive. In a real `mcprt call` invocation the
	// process exits right after and takes that goroutine with it, but in a
	// test the process doesn't exit, so context.Background() would leak it
	// into later tests, racing their writes to package-level vars like
	// backendConnectTimeout under `go test -race` (see this plan's Task 2
	// review, finding 1). cancel() runs (via defer, LIFO) before
	// backendA.Close() above, so any straggling supervisor stops retrying
	// before the backend it would retry against goes away.
	ctx := t.Context()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, `"message":"hi"`) {
		t.Fatalf("text output = %q, want it to contain the echoed arguments", got)
	}
}

func TestCallCommand_JSON(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	root := cli.NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"call", "--config", configPath, "--args", `{"message":"hi"}`, "--json", "echo"})

	// See TestCallCommand_Text's matching comment: a cancellable context so
	// runCall's backend supervisor goroutine doesn't outlive this test.
	ctx := t.Context()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal JSON output: %v\noutput: %s", err, out.String())
	}
	if len(parsed.Content) != 1 || !strings.Contains(parsed.Content[0].Text, `"message":"hi"`) {
		t.Fatalf("Content = %+v, want one text block containing the echoed arguments", parsed.Content)
	}
	if parsed.IsError {
		t.Fatalf("IsError = true, want false")
	}
}

func TestCallCommand_NoArgs(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	// See TestCallCommand_Text's matching comment: a cancellable context so
	// runCall's backend supervisor goroutine doesn't outlive this test.
	ctx := t.Context()
	if err := cli.Execute(ctx, []string{"call", "--config", configPath, "echo"}); err != nil {
		t.Fatalf("Execute: unexpected error calling with no --args: %v", err)
	}
}

func TestCallCommand_UnknownTool(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	// See TestCallCommand_Text's matching comment: a cancellable context so
	// runCall's backend supervisor goroutine doesn't outlive this test.
	ctx := t.Context()
	if err := cli.Execute(ctx, []string{"call", "--config", configPath, "does-not-exist"}); err == nil {
		t.Fatal("Execute: expected error for unknown tool name, got nil")
	}
}

func TestCallCommand_InvalidArgsJSON(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	// See TestCallCommand_Text's matching comment: a cancellable context so
	// runCall's backend supervisor goroutine doesn't outlive this test. (This
	// particular call fails before connectBackends even runs -- --args is
	// validated first -- but the context is fixed uniformly across this file
	// rather than special-cased per test, since it costs nothing here and
	// keeps the pattern obviously safe against a future reordering inside
	// runCall.)
	ctx := t.Context()
	if err := cli.Execute(ctx, []string{"call", "--config", configPath, "--args", "not json", "echo"}); err == nil {
		t.Fatal("Execute: expected error for invalid --args JSON, got nil")
	}
}

func TestCallCommand_ToolReturnsError(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	// See TestCallCommand_Text's matching comment: a cancellable context so
	// runCall's backend supervisor goroutine doesn't outlive this test.
	ctx := t.Context()
	if err := cli.Execute(ctx, []string{"call", "--config", configPath, "boom"}); err == nil {
		t.Fatal("Execute: expected a non-nil error when the tool result has IsError=true, got nil")
	}
}

// syncCallLogBuffer is a bytes.Buffer guarded by a mutex, safe for the
// background superviseBackend goroutine runCall's connectBackends spawns
// (which keeps logging -- notably "backend disconnected", once the
// streamable-HTTP session's Wait() returns, which can happen essentially
// concurrently with CallTool returning) and the test's own read of the
// captured stderr to touch at the same time. A plain bytes.Buffer here would
// be a genuine data race under `go test -race`: nothing about a one-shot
// CallTool guarantees the supervisor's own log write has quiesced by the
// time ExecuteContext returns and the test reads the buffer.
type syncCallLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncCallLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncCallLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestCallCommand_LogsAuditLineWithMaskedArguments checks that mcprt call
// now emits a "cli tool call" audit-style log line (to stderr, via the
// logger runCall already builds) carrying the backend name, tool name, and
// masked arguments -- mirroring the gateway's own tools/call audit line but
// for the standalone CLI path, which previously logged nothing at all.
func TestCallCommand_LogsAuditLineWithMaskedArguments(t *testing.T) {
	backendA := newFakeCallBackend("backend-a")
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	root := cli.NewRootCmd()
	var out bytes.Buffer
	var errOut syncCallLogBuffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"call", "--config", configPath, "--args", `{"message":"hi","api_key":"secret-value"}`, "echo"})

	ctx := t.Context()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("call: %v", err)
	}

	logged := errOut.String()
	line := findLogLine(t, logged, `msg="cli tool call"`)
	if !strings.Contains(line, "backend=backend-a") {
		t.Fatalf("log line = %q, want backend=backend-a", line)
	}
	if !hasField(line, "tool", "echo") {
		t.Fatalf("log line = %q, want tool=echo", line)
	}
	if !hasField(line, "original_tool", "echo") {
		t.Fatalf("log line = %q, want original_tool=echo", line)
	}
	if strings.Contains(line, "secret-value") {
		t.Fatalf("log line = %q, want api_key's value masked, not present in plaintext", line)
	}
	if !strings.Contains(line, "***") {
		t.Fatalf("log line = %q, want a masked (***) value present", line)
	}
}

// findLogLine splits logged on newlines and returns the first line
// containing anchor -- a msg="..." fragment naming the exact log message
// (slog's TextHandler quotes a message containing spaces, so e.g.
// msg="cli tool call" is not a substring of msg="cli tool call failed",
// which lets callers pick out one specific line rather than accidentally
// matching a similarly-named one). It fails the test (showing the whole
// buffer for debugging) if no line matches.
func findLogLine(t *testing.T, logged, anchor string) string {
	t.Helper()
	for line := range strings.SplitSeq(logged, "\n") {
		if strings.Contains(line, anchor) {
			return line
		}
	}
	t.Fatalf("log output = %q, want a line containing %s", logged, anchor)
	return ""
}

// hasField reports whether line -- a single slog TextHandler log line --
// carries a space-separated key=value token exactly matching key=value.
// This is NOT the same as strings.Contains(line, key+"="+value): logCLICall
// emits both "tool" and "original_tool" on the same line, and
// "original_tool=echo" contains "tool=echo" as a plain substring (the
// "_tool=echo" tail of "original_tool=echo"), so a bare Contains check for
// "tool=echo" would still pass even if the "tool" key itself were dropped
// and only "original_tool" remained. Splitting on spaces (TextHandler
// always separates key=value pairs with exactly one space) and comparing a
// whole token avoids that collision. Do not "simplify" callers of this back
// into strings.Contains.
func hasField(line, key, value string) bool {
	want := key + "=" + value
	return slices.Contains(strings.Fields(line), want)
}

// TestCallCommand_LogsAuditLineOnFailure checks the failure path: a tool
// call that itself fails with a Go error (not just an IsError result) still
// produces a "cli tool call failed" line with the error. The fake backend's
// "explode" tool handler returns a non-nil error (rather than a result with
// IsError: true, which the existing "boom" tool in newFakeCallBackend uses)
// -- go-sdk's mcp.AddTool translates a handler's returned error into a
// JSON-RPC error response, which surfaces at the CALLER's CallTool as a
// genuine Go error, exactly the path logCLICall's err branch needs to
// exercise. This is a dedicated inline backend (not the shared
// newFakeCallBackend helper), since none of its existing tools return a Go
// error this way and it's simpler to add one tool here than to change a
// helper other tests also rely on.
func TestCallCommand_LogsAuditLineOnFailure(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "backend-a", Version: "v1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "explode", Description: "always fails", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, fmt.Errorf("boom")
		})
	backendA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer backendA.Close()

	configPath := writeConfig(t, fmt.Sprintf(`
backends:
  - name: backend-a
    transport: http
    url: %q
`, backendA.URL))

	root := cli.NewRootCmd()
	var out bytes.Buffer
	var errOut syncCallLogBuffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"call", "--config", configPath, "explode"})

	ctx := t.Context()
	if err := root.ExecuteContext(ctx); err == nil {
		t.Fatal("call explode: got no error, want one (the tool handler always returns an error)")
	}

	logged := errOut.String()
	line := findLogLine(t, logged, `msg="cli tool call failed"`)
	if !strings.Contains(line, "backend=backend-a") || !hasField(line, "tool", "explode") {
		t.Fatalf("log line = %q, want backend=backend-a and tool=explode", line)
	}
	if !strings.Contains(line, "error=") {
		t.Fatalf("log line = %q, want an error= field", line)
	}
	if !strings.Contains(line, "boom") {
		t.Fatalf("log line = %q, want the error message (\"boom\") present", line)
	}
}
