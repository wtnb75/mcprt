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
