package router

import "github.com/modelcontextprotocol/go-sdk/mcp"

// Entry is one backend's tool list, tagged with the backend's exposed-name
// prefix. entries passed to Resolve must be ordered by priority: index 0 is
// the highest priority (wins ties absent an override).
type Entry struct {
	BackendName string
	Prefix      string
	Tools       []*mcp.Tool
}

// Resolved is a single tool exposed by the gateway, mapped back to the
// backend and original (un-prefixed) tool name that serves it.
type Resolved struct {
	Tool         *mcp.Tool
	BackendName  string
	OriginalName string
}

// Conflict records that multiple backends produced the same exposed tool
// name, and which backend's tool won.
type Conflict struct {
	ExposedName string
	Winner      string
	Losers      []string
}

// Table is the fully resolved routing table: exposed tool name -> the
// backend/tool that serves it, plus a record of any naming conflicts that
// were resolved along the way.
type Table struct {
	Tools     map[string]*Resolved
	Conflicts []Conflict
}

type candidate struct {
	backendName  string
	originalName string
	tool         *mcp.Tool
}

// Resolve merges the tool lists from entries into a single routing table.
// overrides maps an exposed tool name to the backend name that must win
// that name's conflict; an override that names a real backend which does
// not produce a tool under that exposed name has no effect (resolution
// falls back to list order).
func Resolve(entries []Entry, overrides map[string]string) *Table {
	candidatesByName := make(map[string][]candidate)
	var order []string // first-seen order, for deterministic conflict reporting

	for _, e := range entries {
		for _, t := range e.Tools {
			exposedName := e.Prefix + t.Name
			if _, seen := candidatesByName[exposedName]; !seen {
				order = append(order, exposedName)
			}
			candidatesByName[exposedName] = append(candidatesByName[exposedName], candidate{
				backendName:  e.BackendName,
				originalName: t.Name,
				tool:         t,
			})
		}
	}

	table := &Table{Tools: make(map[string]*Resolved, len(order))}
	for _, exposedName := range order {
		cands := candidatesByName[exposedName]
		winnerIdx := 0
		if wantBackend, ok := overrides[exposedName]; ok {
			for i, c := range cands {
				if c.backendName == wantBackend {
					winnerIdx = i
					break
				}
			}
		}
		winner := cands[winnerIdx]
		table.Tools[exposedName] = &Resolved{
			Tool:         exposedTool(winner.tool, exposedName),
			BackendName:  winner.backendName,
			OriginalName: winner.originalName,
		}
		if len(cands) > 1 {
			conflict := Conflict{ExposedName: exposedName, Winner: winner.backendName}
			for i, c := range cands {
				if i != winnerIdx {
					conflict.Losers = append(conflict.Losers, c.backendName)
				}
			}
			table.Conflicts = append(table.Conflicts, conflict)
		}
	}
	return table
}

// exposedTool returns a copy of t with Name set to exposedName, so the
// gateway can register it under its (possibly prefixed) public name while
// keeping the rest of the backend's tool definition (description, schema)
// intact.
func exposedTool(t *mcp.Tool, exposedName string) *mcp.Tool {
	clone := *t
	clone.Name = exposedName
	return &clone
}
