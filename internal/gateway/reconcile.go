package gateway

import (
	"context"
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

// logNewConflicts records, via LogEvent, every conflict in newConflicts
// whose exposed name wasn't already conflicting in oldConflicts -- an
// already-known conflict isn't re-logged, and a conflict that disappears
// isn't logged at all, matching startup's one-shot conflict logging.
// context.Background() is used rather than a caller-supplied ctx: this
// fires from Server.UpdateTools/UpdateResources/UpdatePrompts, whose
// signatures carry no ctx (they reconcile a whole backend's list, not a
// single tracked request), so there is no request-scoped context to thread
// through here without a larger, out-of-scope signature change.
func logNewConflicts(logger *slog.Logger, kind string, oldConflicts, newConflicts []router.Conflict) {
	seen := make(map[string]bool, len(oldConflicts))
	for _, c := range oldConflicts {
		seen[c.ExposedName] = true
	}
	for _, c := range newConflicts {
		if !seen[c.ExposedName] {
			LogEvent(context.Background(), logger, slog.LevelWarn, EventNameConflict,
				"kind", kind, "name", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
		}
	}
}

// boundTo reports whether resolved's registration binds its handler to
// backendName -- either as the winning candidate, or as one of the fallbacks
// registerTool/registerResource/registerResourceTemplate falls back to when a
// higher-priority candidate turns out to be unregisterable. It is what scopes
// ConnectBackend's forced re-registration (see rebind below) to the items the
// (re)connecting backend actually serves, leaving every other backend's
// handlers untouched.
func boundTo[T any](resolved *router.Resolved[T], backendName string) bool {
	if resolved.BackendName == backendName {
		return true
	}
	for _, f := range resolved.Fallbacks {
		if f.BackendName == backendName {
			return true
		}
	}
	return false
}

// touchedBy reports whether name's resolution could possibly have changed
// as a result of backendName's own entry changing -- true if backendName is
// a candidate (winner or fallback, see boundTo) for name in EITHER the old
// resolved value (old, valid only when hadOld) or the new one (resolved).
//
// router.Resolve builds an exposed name's candidate set entirely from the
// entries that currently produce that name (see Resolve): a single
// UpdateTools/UpdateResources/UpdatePrompts call only ever changes
// backendName's own entry, so if backendName was not a candidate for name
// before AND is not one now, nothing about name's candidate set changed at
// all, and its resolution is therefore guaranteed byte-for-byte identical
// to before -- reflect.DeepEqual would always report "unchanged" for it.
// Skipping those names outright, instead of running reflect.DeepEqual
// against every name in the WHOLE merged table on every single backend's
// list_changed, is what keeps one backend's reconcile cost proportional to
// that backend's own item count, not the total across every connected
// backend.
func touchedBy[T any](resolved, old *router.Resolved[T], hadOld bool, backendName string) bool {
	return boundTo(resolved, backendName) || (hadOld && boundTo(old, backendName))
}

// UpdateTools replaces backendName's tool entry with items, re-resolves the
// merged table, and applies the diff (Remove vanished names, Add
// new/changed names) to the underlying *mcp.Server. Called from the cli
// layer when a backend reports notifications/tools/list_changed.
func (s *Server) UpdateTools(backendName string, items []*mcp.Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateToolsLocked(backendName, items, false)
}

// updateToolsLocked is UpdateTools' body, callable with s.mu already held so
// ConnectBackend can do its whole registration in one atomic step.
//
// rebind forces re-registration of every item bound to backendName (see
// boundTo) even when the re-resolved definition is value-identical to the
// one currently registered. Only ConnectBackend passes true: a reconnecting
// backend replaces a dead *backend.Backend with a live one, and the handler
// closures registered on the *mcp.Server captured the OLD object -- an
// identity change that the value-equality diff below cannot observe. The
// list_changed path (rebind == false) is unaffected: for an already-live
// backend the object is the same one the handlers already hold, so
// re-registering an unchanged definition would be pure churn (and an
// unnecessary tools/list_changed notification to every downstream client).
func (s *Server) updateToolsLocked(backendName string, items []*mcp.Tool, rebind bool) {
	s.toolEntries = replaceEntry(s.toolEntries, backendName, items)
	newTable := router.Resolve(s.toolEntries, ToolNameOf, ToolRename, s.toolOverrides)

	for name := range s.toolTable.Items {
		if _, ok := newTable.Items[name]; !ok {
			s.mcp.RemoveTools(name)
		}
	}
	for name, resolved := range newTable.Items {
		old, ok := s.toolTable.Items[name]
		if !touchedBy(resolved, old, ok, backendName) {
			continue
		}
		unchanged := ok && reflect.DeepEqual(old, resolved)
		if unchanged && (!rebind || !boundTo(resolved, backendName)) {
			continue
		}
		if !registerTool(s.mcp, s.logger, s.backends, resolved, s.maskKeys, s.relays) {
			// Every candidate for name was unregisterable (registerTool
			// already logged this). Without this, a prior successful
			// registration for the same exposed name would keep serving
			// its now-superseded definition. RemoveTools on a name that
			// was never registered is a safe no-op (go-sdk's
			// featureSet.remove returns false and skips notifying).
			s.mcp.RemoveTools(name)
		}
	}
	logNewConflicts(s.logger, "tool", s.toolTable.Conflicts, newTable.Conflicts)

	s.toolTable = newTable
}

// UpdateResources replaces backendName's resource AND resource-template
// entries together (MCP fires one notification, notifications/resources
// /list_changed, for both), re-resolves both tables, and applies both
// diffs under one lock acquisition.
func (s *Server) UpdateResources(backendName string, resources []*mcp.Resource, templates []*mcp.ResourceTemplate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateResourcesLocked(backendName, resources, templates, false)
}

// updateResourcesLocked is UpdateResources' body, callable with s.mu already
// held. See updateToolsLocked for what rebind means and why only
// ConnectBackend passes true.
func (s *Server) updateResourcesLocked(backendName string, resources []*mcp.Resource, templates []*mcp.ResourceTemplate, rebind bool) {
	s.resourceEntries = replaceEntry(s.resourceEntries, backendName, resources)
	newResourceTable := router.Resolve(s.resourceEntries, ResourceNameOf, ResourceRename, s.resourceOverrides)
	for name := range s.resourceTable.Items {
		if _, ok := newResourceTable.Items[name]; !ok {
			s.mcp.RemoveResources(name)
		}
	}
	for name, resolved := range newResourceTable.Items {
		old, ok := s.resourceTable.Items[name]
		if !touchedBy(resolved, old, ok, backendName) {
			continue
		}
		unchanged := ok && reflect.DeepEqual(old, resolved)
		if unchanged && (!rebind || !boundTo(resolved, backendName)) {
			continue
		}
		registerResource(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
	}
	logNewConflicts(s.logger, "resource", s.resourceTable.Conflicts, newResourceTable.Conflicts)
	s.resourceTable = newResourceTable

	s.resourceTemplateEntries = replaceEntry(s.resourceTemplateEntries, backendName, templates)
	newTemplateTable := router.Resolve(s.resourceTemplateEntries, ResourceTemplateNameOf, ResourceTemplateRename, s.resourceTemplateOverrides)
	for name := range s.resourceTemplateTable.Items {
		if _, ok := newTemplateTable.Items[name]; !ok {
			s.mcp.RemoveResourceTemplates(name)
		}
	}
	for name, resolved := range newTemplateTable.Items {
		old, ok := s.resourceTemplateTable.Items[name]
		if !touchedBy(resolved, old, ok, backendName) {
			continue
		}
		unchanged := ok && reflect.DeepEqual(old, resolved)
		if unchanged && (!rebind || !boundTo(resolved, backendName)) {
			continue
		}
		registerResourceTemplate(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
	}
	logNewConflicts(s.logger, "resourceTemplate", s.resourceTemplateTable.Conflicts, newTemplateTable.Conflicts)
	s.resourceTemplateTable = newTemplateTable
}

// UpdatePrompts replaces backendName's prompt entry with items, re-resolves
// the merged table, and applies the diff to the underlying *mcp.Server.
func (s *Server) UpdatePrompts(backendName string, items []*mcp.Prompt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updatePromptsLocked(backendName, items, false)
}

// updatePromptsLocked is UpdatePrompts' body, callable with s.mu already
// held. See updateToolsLocked for what rebind means and why only
// ConnectBackend passes true.
func (s *Server) updatePromptsLocked(backendName string, items []*mcp.Prompt, rebind bool) {
	s.promptEntries = replaceEntry(s.promptEntries, backendName, items)
	newTable := router.Resolve(s.promptEntries, PromptNameOf, PromptRename, s.promptOverrides)

	for name := range s.promptTable.Items {
		if _, ok := newTable.Items[name]; !ok {
			s.mcp.RemovePrompts(name)
		}
	}
	for name, resolved := range newTable.Items {
		old, ok := s.promptTable.Items[name]
		if !touchedBy(resolved, old, ok, backendName) {
			continue
		}
		unchanged := ok && reflect.DeepEqual(old, resolved)
		if unchanged && (!rebind || !boundTo(resolved, backendName)) {
			continue
		}
		registerPrompt(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
	}
	logNewConflicts(s.logger, "prompt", s.promptTable.Conflicts, newTable.Conflicts)

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
// This ensures an entry (seeded with nil Items) exists for backendName,
// registers b as its live connection, and reconciles all four item kinds --
// the whole thing under a single s.mu acquisition, so no concurrent reader
// or other backend's reconcile can observe a half-connected state.
//
// The three reconciles run in rebind mode (see updateToolsLocked): every
// item this backend serves is re-registered even when its definition is
// value-identical to the one already registered. That identical case is the
// norm for a reconnect, and it is exactly the case a value-equality diff
// gets wrong here -- the handlers registered on the *mcp.Server captured the
// PREVIOUS *backend.Backend, which is now closed, so leaving them in place
// would keep advertising this backend's tools while every call through them
// failed permanently.
func (s *Server) ConnectBackend(name string, b *backend.Backend, prefix string, tools []*mcp.Tool, resources []*mcp.Resource, templates []*mcp.ResourceTemplate, prompts []*mcp.Prompt) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.backends[name] = b
	s.toolEntries = upsertEntry(s.toolEntries, name, prefix, nil)
	s.resourceEntries = upsertEntry(s.resourceEntries, name, "", nil)
	s.resourceTemplateEntries = upsertEntry(s.resourceTemplateEntries, name, "", nil)
	s.promptEntries = upsertEntry(s.promptEntries, name, prefix, nil)

	s.updateToolsLocked(name, tools, true)
	s.updateResourcesLocked(name, resources, templates, true)
	s.updatePromptsLocked(name, prompts, true)
}
