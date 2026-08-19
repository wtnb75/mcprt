package router_test

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/router"
)

func tool(name string) *mcp.Tool {
	return &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}
}

func TestResolve_NoConflicts(t *testing.T) {
	table := router.Resolve([]router.Entry{
		{BackendName: "a", Tools: []*mcp.Tool{tool("alpha")}},
		{BackendName: "b", Tools: []*mcp.Tool{tool("beta")}},
	}, nil)

	if len(table.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none", table.Conflicts)
	}
	if len(table.Tools) != 2 {
		t.Fatalf("len(Tools) = %d, want 2", len(table.Tools))
	}
	if r := table.Tools["alpha"]; r == nil || r.BackendName != "a" || r.OriginalName != "alpha" {
		t.Fatalf("Tools[alpha] = %+v, unexpected", r)
	}
}

func TestResolve_CollisionFirstListedWins(t *testing.T) {
	table := router.Resolve([]router.Entry{
		{BackendName: "first", Tools: []*mcp.Tool{tool("search")}},
		{BackendName: "second", Tools: []*mcp.Tool{tool("search")}},
	}, nil)

	got := table.Tools["search"]
	if got == nil || got.BackendName != "first" {
		t.Fatalf("Tools[search] = %+v, want backend \"first\"", got)
	}
	if len(table.Conflicts) != 1 || table.Conflicts[0].Winner != "first" || len(table.Conflicts[0].Losers) != 1 || table.Conflicts[0].Losers[0] != "second" {
		t.Fatalf("Conflicts = %+v, want one conflict won by \"first\", hiding \"second\"", table.Conflicts)
	}
}

func TestResolve_OverrideWinsOverListOrder(t *testing.T) {
	table := router.Resolve([]router.Entry{
		{BackendName: "first", Tools: []*mcp.Tool{tool("search")}},
		{BackendName: "second", Tools: []*mcp.Tool{tool("search")}},
	}, map[string]string{"search": "second"})

	got := table.Tools["search"]
	if got == nil || got.BackendName != "second" {
		t.Fatalf("Tools[search] = %+v, want backend \"second\" (via override)", got)
	}
	if len(table.Conflicts) != 1 || table.Conflicts[0].Winner != "second" {
		t.Fatalf("Conflicts = %+v, want winner \"second\"", table.Conflicts)
	}
}

func TestResolve_PrefixAppliedBeforeCollisionCheck(t *testing.T) {
	table := router.Resolve([]router.Entry{
		{BackendName: "a", Prefix: "a__", Tools: []*mcp.Tool{tool("search")}},
		{BackendName: "b", Prefix: "b__", Tools: []*mcp.Tool{tool("search")}},
	}, nil)

	if len(table.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none (prefixes make names distinct)", table.Conflicts)
	}
	if _, ok := table.Tools["a__search"]; !ok {
		t.Fatal("Tools[a__search] missing")
	}
	if _, ok := table.Tools["b__search"]; !ok {
		t.Fatal("Tools[b__search] missing")
	}
}

func TestResolve_IneffectiveOverrideFallsBackToListOrder(t *testing.T) {
	// "third" is a real backend but doesn't produce a "search" tool, so the
	// override can't apply to it; resolution falls back to list order.
	table := router.Resolve([]router.Entry{
		{BackendName: "first", Tools: []*mcp.Tool{tool("search")}},
		{BackendName: "second", Tools: []*mcp.Tool{tool("search")}},
	}, map[string]string{"search": "third"})

	got := table.Tools["search"]
	if got == nil || got.BackendName != "first" {
		t.Fatalf("Tools[search] = %+v, want backend \"first\" (override target has no such tool)", got)
	}
}
