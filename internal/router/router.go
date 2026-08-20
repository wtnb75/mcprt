package router

// Entry is one backend's list of items (tools, resources, or resource
// templates), tagged with the backend's exposed-name prefix. entries passed
// to Resolve must be ordered by priority: index 0 is the highest priority
// (wins ties absent an override).
type Entry[T any] struct {
	BackendName string
	Prefix      string
	Items       []T
}

// Resolved is a single item exposed by the gateway, mapped back to the
// backend and original (un-prefixed) name that serves it.
type Resolved[T any] struct {
	Item         T
	BackendName  string
	OriginalName string
	// Fallbacks holds the other backends' definitions for this same exposed
	// name (in priority order), for a caller that wants to try the next one
	// if Item turns out to be unregisterable (e.g. a malformed schema or an
	// invalid URI).
	Fallbacks []Candidate[T]
}

// Candidate is one backend's definition that could serve a given exposed
// name.
type Candidate[T any] struct {
	Item         T
	BackendName  string
	OriginalName string
}

// Conflict records that multiple backends produced the same exposed name,
// and which backend's item won.
type Conflict struct {
	ExposedName string
	Winner      string
	Losers      []string
}

// Table is the fully resolved routing table: exposed name -> the
// backend/item that serves it, plus a record of any naming conflicts that
// were resolved along the way.
type Table[T any] struct {
	Items     map[string]*Resolved[T]
	Conflicts []Conflict
}

type candidate[T any] struct {
	backendName  string
	originalName string
	item         T
}

// Resolve merges entries' item lists into a single routing table. nameOf
// returns an item's own (un-prefixed) name; rename returns a copy of an
// item with its name (or URI) set to the given exposed name. overrides maps
// an exposed name to the backend name that must win that name's conflict;
// an override that names a real backend which does not produce an item
// under that exposed name has no effect (resolution falls back to list
// order).
func Resolve[T any](entries []Entry[T], nameOf func(T) string, rename func(T, string) T, overrides map[string]string) *Table[T] {
	candidatesByName := make(map[string][]candidate[T])
	var order []string // first-seen order, for deterministic conflict reporting

	for _, e := range entries {
		for _, item := range e.Items {
			exposedName := e.Prefix + nameOf(item)
			if _, seen := candidatesByName[exposedName]; !seen {
				order = append(order, exposedName)
			}
			candidatesByName[exposedName] = append(candidatesByName[exposedName], candidate[T]{
				backendName:  e.BackendName,
				originalName: nameOf(item),
				item:         item,
			})
		}
	}

	table := &Table[T]{Items: make(map[string]*Resolved[T], len(order))}
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
		resolved := &Resolved[T]{
			Item:         rename(winner.item, exposedName),
			BackendName:  winner.backendName,
			OriginalName: winner.originalName,
		}
		if len(cands) > 1 {
			conflict := Conflict{ExposedName: exposedName, Winner: winner.backendName}
			for i, c := range cands {
				if i == winnerIdx {
					continue
				}
				conflict.Losers = append(conflict.Losers, c.backendName)
				resolved.Fallbacks = append(resolved.Fallbacks, Candidate[T]{
					Item:         rename(c.item, exposedName),
					BackendName:  c.backendName,
					OriginalName: c.originalName,
				})
			}
			table.Conflicts = append(table.Conflicts, conflict)
		}
		table.Items[exposedName] = resolved
	}
	return table
}
