package gateway

import (
	"testing"

	"github.com/wtnb75/mcprt/internal/router"
)

func TestUpsertEntry_ReplacesExistingBackendName(t *testing.T) {
	entries := []router.Entry[string]{
		{BackendName: "a", Prefix: "a-", Items: []string{"old"}},
		{BackendName: "b", Prefix: "b-", Items: []string{"other"}},
	}
	got := upsertEntry(entries, "a", "a-", []string{"new"})
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].BackendName != "a" || got[0].Prefix != "a-" || len(got[0].Items) != 1 || got[0].Items[0] != "new" {
		t.Fatalf("got[0] = %+v, want {a a- [new]}", got[0])
	}
	if got[1].BackendName != "b" || len(got[1].Items) != 1 || got[1].Items[0] != "other" {
		t.Fatalf("got[1] = %+v, want unchanged {b b- [other]}", got[1])
	}
	// entries itself must be unmodified -- upsertEntry copies, like replaceEntry.
	if entries[0].Items[0] != "old" {
		t.Fatalf("original entries mutated: entries[0] = %+v", entries[0])
	}
}

func TestUpsertEntry_AppendsUnknownBackendName(t *testing.T) {
	entries := []router.Entry[string]{
		{BackendName: "a", Prefix: "a-", Items: []string{"x"}},
	}
	got := upsertEntry(entries, "new-backend", "nb-", []string{"y", "z"})
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[1].BackendName != "new-backend" || got[1].Prefix != "nb-" || len(got[1].Items) != 2 || got[1].Items[0] != "y" || got[1].Items[1] != "z" {
		t.Fatalf("got[1] = %+v, want {new-backend nb- [y z]}", got[1])
	}
}

func TestUpsertEntry_EmptyEntriesAppends(t *testing.T) {
	got := upsertEntry[string](nil, "only", "o-", []string{"a"})
	if len(got) != 1 || got[0].BackendName != "only" || got[0].Prefix != "o-" || len(got[0].Items) != 1 || got[0].Items[0] != "a" {
		t.Fatalf("got = %+v, want one entry {only o- [a]}", got)
	}
}
