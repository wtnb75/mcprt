package cli

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
)

// waitForPrompt polls session.GetPrompt(name) until it succeeds/fails
// matching want, or deadline passes -- watchSkillDir's debounce plus
// fsnotify's own OS-level delivery latency mean a file-system change is
// never visible to the gateway synchronously.
func waitForPromptPresence(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, wantPresent bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: name})
		if (err == nil) == wantPresent {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("prompt %q presence never became %v within the deadline", name, wantPresent)
}

func TestWatchSkillDir_PicksUpAddedChangedAndRemovedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.md"), []byte("keep body v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initial, err := config.ScanSkillDir(dir)
	if err != nil {
		t.Fatalf("ScanSkillDir: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var staticPrompts []*gateway.StaticPrompt
	for _, p := range initial {
		sp, err := buildOneStaticPrompt(p, 0)
		if err != nil {
			t.Fatalf("buildOneStaticPrompt: %v", err)
		}
		staticPrompts = append(staticPrompts, sp)
	}
	srv := gateway.New(gateway.NewConfig{Logger: logger, Backends: map[string]*backend.Backend{}, StaticPrompts: staticPrompts})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchSkillDir(ctx, logger, dir, 0, initial, srv)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	// Add a new file.
	if err := os.WriteFile(filepath.Join(dir, "added.md"), []byte("added body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForPromptPresence(t, ctx, session, "added", true)

	// Change an existing file's content.
	if err := os.WriteFile(filepath.Join(dir, "keep.md"), []byte("keep body v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "keep"})
		if err == nil {
			if text, ok := res.Messages[0].Content.(*mcp.TextContent); ok && text.Text == "keep body v2\n" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("prompt \"keep\" never picked up its updated body within the deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Remove a file.
	if err := os.Remove(filepath.Join(dir, "added.md")); err != nil {
		t.Fatal(err)
	}
	waitForPromptPresence(t, ctx, session, "added", false)
}

func TestWatchSkillDir_SkipsUnparseableEditWithoutRemovingOthers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "good.md"), []byte("good body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initial, err := config.ScanSkillDir(dir)
	if err != nil {
		t.Fatalf("ScanSkillDir: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sp, err := buildOneStaticPrompt(initial[0], 0)
	if err != nil {
		t.Fatalf("buildOneStaticPrompt: %v", err)
	}
	srv := gateway.New(gateway.NewConfig{Logger: logger, Backends: map[string]*backend.Backend{}, StaticPrompts: []*gateway.StaticPrompt{sp}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchSkillDir(ctx, logger, dir, 0, initial, srv)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	// Add a file with an unterminated template action -- buildOneStaticPrompt
	// must reject it, and that rejection must not remove "good".
	if err := os.WriteFile(filepath.Join(dir, "bad.md"), []byte("{{.unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// There is no positive event to wait for here (the bad file is never
	// meant to become visible), so wait out one debounce window instead.
	time.Sleep(skillDirDebounce * 3)

	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "good"}); err != nil {
		t.Fatalf("GetPrompt(good): %v, want it still registered after an unrelated bad file appeared", err)
	}
	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "bad"}); err == nil {
		t.Fatal("GetPrompt(bad) succeeded, want it rejected (unterminated template action)")
	}
}

// TestWatchSkillDir_RejectedNameIsRetriedOnLaterRescan is the final-review
// fix's regression test for findings 2+3 (internal/gateway/reconcile.go's
// promptOwner zero-value bug, and watchSkillDir's `last` map recording
// ownership-conflict losers as if they'd been applied).
//
// dirHigh (entryIndex 0, the higher-priority entry -- a smaller EntryIndex
// wins, see StaticPrompt.EntryIndex) starts out already owning "shared".
// dirLow (entryIndex 1) then creates its own "shared.md" and must lose the
// ownership race. Before this fix, watchSkillDir would still record
// dirLow's rejected "shared" into its `last` map as if UpdateDirPrompts had
// accepted it -- so once dirHigh's file is removed (freeing the name),
// dirLow would never retry claiming it unless "shared.md"'s own content
// changed again (reflect.DeepEqual(old, p) would keep reporting
// "unchanged", so the name is never re-offered to UpdateDirPrompts). This
// test proves dirLow reclaims "shared" automatically on its very next
// rescan -- triggered here by an unrelated file appearing in dirLow, not by
// any edit to shared.md itself.
func TestWatchSkillDir_RejectedNameIsRetriedOnLaterRescan(t *testing.T) {
	dirHigh := t.TempDir()
	dirLow := t.TempDir()

	if err := os.WriteFile(filepath.Join(dirHigh, "shared.md"), []byte("high priority body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initialHigh, err := config.ScanSkillDir(dirHigh)
	if err != nil {
		t.Fatalf("ScanSkillDir(dirHigh): %v", err)
	}
	initialLow, err := config.ScanSkillDir(dirLow)
	if err != nil {
		t.Fatalf("ScanSkillDir(dirLow): %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var staticPrompts []*gateway.StaticPrompt
	for _, p := range initialHigh {
		sp, err := buildOneStaticPrompt(p, 0)
		if err != nil {
			t.Fatalf("buildOneStaticPrompt: %v", err)
		}
		staticPrompts = append(staticPrompts, sp)
	}
	srv := gateway.New(gateway.NewConfig{Logger: logger, Backends: map[string]*backend.Backend{}, StaticPrompts: staticPrompts})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchSkillDir(ctx, logger, dirHigh, 0, initialHigh, srv)
	go watchSkillDir(ctx, logger, dirLow, 1, initialLow, srv)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	// dirLow tries to claim the same name; it must lose the race.
	if err := os.WriteFile(filepath.Join(dirLow, "shared.md"), []byte("low priority body, must be rejected\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No positive event to wait for (the rejected version must never
	// surface), so wait out a few debounce windows instead.
	time.Sleep(skillDirDebounce * 3)
	if res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "shared"}); err != nil {
		t.Fatalf("GetPrompt(shared): %v", err)
	} else if text, ok := res.Messages[0].Content.(*mcp.TextContent); !ok || text.Text != "high priority body\n" {
		t.Fatalf("shared prompt = %+v, want dirHigh's version to still win the ownership race", res.Messages[0].Content)
	}

	// Free the name: remove dirHigh's file.
	if err := os.Remove(filepath.Join(dirHigh, "shared.md")); err != nil {
		t.Fatal(err)
	}
	waitForPromptPresence(t, ctx, session, "shared", false)

	// Trigger a rescan of dirLow via an UNRELATED file, not by touching
	// shared.md -- proving the retry happens on any rescan of that
	// directory, not only when the rejected file's own content changes.
	if err := os.WriteFile(filepath.Join(dirLow, "unrelated.md"), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "shared"})
		if err == nil {
			if text, ok := res.Messages[0].Content.(*mcp.TextContent); ok && text.Text == "low priority body, must be rejected\n" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("dirLow's \"shared\" was never reclaimed after dirHigh's copy was removed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
