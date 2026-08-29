package gateway

import (
	"log/slog"
	"reflect"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/router"
)

// ToolNameOf, ToolRename, ResourceNameOf, ResourceRename,
// ResourceTemplateNameOf, ResourceTemplateRename, PromptNameOf, and
// PromptRename are the router.Resolve nameOf/rename pair for each category.
// They are exported so both the initial startup resolution (internal/cli's
// runServer/runCall/runList) and every later reconcile (UpdateTools/
// UpdateResources/UpdatePrompts below) resolve names identically -- two
// independent copies of "how to name/rename a tool" drifting apart would
// silently break reconcile's diffing against the startup-built table.

func ToolNameOf(t *mcp.Tool) string { return t.Name }

func ToolRename(t *mcp.Tool, name string) *mcp.Tool {
	c := *t
	c.Name = name
	return &c
}

func ResourceNameOf(r *mcp.Resource) string { return r.URI }

func ResourceRename(r *mcp.Resource, name string) *mcp.Resource {
	c := *r
	c.URI = name
	return &c
}

func ResourceTemplateNameOf(t *mcp.ResourceTemplate) string { return t.URITemplate }

func ResourceTemplateRename(t *mcp.ResourceTemplate, name string) *mcp.ResourceTemplate {
	c := *t
	c.URITemplate = name
	return &c
}

func PromptNameOf(p *mcp.Prompt) string { return p.Name }

func PromptRename(p *mcp.Prompt, name string) *mcp.Prompt {
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
	newTable := router.Resolve(s.toolEntries, ToolNameOf, ToolRename, s.toolOverrides)

	for name := range s.toolTable.Items {
		if _, ok := newTable.Items[name]; !ok {
			s.mcp.RemoveTools(name)
		}
	}
	for name, resolved := range newTable.Items {
		if old, ok := s.toolTable.Items[name]; !ok || !reflect.DeepEqual(old, resolved) {
			if !registerTool(s.mcp, s.logger, s.backends, resolved, s.maskKeys) {
				// Every candidate for name was unregisterable (registerTool
				// already logged this). Without this, a prior successful
				// registration for the same exposed name would keep serving
				// its now-superseded definition. RemoveTools on a name that
				// was never registered is a safe no-op (go-sdk's
				// featureSet.remove returns false and skips notifying).
				s.mcp.RemoveTools(name)
			}
		}
	}
	logNewConflicts(s.logger, "tool name conflict", "tool", s.toolTable.Conflicts, newTable.Conflicts)

	s.toolTable = newTable
}

// UpdateResources replaces backendName's resource AND resource-template
// entries together (MCP fires one notification, notifications/resources
// /list_changed, for both), re-resolves both tables, and applies both
// diffs under one lock acquisition.
func (s *Server) UpdateResources(backendName string, resources []*mcp.Resource, templates []*mcp.ResourceTemplate) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.resourceEntries = replaceEntry(s.resourceEntries, backendName, resources)
	newResourceTable := router.Resolve(s.resourceEntries, ResourceNameOf, ResourceRename, s.resourceOverrides)
	for name := range s.resourceTable.Items {
		if _, ok := newResourceTable.Items[name]; !ok {
			s.mcp.RemoveResources(name)
		}
	}
	for name, resolved := range newResourceTable.Items {
		if old, ok := s.resourceTable.Items[name]; !ok || !reflect.DeepEqual(old, resolved) {
			registerResource(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
		}
	}
	logNewConflicts(s.logger, "resource URI conflict", "uri", s.resourceTable.Conflicts, newResourceTable.Conflicts)
	s.resourceTable = newResourceTable

	s.resourceTemplateEntries = replaceEntry(s.resourceTemplateEntries, backendName, templates)
	newTemplateTable := router.Resolve(s.resourceTemplateEntries, ResourceTemplateNameOf, ResourceTemplateRename, s.resourceTemplateOverrides)
	for name := range s.resourceTemplateTable.Items {
		if _, ok := newTemplateTable.Items[name]; !ok {
			s.mcp.RemoveResourceTemplates(name)
		}
	}
	for name, resolved := range newTemplateTable.Items {
		if old, ok := s.resourceTemplateTable.Items[name]; !ok || !reflect.DeepEqual(old, resolved) {
			registerResourceTemplate(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
		}
	}
	logNewConflicts(s.logger, "resource template URI conflict", "uriTemplate", s.resourceTemplateTable.Conflicts, newTemplateTable.Conflicts)
	s.resourceTemplateTable = newTemplateTable
}

// UpdatePrompts replaces backendName's prompt entry with items, re-resolves
// the merged table, and applies the diff to the underlying *mcp.Server.
func (s *Server) UpdatePrompts(backendName string, items []*mcp.Prompt) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.promptEntries = replaceEntry(s.promptEntries, backendName, items)
	newTable := router.Resolve(s.promptEntries, PromptNameOf, PromptRename, s.promptOverrides)

	for name := range s.promptTable.Items {
		if _, ok := newTable.Items[name]; !ok {
			s.mcp.RemovePrompts(name)
		}
	}
	for name, resolved := range newTable.Items {
		if old, ok := s.promptTable.Items[name]; !ok || !reflect.DeepEqual(old, resolved) {
			registerPrompt(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
		}
	}
	logNewConflicts(s.logger, "prompt name conflict", "prompt", s.promptTable.Conflicts, newTable.Conflicts)

	s.promptTable = newTable
}

// upsertEntry is replaceEntry's counterpart for a backend connecting: it
// replaces backendName's entry if one already exists (a reconnect), or
// appends a new one with the given prefix if this is the first time
// backendName has ever appeared in entries (a backend that failed to
// connect at mcprt startup, connecting for the first time). Unlike
// replaceEntry, this needs prefix because a first-time entry has no prior
// Prefix to inherit.
func upsertEntry[T any](entries []router.Entry[T], backendName, prefix string, items []T) []router.Entry[T] {
	out := make([]router.Entry[T], len(entries))
	copy(out, entries)
	for i, e := range out {
		if e.BackendName == backendName {
			out[i].Prefix = prefix
			out[i].Items = items
			return out
		}
	}
	return append(out, router.Entry[T]{BackendName: backendName, Prefix: prefix, Items: items})
}

// ConnectBackend registers (or re-registers) backendName's live connection
// and reconciles its full item set -- used both when a backend that failed
// to connect at mcprt startup finally connects for the first time, and
// when a previously-connected backend reconnects after a disconnect (see
// internal/cli/server.go's superviseBackend). prefix is the backend's
// configured Prefix (from config.BackendConfig; resources and resource
// templates never carry one, matching connectBackends' existing
// convention).
//
// This first ensures an entry (seeded with nil Items) exists for
// backendName under s.mu, in one atomic step with setting s.backends[name]
// -- then delegates the actual item reconciliation to the existing
// UpdateTools/UpdateResources/UpdatePrompts, each of which takes its own
// lock. A backend name is only ever driven by one supervisor goroutine at
// a time (see internal/cli/server.go), so nothing else touches this
// backend's entry between the two phases.
func (s *Server) ConnectBackend(name string, b *backend.Backend, prefix string, tools []*mcp.Tool, resources []*mcp.Resource, templates []*mcp.ResourceTemplate, prompts []*mcp.Prompt) {
	s.mu.Lock()
	s.backends[name] = b
	s.toolEntries = upsertEntry(s.toolEntries, name, "", nil)
	s.resourceEntries = upsertEntry(s.resourceEntries, name, "", nil)
	s.resourceTemplateEntries = upsertEntry(s.resourceTemplateEntries, name, "", nil)
	s.promptEntries = upsertEntry(s.promptEntries, name, "", nil)
	s.mu.Unlock()

	s.UpdateTools(name, tools)
	s.UpdateResources(name, resources, templates)
	s.UpdatePrompts(name, prompts)
}
