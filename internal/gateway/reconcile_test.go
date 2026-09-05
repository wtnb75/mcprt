package gateway_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/gateway"
	"github.com/wtnb75/mcprt/internal/router"
)

// toolSchema is a minimal valid input schema shared by every test *mcp.Tool
// literal in this file: mcp.Server.AddTool panics on a nil InputSchema (see
// TestGateway_FallsBackWhenWinnerSchemaInvalid in gateway_test.go, which
// exercises that panic deliberately), so every tool that's meant to
// register successfully here needs one.
var toolSchema = map[string]any{"type": "object"}

// downstreamToolNames connects a fresh test client to srv and returns the
// sorted list of tool names it currently sees, for asserting on the effect
// of an UpdateTools call.
func downstreamToolNames(t *testing.T, ctx context.Context, srv *mcp.Server) []string {
	t.Helper()
	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var names []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestUpdateTools_AddsRemovesAndChangesItems(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{
			{Name: "keep", Description: "v1", InputSchema: toolSchema},
			{Name: "gone", Description: "v1", InputSchema: toolSchema},
		}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"a": {Name: "a"}},
		Tables:   gateway.Tables{Tools: table},
		Entries:  gateway.Entries{Tools: entries},
	})

	if got := downstreamToolNames(t, ctx, srv.MCP()); !equalStrings(got, []string{"gone", "keep"}) {
		t.Fatalf("initial tools = %v, want [gone keep]", got)
	}

	// "gone" disappears, "keep" changes description, "new" appears.
	srv.UpdateTools("a", []*mcp.Tool{
		{Name: "keep", Description: "v2", InputSchema: toolSchema},
		{Name: "new", Description: "v1", InputSchema: toolSchema},
	})

	if got := downstreamToolNames(t, ctx, srv.MCP()); !equalStrings(got, []string{"keep", "new"}) {
		t.Fatalf("tools after UpdateTools = %v, want [keep new]", got)
	}
}

func TestUpdateTools_ConflictFallbackPromotesWhenWinnerRemoved(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "search", Description: "from a", InputSchema: toolSchema}}},
		{BackendName: "b", Items: []*mcp.Tool{{Name: "search", Description: "from b", InputSchema: toolSchema}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	if len(table.Conflicts) != 1 || table.Conflicts[0].Winner != "a" {
		t.Fatalf("initial table.Conflicts = %+v, want one conflict won by \"a\"", table.Conflicts)
	}
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"a": {Name: "a"}, "b": {Name: "b"}},
		Tables:   gateway.Tables{Tools: table},
		Entries:  gateway.Entries{Tools: entries},
	})

	// backend "a" no longer serves "search": "b"'s definition should take over.
	srv.UpdateTools("a", nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var found *mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		if tool.Name == "search" {
			found = tool
		}
	}
	if found == nil || found.Description != "from b" {
		t.Fatalf("tool \"search\" = %+v, want description \"from b\" (promoted fallback)", found)
	}
}

// TestUpdateTools_RemovesStaleRegistrationWhenNewWinnerIsInvalid is the
// regression test for the bug found in this feature's final whole-branch
// review: when a backend's list_changed reconcile picks a winner whose
// definition registerTool can't register (invalid schema) and there is no
// valid fallback, the tool must be REMOVED from the gateway, not left
// serving its previous (now-stale) definition.
func TestUpdateTools_RemovesStaleRegistrationWhenNewWinnerIsInvalid(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "search", Description: "v1", InputSchema: toolSchema}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"a": {Name: "a"}},
		Tables:   gateway.Tables{Tools: table},
		Entries:  gateway.Entries{Tools: entries},
	})

	if got := downstreamToolNames(t, ctx, srv.MCP()); !equalStrings(got, []string{"search"}) {
		t.Fatalf("initial tools = %v, want [search]", got)
	}

	// Backend "a" changes "search" to a definition with no InputSchema --
	// mcp.Server.AddTool panics on that, registerTool's addTool wrapper
	// recovers and reports failure, and there is no other backend to fall
	// back to. The exposed name ("search") is unchanged, so this is a
	// same-name valid-to-invalid transition, not a brand-new invalid name.
	srv.UpdateTools("a", []*mcp.Tool{{Name: "search", Description: "v2, broken"}})

	if got := downstreamToolNames(t, ctx, srv.MCP()); len(got) != 0 {
		t.Fatalf("tools after UpdateTools with an invalid winner and no fallback = %v, want [] (stale registration must be removed, not left serving the old definition)", got)
	}
}

func TestUpdateTools_LogsOnlyNewConflicts(t *testing.T) {
	var buf logBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	// "b" is included from the start (with no tools yet) because
	// replaceEntry only replaces an *existing* entry's Items -- a
	// list_changed callback only ever fires for a backend that was already
	// connected and entered into entries at startup (see replaceEntry's
	// doc comment in reconcile.go).
	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "search", InputSchema: toolSchema}}},
		{BackendName: "b", Items: nil},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"a": {Name: "a"}, "b": {Name: "b"}},
		Tables:   gateway.Tables{Tools: table},
		Entries:  gateway.Entries{Tools: entries},
	})

	buf.reset() // discard whatever New itself may have logged (nothing, in this case, but keep the assertion below scoped to UpdateTools)

	// First reconcile introduces a brand-new conflict: must log.
	srv.UpdateTools("b", []*mcp.Tool{{Name: "search", InputSchema: toolSchema}})
	if !buf.contains("gateway event") || !buf.contains("event=name_conflict") {
		t.Fatalf("log output = %q, want a \"gateway event\" warning with event=name_conflict for the newly-introduced conflict", buf.String())
	}
	buf.reset()

	// Second reconcile touches an unrelated tool; the SAME conflict persists
	// but must NOT be re-logged.
	srv.UpdateTools("a", []*mcp.Tool{{Name: "search", InputSchema: toolSchema}, {Name: "other", InputSchema: toolSchema}})
	if buf.contains("event=name_conflict") {
		t.Fatalf("log output = %q, want no conflict warning for an already-known conflict", buf.String())
	}
}

func TestUpdateTools_ConcurrentCallsDoNotRace(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "x", InputSchema: toolSchema}}},
		{BackendName: "b", Items: []*mcp.Tool{{Name: "y", InputSchema: toolSchema}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"a": {Name: "a"}, "b": {Name: "b"}},
		Tables:   gateway.Tables{Tools: table},
		Entries:  gateway.Entries{Tools: entries},
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			srv.UpdateTools("a", []*mcp.Tool{{Name: "x", InputSchema: toolSchema}, {Name: "extra-a", InputSchema: toolSchema}})
		}(i)
		go func(i int) {
			defer wg.Done()
			srv.UpdateTools("b", []*mcp.Tool{{Name: "y", InputSchema: toolSchema}, {Name: "extra-b", InputSchema: toolSchema}})
		}(i)
	}
	wg.Wait()
}

func resourceNameOf(r *mcp.Resource) string { return r.URI }

func resourceRename(r *mcp.Resource, name string) *mcp.Resource {
	c := *r
	c.URI = name
	return &c
}

func resourceTemplateNameOf(t *mcp.ResourceTemplate) string { return t.URITemplate }

func resourceTemplateRename(t *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate {
	c := *t
	c.URITemplate = name
	return &c
}

func TestUpdateResources_AddsRemovesResourcesAndTemplatesInOneCall(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	resourceEntries := []router.Entry[*mcp.Resource]{
		{BackendName: "a", Items: []*mcp.Resource{{URI: "file:///keep", Name: "keep"}, {URI: "file:///gone", Name: "gone"}}},
	}
	templateEntries := []router.Entry[*mcp.ResourceTemplate]{
		{BackendName: "a", Items: []*mcp.ResourceTemplate{{URITemplate: "file:///dir/{f}", Name: "dir"}}},
	}
	resourceTable := router.Resolve(resourceEntries, resourceNameOf, resourceRename, nil)
	templateTable := router.Resolve(templateEntries, resourceTemplateNameOf, resourceTemplateRename, nil)

	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"a": {Name: "a"}},
		Tables:   gateway.Tables{Resources: resourceTable, ResourceTemplates: templateTable},
		Entries:  gateway.Entries{Resources: resourceEntries, ResourceTemplates: templateEntries},
	})

	srv.UpdateResources("a",
		[]*mcp.Resource{{URI: "file:///keep", Name: "keep"}, {URI: "file:///new", Name: "new"}},
		nil, // templates all removed
	)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var uris []string
	for r, err := range session.Resources(ctx, nil) {
		if err != nil {
			t.Fatalf("listing resources: %v", err)
		}
		uris = append(uris, r.URI)
	}
	sort.Strings(uris)
	if !equalStrings(uris, []string{"file:///keep", "file:///new"}) {
		t.Fatalf("resources after UpdateResources = %v, want [file:///keep file:///new]", uris)
	}

	var templateCount int
	for range session.ResourceTemplates(ctx, nil) {
		templateCount++
	}
	if templateCount != 0 {
		t.Fatalf("resource templates after UpdateResources = %d, want 0", templateCount)
	}
}

func TestUpdatePrompts_AddsRemovesAndChangesItems(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	entries := []router.Entry[*mcp.Prompt]{
		{BackendName: "a", Items: []*mcp.Prompt{{Name: "keep", Description: "v1"}, {Name: "gone", Description: "v1"}}},
	}
	table := router.Resolve(entries, promptNameOf, promptRename, nil)
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"a": {Name: "a"}},
		Tables:   gateway.Tables{Prompts: table},
		Entries:  gateway.Entries{Prompts: entries},
	})

	srv.UpdatePrompts("a", []*mcp.Prompt{{Name: "keep", Description: "v2"}, {Name: "new", Description: "v1"}})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var names []string
	for p, err := range session.Prompts(ctx, nil) {
		if err != nil {
			t.Fatalf("listing prompts: %v", err)
		}
		names = append(names, p.Name)
	}
	sort.Strings(names)
	if !equalStrings(names, []string{"keep", "new"}) {
		t.Fatalf("prompts after UpdatePrompts = %v, want [keep new]", names)
	}
}

func TestUpdateResourcesAndUpdatePrompts_ConcurrentCallsDoNotRace(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	resourceEntries := []router.Entry[*mcp.Resource]{{BackendName: "a", Items: []*mcp.Resource{{URI: "file:///x", Name: "x"}}}}
	promptEntries := []router.Entry[*mcp.Prompt]{{BackendName: "a", Items: []*mcp.Prompt{{Name: "p"}}}}
	resourceTable := router.Resolve(resourceEntries, resourceNameOf, resourceRename, nil)
	promptTable := router.Resolve(promptEntries, promptNameOf, promptRename, nil)

	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"a": {Name: "a"}},
		Tables:   gateway.Tables{Resources: resourceTable, Prompts: promptTable},
		Entries:  gateway.Entries{Resources: resourceEntries, Prompts: promptEntries},
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			srv.UpdateResources("a", []*mcp.Resource{{URI: "file:///x", Name: "x"}, {URI: "file:///y", Name: "y"}}, nil)
		}()
		go func() {
			defer wg.Done()
			srv.UpdatePrompts("a", []*mcp.Prompt{{Name: "p"}, {Name: "q"}})
		}()
	}
	wg.Wait()
}

// TestConnectBackend_AddsNewBackendNotInEntries checks that ConnectBackend
// registers a backend that was never part of the Server's founding entries
// -- the case of a backend that failed to connect at mcprt startup and
// joins later via superviseBackend (see internal/cli/server.go).
func TestConnectBackend_AddsNewBackendNotInEntries(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "existing", InputSchema: toolSchema}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"a": {Name: "a"}},
		Tables:   gateway.Tables{Tools: table},
		Entries:  gateway.Entries{Tools: entries},
	})

	newConn := &backend.Backend{Name: "new"}
	srv.ConnectBackend("new", newConn, "new-",
		[]*mcp.Tool{{Name: "fresh", InputSchema: toolSchema}}, nil, nil, nil)

	if srv.Backends()["new"] != newConn {
		t.Fatalf("Backends()[\"new\"] = %v, want %v", srv.Backends()["new"], newConn)
	}
	if got := downstreamToolNames(t, ctx, srv.MCP()); !equalStrings(got, []string{"existing", "new-fresh"}) {
		t.Fatalf("tools after ConnectBackend = %v, want [existing new-fresh]", got)
	}
}

// TestConnectBackend_ReconnectsExistingBackend checks that ConnectBackend
// replaces an already-known backend's connection and item set -- the case
// of a backend that disconnected and successfully reconnected.
func TestConnectBackend_ReconnectsExistingBackend(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Prefix: "a-", Items: []*mcp.Tool{{Name: "old", InputSchema: toolSchema}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	oldConn := &backend.Backend{Name: "a"}
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"a": oldConn},
		Tables:   gateway.Tables{Tools: table},
		Entries:  gateway.Entries{Tools: entries},
	})

	// Simulate a disconnect (as superviseBackend does) before the reconnect.
	srv.UpdateTools("a", nil)
	if got := downstreamToolNames(t, ctx, srv.MCP()); len(got) != 0 {
		t.Fatalf("tools after simulated disconnect = %v, want []", got)
	}

	newConn := &backend.Backend{Name: "a"}
	srv.ConnectBackend("a", newConn, "a-",
		[]*mcp.Tool{{Name: "reconnected", InputSchema: toolSchema}}, nil, nil, nil)

	if srv.Backends()["a"] != newConn {
		t.Fatalf("Backends()[\"a\"] = %v, want the new connection %v", srv.Backends()["a"], newConn)
	}
	if got := downstreamToolNames(t, ctx, srv.MCP()); !equalStrings(got, []string{"a-reconnected"}) {
		t.Fatalf("tools after ConnectBackend reconnect = %v, want [a-reconnected]", got)
	}
}

// TestConnectBackend_ReconnectIntroducingConflictLogsIt checks that a
// reconnect which introduces a brand-new name conflict logs it, exactly
// like UpdateTools does (ConnectBackend delegates to UpdateTools for the
// actual reconcile).
func TestConnectBackend_ReconnectIntroducingConflictLogsIt(t *testing.T) {
	var buf logBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "search", InputSchema: toolSchema}}},
		{BackendName: "b", Items: nil}, // "b" starts with nothing, matching UpdateTools' precedent
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"a": {Name: "a"}, "b": {Name: "b"}},
		Tables:   gateway.Tables{Tools: table},
		Entries:  gateway.Entries{Tools: entries},
	})

	buf.reset()
	srv.ConnectBackend("b", &backend.Backend{Name: "b"}, "",
		[]*mcp.Tool{{Name: "search", InputSchema: toolSchema}}, nil, nil, nil)
	if !buf.contains("gateway event") || !buf.contains("event=name_conflict") {
		t.Fatalf("log output = %q, want a \"gateway event\" warning with event=name_conflict", buf.String())
	}
}

// TestConnectBackend_ConcurrentWithUpdateToolsDoesNotRace checks that
// ConnectBackend for a brand-new backend and UpdateTools for an existing
// one can run concurrently without racing -- superviseBackend spawns one
// goroutine per backend, so a new backend's first connect can land at the
// same moment another backend's list_changed fires.
func TestConnectBackend_ConcurrentWithUpdateToolsDoesNotRace(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	entries := []router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{{Name: "x", InputSchema: toolSchema}}},
	}
	table := router.Resolve(entries, toolNameOf, toolRename, nil)
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"a": {Name: "a"}},
		Tables:   gateway.Tables{Tools: table},
		Entries:  gateway.Entries{Tools: entries},
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			srv.UpdateTools("a", []*mcp.Tool{{Name: "x", InputSchema: toolSchema}, {Name: "extra", InputSchema: toolSchema}})
		}()
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("new-%d", i)
			srv.ConnectBackend(name, &backend.Backend{Name: name}, "",
				[]*mcp.Tool{{Name: fmt.Sprintf("tool-%d", i), InputSchema: toolSchema}}, nil, nil, nil)
		}(i)
	}
	wg.Wait()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
