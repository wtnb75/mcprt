package router_test

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/router"
)

func tool(name string) *mcp.Tool {
	return &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}
}

func toolNameOf(t *mcp.Tool) string { return t.Name }

func toolRename(t *mcp.Tool, name string) *mcp.Tool {
	c := *t
	c.Name = name
	return &c
}

func TestResolve_NoConflicts(t *testing.T) {
	table := router.Resolve([]router.Entry[*mcp.Tool]{
		{BackendName: "a", Items: []*mcp.Tool{tool("alpha")}},
		{BackendName: "b", Items: []*mcp.Tool{tool("beta")}},
	}, toolNameOf, toolRename, nil)

	if len(table.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none", table.Conflicts)
	}
	if len(table.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(table.Items))
	}
	if r := table.Items["alpha"]; r == nil || r.BackendName != "a" || r.OriginalName != "alpha" {
		t.Fatalf("Items[alpha] = %+v, unexpected", r)
	}
}

func TestResolve_CollisionFirstListedWins(t *testing.T) {
	table := router.Resolve([]router.Entry[*mcp.Tool]{
		{BackendName: "first", Items: []*mcp.Tool{tool("search")}},
		{BackendName: "second", Items: []*mcp.Tool{tool("search")}},
	}, toolNameOf, toolRename, nil)

	got := table.Items["search"]
	if got == nil || got.BackendName != "first" {
		t.Fatalf("Items[search] = %+v, want backend \"first\"", got)
	}
	if len(table.Conflicts) != 1 || table.Conflicts[0].Winner != "first" || len(table.Conflicts[0].Losers) != 1 || table.Conflicts[0].Losers[0] != "second" {
		t.Fatalf("Conflicts = %+v, want one conflict won by \"first\", hiding \"second\"", table.Conflicts)
	}
	if len(got.Fallbacks) != 1 || got.Fallbacks[0].BackendName != "second" || got.Fallbacks[0].Item.Name != "search" {
		t.Fatalf("Items[search].Fallbacks = %+v, want one fallback candidate from \"second\"", got.Fallbacks)
	}
}

func TestResolve_OverrideWinsOverListOrder(t *testing.T) {
	table := router.Resolve([]router.Entry[*mcp.Tool]{
		{BackendName: "first", Items: []*mcp.Tool{tool("search")}},
		{BackendName: "second", Items: []*mcp.Tool{tool("search")}},
	}, toolNameOf, toolRename, map[string]string{"search": "second"})

	got := table.Items["search"]
	if got == nil || got.BackendName != "second" {
		t.Fatalf("Items[search] = %+v, want backend \"second\" (via override)", got)
	}
	if len(table.Conflicts) != 1 || table.Conflicts[0].Winner != "second" {
		t.Fatalf("Conflicts = %+v, want winner \"second\"", table.Conflicts)
	}
}

func TestResolve_PrefixAppliedBeforeCollisionCheck(t *testing.T) {
	table := router.Resolve([]router.Entry[*mcp.Tool]{
		{BackendName: "a", Prefix: "a__", Items: []*mcp.Tool{tool("search")}},
		{BackendName: "b", Prefix: "b__", Items: []*mcp.Tool{tool("search")}},
	}, toolNameOf, toolRename, nil)

	if len(table.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none (prefixes make names distinct)", table.Conflicts)
	}
	if _, ok := table.Items["a__search"]; !ok {
		t.Fatal("Items[a__search] missing")
	}
	if _, ok := table.Items["b__search"]; !ok {
		t.Fatal("Items[b__search] missing")
	}
}

func TestResolve_IneffectiveOverrideFallsBackToListOrder(t *testing.T) {
	// "third" is a real backend but doesn't produce a "search" tool, so the
	// override can't apply to it; resolution falls back to list order.
	table := router.Resolve([]router.Entry[*mcp.Tool]{
		{BackendName: "first", Items: []*mcp.Tool{tool("search")}},
		{BackendName: "second", Items: []*mcp.Tool{tool("search")}},
	}, toolNameOf, toolRename, map[string]string{"search": "third"})

	got := table.Items["search"]
	if got == nil || got.BackendName != "first" {
		t.Fatalf("Items[search] = %+v, want backend \"first\" (override target has no such tool)", got)
	}
}

func TestResolve_URIKeyedItems(t *testing.T) {
	resource := func(uri string) *mcp.Resource { return &mcp.Resource{URI: uri, Name: uri} }
	nameOf := func(r *mcp.Resource) string { return r.URI }
	rename := func(r *mcp.Resource, name string) *mcp.Resource { c := *r; c.URI = name; return &c }

	table := router.Resolve([]router.Entry[*mcp.Resource]{
		{BackendName: "a", Items: []*mcp.Resource{resource("file:///data/README.md")}},
		{BackendName: "b", Items: []*mcp.Resource{resource("file:///data/README.md")}},
	}, nameOf, rename, map[string]string{"file:///data/README.md": "b"})

	got := table.Items["file:///data/README.md"]
	if got == nil || got.BackendName != "b" {
		t.Fatalf("Items[...] = %+v, want backend \"b\" (via override)", got)
	}
	if got.Item.URI != "file:///data/README.md" {
		t.Fatalf("Item.URI = %q, want unchanged (prefix empty)", got.Item.URI)
	}
}

func TestResolve_URITemplateKeyedItems(t *testing.T) {
	tmpl := func(uriTemplate string) *mcp.ResourceTemplate {
		return &mcp.ResourceTemplate{URITemplate: uriTemplate, Name: uriTemplate}
	}
	nameOf := func(t *mcp.ResourceTemplate) string { return t.URITemplate }
	rename := func(t *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate {
		c := *t
		c.URITemplate = name
		return &c
	}

	table := router.Resolve([]router.Entry[*mcp.ResourceTemplate]{
		{BackendName: "a", Items: []*mcp.ResourceTemplate{tmpl("file:///data/{path}")}},
	}, nameOf, rename, nil)

	got := table.Items["file:///data/{path}"]
	if got == nil || got.BackendName != "a" || got.Item.URITemplate != "file:///data/{path}" {
		t.Fatalf("Items[...] = %+v, unexpected", got)
	}
}
