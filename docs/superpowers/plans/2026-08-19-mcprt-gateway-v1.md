# mcprt Gateway v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `mcprt`, a Go CLI that aggregates multiple MCP servers (local stdio subprocesses and remote HTTP servers) behind a single MCP gateway endpoint, resolving tool-name collisions by config order and explicit overrides.

**Architecture:** A single long-running process connects to all configured backends at startup using the official Go SDK's `mcp.Client`, merges their tool lists into a static routing table, and exposes a single `mcp.Server` (over stdio and/or Streamable HTTP) whose `tools/call` handlers forward transparently to the owning backend's `mcp.ClientSession`.

**Tech Stack:** Go 1.25, `github.com/modelcontextprotocol/go-sdk` v1.7.0 (MCP client/server), `github.com/spf13/cobra` v1.10.2 (CLI), `gopkg.in/yaml.v3` (config), `log/slog` (logging).

**Spec:** `docs/superpowers/specs/2026-08-19-mcprt-gateway-design.md`

## Global Constraints

- Go module path: `github.com/wtnb75/mcprt`
- Go version: 1.25 (matches the go-sdk's `go 1.25.0` requirement)
- MCP SDK: `github.com/modelcontextprotocol/go-sdk` v1.7.0 — use `mcp.Client`/`mcp.Server`, never hand-roll JSON-RPC framing
- CLI: `github.com/spf13/cobra` — the binary is subcommand-based from v1; the server is started via `mcprt server`, not a bare root command
- Config format: YAML, loaded from a path given via `--config`
- Logging: `log/slog` only, text handler to stderr, level controlled by `--log-level` (`debug`/`info`/`warn`/`error`, default `info`)
- v1 excludes: dynamic `list_changed` handling, `resources/*`/`prompts/*` proxying, `sampling/createMessage` reverse-proxying, backend auto-reconnect, gateway-side authentication — do not build these now
- Tool priority is **config file order only** (index 0 = highest); there is no numeric priority field
- `overrides` keys are **post-prefix (exposed) tool names**
- Backend connect/list-tools failures are best-effort (log and exclude that backend); config validation errors (bad transport, missing required field, `overrides` referencing an unknown backend) fail startup

---

## File Structure

```
go.mod
go.sum
Taskfile.yml
cmd/mcprt/
  main.go                        # entrypoint: signal handling + cli.Execute
internal/cli/
  root.go                        # NewRootCmd, Execute
  server.go                      # `server` subcommand: flags, wiring, runServer
  server_test.go
internal/config/
  config.go                      # Config, Load, Parse, validation, env expansion
  config_test.go
internal/router/
  router.go                      # Entry, Resolved, Conflict, Table, Resolve
  router_test.go
internal/backend/
  backend.go                     # Backend, Connect, ListTools, Close
  backend_test.go
  testdata/echoserver/main.go    # tiny stdio MCP server used by backend_test.go
internal/gateway/
  gateway.go                     # New, ServeStdio, ServeHTTP
  gateway_test.go
README.md
```

---

### Task 1: Project scaffolding

**Files:**
- Create: `go.mod`, `go.sum`
- Create: `Taskfile.yml`
- Create: `cmd/mcprt/main.go`
- Create: `internal/cli/root.go`

**Interfaces:**
- Produces: `cli.NewRootCmd() *cobra.Command`, `cli.Execute(ctx context.Context, args []string) error`

- [ ] **Step 1: Initialize the module and add dependencies**

Run:
```bash
cd /Users/watanabe/x/copilot-cli/project-mcprt
go mod init github.com/wtnb75/mcprt
go get github.com/modelcontextprotocol/go-sdk@v1.7.0
go get github.com/spf13/cobra@v1.10.2
go get gopkg.in/yaml.v3@v3.0.1
```
Expected: `go.mod` and `go.sum` are created/updated with no errors.

- [ ] **Step 2: Write `internal/cli/root.go`**

```go
package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// NewRootCmd builds the mcprt root command. Subcommands are attached to it
// (mcprt is subcommand-based from v1, even though "server" is currently the
// only one).
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "mcprt",
		Short: "mcprt aggregates multiple MCP servers behind a single gateway",
	}
	root.AddCommand(newServerCmd())
	return root
}

// Execute runs the mcprt CLI with the given arguments (typically
// os.Args[1:]) and returns its error, if any.
func Execute(ctx context.Context, args []string) error {
	root := NewRootCmd()
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}
```

This references `newServerCmd`, which does not exist yet — add a placeholder in `internal/cli/server.go` so the package compiles:

```go
package cli

import "github.com/spf13/cobra"

func newServerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "run the mcprt gateway server",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
}
```

(Task 6 replaces this placeholder body with the real implementation.)

- [ ] **Step 3: Write `cmd/mcprt/main.go`**

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/wtnb75/mcprt/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := cli.Execute(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Write `Taskfile.yml`**

```yaml
version: '3'

tasks:
  build:
    cmds:
      - go build -o bin/mcprt ./cmd/mcprt

  test:
    cmds:
      - go test ./...

  lint:
    cmds:
      - gofmt -l .
      - go vet ./...
      - golangci-lint run ./...

  fmt:
    cmds:
      - gofmt -w .
```

- [ ] **Step 5: Verify the skeleton builds and runs**

Run:
```bash
go build ./...
go run ./cmd/mcprt --help
go run ./cmd/mcprt server --help
```
Expected: all three commands succeed; the first `--help` lists a `server` subcommand, the second shows the (currently empty) server command help.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum Taskfile.yml cmd/mcprt/main.go internal/cli/root.go internal/cli/server.go
git commit -m "feat: scaffold mcprt CLI with cobra root and server subcommand stub"
```

---

### Task 2: `internal/config`

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:
  - `type Config struct { Listen ListenConfig; Backends []BackendConfig; Overrides map[string]string }`
  - `type ListenConfig struct { Stdio bool; HTTP string }`
  - `type BackendConfig struct { Name, Transport string; Command []string; Env map[string]string; URL string; Headers map[string]string; Prefix string }`
  - `func Load(path string) (*Config, error)`
  - `func Parse(data []byte) (*Config, error)`

- [ ] **Step 1: Write the failing tests**

```go
package config_test

import (
	"testing"

	"github.com/wtnb75/mcprt/internal/config"
)

func TestParse_ValidConfig(t *testing.T) {
	data := []byte(`
listen:
  stdio: true
  http: ":8080"

backends:
  - name: filesystem
    transport: stdio
    command: ["mcp-server-filesystem", "--root", "/data"]
    env:
      FOO: bar
  - name: github
    transport: http
    url: "http://localhost:9090/mcp"
    headers:
      Authorization: "Bearer ${TEST_GITHUB_TOKEN}"
    prefix: "gh__"

overrides:
  gh__search: github
`)
	t.Setenv("TEST_GITHUB_TOKEN", "secret123")

	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if !cfg.Listen.Stdio || cfg.Listen.HTTP != ":8080" {
		t.Fatalf("Listen = %+v, want stdio=true http=\":8080\"", cfg.Listen)
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("len(Backends) = %d, want 2", len(cfg.Backends))
	}
	fs := cfg.Backends[0]
	if fs.Name != "filesystem" || fs.Transport != "stdio" || len(fs.Command) != 3 || fs.Env["FOO"] != "bar" {
		t.Fatalf("Backends[0] = %+v, unexpected", fs)
	}
	gh := cfg.Backends[1]
	if gh.Prefix != "gh__" || gh.Headers["Authorization"] != "Bearer secret123" {
		t.Fatalf("Backends[1] = %+v, want expanded Authorization header", gh)
	}
	if cfg.Overrides["gh__search"] != "github" {
		t.Fatalf("Overrides[gh__search] = %q, want %q", cfg.Overrides["gh__search"], "github")
	}
}

func TestParse_DuplicateBackendName(t *testing.T) {
	data := []byte(`
backends:
  - name: dup
    transport: stdio
    command: ["a"]
  - name: dup
    transport: stdio
    command: ["b"]
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for duplicate backend name, got nil")
	}
}

func TestParse_UnknownTransport(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: carrier-pigeon
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for unknown transport, got nil")
	}
}

func TestParse_StdioMissingCommand(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: stdio
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for stdio backend with no command, got nil")
	}
}

func TestParse_HTTPMissingURL(t *testing.T) {
	data := []byte(`
backends:
  - name: bad
    transport: http
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for http backend with no url, got nil")
	}
}

func TestParse_OverrideReferencesUnknownBackend(t *testing.T) {
	data := []byte(`
backends:
  - name: known
    transport: stdio
    command: ["a"]

overrides:
  search: nonexistent
`)
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for override referencing unknown backend, got nil")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/... -v`
Expected: FAIL — `package config: cannot find package` / undefined `config.Parse` (the package doesn't exist yet).

- [ ] **Step 3: Write `internal/config/config.go`**

```go
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level gateway configuration, loaded from a YAML file.
type Config struct {
	Listen    ListenConfig      `yaml:"listen"`
	Backends  []BackendConfig   `yaml:"backends"`
	Overrides map[string]string `yaml:"overrides"`
}

// ListenConfig controls which client-facing transports the gateway serves.
type ListenConfig struct {
	Stdio bool   `yaml:"stdio"`
	HTTP  string `yaml:"http"`
}

// BackendConfig describes one backend MCP server to connect to.
type BackendConfig struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"` // "stdio" or "http"
	Command   []string          `yaml:"command"`
	Env       map[string]string `yaml:"env"`
	URL       string            `yaml:"url"`
	Headers   map[string]string `yaml:"headers"`
	Prefix    string            `yaml:"prefix"`
}

// Load reads and parses the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	return Parse(data)
}

// Parse parses YAML config data, expands ${VAR} references in backend env
// and header values, and validates the result.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}
	expandEnvRefs(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func expandEnvRefs(cfg *Config) {
	for i := range cfg.Backends {
		for k, v := range cfg.Backends[i].Env {
			cfg.Backends[i].Env[k] = os.Expand(v, os.Getenv)
		}
		for k, v := range cfg.Backends[i].Headers {
			cfg.Backends[i].Headers[k] = os.Expand(v, os.Getenv)
		}
	}
}

func validate(cfg *Config) error {
	names := make(map[string]bool, len(cfg.Backends))
	for _, b := range cfg.Backends {
		if b.Name == "" {
			return fmt.Errorf("backend has empty name")
		}
		if names[b.Name] {
			return fmt.Errorf("duplicate backend name: %q", b.Name)
		}
		names[b.Name] = true

		switch b.Transport {
		case "stdio":
			if len(b.Command) == 0 {
				return fmt.Errorf("backend %q: stdio transport requires command", b.Name)
			}
		case "http":
			if b.URL == "" {
				return fmt.Errorf("backend %q: http transport requires url", b.Name)
			}
		default:
			return fmt.Errorf("backend %q: unknown transport %q (must be \"stdio\" or \"http\")", b.Name, b.Transport)
		}
	}

	for toolName, backendName := range cfg.Overrides {
		if !names[backendName] {
			return fmt.Errorf("override %q references unknown backend %q", toolName, backendName)
		}
	}

	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: PASS for all six tests.

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat: add config package with YAML loading and validation"
```

---

### Task 3: `internal/router`

**Files:**
- Create: `internal/router/router.go`
- Test: `internal/router/router_test.go`

**Interfaces:**
- Consumes: `*mcp.Tool` (`Name string`, `InputSchema any`) from `github.com/modelcontextprotocol/go-sdk/mcp`
- Produces:
  - `type Entry struct { BackendName, Prefix string; Tools []*mcp.Tool }`
  - `type Resolved struct { Tool *mcp.Tool; BackendName, OriginalName string }`
  - `type Conflict struct { ExposedName, Winner string; Losers []string }`
  - `type Table struct { Tools map[string]*Resolved; Conflicts []Conflict }`
  - `func Resolve(entries []Entry, overrides map[string]string) *Table`

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/router/... -v`
Expected: FAIL — `undefined: router.Resolve` and related.

- [ ] **Step 3: Write `internal/router/router.go`**

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/router/... -v`
Expected: PASS for all five tests.

- [ ] **Step 5: Commit**

```bash
git add internal/router
git commit -m "feat: add router package for tool merge and conflict resolution"
```

---

### Task 4: `internal/backend`

**Files:**
- Create: `internal/backend/backend.go`
- Test: `internal/backend/backend_test.go`
- Create: `internal/backend/testdata/echoserver/main.go`

**Interfaces:**
- Consumes: `config.BackendConfig` (Task 2), `mcp.Client`/`mcp.ClientSession`/`mcp.CommandTransport`/`mcp.StreamableClientTransport` (go-sdk)
- Produces:
  - `type Backend struct { Name, Prefix string; Session *mcp.ClientSession }`
  - `func Connect(ctx context.Context, cfg config.BackendConfig) (*Backend, error)`
  - `func (b *Backend) ListTools(ctx context.Context) ([]*mcp.Tool, error)`
  - `func (b *Backend) Close() error`

- [ ] **Step 1: Write the test stdio server used by the stdio integration test**

```go
// internal/backend/testdata/echoserver/main.go
package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Message string `json:"message"`
}

type echoOutput struct {
	Message string `json:"message"`
}

func echo(ctx context.Context, req *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
	return nil, echoOutput{Message: in.Message}, nil
}

func main() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "echoserver", Version: "v1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echoes the message"}, echo)
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
```

This is a normal `package main`; `go test` in `internal/backend` will invoke it via `go run ./testdata/echoserver`, with the test's working directory as `internal/backend`.

- [ ] **Step 2: Write the failing tests**

```go
// internal/backend/backend_test.go
package backend_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
)

func TestConnect_Stdio(t *testing.T) {
	ctx := context.Background()
	b, err := backend.Connect(ctx, config.BackendConfig{
		Name:      "echo",
		Transport: "stdio",
		Command:   []string{"go", "run", "./testdata/echoserver"},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	tools, err := b.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools = %v, want a single tool named \"echo\"", tools)
	}
}

func TestConnect_HTTPWithHeaders(t *testing.T) {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	mcp.AddTool(fakeServer, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil)

	var gotAuth string
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		mcpHandler.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	ctx := context.Background()
	b, err := backend.Connect(ctx, config.BackendConfig{
		Name:      "fake",
		Transport: "http",
		URL:       srv.URL,
		Headers:   map[string]string{"Authorization": "Bearer test-token"},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer test-token")
	}
}

func TestConnect_UnknownTransport(t *testing.T) {
	_, err := backend.Connect(context.Background(), config.BackendConfig{Name: "bad", Transport: "carrier-pigeon"})
	if err == nil {
		t.Fatal("Connect: expected error for unknown transport, got nil")
	}
}
```

```go
// internal/backend/backend_internal_test.go (whitebox: tests unexported helpers)
package backend

import (
	"net/http"
	"slices"
	"testing"
)

func TestEnvWithOverrides(t *testing.T) {
	t.Setenv("MCPRT_TEST_BASE", "base-value")
	got := envWithOverrides(map[string]string{"EXTRA": "extra-value"})
	if !slices.Contains(got, "MCPRT_TEST_BASE=base-value") {
		t.Fatalf("envWithOverrides did not preserve base environment: %v", got)
	}
	if !slices.Contains(got, "EXTRA=extra-value") {
		t.Fatalf("envWithOverrides did not include extra entry: %v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestHeaderRoundTripper(t *testing.T) {
	var gotHeader string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotHeader = req.Header.Get("Authorization")
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	rt := headerRoundTripper{headers: map[string]string{"Authorization": "Bearer xyz"}, base: base}
	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if gotHeader != "Bearer xyz" {
		t.Fatalf("Authorization header = %q, want %q", gotHeader, "Bearer xyz")
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/backend/... -v`
Expected: FAIL — package `backend` does not exist yet.

- [ ] **Step 4: Write `internal/backend/backend.go`**

```go
package backend

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/config"
)

// Backend is a live connection to one backend MCP server.
type Backend struct {
	Name    string
	Prefix  string
	Session *mcp.ClientSession
}

// Connect starts (for stdio) or dials (for http) the backend described by
// cfg, and performs the MCP initialize handshake.
func Connect(ctx context.Context, cfg config.BackendConfig) (*Backend, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "mcprt", Version: "v1"}, nil)

	var transport mcp.Transport
	switch cfg.Transport {
	case "stdio":
		cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
		cmd.Env = envWithOverrides(cfg.Env)
		transport = &mcp.CommandTransport{Command: cmd}
	case "http":
		transport = &mcp.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: &http.Client{Transport: headerRoundTripper{headers: cfg.Headers, base: http.DefaultTransport}},
		}
	default:
		return nil, fmt.Errorf("backend %q: unknown transport %q", cfg.Name, cfg.Transport)
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("backend %q: connect: %w", cfg.Name, err)
	}

	return &Backend{Name: cfg.Name, Prefix: cfg.Prefix, Session: session}, nil
}

// ListTools fetches the backend's full tool list, following pagination.
func (b *Backend) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	for t, err := range b.Session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("backend %q: list tools: %w", b.Name, err)
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// Close closes the connection to the backend.
func (b *Backend) Close() error {
	return b.Session.Close()
}

// envWithOverrides returns the current process environment plus extra,
// suitable for exec.Cmd.Env (a stdio backend subprocess should inherit the
// gateway's environment, not replace it).
func envWithOverrides(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// headerRoundTripper injects fixed headers (e.g. an Authorization token)
// into every outgoing request to a remote HTTP backend.
type headerRoundTripper struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/backend/... -v`
Expected: PASS for all five tests (the stdio test takes longer than the others since it shells out to `go run`).

- [ ] **Step 6: Commit**

```bash
git add internal/backend
git commit -m "feat: add backend package for stdio/http MCP client connections"
```

---

### Task 5: `internal/gateway`

**Files:**
- Create: `internal/gateway/gateway.go`
- Test: `internal/gateway/gateway_test.go`

**Interfaces:**
- Consumes: `router.Table`/`router.Resolved` (Task 3), `backend.Backend` (Task 4), `mcp.Server`/`mcp.ToolHandler`/`mcp.NewStreamableHTTPHandler`/`mcp.StdioTransport` (go-sdk)
- Produces:
  - `func New(logger *slog.Logger, backends map[string]*backend.Backend, table *router.Table) *mcp.Server`
  - `func ServeStdio(ctx context.Context, srv *mcp.Server) error`
  - `func ServeHTTP(ctx context.Context, srv *mcp.Server, addr string) error`

- [ ] **Step 1: Write the failing test**

```go
// internal/gateway/gateway_test.go
package gateway_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
	"github.com/wtnb75/mcprt/internal/router"
)

type sourceOutput struct {
	Source string `json:"source"`
}

func newFakeBackendServer(name string, toolNames ...string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, nil)
	for _, toolName := range toolNames {
		mcp.AddTool(srv, &mcp.Tool{Name: toolName, Description: "fake tool " + toolName},
			func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, sourceOutput, error) {
				return nil, sourceOutput{Source: name}, nil
			})
	}
	return srv
}

func TestGateway_RoutesByPriorityAndExposesUniqueTools(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backendAServer := newFakeBackendServer("backend-a", "search", "unique_a")
	backendBServer := newFakeBackendServer("backend-b", "search", "unique_b")

	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendAServer }, nil))
	defer httpA.Close()
	httpB := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendBServer }, nil))
	defer httpB.Close()

	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL})
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}
	defer func() { _ = connA.Close() }()
	connB, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-b", Transport: "http", URL: httpB.URL})
	if err != nil {
		t.Fatalf("connect backend-b: %v", err)
	}
	defer func() { _ = connB.Close() }()

	toolsA, err := connA.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-a tools: %v", err)
	}
	toolsB, err := connB.ListTools(ctx)
	if err != nil {
		t.Fatalf("list backend-b tools: %v", err)
	}

	table := router.Resolve([]router.Entry{
		{BackendName: "backend-a", Tools: toolsA},
		{BackendName: "backend-b", Tools: toolsB},
	}, nil)

	if len(table.Conflicts) != 1 || table.Conflicts[0].ExposedName != "search" || table.Conflicts[0].Winner != "backend-a" {
		t.Fatalf("unexpected conflicts: %+v", table.Conflicts)
	}

	srv := gateway.New(logger, map[string]*backend.Backend{"backend-a": connA, "backend-b": connB}, table)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer gw.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	var names []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list gateway tools: %v", err)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"search", "unique_a", "unique_b"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools/list = %v, want %v", names, want)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call search: %v", err)
	}
	structured, ok := res.StructuredContent.(map[string]any)
	if !ok || structured["source"] != "backend-a" {
		t.Fatalf("search result = %+v, want structured content with source=backend-a", res.StructuredContent)
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "unique_b", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call unique_b: %v", err)
	}
	structured, ok = res.StructuredContent.(map[string]any)
	if !ok || structured["source"] != "backend-b" {
		t.Fatalf("unique_b result = %+v, want structured content with source=backend-b", res.StructuredContent)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/gateway/... -v`
Expected: FAIL — package `gateway` does not exist yet.

- [ ] **Step 3: Write `internal/gateway/gateway.go`**

```go
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/router"
)

// New builds an mcp.Server that exposes table's resolved tools, forwarding
// each tools/call to the backend that owns it. backends must contain an
// entry for every BackendName referenced in table (the caller builds both
// from the same set of connected backends).
func New(logger *slog.Logger, backends map[string]*backend.Backend, table *router.Table) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, &mcp.ServerOptions{Logger: logger})

	for _, resolved := range table.Tools {
		b := backends[resolved.BackendName]
		addTool(srv, logger, resolved.Tool, callHandler(b, resolved.OriginalName))
	}

	return srv
}

// callHandler forwards a tools/call to originalName on backend b, passing
// the raw arguments through unchanged.
func callHandler(b *backend.Backend, originalName string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return b.Session.CallTool(ctx, &mcp.CallToolParams{
			Name:      originalName,
			Arguments: req.Params.Arguments,
		})
	}
}

// addTool registers t on srv, recovering from AddTool's panic on a
// malformed schema so that one broken backend tool definition can't take
// down the whole gateway process at startup.
func addTool(srv *mcp.Server, logger *slog.Logger, t *mcp.Tool, h mcp.ToolHandler) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("skipping tool: invalid definition", "tool", t.Name, "error", r)
		}
	}()
	srv.AddTool(t, h)
}

// ServeStdio runs srv over stdin/stdout until ctx is cancelled or the
// client disconnects.
func ServeStdio(ctx context.Context, srv *mcp.Server) error {
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// ServeHTTP runs srv as a Streamable HTTP server listening on addr, until
// ctx is cancelled.
func ServeHTTP(ctx context.Context, srv *mcp.Server, addr string) error {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	httpServer := &http.Server{Addr: addr, Handler: handler}

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	select {
	case <-ctx.Done():
		return httpServer.Shutdown(context.Background())
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/gateway/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway
git commit -m "feat: add gateway package wiring router table to an mcp.Server"
```

---

### Task 6: `internal/cli` server subcommand

**Files:**
- Modify: `internal/cli/server.go` (replace the Task 1 placeholder)
- Test: `internal/cli/server_test.go`

**Interfaces:**
- Consumes: `config.Load` (Task 2), `router.Resolve`/`router.Entry` (Task 3), `backend.Connect`/`backend.ListTools`/`backend.Close` (Task 4), `gateway.New`/`gateway.ServeStdio`/`gateway.ServeHTTP` (Task 5)
- Produces: `cli.Execute` behavior for `mcprt server --config <path> [--log-level <level>]` (signature unchanged from Task 1)

- [ ] **Step 1: Write the failing tests**

```go
// internal/cli/server_test.go
package cli_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/cli"
)

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing probe listener: %v", err)
	}
	return addr
}

func writeConfig(t *testing.T, yamlContent string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestServerCommand_ServesAggregatedTools(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, nil)
	mcp.AddTool(backendSrv, &mcp.Tool{Name: "ping", Description: "ping"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: fake
    transport: http
    url: %q
`, gatewayAddr, backendHTTP.URL))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + gatewayAddr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}

	var names []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if len(names) != 1 || names[0] != "ping" {
		t.Fatalf("tools/list = %v, want [ping]", names)
	}
	_ = session.Close()

	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}

func TestServerCommand_NoListenerConfigured(t *testing.T) {
	configPath := writeConfig(t, "backends: []\n")

	err := cli.Execute(context.Background(), []string{"server", "--config", configPath})
	if err == nil {
		t.Fatal("Execute: expected error when no listener is configured, got nil")
	}
}
```

```go
// internal/cli/server_internal_test.go (whitebox: tests unexported helper)
package cli

import (
	"log/slog"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for s, want := range cases {
		got, err := parseLogLevel(s)
		if err != nil {
			t.Fatalf("parseLogLevel(%q): %v", s, err)
		}
		if got != want {
			t.Fatalf("parseLogLevel(%q) = %v, want %v", s, got, want)
		}
	}
	if _, err := parseLogLevel("bogus"); err == nil {
		t.Fatal("parseLogLevel(\"bogus\"): expected error, got nil")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/... -v`
Expected: FAIL to build — `server_internal_test.go` references `parseLogLevel`, which the Task 1 placeholder doesn't define yet (`undefined: parseLogLevel`), so the whole `internal/cli` test binary fails to compile and no test in the package runs.

- [ ] **Step 3: Replace `internal/cli/server.go`**

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/spf13/cobra"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
	"github.com/wtnb75/mcprt/internal/router"
)

func newServerCmd() *cobra.Command {
	var configPath string
	var logLevel string

	// SilenceUsage/SilenceErrors: don't dump flag usage on a runtime error
	// (bad config, backend failure, etc.), and let cli.Execute's caller
	// (main.go) be the one place that prints the error.
	cmd := &cobra.Command{
		Use:           "server",
		Short:         "run the mcprt gateway server",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			level, err := parseLogLevel(logLevel)
			if err != nil {
				return err
			}
			logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))
			return runServer(cmd.Context(), logger, configPath)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to the gateway config file (required)")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, error")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err) // programmer error: "config" flag name must match Flags().StringVar above
	}

	return cmd
}

func parseLogLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (want debug, info, warn, or error)", s)
	}
}

func runServer(ctx context.Context, logger *slog.Logger, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if !cfg.Listen.Stdio && cfg.Listen.HTTP == "" {
		return errors.New("no listener configured: enable listen.stdio or set listen.http")
	}

	backends, entries := connectBackends(ctx, logger, cfg.Backends)
	defer func() {
		for _, b := range backends {
			_ = b.Close()
		}
	}()

	table := router.Resolve(entries, cfg.Overrides)
	for _, c := range table.Conflicts {
		logger.Warn("tool name conflict", "tool", c.ExposedName, "winner", c.Winner, "hidden", c.Losers)
	}

	srv := gateway.New(logger, backends, table)

	running := 0
	errCh := make(chan error, 2)
	if cfg.Listen.Stdio {
		running++
		go func() { errCh <- gateway.ServeStdio(ctx, srv) }()
	}
	if cfg.Listen.HTTP != "" {
		running++
		go func() { errCh <- gateway.ServeHTTP(ctx, srv, cfg.Listen.HTTP) }()
	}

	var firstErr error
	for i := 0; i < running; i++ {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// connectBackends connects to every configured backend concurrently and
// lists its tools. A backend that fails to connect or list tools is logged
// and excluded (best-effort); it does not fail the whole startup. The
// returned entries preserve configs' order, since router.Resolve treats
// that order as priority (index 0 = highest).
func connectBackends(ctx context.Context, logger *slog.Logger, configs []config.BackendConfig) (map[string]*backend.Backend, []router.Entry) {
	type outcome struct {
		backend *backend.Backend
		entry   router.Entry
	}
	outcomes := make([]*outcome, len(configs))

	var wg sync.WaitGroup
	for i, bc := range configs {
		wg.Add(1)
		go func(i int, bc config.BackendConfig) {
			defer wg.Done()
			b, err := backend.Connect(ctx, bc)
			if err != nil {
				logger.Error("skipping backend: connect failed", "backend", bc.Name, "error", err)
				return
			}
			tools, err := b.ListTools(ctx)
			if err != nil {
				logger.Error("skipping backend: list tools failed", "backend", bc.Name, "error", err)
				_ = b.Close()
				return
			}
			outcomes[i] = &outcome{
				backend: b,
				entry:   router.Entry{BackendName: bc.Name, Prefix: bc.Prefix, Tools: tools},
			}
		}(i, bc)
	}
	wg.Wait()

	backends := make(map[string]*backend.Backend, len(configs))
	entries := make([]router.Entry, 0, len(configs))
	for _, o := range outcomes {
		if o == nil {
			continue
		}
		backends[o.entry.BackendName] = o.backend
		entries = append(entries, o.entry)
	}
	return backends, entries
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/... -v`
Expected: PASS for both tests in `server_test.go` and `TestParseLogLevel`.

- [ ] **Step 5: Commit**

```bash
git add internal/cli
git commit -m "feat: wire up mcprt server subcommand end-to-end"
```

---

### Task 7: Final polish

**Files:**
- Create: `README.md`
- Modify: none (verification only)

**Interfaces:**
- Consumes: everything from Tasks 1-6
- Produces: nothing new; this task only verifies and documents

- [ ] **Step 1: Write `README.md`**

```markdown
# mcprt

mcprt aggregates multiple MCP servers (local stdio subprocesses and remote
HTTP servers) behind a single MCP gateway endpoint.

## Usage

    mcprt server --config config.yaml [--log-level info]

## Configuration

See `docs/superpowers/specs/2026-08-19-mcprt-gateway-design.md` for the full
design, including the config file format and conflict-resolution rules.

Minimal example:

    listen:
      stdio: true
      http: ":8080"

    backends:
      - name: filesystem
        transport: stdio
        command: ["mcp-server-filesystem", "--root", "/data"]

      - name: github
        transport: http
        url: "http://localhost:9090/mcp"
        headers:
          Authorization: "Bearer ${GITHUB_TOKEN}"
        prefix: "gh__"

    overrides:
      gh__search: github

## Development

    task build   # build ./bin/mcprt
    task test    # go test ./...
    task lint    # gofmt -l . && go vet ./... && golangci-lint run ./...
    task fmt     # gofmt -w .
```

- [ ] **Step 2: Run the full test suite**

Run: `go test ./...`
Expected: PASS for every package.

- [ ] **Step 3: Run formatting and linting**

Run:
```bash
gofmt -l .
go vet ./...
golangci-lint run ./...
```
Expected: `gofmt -l .` prints nothing (no unformatted files); `go vet` and `golangci-lint run` report no issues. Fix anything they flag before continuing — do not disable checks to make them pass.

- [ ] **Step 4: Manual smoke test with two real local backends**

Run:
```bash
go build -o bin/mcprt ./cmd/mcprt
cat > /tmp/mcprt-smoke.yaml <<'EOF'
listen:
  stdio: true

backends:
  - name: echo
    transport: stdio
    command: ["go", "run", "./internal/backend/testdata/echoserver"]
EOF
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' | ./bin/mcprt server --config /tmp/mcprt-smoke.yaml --log-level debug
```
Expected: the process logs connecting to the `echo` backend and responds on stdout with an `initialize` result (press Ctrl+C to stop it; a hung process waiting for more stdin input after printing the response is normal — that confirms the server is alive and serving).

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: add mcprt README with usage and development commands"
```
