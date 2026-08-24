package gateway

import (
	"log/slog"
	"reflect"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/router"
)

func toolNameOf(t *mcp.Tool) string { return t.Name }

func toolRename(t *mcp.Tool, name string) *mcp.Tool {
	c := *t
	c.Name = name
	return &c
}

//nolint:unused // used by Task 4's UpdateResources
func resourceNameOf(r *mcp.Resource) string { return r.URI }

//nolint:unused // used by Task 4's UpdateResources
func resourceRename(r *mcp.Resource, name string) *mcp.Resource {
	c := *r
	c.URI = name
	return &c
}

//nolint:unused // used by Task 4's UpdateResources
func resourceTemplateNameOf(t *mcp.ResourceTemplate) string { return t.URITemplate }

//nolint:unused // used by Task 4's UpdateResources
func resourceTemplateRename(t *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate {
	c := *t
	c.URITemplate = name
	return &c
}

//nolint:unused // used by Task 4's UpdatePrompts
func promptNameOf(p *mcp.Prompt) string { return p.Name }

//nolint:unused // used by Task 4's UpdatePrompts
func promptRename(p *mcp.Prompt, name string) *mcp.Prompt {
	c := *p
	c.Name = name
	return &c
}

// replaceEntry returns a copy of entries with backendName's Items replaced
// by items. If no entry for backendName exists yet, entries is returned
// unchanged (a list_changed callback only fires for a backend that was
// already connected and entered into entries at startup).
func replaceEntry[T any](entries []router.Entry[T], backendName string, items []T) []router.Entry[T] {
	out := make([]router.Entry[T], len(entries))
	copy(out, entries)
	for i, e := range out {
		if e.BackendName == backendName {
			out[i].Items = items
			break
		}
	}
	return out
}

// logNewConflicts warns about every conflict in newConflicts whose exposed
// name wasn't already conflicting in oldConflicts -- an already-known
// conflict isn't re-logged, and a conflict that disappears isn't logged at
// all, matching startup's one-shot conflict logging.
func logNewConflicts(logger *slog.Logger, msg, field string, oldConflicts, newConflicts []router.Conflict) {
	seen := make(map[string]bool, len(oldConflicts))
	for _, c := range oldConflicts {
		seen[c.ExposedName] = true
	}
	for _, c := range newConflicts {
		if !seen[c.ExposedName] {
			logger.Warn(msg, field, c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
		}
	}
}

// UpdateTools replaces backendName's tool entry with items, re-resolves the
// merged table, and applies the diff (Remove vanished names, Add
// new/changed names) to the underlying *mcp.Server. Called from the cli
// layer when a backend reports notifications/tools/list_changed.
func (s *Server) UpdateTools(backendName string, items []*mcp.Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.toolEntries = replaceEntry(s.toolEntries, backendName, items)
	newTable := router.Resolve(s.toolEntries, toolNameOf, toolRename, s.toolOverrides)

	for name := range s.toolTable.Items {
		if _, ok := newTable.Items[name]; !ok {
			s.mcp.RemoveTools(name)
		}
	}
	for name, resolved := range newTable.Items {
		if old, ok := s.toolTable.Items[name]; !ok || !reflect.DeepEqual(old, resolved) {
			registerTool(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
		}
	}
	logNewConflicts(s.logger, "tool name conflict", "tool", s.toolTable.Conflicts, newTable.Conflicts)

	s.toolTable = newTable
}
